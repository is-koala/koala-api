//go:build integration

package authn_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river/rivertype"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/mailer"
)

const (
	member   = "ayse@example.com"
	newcomer = "yeni@example.com"
	// 15 characters: the shortest password the rule accepts (USR-021).
	goodPw  = "on beş karakter"
	otherPw = "bambaşka uzun parola"
	webBase = "https://iskoala.test"
)

// mailbox is the relay of these tests: it keeps what it is given, or
// fails every send with refuse.
type mailbox struct {
	mu     sync.Mutex
	msgs   []mailer.Message
	refuse error
}

func (m *mailbox) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuse != nil {
		return m.refuse
	}
	m.msgs = append(m.msgs, msg)
	return nil
}

// to returns the messages sent to addr.
func (m *mailbox) to(addr string) []mailer.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mailer.Message
	for _, msg := range m.msgs {
		if msg.To == addr {
			out = append(out, msg)
		}
	}
	return out
}

// accountsEnv is env with the account flows on a real insert-only River
// client and the mailbox.
type accountsEnv struct {
	*env
	accounts *authn.Accounts
	mail     *mailbox
}

func newAccountsEnv(t *testing.T) *accountsEnv {
	t.Helper()
	return newAccountsEnvCap(t, 0)
}

// newAccountsEnvCap is newAccountsEnv with a global hourly cap of
// registration mails (0: the default).
func newAccountsEnvCap(t *testing.T, registrationsPerHour int) *accountsEnv {
	t.Helper()
	e := newEnv(t)
	queue, err := jobs.NewClient(e.db.Pool(), jobs.Config{Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	box := &mailbox{}
	accounts, err := authn.NewAccounts(e.users, queue, box, webBase, registrationsPerHour)
	if err != nil {
		t.Fatal(err)
	}
	return &accountsEnv{env: e, accounts: accounts, mail: box}
}

// queuedJob is a row of river_job.
type queuedJob struct {
	ID   int64
	Kind string
	Args []byte
}

// work runs the account jobs queued so far, oldest first, as River's
// workers would, until none is left; each is marked completed once it ran
// and stays in river_job, as River keeps a finished job for a day.
func (a *accountsEnv) work(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	for range 20 {
		rows, err := a.db.Pool().Query(ctx, `
			SELECT id, kind, args FROM river_job
			WHERE kind IN ('registration_requested', 'password_reset_requested', 'auth_mail') AND state = 'available'
			ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		queued, err := pgx.CollectRows(rows, pgx.RowToStructByPos[queuedJob])
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) == 0 {
			return
		}
		for _, j := range queued {
			var args struct {
				Email     string    `json:"email"`
				FirstName string    `json:"first_name"`
				LastName  string    `json:"last_name"`
				Employer  bool      `json:"employer"`
				Mail      string    `json:"mail"`
				Ref       uuid.UUID `json:"ref"`
			}
			if err := json.Unmarshal(j.Args, &args); err != nil {
				t.Fatal(err)
			}
			switch j.Kind {
			case "registration_requested":
				err = a.accounts.HandleRegistrationRequest(ctx, authn.RegistrationRequest{
					Email: args.Email, FirstName: args.FirstName, LastName: args.LastName, Employer: args.Employer,
				})
			case "password_reset_requested":
				err = a.accounts.HandlePasswordResetRequest(ctx, args.Email)
			default:
				err = a.accounts.SendAccountMail(ctx, args.Mail, args.Ref)
			}
			if err != nil {
				t.Fatalf("job %s: %v", j.Kind, err)
			}
			if _, err := a.db.Pool().Exec(ctx, "UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1", j.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("the account jobs keep queueing jobs")
}

// queued counts the jobs of a kind that wait to run.
func (a *accountsEnv) queued(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := a.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM river_job WHERE kind = $1 AND state = 'available'", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// tokenIn takes the token out of the link of a mail.
func tokenIn(t *testing.T, m mailer.Message) string {
	t.Helper()
	const marker = "?token="
	i := strings.Index(m.Text, marker)
	if i < 0 || len(m.Text) < i+len(marker)+43 {
		t.Fatalf("mail %q carries no link token", m.Subject)
	}
	if !strings.Contains(m.Text, webBase+"/") || !strings.Contains(m.HTML, m.Text[i:i+len(marker)+43]) {
		t.Fatalf("mail %q: the link is not on %s in both parts", m.Subject, webBase)
	}
	return m.Text[i+len(marker) : i+len(marker)+43]
}

// lastToken returns the token of the newest mail to addr.
func (a *accountsEnv) lastToken(t *testing.T, addr string) string {
	t.Helper()
	msgs := a.mail.to(addr)
	if len(msgs) == 0 {
		t.Fatalf("no mail to %s", addr)
	}
	return tokenIn(t, msgs[len(msgs)-1])
}

// register runs the first step for addr and returns the link token.
func (a *accountsEnv) register(t *testing.T, addr string) string {
	t.Helper()
	err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{
		Email: addr, FirstName: "Ayşe", LastName: "Yılmaz", Employer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.work(t)
	return a.lastToken(t, addr)
}

// consents grants every current agreement.
func consents() []authn.ConsentDecision {
	return []authn.ConsentDecision{
		{DocumentCode: "TERMS_OF_SERVICE", Version: "1.0", Granted: true},
		{DocumentCode: "PRIVACY_POLICY", Version: "1.0", Granted: true},
		{DocumentCode: "EXPLICIT_CONSENT", Version: "1.0", Granted: true},
		{DocumentCode: "COMMERCIAL_MESSAGE", Version: "1.0", Granted: false},
	}
}

func (a *accountsEnv) complete(token, password string, decisions []authn.ConsentDecision) (authn.TokenPair, error) {
	return a.accounts.CompleteRegistration(context.Background(), authn.RegistrationCompletion{
		Token: token, Password: password, Consents: decisions, DeviceKey: deviceW1, Platform: "WEB",
		ClientIP: netip.MustParseAddr("192.0.2.44"), UserAgent: "Koala/1.4.0 (iOS 19)",
	})
}

// completeReset completes a reset from device.
func (a *accountsEnv) completeReset(token, password, device string) error {
	return a.accounts.CompletePasswordReset(context.Background(), authn.PasswordResetCompletion{
		Token: token, Password: password, DeviceKey: device, Platform: "IOS",
	})
}

// pendingRow is a registration as stored.
type pendingRow struct {
	LinkTokenHash []byte
	ConsumedAt    *time.Time
	RevokedAt     *time.Time
	Email         *string
	FirstName     *string
	LastName      *string
}

// pendings reads the registrations of addr's mailbox, ended ones
// included. The stored key is the keyed hash of the mailbox, so it is
// derived the way the service derives it, not compared with the address.
func (a *accountsEnv) pendings(t *testing.T, addr string) []pendingRow {
	t.Helper()
	rows, err := a.db.Pool().Query(t.Context(), `
		SELECT link_token_hash, consumed_at, revoked_at, email, first_name, last_name
		FROM pending_registrations WHERE mailbox_key = $1 ORDER BY created_at`, a.users.MailboxTarget(addr))
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pendingRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func openRows(rows []pendingRow) int {
	n := 0
	for _, r := range rows {
		if r.ConsumedAt == nil && r.RevokedAt == nil {
			n++
		}
	}
	return n
}

// The first step does not tell a registered address apart: the same
// answer, one job each; the job writes nothing and sends nothing for the
// registered one (USR-001, AUTH-008). The answer bodies are compared on
// the wire in internal/app.TestAccountRequestsDoNotTellAddressesApart.
func TestRegistrationStartDoesNotTellAddressesApart(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", goodPw)

	for _, addr := range []string{"ayse@example.com", newcomer} {
		err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: addr, FirstName: "A", LastName: "B"})
		if err != nil {
			t.Fatalf("USR-001: start for %s = %v", addr, err)
		}
	}
	if n := a.queued(t, "registration_requested"); n != 2 {
		t.Fatalf("USR-001: %d jobs queued, want one per request", n)
	}
	a.work(t)
	if rows, msgs := a.pendings(t, "ayse@example.com"), a.mail.to("ayse@example.com"); len(rows) != 0 || len(msgs) != 0 {
		t.Errorf("USR-001: registered address got %d pending registrations and %d mails, want none", len(rows), len(msgs))
	}
	if rows, msgs := a.pendings(t, newcomer), a.mail.to(newcomer); len(rows) != 1 || len(msgs) != 1 {
		t.Errorf("AUTH-008: new address got %d pending registrations and %d mails, want one each", len(rows), len(msgs))
	}
	// An address that is not one is refused before anything is queued.
	err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: "Ayşe <a@example.com>", FirstName: "A", LastName: "B"})
	wantErr(t, err, apperr.KindInvalid, httpx.CodeRequestInvalid)
}

// A second start is the resend: the open registration is revoked, its
// link stops working, and one registration is open (USR-002, AUTH-008).
func TestRegistrationStartResends(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	first := a.register(t, newcomer)
	a.clock.Advance(authn.LinkMailCooldown + time.Second)
	second := a.register(t, newcomer)
	if first == second {
		t.Fatal("USR-002: the resend mailed the same token")
	}
	_, err := a.accounts.VerifyRegistration(t.Context(), first)
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)
	if _, err := a.accounts.VerifyRegistration(t.Context(), second); err != nil {
		t.Fatalf("USR-002: the new link = %v", err)
	}
	rows := a.pendings(t, newcomer)
	if n := openRows(rows); n != 1 {
		t.Errorf("USR-002 / AUTH-008: %d open registrations, want 1", n)
	}
	if rows[0].RevokedAt == nil || rows[0].Email != nil || rows[0].FirstName != nil {
		t.Error("AUTH-008: the replaced registration is not revoked, or its address and names stayed")
	}
}

// The link token is 32 random bytes, only its SHA-256 is stored, it lives
// one hour, and an expired one revokes its registration (USR-003).
func TestRegistrationLinkIsOpaqueAndExpires(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	token := a.register(t, newcomer)
	rows := a.pendings(t, newcomer)
	sum := sha256.Sum256([]byte(token))
	if len(rows) != 1 || !bytes.Equal(rows[0].LinkTokenHash, sum[:]) {
		t.Fatal("USR-003: the stored link is not the SHA-256 of the mailed token")
	}
	var plain int
	err := a.db.Pool().QueryRow(t.Context(), `
		SELECT count(*) FROM pending_registrations p
		WHERE strpos(p::text, $1) > 0`, token).Scan(&plain)
	if err != nil {
		t.Fatal(err)
	}
	if plain != 0 {
		t.Error("USR-003: the token is stored in plain text")
	}
	// The job queue holds the start and the mail job, both run and kept as
	// River keeps them; neither carries the token, and the mail job carries
	// no address either (ADR-0032 Karar 3).
	var queuedJobs, withToken, mailWithAddress int
	err = a.db.Pool().QueryRow(t.Context(), `
		SELECT count(*), count(*) FILTER (WHERE strpos(args::text, $1) > 0),
		       count(*) FILTER (WHERE kind = 'auth_mail' AND strpos(args::text, $2) > 0)
		FROM river_job`, token, newcomer).Scan(&queuedJobs, &withToken, &mailWithAddress)
	if err != nil {
		t.Fatal(err)
	}
	if queuedJobs < 2 || withToken != 0 || mailWithAddress != 0 {
		t.Errorf("USR-003: %d jobs, %d carry the token, %d mail jobs carry the address; want at least 2, 0, 0",
			queuedJobs, withToken, mailWithAddress)
	}

	a.clock.Advance(authn.RegistrationLinkTTL)
	_, err = a.accounts.VerifyRegistration(t.Context(), token)
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)
	if r := a.pendings(t, newcomer); r[0].RevokedAt == nil || r[0].Email != nil || r[0].FirstName != nil || r[0].LastName != nil {
		t.Error("USR-003: the expired link did not revoke its registration, or its address and names stayed")
	}
	_, err = a.accounts.VerifyRegistration(t.Context(), token)
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)
}

// The registration token works once and for fifteen minutes; the link
// is not consumed by verifying it (USR-004).
func TestRegistrationTokenIsSingleUse(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	link := a.register(t, newcomer)
	// The link token itself does not complete a registration: only the
	// token the second step hands out does.
	_, err := a.complete(link, goodPw, consents())
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)
	grant, err := a.accounts.VerifyRegistration(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	if !grant.ExpiresAt.Equal(a.clock.Now().Add(authn.RegistrationTokenTTL)) || !grant.Employer {
		t.Errorf("USR-004: grant = %+v", grant)
	}
	// The link can be opened twice; the second verification replaces
	// the first registration token.
	again, err := a.accounts.VerifyRegistration(t.Context(), link)
	if err != nil {
		t.Fatalf("USR-004: second verification = %v", err)
	}
	_, err = a.complete(grant.Token, goodPw, consents())
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)
	if _, err := a.complete(again.Token, goodPw, consents()); err != nil {
		t.Fatalf("USR-004: complete = %v", err)
	}
	_, err = a.complete(again.Token, goodPw, consents())
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)

	other := a.register(t, "ikinci@example.com")
	late, err := a.accounts.VerifyRegistration(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(authn.RegistrationTokenTTL)
	_, err = a.complete(late.Token, goodPw, consents())
	wantErr(t, err, apperr.KindInvalid, authn.CodeRegistrationInvalid)
}

// A missing mandatory consent is refused before the token is read: the
// token is not consumed and works afterwards (USR-005).
func TestConsentsComeBeforeTheToken(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	grant, err := a.accounts.VerifyRegistration(t.Context(), a.register(t, newcomer))
	if err != nil {
		t.Fatal(err)
	}
	missing := consents()
	missing[2].Granted = false
	_, err = a.complete(grant.Token, goodPw, missing)
	wantErr(t, err, apperr.KindInvalid, authn.CodeConsentRequired)
	// The same answer for a token that never existed: the consent check
	// says nothing about the token.
	_, err = a.complete(strings.Repeat("A", 43), goodPw, missing)
	wantErr(t, err, apperr.KindInvalid, authn.CodeConsentRequired)
	// A decision for a version that is not current is malformed.
	stale := append(consents(), authn.ConsentDecision{DocumentCode: "TERMS_OF_SERVICE", Version: "0.9", Granted: true})
	_, err = a.complete(grant.Token, goodPw, stale)
	wantErr(t, err, apperr.KindInvalid, httpx.CodeRequestInvalid)
	if _, err := a.q.GetUserCredentials(t.Context(), newcomer); err == nil {
		t.Fatal("USR-005: a user exists after the refused completions")
	}
	if _, err := a.complete(grant.Token, goodPw, consents()); err != nil {
		t.Fatalf("USR-005: the token after a refused completion = %v", err)
	}
}

// Completing creates the user from the pending registration, e-mail
// verified, phone and identity not; the password is stored as a hash, the
// consents are recorded, the registration consumed and the caller signed
// in on its device (USR-006).
func TestCompletingRegistrationSignsIn(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	grant, err := a.accounts.VerifyRegistration(t.Context(), a.register(t, newcomer))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.complete(grant.Token, goodPw, consents())
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.users.Authenticate(t.Context(), pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatalf("USR-006: the new session = %v", err)
	}
	// The welcome mail was queued with the user (Java's parity).
	a.work(t)
	if msgs := a.mail.to(newcomer); len(msgs) != 2 || !strings.Contains(msgs[1].Subject, "Hoş Geldin Ayşe") ||
		!strings.Contains(msgs[1].Text, "Ayşe Yılmaz") {
		t.Errorf("USR-006: mails %d; want the link and then the welcome mail naming the user", len(msgs))
	}
	owner := a.dbt.Open(t, db.RoleOwner)
	var (
		first, last, hash                   string
		employer, email, phone, identityVer bool
		recorded                            int
	)
	err = owner.Pool().QueryRow(t.Context(), `
		SELECT first_name, last_name, password_hash, employer, email_verified, phone_verified, identity_verified,
		       (SELECT count(*) FROM consent_records c WHERE c.user_id = u.id)
		FROM users u WHERE id = $1`, p.UserID).
		Scan(&first, &last, &hash, &employer, &email, &phone, &identityVer, &recorded)
	if err != nil {
		t.Fatal(err)
	}
	if first != "Ayşe" || last != "Yılmaz" || !employer || !email || phone || identityVer {
		t.Errorf("USR-006: user = %s %s employer=%t email=%t phone=%t identity=%t", first, last, employer, email, phone, identityVer)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, goodPw) {
		t.Error("USR-006: the password is not stored as an argon2id hash")
	}
	if recorded != len(consents()) {
		t.Errorf("USR-006: %d consent records, want %d", recorded, len(consents()))
	}
	// The evidence names the client (ADR-0032 Karar 8).
	var withClient int
	err = owner.Pool().QueryRow(t.Context(), `
		SELECT count(*) FROM consent_records
		WHERE user_id = $1 AND ip_address = '192.0.2.44' AND user_agent = 'Koala/1.4.0 (iOS 19)'`, p.UserID).Scan(&withClient)
	if err != nil {
		t.Fatal(err)
	}
	if withClient != len(consents()) {
		t.Errorf("USR-006: %d consent records carry the client address and User-Agent, want %d", withClient, len(consents()))
	}
	// Consumed, and its address and names gone (KVKK data minimization).
	if rows := a.pendings(t, newcomer); len(rows) != 1 || rows[0].ConsumedAt == nil || rows[0].Email != nil ||
		rows[0].FirstName != nil || rows[0].LastName != nil {
		t.Error("USR-006: the pending registration is not consumed, or its address and names stayed")
	}
	// The address now has an account: the first step sends nothing.
	a.clock.Advance(authn.LinkMailCooldown + time.Second)
	before := len(a.mail.to(newcomer))
	if err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: newcomer, FirstName: "A", LastName: "B"}); err != nil {
		t.Fatal(err)
	}
	a.work(t)
	if len(a.mail.to(newcomer)) != before {
		t.Error("USR-006: a registered address got a registration mail")
	}
}

// An address that got an account between the first and the third step
// is a conflict, and the registration token stays unconsumed.
func TestCompletingARegistrationOfATakenAddress(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	grant, err := a.accounts.VerifyRegistration(t.Context(), a.register(t, newcomer))
	if err != nil {
		t.Fatal(err)
	}
	a.addUser(t, newcomer, otherPw)
	_, err = a.complete(grant.Token, goodPw, consents())
	wantErr(t, err, apperr.KindConflict, authn.CodeVerificationTargetInUse)
	if rows := a.pendings(t, newcomer); rows[0].ConsumedAt != nil {
		t.Error("the conflict consumed the registration")
	}
}

// Half-finished registrations do not lock the address and are purged
// after their retention (USR-007).
func TestPendingRegistrationsArePurged(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	a.register(t, newcomer)
	a.clock.Advance(authn.AccountRowRetention + time.Hour)
	pending, _, _, err := a.accounts.Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 || len(a.pendings(t, newcomer)) != 0 {
		t.Errorf("USR-007: purged %d, %d rows left", pending, len(a.pendings(t, newcomer)))
	}
	if _, err := a.accounts.VerifyRegistration(t.Context(), a.register(t, newcomer)); err != nil {
		t.Errorf("USR-007: a new registration of the address = %v", err)
	}
}

// One address gets at most one link mail a minute and five an hour; a
// request over the budget is answered like any other (ADR-0032).
func TestLinkMailBudget(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	start := func() {
		t.Helper()
		if err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: newcomer, FirstName: "A", LastName: "B"}); err != nil {
			t.Fatal(err)
		}
		a.work(t)
	}
	start()
	start() // within the cooldown
	if n := len(a.mail.to(newcomer)); n != 1 {
		t.Fatalf("%d mails within the cooldown, want 1", n)
	}
	for range 6 {
		a.clock.Advance(authn.LinkMailCooldown + time.Second)
		start()
	}
	if n := len(a.mail.to(newcomer)); n != authn.LinkMailsPerHour {
		t.Errorf("%d mails within the hour, want %d", n, authn.LinkMailsPerHour)
	}
}

// reset runs a reset request for member and returns the mailed
// token.
func (a *accountsEnv) reset(t *testing.T) string {
	t.Helper()
	if err := a.accounts.RequestPasswordReset(t.Context(), member); err != nil {
		t.Fatal(err)
	}
	a.work(t)
	return a.lastToken(t, member)
}

// The reset token works once, in the transaction that writes the
// password (USR-015, AUTH-006); the notice is queued with that commit and
// a failed attempt queues none.
func TestResetTokenIsSingleUse(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)

	token := a.reset(t)
	if err := a.completeReset(token, goodPw, deviceW1); err != nil {
		t.Fatalf("USR-015: reset = %v", err)
	}
	if n := a.queued(t, "auth_mail"); n != 1 {
		t.Errorf("AUTH-006: %d notice jobs after the reset, want 1", n)
	}
	a.work(t)
	err := a.completeReset(token, "yine başka bir parola", deviceW1)
	wantErr(t, err, apperr.KindInvalid, authn.CodeResetLinkInvalid)
	if n := a.queued(t, "auth_mail"); n != 0 {
		t.Errorf("AUTH-006: a refused reset queued %d notices", n)
	}
	a.login(t, "ayse@example.com", goodPw, deviceW1) // the first reset's password holds
}

// Unknown, expired, revoked and used tokens get the same answer
// (USR-016, AUTH-007).
func TestInvalidResetTokensAreOneError(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)

	used := a.reset(t)
	if err := a.completeReset(used, goodPw, deviceW1); err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(authn.LinkMailCooldown + time.Second)
	revoked := a.reset(t)
	a.clock.Advance(authn.LinkMailCooldown + time.Second)
	expired := a.reset(t) // revokes the one before
	a.clock.Advance(authn.ResetLinkTTL)

	for name, token := range map[string]string{
		"unknown": strings.Repeat("x", 43), "malformed": "short", "expired": expired, "revoked": revoked, "used": used,
	} {
		err := a.completeReset(token, "yepyeni bir uzun parola", deviceW1)
		got := apperr.From(err)
		if got.Kind != apperr.KindInvalid || got.Code != authn.CodeResetLinkInvalid || len(got.Fields) != 0 {
			t.Errorf("USR-016 / AUTH-006 / AUTH-007: %s token = %v", name, err)
		}
	}
	a.login(t, "ayse@example.com", goodPw, deviceW1) // AUTH-007: nothing changed
}

// One open reset token per user: a new request revokes the open one, and
// an in-app password change revokes it too (USR-017, AUTH-006).
func TestOneOpenResetToken(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)

	t1 := a.reset(t)
	a.clock.Advance(authn.LinkMailCooldown + time.Second)
	t2 := a.reset(t)
	err := a.completeReset(t1, goodPw, deviceW1)
	wantErr(t, err, apperr.KindInvalid, authn.CodeResetLinkInvalid)

	// The database holds the rule too: a second open token of a user is
	// refused by the partial unique index.
	insert := "INSERT INTO password_reset_tokens (id, user_id, created_at) VALUES ($1, (SELECT id FROM active_users WHERE email = $2), $3)"
	_, err = a.db.Pool().Exec(t.Context(), insert, uuid.Must(uuid.NewV7()), member, a.clock.Now())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "password_reset_tokens_open_user_key" {
		t.Errorf("USR-017: a second open token = %v, want the open-user unique index to refuse it", err)
	}

	pair := a.login(t, "ayse@example.com", otherPw, deviceW1)
	p, err := a.users.Authenticate(t.Context(), pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: otherPw, New: goodPw, Confirmation: goodPw}); err != nil {
		t.Fatalf("USR-017 / AUTH-006: change = %v", err)
	}
	err = a.completeReset(t2, "yepyeni bir uzun parola", deviceW1)
	wantErr(t, err, apperr.KindInvalid, authn.CodeResetLinkInvalid)
}

// A reset request for an unknown address writes and sends nothing; the
// request itself is the same one job (USR-018, AUTH-007).
func TestResetRequestDoesNotTellAddressesApart(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)

	for _, addr := range []string{"ayse@example.com", newcomer} {
		if err := a.accounts.RequestPasswordReset(t.Context(), addr); err != nil {
			t.Fatalf("USR-018: request for %s = %v", addr, err)
		}
	}
	if n := a.queued(t, "password_reset_requested"); n != 2 {
		t.Fatalf("USR-018: %d jobs, want one per request", n)
	}
	a.work(t)
	if len(a.mail.to(newcomer)) != 0 || len(a.mail.to("ayse@example.com")) != 1 {
		t.Errorf("AUTH-007: mails to the unknown and the known address = %d, %d; want 0, 1",
			len(a.mail.to(newcomer)), len(a.mail.to("ayse@example.com")))
	}
	var rows int
	if err := a.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM password_reset_tokens").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("USR-018: %d reset rows, want only the known address's", rows)
	}
}

// A reset ends every session, on every device, and hands out no tokens;
// the user is told by mail (USR-019, AUTH-007).
func TestResetEndsEverySession(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)
	web := a.login(t, "ayse@example.com", otherPw, deviceW1)
	phone := a.login(t, "ayse@example.com", otherPw, devicePhone)

	token := a.reset(t)
	if err := a.completeReset(token, goodPw, deviceW1); err != nil {
		t.Fatal(err)
	}
	for device, pair := range map[string]authn.TokenPair{deviceW1: web, devicePhone: phone} {
		_, err := a.users.Authenticate(t.Context(), pair.AccessToken, device)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
		if !a.revoked.has(pair.SessionID) {
			t.Errorf("USR-019: the realtime hook did not hear of the session on %s", device)
		}
	}
	a.work(t)
	msgs := a.mail.to("ayse@example.com")
	if len(msgs) != 2 || strings.Contains(msgs[1].Text, "?token=") {
		t.Errorf("USR-019: %d mails; want the link and then a notice without a link", len(msgs))
	}
	if _, err := a.users.Login(t.Context(), authn.LoginInput{Email: "ayse@example.com", Password: otherPw, DeviceKey: deviceW1}); err == nil {
		t.Error("AUTH-007: the old password still signs in")
	}
	a.login(t, "ayse@example.com", goodPw, deviceW1)
}

// An in-app change keeps the caller's session and ends the others; a
// wrong current password changes nothing (USR-020).
func TestPasswordChangeKeepsTheCaller(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)
	caller := a.login(t, "ayse@example.com", otherPw, deviceW1)
	second := a.login(t, "ayse@example.com", otherPw, deviceW2)
	third := a.login(t, "ayse@example.com", otherPw, devicePhone)
	p, err := a.users.Authenticate(t.Context(), caller.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}

	err = a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: "yanlış ama uzun parola", New: goodPw, Confirmation: goodPw})
	wantErr(t, err, apperr.KindForbidden, authn.CodeCurrentMismatch)
	err = a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: otherPw, New: otherPw, Confirmation: otherPw})
	wantErr(t, err, apperr.KindInvalid, authn.CodeNewEqualsCurrent)
	err = a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: otherPw, New: goodPw, Confirmation: goodPw + "!"})
	wantErr(t, err, apperr.KindInvalid, authn.CodeConfirmationMismatch)
	if _, err := a.users.Authenticate(t.Context(), third.AccessToken, devicePhone); err != nil {
		t.Fatalf("USR-020: a refused change ended a session: %v", err)
	}

	if err := a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: otherPw, New: goodPw, Confirmation: goodPw}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.users.Authenticate(t.Context(), caller.AccessToken, deviceW1); err != nil {
		t.Errorf("USR-020: the caller's session = %v", err)
	}
	for device, pair := range map[string]authn.TokenPair{deviceW2: second, devicePhone: third} {
		_, err := a.users.Authenticate(t.Context(), pair.AccessToken, device)
		wantErr(t, err, apperr.KindUnauthenticated, authn.CodeSessionInactive)
	}
	a.work(t)
	if n := len(a.mail.to("ayse@example.com")); n != 1 {
		t.Errorf("USR-020: %d notices, want 1", n)
	}
	a.login(t, "ayse@example.com", goodPw, deviceW2)
}

// Every surface that sets a password applies the one rule: 15 to 128
// characters, nothing else (USR-021, AUTH-009).
func TestAccountPasswordRule(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, "ayse@example.com", otherPw)
	pair := a.login(t, "ayse@example.com", otherPw, deviceW1)
	p, err := a.users.Authenticate(t.Context(), pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := a.accounts.VerifyRegistration(t.Context(), a.register(t, newcomer))
	if err != nil {
		t.Fatal(err)
	}
	reset := a.reset(t)

	for _, bad := range []string{strings.Repeat("a", 14), strings.Repeat("a", 129)} {
		_, err := a.complete(grant.Token, bad, consents())
		wantErr(t, err, apperr.KindInvalid, authn.CodePasswordPolicy)
		err = a.completeReset(reset, bad, deviceW1)
		wantErr(t, err, apperr.KindInvalid, authn.CodePasswordPolicy)
		err = a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: otherPw, New: bad, Confirmation: bad})
		wantErr(t, err, apperr.KindInvalid, authn.CodePasswordPolicy)
	}
	// The refusals consumed nothing: the shortest good password works on
	// each surface.
	if _, err := a.complete(grant.Token, goodPw, consents()); err != nil {
		t.Errorf("USR-021: registration with 15 characters = %v", err)
	}
	if err := a.completeReset(reset, goodPw, deviceW1); err != nil {
		t.Errorf("AUTH-009: reset with 15 characters = %v", err)
	}
}

// lockOut writes a hundred failed sign-ins on member just before now: the
// target is locked for an hour (ADR-0025 §5).
func (a *accountsEnv) lockOut(t *testing.T) {
	t.Helper()
	target := a.users.Target(member)
	for i := range authn.UserLockThreshold {
		_, err := a.db.Pool().Exec(t.Context(),
			"INSERT INTO login_attempts (id, principal_kind, target_hash, outcome, created_at) VALUES ($1, 'USER', $2, 'FAILED', $3)",
			uuid.Must(uuid.NewV7()), target, a.clock.Now().Add(-time.Duration(authn.UserLockThreshold-i)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
	}
}

// known reports whether device is a known-good device of member.
func (a *accountsEnv) known(t *testing.T, device string) bool {
	t.Helper()
	known, err := a.users.KnownDevice(t.Context(), member, device)
	if err != nil {
		t.Fatal(err)
	}
	return known
}

// After a reset, a device whose sessions were opened before it vouches for
// nothing, however those sessions ended: the attacker's device, signed in
// twice (the first session REPLACED), is not known. The device the reset
// was completed from is, and so is a device signed in on afterwards
// (ADR-0032 Karar 7).
func TestResetForgetsTheDevicesBeforeIt(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	const attacker = "attacker-device-1"
	a.login(t, member, otherPw, attacker)
	a.clock.Advance(time.Second)
	a.login(t, member, otherPw, attacker) // the first session is REPLACED
	a.clock.Advance(time.Second)
	if !a.known(t, attacker) {
		t.Fatal("the attacker's device is not known before the reset; the test proves nothing")
	}

	if err := a.completeReset(a.reset(t), goodPw, devicePhone); err != nil {
		t.Fatal(err)
	}
	if a.known(t, attacker) {
		t.Error("the attacker's device still vouches for itself after the reset")
	}
	if !a.known(t, devicePhone) {
		t.Error("the device the reset was completed from is not known")
	}
	a.clock.Advance(time.Second)
	a.login(t, member, goodPw, deviceW2)
	if !a.known(t, deviceW2) {
		t.Error("a device signed in on after the reset is not known")
	}
}

// The two password revoke reasons do not vouch either, whenever the
// session was opened: the rule of the query itself, held on a fixture
// the code does not produce (a session ended for a password reason after
// the credential epoch).
func TestPasswordRevokeReasonsDoNotVouch(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"PASSWORD_RESET", "PASSWORD_CHANGED"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			a := newAccountsEnv(t)
			a.addUser(t, member, otherPw)
			pair := a.login(t, member, otherPw, deviceW1)
			_, err := a.db.Pool().Exec(t.Context(),
				"UPDATE user_sessions SET revoked_at = $1, revoke_reason = $2 WHERE id = $3", a.clock.Now(), reason, pair.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if a.known(t, deviceW1) {
				t.Errorf("a session ended for %s vouches for its device", reason)
			}
		})
	}
}

// A user locked out by someone else's guesses resets the password and
// signs in at once: from the device the reset was completed from, and
// from any other, since the reset counts as a success on the target
// (ADR-0032 Karar 7). The reset itself opens no session (USR-019).
func TestResetLiftsTheLock(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	a.clock.Advance(time.Second)
	a.lockOut(t)
	if _, err := a.users.Login(t.Context(), authn.LoginInput{Email: member, Password: otherPw, DeviceKey: devicePhone}); err == nil {
		t.Fatal("signed in while locked; the test proves nothing")
	}

	if err := a.completeReset(a.reset(t), goodPw, devicePhone); err != nil {
		t.Fatal(err)
	}
	if !a.known(t, devicePhone) {
		t.Error("the device the reset was completed from is not known")
	}
	a.login(t, member, goodPw, deviceW2) // another device: the lock is lifted
	a.login(t, member, goodPw, devicePhone)
}

// The device an in-app change was made from proved the current password
// there, so it stays known-good past the credential epoch the change
// moves: an attacker who locks the account out afterwards does not keep
// the user out of it. A device signed in on before the change, and not
// the one it was made from, is held by the lock (ADR-0032 Karar 7).
func TestPasswordChangeProvesTheDevice(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	pair := a.login(t, member, otherPw, deviceW1)
	a.login(t, member, otherPw, deviceW2)
	p, err := a.users.Authenticate(t.Context(), pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(time.Second)
	if err := a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: otherPw, New: goodPw, Confirmation: goodPw}); err != nil {
		t.Fatal(err)
	}
	if !a.known(t, deviceW1) {
		t.Error("the device the change was made from is not known")
	}
	if a.known(t, deviceW2) {
		t.Error("a device signed in on before the change is still known")
	}
	a.clock.Advance(time.Second)
	a.lockOut(t)
	_, err = a.users.Login(t.Context(), authn.LoginInput{Email: member, Password: goodPw, DeviceKey: deviceW2, Platform: "web"})
	wantErr(t, err, apperr.KindUnauthenticated, authn.CodeEmailOrPasswordMismatch)
	a.login(t, member, goodPw, deviceW1)
}

// A wrong current password counts on the account's target like a wrong
// sign-in, and while the target is locked the change is refused
// unexamined, the right password included (ADR-0032 Karar 7).
func TestPasswordChangeCountsFailures(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	pair := a.login(t, member, otherPw, deviceW1)
	p, err := a.users.Authenticate(t.Context(), pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(time.Second)
	change := func(current string) error {
		return a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: current, New: goodPw, Confirmation: goodPw})
	}
	wantErr(t, change("yanlış ama uzun parola"), apperr.KindForbidden, authn.CodeCurrentMismatch)
	if row := a.attempts(t, "USER", member); row.Failed != 1 {
		t.Fatalf("attempts after a wrong current password = %+v, want one failure", row)
	}
	a.clock.Advance(time.Second)
	a.lockOut(t)
	wantErr(t, change(otherPw), apperr.KindForbidden, authn.CodeCurrentMismatch)
	if row := a.attempts(t, "USER", member); row.Locked != 1 {
		t.Errorf("attempts while locked = %+v, want the refusal logged as LOCKED", row)
	}
	a.clock.Advance(authn.UserLockDuration)
	if err := change(otherPw); err != nil {
		t.Fatalf("the change after the lock = %v", err)
	}
}

// A suspended account gets no reset mail and writes no reset row; the
// request answered like any other (ADR-0032 Karar 7). A reset of an
// account suspended after its mail is refused like an invalid token.
func TestSuspendedAccountsGetNoReset(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	token := a.reset(t)
	if _, err := a.db.Pool().Exec(t.Context(), "UPDATE active_users SET status = 'SUSPENDED' WHERE email = $1", member); err != nil {
		t.Fatal(err)
	}
	err := a.completeReset(token, goodPw, deviceW1)
	wantErr(t, err, apperr.KindInvalid, authn.CodeResetLinkInvalid)

	a.clock.Advance(authn.LinkMailCooldown + time.Second)
	if err := a.accounts.RequestPasswordReset(t.Context(), member); err != nil {
		t.Fatal(err)
	}
	a.work(t)
	if n := len(a.mail.to(member)); n != 1 {
		t.Errorf("a suspended account got %d reset mails in all, want only the one before the suspension", n)
	}
	var rows int
	if err := a.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM password_reset_tokens").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("%d reset rows, want none written for the suspended account", rows)
	}
}

// The registration budget counts mailboxes: sub-addresses and, at Gmail,
// dots and googlemail.com reach one mailbox (ADR-0032 Karar 6).
func TestRegistrationBudgetCountsMailboxes(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)

	start := func(addr string) {
		t.Helper()
		if err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: addr, FirstName: "A", LastName: "B"}); err != nil {
			t.Fatal(err)
		}
		a.work(t)
	}
	start("kurban+1@example.com")
	start("kurban+2@example.com") // the same mailbox, within the cooldown
	start("kur.ban@gmail.com")
	start("kurban+x@googlemail.com") // the same Gmail mailbox
	got := len(a.mail.to("kurban+1@example.com")) + len(a.mail.to("kurban+2@example.com")) +
		len(a.mail.to("kur.ban@gmail.com")) + len(a.mail.to("kurban+x@googlemail.com"))
	if got != 2 {
		t.Errorf("%d mails to two mailboxes within the cooldown, want one each", got)
	}
}

// The global hourly cap holds registration mails over every address, and
// counts what it held (ADR-0032 Karar 6).
func TestGlobalRegistrationCap(t *testing.T) {
	t.Parallel()
	a := newAccountsEnvCap(t, 2)
	for _, addr := range []string{"bir@example.com", "iki@example.com", "uc@example.com"} {
		if err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: addr, FirstName: "A", LastName: "B"}); err != nil {
			t.Fatal(err)
		}
	}
	a.work(t)
	if n := len(a.mail.to("uc@example.com")); n != 0 {
		t.Errorf("the third address got %d mails over a cap of 2", n)
	}
	if got := a.held(t, "global"); got != 1 {
		t.Errorf("%s{reason=global} = %v, want 1", authn.MetricAccountMailsHeld, got)
	}
}

// The global cap is spent before the address is looked up, so a
// registered address takes the same place in it as an unknown one: with
// the cap full of a request for the target, the requester's own mailbox
// gets nothing either way, and the cap does not tell whether the target
// is registered (ADR-0032 Karar 2 and 6).
func TestRegistrationCapCountsEveryAddress(t *testing.T) {
	t.Parallel()
	for _, target := range []string{member, newcomer} {
		a := newAccountsEnvCap(t, 1)
		a.addUser(t, member, goodPw)
		for _, addr := range []string{target, "saldirgan@example.com"} {
			if err := a.accounts.StartRegistration(t.Context(), authn.RegistrationRequest{Email: addr, FirstName: "A", LastName: "B"}); err != nil {
				t.Fatal(err)
			}
			a.work(t)
		}
		var ledger int
		if err := a.db.Pool().QueryRow(t.Context(), "SELECT count(*) FROM registration_requests").Scan(&ledger); err != nil {
			t.Fatal(err)
		}
		if ledger != 2 {
			t.Errorf("target %s: the ledger has %d requests, want both", target, ledger)
		}
		if n := len(a.mail.to("saldirgan@example.com")); n != 0 {
			t.Errorf("target %s: the requester's own mailbox got %d mails over a cap the target's request filled", target, n)
		}
		if got := a.held(t, "global"); got != 1 {
			t.Errorf("target %s: %s{reason=global} = %v, want 1", target, authn.MetricAccountMailsHeld, got)
		}
	}
}

// held reads koala_account_mails_held_total for a reason.
func (a *accountsEnv) held(t *testing.T, reason string) float64 {
	t.Helper()
	families, err := a.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != authn.MetricAccountMailsHeld {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no %s{reason=%q}", authn.MetricAccountMailsHeld, reason)
	return 0
}

// The reset budget: one mail a minute and five an hour per account
// (ADR-0032 Karar 6).
func TestResetMailBudget(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	request := func() {
		t.Helper()
		if err := a.accounts.RequestPasswordReset(t.Context(), member); err != nil {
			t.Fatal(err)
		}
		a.work(t)
	}
	request()
	request() // within the cooldown
	if n := len(a.mail.to(member)); n != 1 {
		t.Fatalf("%d reset mails within the cooldown, want 1", n)
	}
	for range 6 {
		a.clock.Advance(authn.LinkMailCooldown + time.Second)
		request()
	}
	if n := len(a.mail.to(member)); n != authn.LinkMailsPerHour {
		t.Errorf("%d reset mails within the hour, want %d", n, authn.LinkMailsPerHour)
	}
	// One held by the cooldown, two by the hourly budget.
	if got := a.held(t, "account"); got != 3 {
		t.Errorf("%s{reason=account} = %v, want 3", authn.MetricAccountMailsHeld, got)
	}
}

// An undeliverable recipient cancels the mail job instead of retrying it
// ten times.
func TestUndeliverableMailIsCancelled(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.register(t, newcomer)
	var id uuid.UUID
	if err := a.db.Pool().QueryRow(t.Context(), "SELECT id FROM pending_registrations WHERE mailbox_key = $1",
		a.users.MailboxTarget(newcomer)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	a.mail.mu.Lock()
	a.mail.refuse = fmt.Errorf("%w: refused by the relay", mailer.ErrUndeliverable)
	a.mail.mu.Unlock()
	err := a.accounts.SendAccountMail(t.Context(), "registration_link", id)
	var cancel *rivertype.JobCancelError
	if !errors.As(err, &cancel) {
		t.Errorf("SendAccountMail to an undeliverable recipient = %v, want a River job cancellation", err)
	}
	a.mail.mu.Lock()
	a.mail.refuse = errors.New("a temporary relay failure")
	a.mail.mu.Unlock()
	if err := a.accounts.SendAccountMail(t.Context(), "registration_link", id); err == nil || errors.As(err, &cancel) {
		t.Errorf("SendAccountMail on a temporary failure = %v, want an error River retries", err)
	}
}

// The current password is not held to the rule: an account whose password
// was set under an older, shorter rule changes it (USR-021).
func TestCurrentPasswordIsNotHeldToTheRule(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, oldShort)
	pair := a.login(t, member, oldShort, deviceW1)
	p, err := a.users.Authenticate(t.Context(), pair.AccessToken, deviceW1)
	if err != nil {
		t.Fatal(err)
	}
	err = a.accounts.ChangePassword(t.Context(), p, authn.PasswordChange{Current: oldShort, New: goodPw, Confirmation: goodPw})
	if err != nil {
		t.Errorf("USR-021: a change from an 8-character current password = %v", err)
	}
}

// The token's consumption and the password write are one transaction: a
// failure injected into the password write leaves the token unconsumed
// and every session open, and the same token then works (USR-015).
func TestResetIsOneTransaction(t *testing.T) {
	t.Parallel()
	a := newAccountsEnv(t)
	a.addUser(t, member, otherPw)
	pair := a.login(t, member, otherPw, deviceW1)
	token := a.reset(t)

	owner := a.dbt.Open(t, db.RoleOwner)
	ctx := t.Context()
	for _, stmt := range []string{
		`CREATE FUNCTION test_refuse_password() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'injected failure of the password write'; END $$`,
		`CREATE TRIGGER test_refuse_password BEFORE UPDATE OF password_hash ON users
		 FOR EACH ROW EXECUTE FUNCTION test_refuse_password()`,
	} {
		if _, err := owner.Pool().Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.completeReset(token, goodPw, deviceW1); err == nil {
		t.Fatal("USR-015: the reset succeeded although the password write failed")
	}
	var consumed *time.Time
	if err := a.db.Pool().QueryRow(ctx, "SELECT consumed_at FROM password_reset_tokens").Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != nil {
		t.Error("USR-015: the token was consumed although the password write failed")
	}
	if _, err := a.users.Authenticate(ctx, pair.AccessToken, deviceW1); err != nil {
		t.Errorf("USR-015: the failed reset ended a session: %v", err)
	}
	if _, err := owner.Pool().Exec(ctx, "DROP TRIGGER test_refuse_password ON users"); err != nil {
		t.Fatal(err)
	}
	if err := a.completeReset(token, goodPw, deviceW1); err != nil {
		t.Errorf("USR-015: the token after the failed reset = %v", err)
	}
}
