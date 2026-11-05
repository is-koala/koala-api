package staff

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/audit"
	"github.com/is-koala/koala-api/internal/audit/auditdb"
	"github.com/is-koala/koala-api/internal/authz"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
)

// hasher hashes the one-time password (authn.Passwords).
type hasher interface {
	Hash(ctx context.Context, password string) (string, error)
}

// trail writes the audit rows of staff management (audit.Recorder).
type trail interface {
	Pending(ctx context.Context, q auditdb.DBTX, e audit.Entry) (uuid.UUID, error)
	Result(ctx context.Context, q auditdb.DBTX, pending uuid.UUID, e audit.Entry, succeeded bool) error
	Detail(ctx context.Context, q auditdb.DBTX, entry uuid.UUID, body any, clientKey string) error
}

// Admin manages staff accounts on a koala_maint pool: the `koala staff`
// command, the only way to create staff until staff management lands in
// the API (Faz 8). The first super admin is made with it (`koala staff
// create --email … --role OWNER`). Every change is audited with the
// system as actor: a pending row before, a result row after.
type Admin struct {
	maint     *db.DB
	passwords hasher
	trail     trail
	store     store
	clock     clock.Clock
}

// NewAdmin returns staff management on maint, a koala_maint pool.
func NewAdmin(maint *db.DB, passwords hasher, rec trail, sink relationSink, clk clock.Clock) *Admin {
	return &Admin{maint: maint, passwords: passwords, trail: rec, store: store{sink: sink}, clock: clk}
}

// Created is a new staff account and its one-time password, which the
// operator hands over out of band; the member must change it at the first
// sign-in (AUTH-014).
type Created struct {
	ID       uuid.UUID
	Email    string
	Roles    []string
	Password string
}

// normalizeEmail is the login address as staff accounts store it.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// Create opens a staff account with the given roles and a one-time
// password.
func (a *Admin) Create(ctx context.Context, email string, roles []string) (Created, error) {
	email = normalizeEmail(email)
	if !validEmail(email) {
		return Created{}, errors.New("staff: the login address is not valid")
	}
	if !distinctRoles(roles) {
		return Created{}, fmt.Errorf("staff: give between 1 and %d distinct roles", maxRolesPerCreate)
	}
	password, err := oneTimePassword()
	if err != nil {
		return Created{}, err
	}
	hash, err := a.passwords.Hash(ctx, password)
	if err != nil {
		return Created{}, fmt.Errorf("staff: hash the one-time password: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Created{}, fmt.Errorf("staff: member id: %w", err)
	}
	e := entry(ActionCreate, id)
	body := map[string]any{"email": email, "roles": roles}
	err = a.audited(ctx, e, body, func(ctx context.Context, q db.Querier) error {
		now := a.clock.Now()
		if err := a.store.insertMember(ctx, q, id, email, hash, now); err != nil {
			return err
		}
		return a.store.assignRoles(ctx, q, id, roles, authz.Subject{Kind: authz.SubjectSystem}, now)
	})
	if err != nil {
		return Created{}, err
	}
	return Created{ID: id, Email: email, Roles: roles, Password: password}, nil
}

// Disabled is an account taken out of service.
type Disabled struct {
	ID           uuid.UUID
	TokenVersion int64
}

// Disable takes the account with the login address email out of service.
// Its token version moves (the database does it), so every access and
// refresh token of the account dies on its next use (ADR-0025 §6, issue
// #7).
func (a *Admin) Disable(ctx context.Context, email string) (Disabled, error) {
	email = normalizeEmail(email)
	var found member
	err := a.maint.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		m, ok, err := a.store.findMember(ctx, q, email)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("staff: no staff account with that login address")
		}
		found = m
		return nil
	})
	if err != nil {
		return Disabled{}, fmt.Errorf("staff: disable: %w", err)
	}
	var out Disabled
	err = a.audited(ctx, entry(ActionDisable, found.ID), nil, func(ctx context.Context, q db.Querier) error {
		m, ok, err := a.store.findMember(ctx, q, email)
		switch {
		case err != nil:
			return err
		case !ok || m.ID != found.ID:
			return errors.New("staff: the account changed meanwhile; run the command again")
		case m.Status != "ACTIVE":
			return errors.New("staff: the account is not active")
		}
		v, err := a.store.disable(ctx, q, m.ID)
		if err != nil {
			return err
		}
		out = Disabled{ID: m.ID, TokenVersion: v}
		return nil
	})
	if err != nil {
		return Disabled{}, err
	}
	return out, nil
}

// entry is the audit entry of a staff management action on a member.
func entry(action string, member uuid.UUID) audit.Entry {
	return audit.Entry{
		Actor: authz.Subject{Kind: authz.SubjectSystem}, Action: action,
		Target:   authz.Object{Type: authz.TypeStaff, ID: member.String()},
		Mutating: true, Sensitivity: authz.SensitivityPersonalData,
	}
}

// audited runs fn in a transaction between a pending audit row (written
// and committed first: no row, no change) and its result row.
func (a *Admin) audited(ctx context.Context, e audit.Entry, body any, fn func(ctx context.Context, q db.Querier) error) error {
	pool := a.maint.Pool()
	pending, err := a.trail.Pending(ctx, pool, e)
	if err != nil {
		return fmt.Errorf("staff: %s: %w", e.Action, err)
	}
	if body != nil {
		if err := a.trail.Detail(ctx, pool, pending, body, ""); err != nil {
			return fmt.Errorf("staff: %s: %w", e.Action, err)
		}
	}
	err = a.maint.InTx(ctx, fn)
	if err != nil {
		err = fmt.Errorf("staff: %s: %w", e.Action, err)
	}
	if rerr := a.trail.Result(context.WithoutCancel(ctx), pool, pending, e, err == nil); rerr != nil {
		return errors.Join(err, fmt.Errorf("staff: %s: audit result: %w", e.Action, rerr))
	}
	return err
}

// oneTimePassword is 24 characters from 18 random bytes, longer than the
// password rule's minimum (AUTH-009).
func oneTimePassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("staff: one-time password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
