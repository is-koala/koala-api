package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
)

// StaffService authenticates staff on the separate staff token class
// (ADR-0015 §5, D5): lockout, must_change_password and token_version.
// Staff sessions are not device-bound; the admin panel sends no device id
// (ADR-0021 §2).
type StaffService struct {
	db        *db.DB
	reads     *authndb.Queries
	passwords PasswordHasher
	tokens    *TokenClass
	clock     clock.Clock
	logger    *slog.Logger
}

// NewStaffService returns the staff authentication service. tokens is the
// staff class.
func NewStaffService(database *db.DB, passwords PasswordHasher, tokens *TokenClass, clk clock.Clock, logger *slog.Logger) (*StaffService, error) {
	if tokens.audience != AudienceStaff {
		return nil, fmt.Errorf("authn: staff service needs the %s token class, got %s", AudienceStaff, tokens.audience)
	}
	return &StaffService{
		db: database, reads: authndb.New(database.Pool()), passwords: passwords,
		tokens: tokens, clock: clk, logger: logger,
	}, nil
}

// Login signs a staff member in. A wrong password, an unknown address, a
// disabled account and a locked account are the same STAFF_LOGIN_FAILED
// and each pays one argon2id verification (AUTH-010, AUTH-011).
//
// The counter and the lock are read and written under the row's lock.
// StaffLockThreshold consecutive wrong passwords lock the account for
// StaffLockDuration; while locked nothing changes them, not even the
// right password. A successful sign-in resets the counter.
func (s *StaffService) Login(ctx context.Context, email, password string) (StaffTokenPair, error) {
	email = normalizeEmail(email)
	target := targetHash(email)
	failed := apperr.New(apperr.KindUnauthenticated, CodeStaffLoginFailed)

	cred, err := s.reads.GetStaffCredentials(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.passwords.VerifyDummy(ctx, password); err != nil {
			return StaffTokenPair{}, fmt.Errorf("authn: verify password: %w", err)
		}
		if err := insertAttempt(ctx, s.reads, principalStaff, target, false, s.clock.Now()); err != nil {
			return StaffTokenPair{}, err
		}
		return StaffTokenPair{}, failed
	}
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: load staff credentials: %w", err)
	}
	ok, rehash, err := s.passwords.Verify(ctx, cred.PasswordHash, password)
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: verify password: %w", err)
	}
	var newHash string
	if ok && rehash {
		if newHash, err = s.passwords.Hash(ctx, password); err != nil {
			return StaffTokenPair{}, fmt.Errorf("authn: rehash password: %w", err)
		}
	}

	now := s.clock.Now()
	var pair StaffTokenPair
	var outcome error
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		m, err := q.LockStaffForSignIn(ctx, cred.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = failed // deleted since the read above
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: lock staff member: %w", err)
		}
		fail := func() error {
			outcome = failed
			return insertAttempt(ctx, q, principalStaff, target, false, now)
		}
		switch {
		case staffLocked(m.LockedUntil, now):
			return fail()
		case !ok || m.PasswordHash != cred.PasswordHash:
			// A hash changed since the read counts as a wrong password.
			n, until := staffFailure(m.FailedLoginAttempts, now)
			err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{FailedLoginAttempts: n, LockedUntil: until, ID: m.ID})
			if err != nil {
				return fmt.Errorf("authn: count staff failure: %w", err)
			}
			if until != nil {
				s.logger.WarnContext(ctx, "staff account locked", slog.String("staff_id", m.ID.String()))
			}
			return fail()
		case m.Status != StaffActive:
			return fail()
		}
		if m.FailedLoginAttempts != 0 || m.LockedUntil != nil {
			if err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{ID: m.ID}); err != nil {
				return fmt.Errorf("authn: reset staff failures: %w", err)
			}
		}
		if newHash != "" {
			if err := q.UpdateStaffPasswordHash(ctx, authndb.UpdateStaffPasswordHashParams{ID: m.ID, PasswordHash: newHash}); err != nil {
				return fmt.Errorf("authn: store rehash: %w", err)
			}
		}
		sid := newID()
		end := now.Add(StaffSessionMaxTTL)
		err = q.InsertStaffSession(ctx, authndb.InsertStaffSessionParams{
			ID: sid, StaffID: m.ID, TokenVersion: m.TokenVersion, CreatedAt: now, LastSeenAt: now, ExpiresAt: end,
		})
		if err != nil {
			return fmt.Errorf("authn: insert staff session: %w", err)
		}
		tp, err := s.issuePair(ctx, q, m.ID, sid, m.TokenVersion, now, end)
		if err != nil {
			return err
		}
		pair = StaffTokenPair{TokenPair: tp, MustChangePassword: m.MustChangePassword}
		return insertAttempt(ctx, q, principalStaff, target, true, now)
	})
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: staff sign in: %w", err)
	}
	if outcome != nil {
		return StaffTokenPair{}, outcome
	}
	return pair, nil
}

func (s *StaffService) issuePair(ctx context.Context, q *authndb.Queries, staffID, sid uuid.UUID, stv int64, now, sessionEnd time.Time) (TokenPair, error) {
	refresh, hash := newOpaque()
	refreshExp := refreshExpiry(now, sessionEnd, StaffRefreshIdleTTL)
	err := q.InsertStaffRefreshToken(ctx, authndb.InsertStaffRefreshTokenParams{
		ID: newID(), SessionID: sid, TokenHash: hash, CreatedAt: now, ExpiresAt: refreshExp,
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: insert staff refresh token: %w", err)
	}
	access, accessExp, err := s.tokens.issue(staffID, sid, stv, []string{amrPassword})
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken: access, AccessTokenExpiresAt: accessExp,
		RefreshToken: refresh, RefreshTokenExpiresAt: refreshExp, SessionID: sid,
	}, nil
}

// Refresh rotates a staff refresh token like the marketplace's Refresh.
// A token issued before the account's token_version moved (logout,
// password change, disabling) no longer works (AUTH-013).
func (s *StaffService) Refresh(ctx context.Context, refreshToken string) (StaffTokenPair, error) {
	if !wellFormedOpaque(refreshToken) {
		return StaffTokenPair{}, apperr.New(apperr.KindUnauthenticated, CodeRefreshTokenInvalid)
	}
	now := s.clock.Now()
	var pair StaffTokenPair
	var outcome error
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		row, err := q.GetStaffRefreshTokenForUpdate(ctx, hashOpaque(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = apperr.New(apperr.KindUnauthenticated, CodeRefreshTokenInvalid)
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: load staff refresh token: %w", err)
		}
		end := func(reason string, code apperr.Code) error {
			outcome = apperr.New(apperr.KindUnauthenticated, code)
			if err := q.RevokeStaffSession(ctx, authndb.RevokeStaffSessionParams{Now: now, Reason: reason, ID: row.SessionID}); err != nil {
				return fmt.Errorf("authn: revoke staff session: %w", err)
			}
			return nil
		}
		switch {
		case row.RevokedAt != nil:
			outcome = apperr.New(apperr.KindUnauthenticated, CodeStaffSessionEnded)
			return nil
		case row.ConsumedAt != nil:
			s.logger.WarnContext(ctx, "staff refresh token reused; session revoked",
				slog.String("session_id", row.SessionID.String()))
			return end(reasonRefreshReused, CodeRefreshTokenReused)
		case row.MemberTokenVersion == nil || *row.MemberTokenVersion != row.TokenVersion ||
			row.MemberStatus == nil || *row.MemberStatus != StaffActive:
			return end(reasonInactive, CodeStaffSessionEnded)
		case !now.Before(row.ExpiresAt) || !now.Before(row.SessionExpiresAt):
			return end(reasonExpired, CodeRefreshTokenExpired)
		}
		if err := q.ConsumeStaffRefreshToken(ctx, authndb.ConsumeStaffRefreshTokenParams{Now: now, ID: row.ID}); err != nil {
			return fmt.Errorf("authn: consume staff refresh token: %w", err)
		}
		if err := q.TouchStaffSession(ctx, authndb.TouchStaffSessionParams{Now: now, ID: row.SessionID, SeenBefore: now}); err != nil {
			return fmt.Errorf("authn: touch staff session: %w", err)
		}
		tp, err := s.issuePair(ctx, q, row.StaffID, row.SessionID, row.TokenVersion, now, row.SessionExpiresAt)
		if err != nil {
			return err
		}
		m, err := q.GetStaffMember(ctx, row.StaffID)
		if err != nil {
			return fmt.Errorf("authn: load staff member: %w", err)
		}
		pair = StaffTokenPair{TokenPair: tp, MustChangePassword: m.MustChangePassword}
		return nil
	})
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: staff refresh: %w", err)
	}
	if outcome != nil {
		return StaffTokenPair{}, outcome
	}
	return pair, nil
}

// Authenticate checks a staff request: the staff-class access token, then
// the session and the account's current token_version and status. Every
// session failure is STAFF_SESSION_ENDED.
func (s *StaffService) Authenticate(ctx context.Context, bearer string) (StaffPrincipal, error) {
	tok, err := s.tokens.verify(bearer)
	if err != nil {
		return StaffPrincipal{}, err
	}
	ended := apperr.New(apperr.KindUnauthenticated, CodeStaffSessionEnded)
	st, err := s.reads.GetStaffSessionState(ctx, tok.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffPrincipal{}, ended
	}
	if err != nil {
		return StaffPrincipal{}, fmt.Errorf("authn: load staff session: %w", err)
	}
	now := s.clock.Now()
	live := st.StaffID == tok.Subject && st.RevokedAt == nil && now.Before(st.ExpiresAt) &&
		st.MemberTokenVersion != nil && *st.MemberTokenVersion == tok.TokenVersion && st.TokenVersion == tok.TokenVersion &&
		st.MemberStatus != nil && *st.MemberStatus == StaffActive && st.MustChangePassword != nil
	if !live {
		return StaffPrincipal{}, ended
	}
	if now.Sub(st.LastSeenAt) >= touchInterval {
		err := s.reads.TouchStaffSession(ctx, authndb.TouchStaffSessionParams{Now: now, ID: tok.SessionID, SeenBefore: now.Add(-touchInterval)})
		if err != nil {
			s.logger.WarnContext(ctx, "touch staff session failed", slog.Any("error", err))
		}
	}
	return StaffPrincipal{
		StaffID: tok.Subject, SessionID: tok.SessionID,
		MustChangePassword: *st.MustChangePassword, MFA: false,
	}, nil
}

// Logout ends every token of the account: the token version moves, which
// kills the access and refresh tokens of all its sessions (AUTH-013).
func (s *StaffService) Logout(ctx context.Context, p StaffPrincipal) error {
	now := s.clock.Now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		if err := q.BumpStaffTokenVersion(ctx, p.StaffID); err != nil {
			return fmt.Errorf("authn: bump token version: %w", err)
		}
		if err := q.RevokeStaffSession(ctx, authndb.RevokeStaffSessionParams{Now: now, Reason: reasonLogout, ID: p.SessionID}); err != nil {
			return fmt.Errorf("authn: revoke staff session: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("authn: staff logout: %w", err)
	}
	return nil
}

// ChangePassword sets a new password after checking the current one. The
// new password follows the rule of AUTH-009; the must-change flag drops
// and the token version moves, so every token, the caller's included, is
// dead and the member signs in again (AUTH-013, AUTH-014).
func (s *StaffService) ChangePassword(ctx context.Context, p StaffPrincipal, current, next string) error {
	if err := CheckPassword(next); err != nil {
		return err
	}
	stored, err := s.reads.GetStaffPasswordHash(ctx, p.StaffID)
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(apperr.KindUnauthenticated, CodeStaffSessionEnded)
	}
	if err != nil {
		return fmt.Errorf("authn: load staff password: %w", err)
	}
	ok, _, err := s.passwords.Verify(ctx, stored, current)
	if err != nil {
		return fmt.Errorf("authn: verify password: %w", err)
	}
	if !ok {
		return apperr.New(apperr.KindForbidden, CodeStaffCurrentPasswordMismatch)
	}
	hash, err := s.passwords.Hash(ctx, next)
	if err != nil {
		return fmt.Errorf("authn: hash password: %w", err)
	}
	now := s.clock.Now()
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		if err := q.ChangeStaffPassword(ctx, authndb.ChangeStaffPasswordParams{PasswordHash: hash, Now: now, ID: p.StaffID}); err != nil {
			return fmt.Errorf("authn: change staff password: %w", err)
		}
		if err := q.RevokeStaffSession(ctx, authndb.RevokeStaffSessionParams{Now: now, Reason: reasonLogout, ID: p.SessionID}); err != nil {
			return fmt.Errorf("authn: revoke staff session: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("authn: staff password change: %w", err)
	}
	return nil
}

// Me returns the caller's staff account.
func (s *StaffService) Me(ctx context.Context, p StaffPrincipal) (StaffMember, error) {
	m, err := s.reads.GetStaffMember(ctx, p.StaffID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffMember{}, apperr.New(apperr.KindUnauthenticated, CodeStaffSessionEnded)
	}
	if err != nil {
		return StaffMember{}, fmt.Errorf("authn: load staff member: %w", err)
	}
	return StaffMember{ID: m.ID, Email: m.Email, MustChangePassword: m.MustChangePassword}, nil
}

// Purge hard-deletes staff sessions that ended more than
// EndedSessionRetention ago, with their refresh tokens.
func (s *StaffService) Purge(ctx context.Context) (int64, error) {
	n, err := s.reads.PurgeStaffSessions(ctx, s.clock.Now().Add(-EndedSessionRetention))
	if err != nil {
		return 0, fmt.Errorf("authn: purge staff sessions: %w", err)
	}
	return n, nil
}
