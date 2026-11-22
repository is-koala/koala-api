package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/pending"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/profilesync"
	"github.com/is-koala/koala-api/internal/users/usersdb"
)

// verifier is the verification start of authn (ADR-0033). Every surface
// that starts a verification proves the password with Reauthenticate and
// opens it with StartTx, where the rules of a start live once: the password
// check and its lock, the target rule, the resend budget and the start
// lock (ADR-0036 Karar 1).
type verifier interface {
	Reauthenticate(ctx context.Context, p authn.UserPrincipal, currentPassword string) (authn.Reauthentication, error)
	StartTx(ctx context.Context, tx db.Querier, p authn.UserPrincipal, r authn.Reauthentication,
		channel, raw, pointer string) (authn.Verification, error)
}

// Service serves the caller's profile and education records.
type Service struct {
	db      *db.DB
	reads   *usersdb.Queries
	verify  verifier
	deleter *softdelete.Deleter
	clock   clock.Clock
}

// NewService returns the service on database. verify starts the
// verifications a profile patch asks for; deleter soft-deletes education
// records along the catalog.
func NewService(database *db.DB, verify verifier, deleter *softdelete.Deleter, clk clock.Clock) (*Service, error) {
	if database == nil || verify == nil || deleter == nil || clk == nil {
		return nil, errors.New("users: the service needs a database, the verification start, a deleter and a clock")
	}
	return &Service{db: database, reads: usersdb.New(database.Pool()), verify: verify, deleter: deleter, clock: clk}, nil
}

// sessionInactive is the answer to a caller whose user is gone or
// suspended: the session check's own code.
func sessionInactive() error {
	return apperr.New(apperr.KindUnauthenticated, authn.CodeSessionInactive)
}

// invalidField is a REQUEST_INVALID naming one body member whose value the
// schema allows but the rule does not.
func invalidField(pointer string) error {
	return apperr.New(apperr.KindInvalid, httpx.CodeRequestInvalid).
		WithFields(apperr.FieldError{Pointer: pointer, Code: httpx.CodeFieldValueInvalid})
}

// Profile returns the caller's profile.
func (s *Service) Profile(ctx context.Context, p authn.UserPrincipal) (Profile, error) {
	row, err := s.reads.GetProfile(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, sessionInactive()
	}
	if err != nil {
		return Profile{}, fmt.Errorf("users: load the profile: %w", err)
	}
	return profileOf(usersdb.LockProfileRow(row)), nil
}

func profileOf(r usersdb.LockProfileRow) Profile {
	return Profile{
		ID: r.ID, Email: r.Email, FirstName: r.FirstName, LastName: r.LastName, Phone: r.Phone,
		BirthDate: r.BirthDate, Gender: r.Gender, JobTitle: r.JobTitle, About: r.About,
		Qualifications: r.Qualifications, Interests: r.Interests, Employer: r.Employer,
		EmailVerified: r.EmailVerified, PhoneVerified: r.PhoneVerified, IdentityVerified: r.IdentityVerified,
		CreatedAt: r.CreatedAt,
	}
}

// Update applies a profile patch (RFC 7396) and returns the profile and
// the verifications it started. In order:
//
//  1. a link to a media or an address is not live yet: 501 (ADR-0036
//     Karar 5);
//  2. the values the schema allows but the rules do not: REQUEST_INVALID
//     (an empty name, a birth date in the future, an address or number a
//     verification refuses);
//  3. an address or a number in the patch needs the current password
//     (ADR-0033 Karar 1b), checked like every start's (Reauthenticate): a
//     wrong one is PASSWORD_MISMATCH and counts on the sign-in lock;
//  4. in one transaction, under the caller's row lock: the identity freeze
//     (USR-024), the fields and the profile sync intent (USR-022), and a
//     verification of each address or number that differs from the one on
//     file (USR-014), under every rule of a start (StartTx). A refusal of
//     any of them rolls the whole patch back.
func (s *Service) Update(ctx context.Context, p authn.UserPrincipal, patch Patch) (Profile, []authn.Verification, error) {
	if patch.links() {
		return Profile{}, nil, apperr.New(apperr.KindNotImplemented, pending.CodeNotImplemented)
	}
	if err := checkPatch(patch, s.clock.Now()); err != nil {
		return Profile{}, nil, err
	}
	email, phone, err := contactTargets(patch)
	if err != nil {
		return Profile{}, nil, err
	}
	var reauth authn.Reauthentication
	if patch.contactChange() {
		r, err := s.verify.Reauthenticate(ctx, p, patch.CurrentPassword)
		if err != nil {
			return Profile{}, nil, fmt.Errorf("users: confirm the password: %w", err)
		}
		reauth = r
	}
	now := s.clock.Now()
	origin := profilesync.Origin{RequestID: httpx.RequestIDFrom(ctx), SessionID: p.SessionID, DeviceKey: p.DeviceKey}
	var out Profile
	var started []authn.Verification
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := usersdb.New(tx)
		row, err := q.LockProfile(ctx, p.UserID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.Status != userActive) {
			return sessionInactive()
		}
		if err != nil {
			return fmt.Errorf("users: lock the profile: %w", err)
		}
		cur := profileOf(row)
		next, err := patch.apply(cur)
		if err != nil {
			return err
		}
		if fieldsChanged(cur, next) {
			err := q.UpdateProfile(ctx, usersdb.UpdateProfileParams{
				ID: p.UserID, FirstName: next.FirstName, LastName: next.LastName, BirthDate: next.BirthDate,
				Gender: next.Gender, JobTitle: next.JobTitle, About: next.About, Qualifications: next.Qualifications,
				Interests: next.Interests,
			})
			if err != nil {
				return fmt.Errorf("users: write the profile: %w", err)
			}
			if err := profilesync.Record(ctx, tx, p.UserID, origin, now); err != nil {
				return fmt.Errorf("users: record the profile change: %w", err)
			}
		}
		// The value on file starts nothing (USR-014).
		if email != "" && email != cur.Email {
			v, err := s.verify.StartTx(ctx, tx, p, reauth, authn.ChannelEmail, *patch.Email, "/email")
			if err != nil {
				return fmt.Errorf("users: start the address's verification: %w", err)
			}
			started = append(started, v)
		}
		if phone != "" && (cur.Phone == nil || *cur.Phone != phone) {
			v, err := s.verify.StartTx(ctx, tx, p, reauth, authn.ChannelPhone, *patch.PhoneNumber, "/phoneNumber")
			if err != nil {
				return fmt.Errorf("users: start the number's verification: %w", err)
			}
			started = append(started, v)
		}
		out = next
		return nil
	})
	if err != nil {
		return Profile{}, nil, fmt.Errorf("users: update the profile: %w", err)
	}
	return out, started, nil
}

// checkPatch refuses the values the schema allows but the rules do not:
// a name that is empty once trimmed, a birth date in the future or before
// 1900.
func checkPatch(p Patch, now time.Time) error {
	if p.FirstName != nil && nameInvalid(strings.TrimSpace(*p.FirstName)) {
		return invalidField("/firstName")
	}
	if p.LastName != nil && nameInvalid(strings.TrimSpace(*p.LastName)) {
		return invalidField("/lastName")
	}
	if p.BirthDate.Value != nil && birthDateInvalid(*p.BirthDate.Value, now) {
		return invalidField("/birthDate")
	}
	return nil
}

// contactTargets are the stored forms of the patch's address and number
// ("" when not sent), under the verification's own target rule: the form
// a start writes and the patch compares with the value on file.
func contactTargets(p Patch) (email, phone string, err error) {
	if p.Email != nil {
		email, err = authn.CheckTarget(authn.ChannelEmail, *p.Email, "/email")
		if err != nil {
			return "", "", fmt.Errorf("users: the new address: %w", err)
		}
	}
	if p.PhoneNumber != nil {
		phone, err = authn.CheckTarget(authn.ChannelPhone, *p.PhoneNumber, "/phoneNumber")
		if err != nil {
			return "", "", fmt.Errorf("users: the new number: %w", err)
		}
	}
	return email, phone, nil
}

// newID is a UUIDv7: time-ordered, so new rows land at the end of the
// primary key's index.
func newID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}
