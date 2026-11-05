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
// (ADR-0015 §5, D5): the address lock and account backoff,
// must_change_password and token_version. Staff sessions are not
// device-bound; the admin panel sends no device id (ADR-0021 §2).
type StaffService struct {
	db        *db.DB
	reads     *authndb.Queries
	passwords PasswordHasher
	tokens    *TokenClass
	targets   targets
	clock     clock.Clock
	logger    *slog.Logger
	metrics   *Metrics
}

// NewStaffService returns the staff authentication service. tokens is the
// staff class; pepper keys the sign-in targets.
func NewStaffService(database *db.DB, passwords PasswordHasher, tokens *TokenClass, pepper []byte, clk clock.Clock,
	logger *slog.Logger, metrics *Metrics,
) (*StaffService, error) {
	if tokens.audience != AudienceStaff {
		return nil, fmt.Errorf("authn: staff service needs the %s token class, got %s", AudienceStaff, tokens.audience)
	}
	if metrics == nil || len(pepper) == 0 {
		return nil, errors.New("authn: metrics and login pepper are required")
	}
	return &StaffService{
		db: database, reads: authndb.New(database.Pool()), passwords: passwords, tokens: tokens,
		targets: targets{pepper: pepper}, clock: clk, logger: logger, metrics: metrics,
	}, nil
}

// Target returns the keyed hash that stands for a staff sign-in
// identifier in the attempt log and the rate limit keys.
func (s *StaffService) Target(email string) []byte { return s.targets.of(normalizeEmail(email)) }

// Login signs a staff member in from clientKey (ClientKey of the request's
// address). A wrong password, an unknown address, a disabled account and
// a locked or backed-off account are the same STAFF_LOGIN_FAILED and each
// pays one argon2id verification (AUTH-010, AUTH-011).
//
// The counters are read and written under the member's row lock
// (ADR-0025 §6): StaffLockThreshold consecutive failures from one address
// lock the account for that address for StaffLockDuration; from
// StaffBackoffAfter consecutive failures on the account, from any
// address, every attempt waits out a doubling backoff. While either is in
// force nothing changes, not even with the right password. A success
// resets the account counter and the address's counter.
func (s *StaffService) Login(ctx context.Context, email, password, clientKey string) (StaffTokenPair, error) {
	email = normalizeEmail(email)
	target := s.targets.of(email)
	failed := apperr.New(apperr.KindUnauthenticated, CodeStaffLoginFailed)

	cred, err := s.reads.GetStaffCredentials(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.passwords.VerifyDummy(ctx, password); err != nil {
			return StaffTokenPair{}, fmt.Errorf("authn: verify password: %w", err)
		}
		if err := insertAttempt(ctx, s.reads, principalStaff, target, outcomeFailed, s.clock.Now()); err != nil {
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
			return insertAttempt(ctx, q, principalStaff, target, outcomeFailed, now)
		}
		if err != nil {
			return fmt.Errorf("authn: lock staff member: %w", err)
		}
		addr, err := addressLock(ctx, q, m.ID, clientKey)
		if err != nil {
			return err
		}
		switch {
		case inForce(addr.LockedUntil, now) || inForce(m.BackoffUntil, now):
			outcome = failed
			return insertAttempt(ctx, q, principalStaff, target, outcomeLocked, now)
		case !ok || m.PasswordHash != cred.PasswordHash:
			// A hash changed since the read counts as a wrong password.
			outcome = failed
			if _, err := s.countFailure(ctx, q, m.ID, m.FailedLoginAttempts, addr.Failures, clientKey, now); err != nil {
				return err
			}
			return insertAttempt(ctx, q, principalStaff, target, outcomeFailed, now)
		case m.Status != StaffActive:
			outcome = failed
			return insertAttempt(ctx, q, principalStaff, target, outcomeFailed, now)
		}
		if err := s.clearFailures(ctx, q, m.ID, m.FailedLoginAttempts != 0 || m.BackoffUntil != nil, clientKey); err != nil {
			return err
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
		tp, err := s.issuePair(ctx, q, m.ID, sid, m.TokenVersion, newOpaque(), now, end)
		if err != nil {
			return err
		}
		pair = StaffTokenPair{TokenPair: tp, MustChangePassword: m.MustChangePassword}
		return insertAttempt(ctx, q, principalStaff, target, outcomeSucceeded, now)
	})
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: staff sign in: %w", err)
	}
	if outcome != nil {
		return StaffTokenPair{}, outcome
	}
	return pair, nil
}

// addressLock reads and locks the (member, address) counter; a pair with
// no row has no failures.
func addressLock(ctx context.Context, q *authndb.Queries, staffID uuid.UUID, clientKey string) (authndb.GetStaffLoginLockRow, error) {
	row, err := q.GetStaffLoginLock(ctx, authndb.GetStaffLoginLockParams{StaffID: staffID, ClientKey: clientKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return authndb.GetStaffLoginLockRow{}, nil
	}
	if err != nil {
		return authndb.GetStaffLoginLockRow{}, fmt.Errorf("authn: load staff address lock: %w", err)
	}
	return row, nil
}

// countFailure applies one wrong password to the account counter and the
// address counter, and reports whether the address is now locked. Locks
// and backoffs are counted (MetricLockouts) and logged as warnings.
func (s *StaffService) countFailure(ctx context.Context, q *authndb.Queries, staffID uuid.UUID,
	accountFailures, addressFailures int32, clientKey string, now time.Time,
) (bool, error) {
	n, backoff := accountFailure(accountFailures, now)
	if err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{FailedLoginAttempts: n, BackoffUntil: backoff, ID: staffID}); err != nil {
		return false, fmt.Errorf("authn: count staff failure: %w", err)
	}
	if backoff != nil {
		s.metrics.lockout("staff", scopeBackoff)
		s.logger.WarnContext(ctx, "staff account backoff after consecutive failures",
			slog.String("staff_id", staffID.String()), slog.Int("failures", int(n)), slog.Time("until", *backoff))
	}
	af, until := addressFailure(addressFailures, now)
	err := q.SetStaffLoginLock(ctx, authndb.SetStaffLoginLockParams{
		StaffID: staffID, ClientKey: clientKey, Failures: af, LockedUntil: until, Now: now,
	})
	if err != nil {
		return false, fmt.Errorf("authn: count staff address failure: %w", err)
	}
	if until != nil {
		s.metrics.lockout("staff", scopeAddress)
		s.logger.WarnContext(ctx, "staff account locked for a client address",
			slog.String("staff_id", staffID.String()), slog.Time("until", *until))
	}
	return until != nil, nil
}

// clearFailures resets the counters a success clears: the account's (when
// it has any) and the address's.
func (s *StaffService) clearFailures(ctx context.Context, q *authndb.Queries, staffID uuid.UUID, account bool, clientKey string) error {
	if account {
		if err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{ID: staffID}); err != nil {
			return fmt.Errorf("authn: reset staff failures: %w", err)
		}
	}
	if err := q.ClearStaffLoginLock(ctx, authndb.ClearStaffLoginLockParams{StaffID: staffID, ClientKey: clientKey}); err != nil {
		return fmt.Errorf("authn: reset staff address failures: %w", err)
	}
	return nil
}

// issuePair stores refresh as a new refresh token of session sid and signs
// an access token.
func (s *StaffService) issuePair(ctx context.Context, q *authndb.Queries, staffID, sid uuid.UUID, stv int64, refresh string, now, sessionEnd time.Time) (TokenPair, error) {
	refreshExp := refreshExpiry(now, sessionEnd, StaffRefreshIdleTTL)
	err := q.InsertStaffRefreshToken(ctx, authndb.InsertStaffRefreshTokenParams{
		ID: newID(), SessionID: sid, TokenHash: hashOpaque(refresh), CreatedAt: now, ExpiresAt: refreshExp,
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

// Refresh rotates a staff refresh token like the marketplace's Refresh,
// with the same grace window for a retry. A token issued before the
// account's token_version moved (logout, password change, disabling) no
// longer works (AUTH-013).
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
		live := row.MemberTokenVersion != nil && *row.MemberTokenVersion == row.TokenVersion &&
			row.MemberStatus != nil && *row.MemberStatus == StaffActive
		var refresh string
		switch {
		case row.RevokedAt != nil:
			outcome = apperr.New(apperr.KindUnauthenticated, CodeStaffSessionEnded)
			return nil
		case row.ConsumedAt != nil:
			if live && !now.After(row.ConsumedAt.Add(RefreshGraceWindow)) && len(row.SuccessorSalt) > 0 {
				retry, ok, err := s.retrySuccessor(ctx, q, successorOf(refreshToken, row.SuccessorSalt), row, now)
				if err != nil || ok {
					pair = retry
					return err
				}
			}
			s.logger.WarnContext(ctx, "staff refresh token reused; session revoked",
				slog.String("session_id", row.SessionID.String()))
			return end(reasonRefreshReused, CodeRefreshTokenReused)
		case !live:
			return end(reasonInactive, CodeStaffSessionEnded)
		case !now.Before(row.ExpiresAt) || !now.Before(row.SessionExpiresAt):
			return end(reasonExpired, CodeRefreshTokenExpired)
		default:
			salt := newSalt()
			err := q.ConsumeStaffRefreshToken(ctx, authndb.ConsumeStaffRefreshTokenParams{Now: now, SuccessorSalt: salt, ID: row.ID})
			if err != nil {
				return fmt.Errorf("authn: consume staff refresh token: %w", err)
			}
			refresh = successorOf(refreshToken, salt)
		}
		if err := q.TouchStaffSession(ctx, authndb.TouchStaffSessionParams{Now: now, ID: row.SessionID, SeenBefore: now}); err != nil {
			return fmt.Errorf("authn: touch staff session: %w", err)
		}
		tp, err := s.issuePair(ctx, q, row.StaffID, row.SessionID, row.TokenVersion, refresh, now, row.SessionExpiresAt)
		if err != nil {
			return err
		}
		mustChange, err := s.mustChange(ctx, q, row.StaffID)
		pair = StaffTokenPair{TokenPair: tp, MustChangePassword: mustChange}
		return err
	})
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: staff refresh: %w", err)
	}
	if outcome != nil {
		return StaffTokenPair{}, outcome
	}
	return pair, nil
}

// retrySuccessor answers a retry inside the grace window with the unused
// successor and a new access token; ok is false when it does not qualify.
func (s *StaffService) retrySuccessor(ctx context.Context, q *authndb.Queries, successor string,
	row authndb.GetStaffRefreshTokenForUpdateRow, now time.Time,
) (StaffTokenPair, bool, error) {
	next, err := q.GetStaffRefreshTokenForUpdate(ctx, hashOpaque(successor))
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffTokenPair{}, false, nil
	}
	if err != nil {
		return StaffTokenPair{}, false, fmt.Errorf("authn: load staff successor: %w", err)
	}
	if next.ConsumedAt != nil || !now.Before(next.ExpiresAt) || !now.Before(next.SessionExpiresAt) {
		return StaffTokenPair{}, false, nil
	}
	access, accessExp, err := s.tokens.issue(row.StaffID, row.SessionID, row.TokenVersion, []string{amrPassword})
	if err != nil {
		return StaffTokenPair{}, false, err
	}
	mustChange, err := s.mustChange(ctx, q, row.StaffID)
	if err != nil {
		return StaffTokenPair{}, false, err
	}
	return StaffTokenPair{
		TokenPair: TokenPair{
			AccessToken: access, AccessTokenExpiresAt: accessExp,
			RefreshToken: successor, RefreshTokenExpiresAt: next.ExpiresAt, SessionID: row.SessionID,
		},
		MustChangePassword: mustChange,
	}, true, nil
}

func (s *StaffService) mustChange(ctx context.Context, q *authndb.Queries, staffID uuid.UUID) (bool, error) {
	m, err := q.GetStaffMember(ctx, staffID)
	if err != nil {
		return false, fmt.Errorf("authn: load staff member: %w", err)
	}
	return m.MustChangePassword, nil
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
//
// A wrong current password counts like a wrong sign-in password from
// clientKey (ADR-0025 §6); when it locks the address the session ends.
func (s *StaffService) ChangePassword(ctx context.Context, p StaffPrincipal, current, next, clientKey string) error {
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
	now := s.clock.Now()
	if !ok {
		err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
			q := authndb.New(tx)
			m, err := q.LockStaffForSignIn(ctx, p.StaffID)
			if err != nil {
				return fmt.Errorf("authn: lock staff member: %w", err)
			}
			addr, err := addressLock(ctx, q, p.StaffID, clientKey)
			if err != nil {
				return err
			}
			locked, err := s.countFailure(ctx, q, p.StaffID, m.FailedLoginAttempts, addr.Failures, clientKey, now)
			if err != nil || !locked {
				return err
			}
			if err := q.RevokeStaffSession(ctx, authndb.RevokeStaffSessionParams{Now: now, Reason: reasonLocked, ID: p.SessionID}); err != nil {
				return fmt.Errorf("authn: revoke staff session: %w", err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("authn: count staff password failure: %w", err)
		}
		return apperr.New(apperr.KindForbidden, CodeStaffCurrentPasswordMismatch)
	}
	hash, err := s.passwords.Hash(ctx, next)
	if err != nil {
		return fmt.Errorf("authn: hash password: %w", err)
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		if err := q.ChangeStaffPassword(ctx, authndb.ChangeStaffPasswordParams{PasswordHash: hash, Now: now, ID: p.StaffID}); err != nil {
			return fmt.Errorf("authn: change staff password: %w", err)
		}
		if err := s.clearFailures(ctx, q, p.StaffID, true, clientKey); err != nil {
			return err
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
// EndedSessionRetention ago, with their refresh tokens, and address
// counters untouched for a day whose lock has ended.
func (s *StaffService) Purge(ctx context.Context) (sessions, locks int64, err error) {
	now := s.clock.Now()
	if sessions, err = s.reads.PurgeStaffSessions(ctx, now.Add(-EndedSessionRetention)); err != nil {
		return 0, 0, fmt.Errorf("authn: purge staff sessions: %w", err)
	}
	if locks, err = s.reads.PurgeStaffLoginLocks(ctx, now.Add(-24*time.Hour)); err != nil {
		return sessions, 0, fmt.Errorf("authn: purge staff address locks: %w", err)
	}
	return sessions, locks, nil
}
