//go:build integration

package authn_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/mailer"
	"github.com/is-koala/koala-api/internal/platform/sms"
)

const (
	phoneN1 = "+905321234567"
	phoneN2 = "+905329876543"
)

// textbox is the SMS provider of these tests: it keeps what it is given,
// or fails every send with refuse.
type textbox struct {
	mu     sync.Mutex
	msgs   []sms.Message
	refuse error
}

func (b *textbox) Send(_ context.Context, m sms.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse != nil {
		return b.refuse
	}
	b.msgs = append(b.msgs, m)
	return nil
}

func (b *textbox) to(number string) []sms.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []sms.Message
	for _, m := range b.msgs {
		if m.To == number {
			out = append(out, m)
		}
	}
	return out
}

func (b *textbox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

// verificationsEnv is accountsEnv with the verification flows on the same
// queue, mailbox and a textbox.
type verificationsEnv struct {
	*accountsEnv
	verifications *authn.Verifications
	texts         *textbox
}

func newVerificationsEnv(t *testing.T, textsPerHour int) *verificationsEnv {
	t.Helper()
	a := newAccountsEnv(t)
	queue, err := jobs.NewClient(a.db.Pool(), jobs.Config{Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	box := &textbox{}
	v, err := authn.NewVerifications(a.users, queue, a.mail, box, textsPerHour)
	if err != nil {
		t.Fatal(err)
	}
	return &verificationsEnv{accountsEnv: a, verifications: v, texts: box}
}

// principal is a signed-in caller; the flows only read its user.
func principal(id uuid.UUID) authn.UserPrincipal {
	return authn.UserPrincipal{UserID: id, SessionID: uuid.New(), DeviceKey: deviceW1}
}

// workCodes runs the verification jobs queued so far, as River's
// workers would, and marks each completed; it returns the error of the
// first job that failed.
func (v *verificationsEnv) workCodes(t *testing.T) error {
	t.Helper()
	ctx := t.Context()
	rows, err := v.db.Pool().Query(ctx, `
		SELECT id, kind, args FROM river_job
		WHERE kind IN ('verification_code', 'contact_changed') AND state = 'available'
		ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := pgx.CollectRows(rows, pgx.RowToStructByPos[queuedJob])
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range queued {
		var args struct {
			Ref      uuid.UUID `json:"ref"`
			Channel  string    `json:"channel"`
			User     uuid.UUID `json:"user"`
			Previous string    `json:"previous"`
		}
		if err := json.Unmarshal(j.Args, &args); err != nil {
			t.Fatal(err)
		}
		if j.Kind == "verification_code" {
			err = v.verifications.SendCode(ctx, args.Ref, args.Channel)
		} else {
			err = v.verifications.SendContactNotice(ctx, args.User, args.Channel, args.Previous)
		}
		if _, e := v.db.Pool().Exec(ctx, "UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1", j.ID); e != nil {
			t.Fatal(e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var sixDigits = regexp.MustCompile(`\b(\d{6})\b`)

// codeIn takes the code out of a text or mail body.
func codeIn(t *testing.T, body string) string {
	t.Helper()
	m := sixDigits.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no code in %q", body)
	}
	return m[1]
}

// start starts a verification for user and sends its code; it returns
// the verification and the code that arrived.
func (v *verificationsEnv) start(t *testing.T, user uuid.UUID, channel, target string) (authn.Verification, string) {
	t.Helper()
	ver, err := v.verifications.Start(t.Context(), principal(user), channel, target)
	if err != nil {
		t.Fatalf("Start(%s, %s) = %v", channel, target, err)
	}
	if err := v.workCodes(t); err != nil {
		t.Fatal(err)
	}
	if channel == authn.ChannelPhone {
		n, _ := authn.NormalizePhone(target)
		msgs := v.texts.to(n)
		if len(msgs) == 0 {
			t.Fatalf("no text to %s", n)
		}
		return ver, codeIn(t, msgs[len(msgs)-1].Text)
	}
	msgs := v.mail.to(strings.ToLower(strings.TrimSpace(target)))
	if len(msgs) == 0 {
		t.Fatalf("no mail to %s", target)
	}
	return ver, codeIn(t, msgs[len(msgs)-1].Text)
}

// wrong returns a code that is not code.
func wrong(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

// verificationRow is a verification as stored.
type verificationRow struct {
	CodeDigest     []byte
	FailedAttempts int32
	ConfirmedAt    *time.Time
	CanceledAt     *time.Time
	BurnedAt       *time.Time
}

func (v *verificationsEnv) row(t *testing.T, id uuid.UUID) verificationRow {
	t.Helper()
	var r verificationRow
	err := v.db.Pool().QueryRow(t.Context(), `
		SELECT code_digest, failed_attempts, confirmed_at, canceled_at, burned_at FROM user_verifications WHERE id = $1`, id).
		Scan(&r.CodeDigest, &r.FailedAttempts, &r.ConfirmedAt, &r.CanceledAt, &r.BurnedAt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// contact reads a user's address, number and flags.
func (v *verificationsEnv) contact(t *testing.T, id uuid.UUID) (email string, emailVerified bool, phone *string, phoneVerified bool) {
	t.Helper()
	err := v.db.Pool().QueryRow(t.Context(),
		"SELECT email, email_verified, phone, phone_verified FROM active_users WHERE id = $1", id).
		Scan(&email, &emailVerified, &phone, &phoneVerified)
	if err != nil {
		t.Fatal(err)
	}
	return email, emailVerified, phone, phoneVerified
}

// textCounter reads koala_verification_codes_total of the text channel
// for an outcome.
func (v *verificationsEnv) textCounter(t *testing.T, outcome string) float64 {
	t.Helper()
	families, err := v.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != authn.MetricVerificationCodes {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["channel"] == authn.ChannelPhone && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no %s{channel=%q,outcome=%q}", authn.MetricVerificationCodes, authn.ChannelPhone, outcome)
	return 0
}

// A code is six digits, minted by the job and stored only as a keyed
// digest: it is in no row of the verification table and in no job's
// arguments. It lives ten minutes from the start; after them the right
// code is refused (USR-008). The space is covered by
// TestCodeUsesTheWholeSpace.
func TestVerificationCodeIsDigestedAndLivesTenMinutes(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)

	ver, code := v.start(t, user, authn.ChannelPhone, "0532 123 45 67")
	if ver.Status != authn.StatusPending || ver.RemainingAttempts != authn.VerificationAttempts ||
		!ver.ExpiresAt.Equal(ver.CreatedAt.Add(authn.VerificationTTL)) || ver.MaskedTarget != "+90******4567" {
		t.Errorf("USR-008: started verification = %+v", ver)
	}
	if !regexp.MustCompile(`^\d{6}$`).MatchString(code) {
		t.Fatalf("USR-008: code %q is not six digits", code)
	}
	// Every stored value of the verification and of every job: none is
	// the code, and no job has a code member.
	for name, q := range map[string]string{
		"user_verifications": "SELECT to_jsonb(v) FROM user_verifications v",
		"river_job":          "SELECT args FROM river_job",
	} {
		rows, err := v.db.Pool().Query(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		docs, err := pgx.CollectRows(rows, pgx.RowTo[map[string]any])
		if err != nil {
			t.Fatal(err)
		}
		if len(docs) == 0 {
			t.Fatalf("USR-008: no rows in %s to search", name)
		}
		for _, doc := range docs {
			for k, val := range doc {
				if s, ok := val.(string); (ok && s == code) || k == "code" {
					t.Errorf("USR-008: %s stores the code in plain text (%s)", name, k)
				}
			}
		}
	}
	d := v.row(t, ver.ID).CodeDigest
	plain := sha256.Sum256([]byte(code))
	if len(d) != 32 || bytes.Equal(d, plain[:]) {
		t.Errorf("USR-008: the stored digest is %d bytes or the code's plain SHA-256", len(d))
	}

	v.clock.Advance(authn.VerificationTTL)
	_, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, code)
	wantErr(t, err, apperr.KindInvalid, authn.CodeVerificationExpired)
	if _, _, phone, _ := v.contact(t, user); phone != nil {
		t.Errorf("USR-008: an expired code wrote the number %v", *phone)
	}
}

// Five wrong codes burn the verification: each is refused and spends one
// attempt, and then even the right code is refused; a new verification
// can be started (USR-009).
func TestVerificationBurnsAfterFiveWrongCodes(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	ver, code := v.start(t, user, authn.ChannelPhone, phoneN1)

	for i := range authn.VerificationAttempts {
		_, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, wrong(code))
		wantErr(t, err, apperr.KindInvalid, authn.CodeVerificationWrong)
		if got := v.row(t, ver.ID).FailedAttempts; int(got) != i+1 {
			t.Fatalf("USR-009: after %d wrong codes the row counts %d", i+1, got)
		}
	}
	r := v.row(t, ver.ID)
	if r.BurnedAt == nil || r.CodeDigest != nil {
		t.Errorf("USR-009: the fifth wrong code did not burn the verification: %+v", r)
	}
	_, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, code)
	wantErr(t, err, apperr.KindConflict, authn.CodeVerificationExhausted)
	if _, _, phone, _ := v.contact(t, user); phone != nil {
		t.Error("USR-009: the right code on a burned verification wrote the number")
	}
	// A burned verification reads as missing.
	_, err = v.verifications.Get(t.Context(), principal(user), ver.ID)
	wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)

	v.clock.Advance(authn.VerificationCooldown)
	next, code := v.start(t, user, authn.ChannelPhone, phoneN1)
	if _, err := v.verifications.Confirm(t.Context(), principal(user), next.ID, code); err != nil {
		t.Errorf("USR-009: a new verification after the burned one = %v", err)
	}
}

// Five holds under concurrency: twelve wrong codes at once spend exactly
// five attempts; the rest find the verification burned (USR-009).
func TestAttemptLimitHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	ver, code := v.start(t, user, authn.ChannelPhone, phoneN1)

	const tries = 12
	errs := make(chan error, tries)
	var ready sync.WaitGroup
	ready.Add(tries)
	gate := make(chan struct{})
	for range tries {
		go func() {
			ready.Done()
			<-gate
			_, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, wrong(code))
			errs <- err
		}()
	}
	ready.Wait()
	close(gate)
	counts := map[apperr.Code]int{}
	for range tries {
		err := <-errs
		e := apperr.From(err)
		counts[e.Code]++
	}
	if counts[authn.CodeVerificationWrong] != authn.VerificationAttempts ||
		counts[authn.CodeVerificationExhausted] != tries-authn.VerificationAttempts {
		t.Errorf("USR-009: concurrent wrong codes answered %v, want %d %s and %d %s", counts,
			authn.VerificationAttempts, authn.CodeVerificationWrong, tries-authn.VerificationAttempts, authn.CodeVerificationExhausted)
	}
	if got := v.row(t, ver.ID).FailedAttempts; got != authn.VerificationAttempts {
		t.Errorf("USR-009: %d attempts counted under concurrency, want %d", got, authn.VerificationAttempts)
	}
}

// One open verification per channel and target: a new start cancels the
// open one and its code stops working. The cooldown and the target's
// hourly budget refuse a resend with the wait, over every user and every
// spelling of the target (USR-010).
func TestOneOpenVerificationPerTarget(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	other := v.addUser(t, newcomer, goodPw)

	first, c1 := v.start(t, user, authn.ChannelPhone, phoneN1)
	// Within the cooldown, by another spelling and by another user.
	for _, tc := range []struct {
		who    uuid.UUID
		target string
	}{{user, phoneN1}, {user, "0532-123-45-67"}, {other, "5321234567"}} {
		_, err := v.verifications.Start(t.Context(), principal(tc.who), authn.ChannelPhone, tc.target)
		wantErr(t, err, apperr.KindRateLimited, authn.CodeVerificationResendTooSoon)
		if wait, ok := authn.RetryAfter(err); !ok || wait <= 0 || wait > authn.VerificationCooldown {
			t.Errorf("USR-010: the cooldown's wait = %v, %v", wait, ok)
		}
	}

	v.clock.Advance(authn.VerificationCooldown)
	second, c2 := v.start(t, user, authn.ChannelPhone, phoneN1)
	if r := v.row(t, first.ID); r.CanceledAt == nil || r.CodeDigest != nil {
		t.Errorf("USR-010: the replaced verification is open: %+v", r)
	}
	_, err := v.verifications.Confirm(t.Context(), principal(user), first.ID, c1)
	wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)
	if c1 != c2 {
		_, err = v.verifications.Confirm(t.Context(), principal(user), second.ID, c1)
		wantErr(t, err, apperr.KindInvalid, authn.CodeVerificationWrong)
	}
	var open int
	if err := v.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM user_verifications
		WHERE user_id = $1 AND confirmed_at IS NULL AND canceled_at IS NULL AND burned_at IS NULL`, user).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Errorf("USR-010: %d open verifications of one target, want 1", open)
	}

	// The hourly budget: five codes an hour, from any user.
	for range authn.VerificationsPerTargetHour - 2 {
		v.clock.Advance(authn.VerificationCooldown)
		v.start(t, other, authn.ChannelPhone, phoneN1)
	}
	v.clock.Advance(authn.VerificationCooldown)
	_, err = v.verifications.Start(t.Context(), principal(user), authn.ChannelPhone, phoneN1)
	wantErr(t, err, apperr.KindRateLimited, authn.CodeVerificationResendTooSoon)
	if wait, ok := authn.RetryAfter(err); !ok || wait <= authn.VerificationCooldown {
		t.Errorf("USR-010: the hourly budget's wait = %v, want more than the cooldown", wait)
	}
	if n := len(v.texts.to(phoneN1)); n != authn.VerificationsPerTargetHour {
		t.Errorf("USR-010: the number got %d codes in the hour, want %d", n, authn.VerificationsPerTargetHour)
	}
	if got := v.textCounter(t, "throttled"); got != 4 {
		t.Errorf("%s{throttled} = %v, want 4", authn.MetricVerificationCodes, got)
	}
}

// Another user's verification is the 404 of one that never existed, for
// every operation, and it is neither touched nor spent (USR-011).
func TestAnotherUsersVerificationIsNotFound(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	owner := v.addUser(t, member, goodPw)
	intruder := v.addUser(t, newcomer, goodPw)
	ver, code := v.start(t, owner, authn.ChannelPhone, phoneN1)

	for _, id := range []uuid.UUID{ver.ID, uuid.New()} {
		_, err := v.verifications.Confirm(t.Context(), principal(intruder), id, code)
		wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)
		_, err = v.verifications.Get(t.Context(), principal(intruder), id)
		wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)
		err = v.verifications.Cancel(t.Context(), principal(intruder), id)
		wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)
	}
	if r := v.row(t, ver.ID); r.FailedAttempts != 0 || r.ConfirmedAt != nil || r.CanceledAt != nil {
		t.Errorf("USR-011: another user's attempts changed the verification: %+v", r)
	}
	if _, _, phone, _ := v.contact(t, intruder); phone != nil {
		t.Error("USR-011: the intruder got the number")
	}
	if _, err := v.verifications.Confirm(t.Context(), principal(owner), ver.ID, code); err != nil {
		t.Errorf("USR-011: the owner's confirmation after the intruder's = %v", err)
	}
}

// After its ten minutes a verification answers the right and a wrong code
// alike, and spends nothing: the expiry comes before the code (USR-012).
func TestExpiryComesBeforeTheCode(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	ver, code := v.start(t, user, authn.ChannelPhone, phoneN1)
	v.clock.Advance(authn.VerificationTTL + time.Second)

	var answers []string
	for _, c := range []string{code, wrong(code)} {
		_, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, c)
		wantErr(t, err, apperr.KindInvalid, authn.CodeVerificationExpired)
		answers = append(answers, apperr.From(err).Error())
	}
	if answers[0] != answers[1] {
		t.Errorf("USR-012: the right and the wrong code got %q and %q", answers[0], answers[1])
	}
	if got := v.row(t, ver.ID).FailedAttempts; got != 0 {
		t.Errorf("USR-012: an expired verification counted %d attempts", got)
	}
	if got, err := v.verifications.Get(t.Context(), principal(user), ver.ID); err != nil || got.Status != authn.StatusExpired {
		t.Errorf("USR-012: the expired verification reads %+v, %v", got, err)
	}
}

// A confirmation consumes the verification: the digest leaves the row,
// and a second confirmation is a conflict whatever the code, with no
// second write or notice (USR-013).
func TestConfirmConsumesTheVerification(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	ver, code := v.start(t, user, authn.ChannelPhone, phoneN1)

	got, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, code)
	if err != nil || got.Status != authn.StatusConfirmed || got.ConfirmedAt == nil {
		t.Fatalf("USR-013: Confirm = %+v, %v", got, err)
	}
	if r := v.row(t, ver.ID); r.ConfirmedAt == nil || r.CodeDigest != nil {
		t.Errorf("USR-013: the confirmed row keeps its digest or is not confirmed: %+v", r)
	}
	for _, c := range []string{code, wrong(code)} {
		_, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, c)
		wantErr(t, err, apperr.KindConflict, authn.CodeVerificationConfirmed)
	}
	if got := v.row(t, ver.ID).FailedAttempts; got != 0 {
		t.Errorf("USR-013: a second confirmation counted %d attempts", got)
	}
	if n := v.queued(t, "contact_changed"); n != 1 {
		t.Errorf("USR-013: %d change notices queued, want one", n)
	}
	// A confirmed verification stays as it is when canceled.
	if err := v.verifications.Cancel(t.Context(), principal(user), ver.ID); err != nil {
		t.Errorf("Cancel of a confirmed verification = %v", err)
	}
	if got, err := v.verifications.Get(t.Context(), principal(user), ver.ID); err != nil || got.Status != authn.StatusConfirmed {
		t.Errorf("the confirmed verification after a cancel reads %+v, %v", got, err)
	}
}

// The new number or address reaches the account only when its code is
// confirmed; until then the old value and its flag stay (USR-014's
// confirmation half; the profile's own half comes with its handler). An
// address change also revokes the reset link mailed to the old address,
// and the change is told at the old address; a number change at the
// account's address.
func TestConfirmedValueIsWrittenOnlyOnConfirmation(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)

	ver, code := v.start(t, user, authn.ChannelPhone, phoneN1)
	if _, _, phone, verified := v.contact(t, user); phone != nil || verified {
		t.Fatalf("USR-014: the number was written before its code: %v, %v", phone, verified)
	}
	if _, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, code); err != nil {
		t.Fatal(err)
	}
	if _, _, phone, verified := v.contact(t, user); phone == nil || *phone != phoneN1 || !verified {
		t.Errorf("USR-014: after the code the number is %v (verified %v), want %s verified", phone, verified, phoneN1)
	}
	if err := v.workCodes(t); err != nil {
		t.Fatal(err)
	}
	if msgs := v.mail.to(member); len(msgs) != 1 || !strings.Contains(msgs[0].Text, "telefon numarası") {
		t.Errorf("the number change notice at the account's address: %+v", msgs)
	}

	// The address: a reset link is open at the old one.
	if err := v.accounts.RequestPasswordReset(t.Context(), member); err != nil {
		t.Fatal(err)
	}
	v.work(t)
	resetToken := v.lastToken(t, member)
	const changed = "ayse.yeni@example.com"
	ver, code = v.start(t, user, authn.ChannelEmail, "Ayse.Yeni@Example.com")
	if email, _, _, _ := v.contact(t, user); email != member {
		t.Fatalf("USR-014: the address changed before its code: %s", email)
	}
	if msg := v.mail.to(changed)[0]; strings.Contains(msg.Text+msg.HTML+msg.Subject, "Ayşe") {
		t.Error("the code mail to an unverified address carries the account's name")
	}
	if _, err := v.verifications.Confirm(t.Context(), principal(user), ver.ID, code); err != nil {
		t.Fatal(err)
	}
	if email, verified, _, _ := v.contact(t, user); email != changed || !verified {
		t.Errorf("USR-014: after the code the address is %s (verified %v)", email, verified)
	}
	err := v.completeReset(resetToken, otherPw, deviceW1)
	wantErr(t, err, apperr.KindInvalid, authn.CodeResetLinkInvalid)
	if err := v.workCodes(t); err != nil {
		t.Fatal(err)
	}
	if msgs := v.mail.to(member); len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Text, "e-posta adresi") {
		t.Errorf("the address change notice did not reach the old address: %d mails", len(msgs))
	}
	v.login(t, changed, goodPw, deviceW1)
}

// A target another account holds is told only to the caller who entered
// the code sent to it: the start answers as for a free target, a wrong
// code is a wrong code, and the right one is VERIFICATION_TARGET_IN_USE
// with nothing written and the verification still open (ADR-0033).
func TestTargetInUseIsToldOnlyAfterTheCode(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	holder := v.addUser(t, newcomer, goodPw)
	if _, err := v.db.Pool().Exec(t.Context(), "UPDATE active_users SET phone = $1, phone_verified = true WHERE id = $2", phoneN1, holder); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ channel, target string }{
		{authn.ChannelEmail, newcomer},
		{authn.ChannelPhone, phoneN1},
	} {
		taken, code := v.start(t, user, tc.channel, tc.target)
		free, _ := v.start(t, user, tc.channel, map[string]string{authn.ChannelEmail: "bos@example.com", authn.ChannelPhone: phoneN2}[tc.channel])
		if taken.Status != free.Status || taken.RemainingAttempts != free.RemainingAttempts {
			t.Errorf("%s: the start of a taken target answers %+v, of a free one %+v", tc.channel, taken, free)
		}
		_, err := v.verifications.Confirm(t.Context(), principal(user), taken.ID, wrong(code))
		wantErr(t, err, apperr.KindInvalid, authn.CodeVerificationWrong)
		_, err = v.verifications.Confirm(t.Context(), principal(user), taken.ID, code)
		wantErr(t, err, apperr.KindConflict, authn.CodeVerificationTargetInUse)
		if r := v.row(t, taken.ID); r.ConfirmedAt != nil || r.CodeDigest == nil {
			t.Errorf("%s: a taken target consumed the verification: %+v", tc.channel, r)
		}
	}
	if email, _, phone, _ := v.contact(t, user); email != member || phone != nil {
		t.Errorf("a taken target was written: %s, %v", email, phone)
	}
}

// SMS pumping (ADR-0033): only Turkish mobile numbers get a code; any
// other number is refused before anything is written or queued.
func TestOnlyTextableNumbersGetACode(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	for _, n := range []string{"+447700900123", "+882123456789", "0850 123 45 67", "0900 123 45 67", "0212 123 45 67", "abc"} {
		_, err := v.verifications.Start(t.Context(), principal(user), authn.ChannelPhone, n)
		wantErr(t, err, apperr.KindInvalid, httpx.CodeRequestInvalid)
	}
	var rows int
	if err := v.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM user_verifications").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || v.queued(t, "verification_code") != 0 {
		t.Errorf("refused numbers wrote %d verifications and queued %d codes", rows, v.queued(t, "verification_code"))
	}
}

// One user's daily cap of texts, over every number: the toll fraud bound
// of an account (ADR-0033).
func TestDailyTextCapPerUser(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	for i := range authn.TextsPerUserDay {
		if _, err := v.verifications.Start(t.Context(), principal(user), authn.ChannelPhone, fmt.Sprintf("+90532000%04d", i)); err != nil {
			t.Fatalf("start %d = %v", i, err)
		}
	}
	_, err := v.verifications.Start(t.Context(), principal(user), authn.ChannelPhone, "+905329999999")
	wantErr(t, err, apperr.KindRateLimited, authn.CodeVerificationResendTooSoon)
	if wait, ok := authn.RetryAfter(err); !ok || wait < 23*time.Hour {
		t.Errorf("the daily cap's wait = %v", wait)
	}
	// The mail channel has its own cap.
	if _, err := v.verifications.Start(t.Context(), principal(user), authn.ChannelEmail, "yeni@example.com"); err != nil {
		t.Errorf("an address after the text cap = %v", err)
	}
}

// The global hourly cap of texts holds a code back, counts it, and sends
// nothing; every user spends it alike (ADR-0033).
func TestGlobalTextCap(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 2)
	for i, n := range []string{phoneN1, phoneN2, "+905051112233"} {
		user := v.addUser(t, fmt.Sprintf("u%d@example.com", i), goodPw)
		if _, err := v.verifications.Start(t.Context(), principal(user), authn.ChannelPhone, n); err != nil {
			t.Fatal(err)
		}
		if err := v.workCodes(t); err != nil {
			t.Fatal(err)
		}
	}
	if n := v.texts.count(); n != 2 {
		t.Errorf("%d texts under a cap of 2", n)
	}
	if got := v.textCounter(t, "held"); got != 1 {
		t.Errorf("%s{PHONE,held} = %v, want 1", authn.MetricVerificationCodes, got)
	}
	if got := v.textCounter(t, "sent"); got != 2 {
		t.Errorf("%s{PHONE,sent} = %v, want 2", authn.MetricVerificationCodes, got)
	}
}

// A refused recipient cancels the code job instead of retrying it; a
// passing failure is retried. Both channels (ADR-0033).
func TestUndeliverableCodeIsCanceled(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	var cancel *rivertype.JobCancelError
	for _, tc := range []struct {
		channel, target string
		refuse          func(error)
	}{
		{authn.ChannelPhone, phoneN1, func(err error) { v.texts.mu.Lock(); v.texts.refuse = err; v.texts.mu.Unlock() }},
		{authn.ChannelEmail, "yeni@example.com", func(err error) { v.mail.mu.Lock(); v.mail.refuse = err; v.mail.mu.Unlock() }},
	} {
		undeliverable := sms.ErrUndeliverable
		if tc.channel == authn.ChannelEmail {
			undeliverable = mailer.ErrUndeliverable
		}
		tc.refuse(fmt.Errorf("%w: refused by the provider", undeliverable))
		if _, err := v.verifications.Start(t.Context(), principal(user), tc.channel, tc.target); err != nil {
			t.Fatal(err)
		}
		if err := v.workCodes(t); !errors.As(err, &cancel) {
			t.Errorf("%s: an undeliverable code = %v, want a River job cancellation", tc.channel, err)
		}
		tc.refuse(errors.New("a passing failure"))
		v.clock.Advance(authn.VerificationCooldown)
		if _, err := v.verifications.Start(t.Context(), principal(user), tc.channel, tc.target); err != nil {
			t.Fatal(err)
		}
		if err := v.workCodes(t); err == nil || errors.As(err, &cancel) {
			t.Errorf("%s: a passing failure = %v, want an error River retries", tc.channel, err)
		}
		tc.refuse(nil)
	}
	if got := v.textCounter(t, "undeliverable"); got != 1 {
		t.Errorf("%s{PHONE,undeliverable} = %v, want 1", authn.MetricVerificationCodes, got)
	}
}

// The list holds the open verifications only, newest first, in keyset
// pages both ways; a canceled one leaves it and reads as missing.
func TestVerificationListAndCancel(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	var ids []uuid.UUID
	for _, n := range []string{phoneN1, phoneN2, "+905051112233"} {
		ver, _ := v.start(t, user, authn.ChannelPhone, n)
		ids = append(ids, ver.ID)
		v.clock.Advance(time.Second)
	}
	p := principal(user)
	page, err := v.verifications.List(t.Context(), p, 2, "")
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != ids[2] || page.Items[1].ID != ids[1] ||
		page.NextCursor == nil || page.PrevCursor != nil {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	next, err := v.verifications.List(t.Context(), p, 2, *page.NextCursor)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != ids[0] || next.NextCursor != nil || next.PrevCursor == nil {
		t.Fatalf("second page = %+v, %v", next, err)
	}
	back, err := v.verifications.List(t.Context(), p, 2, *next.PrevCursor)
	if err != nil || len(back.Items) != 2 || back.Items[0].ID != ids[2] || back.Items[1].ID != ids[1] || back.PrevCursor != nil {
		t.Fatalf("page before the second = %+v, %v", back, err)
	}
	_, err = v.verifications.List(t.Context(), p, 2, "bozuk")
	wantErr(t, err, apperr.KindInvalid, authn.CodeCursorInvalid)

	if err := v.verifications.Cancel(t.Context(), p, ids[1]); err != nil {
		t.Fatal(err)
	}
	err = v.verifications.Cancel(t.Context(), p, ids[1])
	wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)
	_, err = v.verifications.Get(t.Context(), p, ids[1])
	wantErr(t, err, apperr.KindNotFound, authn.CodeVerificationNotFound)
	all, err := v.verifications.List(t.Context(), p, 100, "")
	if err != nil || len(all.Items) != 2 {
		t.Errorf("after a cancel the list = %+v, %v", all, err)
	}
	// Expired ones leave the list too.
	v.clock.Advance(authn.VerificationTTL)
	if all, err := v.verifications.List(t.Context(), p, 100, ""); err != nil || len(all.Items) != 0 {
		t.Errorf("after the expiry the list = %+v, %v", all, err)
	}
}

// Verifications are hard-deleted after their retention (M2).
func TestVerificationsArePurged(t *testing.T) {
	t.Parallel()
	v := newVerificationsEnv(t, 0)
	user := v.addUser(t, member, goodPw)
	v.start(t, user, authn.ChannelPhone, phoneN1)
	v.clock.Advance(authn.VerificationRetention + time.Minute)
	v.start(t, user, authn.ChannelPhone, phoneN2)
	n, err := v.verifications.Purge(t.Context())
	if err != nil || n != 1 {
		t.Errorf("Purge = %d, %v; want the old verification only", n, err)
	}
}
