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
	// successors keys the successor of a rotated refresh token.
	successors []byte
	clock      clock.Clock
	logger     *slog.Logger
	metrics    *Metrics
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
	successors, err := successorKey(pepper)
	if err != nil {
		return nil, err
	}
	return &StaffService{
		db: database, reads: authndb.New(database.Pool()), passwords: passwords, tokens: tokens,
		targets: targets{pepper: pepper}, successors: successors, clock: clk, logger: logger, metrics: metrics,
	}, nil
}

// Target returns the keyed hash that stands for a staff sign-in
// identifier in the attempt log and the rate limit keys.
func (s *StaffService) Target(email string) []byte { return s.targets.of(normalizeEmail(email)) }

// Login signs a staff member in from clientKey (ClientKey of the request's
// address). A wrong password, an unknown address, a disabled account and
// a locked or backed-off account are the same STAFF_LOGIN_FAILED (AUTH-010,
// AUTH-011).
//
// Whether the attempt is examined at all is decided before any hashing,
// from the account's state and its source's (ADR-0025 §6). A disabled
// account, a locked address and the account backoff seen from an unknown
// source pay a decoy verification of the stored hash (its derivations,
// result discarded), so the time of the answer does not depend on whether
// the password was right, and no rehash is paid; an unknown address pays
// the dummy's. Every refusal then runs the same statements under the
// member's row lock: the row lock, the address state FOR UPDATE, two
// counter writes and the attempt row; the ones without a member or
// without a failure to count run on the nil member and match nothing.
// The counters are read again and written under the lock: the address
// lock after StaffLockThreshold consecutive failures from it, the account
// backoff from StaffBackoffAfter on (not for known-good sources). A
// success resets the account counter and the address's and makes the
// address a known-good source; a rehash under current parameters is
// stored after it.
func (s *StaffService) Login(ctx context.Context, email, password, clientKey string) (StaffTokenPair, error) {
	email = normalizeEmail(email)
	target := s.targets.of(email)
	failed := apperr.New(apperr.KindUnauthenticated, CodeStaffLoginFailed)
	now := s.clock.Now()

	cred, err := s.reads.GetStaffCredentials(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return StaffTokenPair{}, fmt.Errorf("authn: load staff credentials: %w", err)
	}
	// An unknown address reads the nil member's source, which has none.
	src, err := peekSource(ctx, s.reads, cred.ID, clientKey)
	if err != nil {
		return StaffTokenPair{}, err
	}
	examined := found && cred.Status == StaffActive && !src.blocked(cred.BackoffUntil, now, cred.TokenVersion)
	var ok, rehash bool
	switch {
	case examined:
		ok, rehash, err = s.passwords.Verify(ctx, cred.PasswordHash, password)
	case found:
		err = s.passwords.VerifyDecoy(ctx, cred.PasswordHash, password)
	default:
		err = s.passwords.VerifyDummy(ctx, password)
	}
	if err != nil {
		return StaffTokenPair{}, fmt.Errorf("authn: verify password: %w", err)
	}

	var pair StaffTokenPair
	var outcome error
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		m, err := q.LockStaffForSignIn(ctx, cred.ID)
		missing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !missing {
			return fmt.Errorf("authn: lock staff member: %w", err)
		}
		// A missing member (an unknown address, or deleted since the read
		// above) reads the nil member's state: m is the zero row.
		src, err := lockSource(ctx, q, m.ID, clientKey)
		if err != nil {
			return err
		}
		switch {
		case missing:
			outcome = failed
			return noopFailure(ctx, q, clientKey, target, outcomeFailed, now)
		case src.blocked(m.BackoffUntil, now, m.TokenVersion) || (!examined && m.Status == StaffActive):
			// Locked, or not examined above and the state changed since:
			// refused without counting.
			outcome = failed
			return noopFailure(ctx, q, clientKey, target, outcomeLocked, now)
		case m.Status != StaffActive:
			outcome = failed
			return noopFailure(ctx, q, clientKey, target, outcomeFailed, now)
		case !ok || m.PasswordHash != cred.PasswordHash:
			// A hash changed since the read counts as a wrong password.
			outcome = failed
			if _, _, err := s.countFailure(ctx, q, m.ID, m.FailedLoginAttempts, src, clientKey, now); err != nil {
				return err
			}
			return insertAttempt(ctx, q, principalStaff, target, outcomeFailed, now)
		}
		if err := s.markSuccess(ctx, q, m.ID, m.FailedLoginAttempts != 0 || m.BackoffUntil != nil, clientKey, m.TokenVersion, now); err != nil {
			return err
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
	if rehash {
		s.rehash(ctx, cred.ID, cred.PasswordHash, password)
	}
	return pair, nil
}

// KnownSource reports whether clientKey is a known-good source of the
// staff account of email (ADR-0025 §6). The rate limit lets such an
// address past the account-wide bucket. An unknown address runs the same
// two queries on the nil member and has none.
func (s *StaffService) KnownSource(ctx context.Context, email, clientKey string) (bool, error) {
	cred, err := s.reads.GetStaffCredentials(ctx, normalizeEmail(email))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("authn: load staff credentials: %w", err)
	}
	src, err := peekSource(ctx, s.reads, cred.ID, clientKey)
	if err != nil {
		return false, err
	}
	return src.known(s.clock.Now(), cred.TokenVersion), nil
}

// rehash stores password under current parameters after a sign-in that
// verified it against an older hash. It runs after the success, so only a
// right password on an examined attempt pays it; a failure leaves the old
// hash, which still verifies.
func (s *StaffService) rehash(ctx context.Context, id uuid.UUID, verified, password string) {
	hash, err := s.passwords.Hash(ctx, password)
	if err == nil {
		err = s.reads.UpdateStaffPasswordHash(ctx, authndb.UpdateStaffPasswordHashParams{
			PasswordHash: hash, ID: id, VerifiedHash: verified,
		})
	}
	if err != nil {
		s.logger.WarnContext(ctx, "staff password rehash failed", slog.Any("error", err))
	}
}

// noopFailure runs, for a refusal that counts nothing, the writes a wrong
// password runs (AUTH-010): the two counter writes, on the nil member so
// they match no row, and the attempt row with outcome.
func noopFailure(ctx context.Context, q *authndb.Queries, clientKey string, target []byte, outcome string, now time.Time) error {
	var none uuid.UUID
	if err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{ID: none}); err != nil {
		return fmt.Errorf("authn: count staff failure: %w", err)
	}
	if err := q.ResetStaffSourceFailures(ctx, authndb.ResetStaffSourceFailuresParams{Now: now, StaffID: none, ClientKey: clientKey}); err != nil {
		return fmt.Errorf("authn: count staff address failure: %w", err)
	}
	return insertAttempt(ctx, q, principalStaff, target, outcome, now)
}

// peekSource reads the member's state at an address without a lock; an
// address with no row has none.
func peekSource(ctx context.Context, q *authndb.Queries, staffID uuid.UUID, clientKey string) (source, error) {
	row, err := q.PeekStaffLoginSource(ctx, authndb.PeekStaffLoginSourceParams{StaffID: staffID, ClientKey: clientKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return source{}, nil
	}
	if err != nil {
		return source{}, fmt.Errorf("authn: load staff address state: %w", err)
	}
	return source{
		failures: row.Failures, lockedUntil: row.LockedUntil,
		lastSuccess: row.LastSuccessAt, successVersion: row.SuccessTokenVersion,
	}, nil
}

// lockSource reads and locks the member's state at an address.
func lockSource(ctx context.Context, q *authndb.Queries, staffID uuid.UUID, clientKey string) (source, error) {
	row, err := q.GetStaffLoginSource(ctx, authndb.GetStaffLoginSourceParams{StaffID: staffID, ClientKey: clientKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return source{}, nil
	}
	if err != nil {
		return source{}, fmt.Errorf("authn: load staff address state: %w", err)
	}
	return source{
		failures: row.Failures, lockedUntil: row.LockedUntil,
		lastSuccess: row.LastSuccessAt, successVersion: row.SuccessTokenVersion,
	}, nil
}

// countFailure applies one wrong password to the account counter and the
// address counter. It reports whether the address is now locked and
// whether the account is now backed off. Locks and backoffs are counted
// (MetricLockouts) and logged as warnings.
func (s *StaffService) countFailure(ctx context.Context, q *authndb.Queries, staffID uuid.UUID,
	accountFailures int32, src source, clientKey string, now time.Time,
) (addressLocked, backedOff bool, err error) {
	n, backoff := accountFailure(accountFailures, now)
	if err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{FailedLoginAttempts: n, BackoffUntil: backoff, ID: staffID}); err != nil {
		return false, false, fmt.Errorf("authn: count staff failure: %w", err)
	}
	if backoff != nil {
		s.metrics.lockout("staff", scopeBackoff)
		s.logger.WarnContext(ctx, "staff account backoff after consecutive failures",
			slog.String("staff_id", staffID.String()), slog.Int("failures", int(n)), slog.Time("until", *backoff))
	}
	af, until := addressFailure(src.failures, now)
	err = q.SetStaffSourceFailures(ctx, authndb.SetStaffSourceFailuresParams{
		StaffID: staffID, ClientKey: clientKey, Failures: af, LockedUntil: until, Now: now,
	})
	if err != nil {
		return false, false, fmt.Errorf("authn: count staff address failure: %w", err)
	}
	if until != nil {
		s.metrics.lockout("staff", scopeAddress)
		s.logger.WarnContext(ctx, "staff account locked for a client address",
			slog.String("staff_id", staffID.String()), slog.Time("until", *until))
	}
	return until != nil, backoff != nil, nil
}

// markSuccess resets the counters a success clears, the account's (when
// it has any) and the address's, and makes the address a known-good
// source under token version.
func (s *StaffService) markSuccess(ctx context.Context, q *authndb.Queries, staffID uuid.UUID, account bool, clientKey string, version int64, now time.Time) error {
	if account {
		if err := q.SetStaffFailures(ctx, authndb.SetStaffFailuresParams{ID: staffID}); err != nil {
			return fmt.Errorf("authn: reset staff failures: %w", err)
		}
	}
	err := q.MarkStaffSourceSuccess(ctx, authndb.MarkStaffSourceSuccessParams{
		StaffID: staffID, ClientKey: clientKey, Now: now, TokenVersion: version,
	})
	if err != nil {
		return fmt.Errorf("authn: mark staff address: %w", err)
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
				retry, ok, err := s.retrySuccessor(ctx, q, successorOf(s.successors, refreshToken, row.SuccessorSalt), row, now)
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
			refresh = successorOf(s.successors, refreshToken, salt)
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
// kills the access and refresh tokens of all its sessions (AUTH-013), and
// with them the known-good standing of every address but clientKey's,
// which keeps it if it had it (ADR-0025 §6).
func (s *StaffService) Logout(ctx context.Context, p StaffPrincipal, clientKey string) error {
	now := s.clock.Now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		m, err := q.LockStaffForSignIn(ctx, p.StaffID)
		if err != nil {
			return fmt.Errorf("authn: lock staff member: %w", err)
		}
		version, err := q.BumpStaffTokenVersion(ctx, p.StaffID)
		if err != nil {
			return fmt.Errorf("authn: bump token version: %w", err)
		}
		err = q.CarryStaffSource(ctx, authndb.CarryStaffSourceParams{
			NewVersion: version, Now: now, StaffID: p.StaffID, ClientKey: clientKey, OldVersion: m.TokenVersion,
		})
		if err != nil {
			return fmt.Errorf("authn: carry staff address: %w", err)
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
// dead and the member signs in again (AUTH-013, AUTH-014). Every address
// but clientKey loses its known-good standing; clientKey, where the
// current password was just proven, becomes one (ADR-0025 §6).
//
// The current password is guarded like a sign-in from clientKey: while
// the address is locked or the account backed off (and the address is
// not a known-good source) the change is refused unexamined, at the cost
// of a decoy verification of the stored hash. The state is read again
// under the member's row lock before anything is written. A wrong current
// password counts on both counters; when it locks the address or backs
// off the account the session ends.
func (s *StaffService) ChangePassword(ctx context.Context, p StaffPrincipal, current, next, clientKey string) error {
	if err := CheckPassword(next); err != nil {
		return err
	}
	mismatch := apperr.New(apperr.KindForbidden, CodeStaffCurrentPasswordMismatch)
	now := s.clock.Now()
	st, err := s.reads.GetStaffPasswordState(ctx, p.StaffID)
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(apperr.KindUnauthenticated, CodeStaffSessionEnded)
	}
	if err != nil {
		return fmt.Errorf("authn: load staff password: %w", err)
	}
	src, err := peekSource(ctx, s.reads, p.StaffID, clientKey)
	if err != nil {
		return err
	}
	if src.blocked(st.BackoffUntil, now, st.TokenVersion) {
		if err := s.passwords.VerifyDecoy(ctx, st.PasswordHash, current); err != nil {
			return fmt.Errorf("authn: verify password: %w", err)
		}
		return mismatch
	}
	ok, _, err := s.passwords.Verify(ctx, st.PasswordHash, current)
	if err != nil {
		return fmt.Errorf("authn: verify password: %w", err)
	}
	var hash string
	if ok {
		if hash, err = s.passwords.Hash(ctx, next); err != nil {
			return fmt.Errorf("authn: hash password: %w", err)
		}
	}
	var refused bool
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		m, err := q.LockStaffForSignIn(ctx, p.StaffID)
		if err != nil {
			return fmt.Errorf("authn: lock staff member: %w", err)
		}
		src, err := lockSource(ctx, q, p.StaffID, clientKey)
		if err != nil {
			return err
		}
		switch {
		case src.blocked(m.BackoffUntil, now, m.TokenVersion):
			// Locked or backed off since the read above: refused, uncounted.
			refused = true
			return nil
		case !ok || m.PasswordHash != st.PasswordHash:
			// Wrong, or the password changed since it was verified.
			refused = true
			return s.countPasswordFailure(ctx, q, p, m.FailedLoginAttempts, src, clientKey, now)
		}
		version, err := q.ChangeStaffPassword(ctx, authndb.ChangeStaffPasswordParams{PasswordHash: hash, Now: now, ID: p.StaffID})
		if err != nil {
			return fmt.Errorf("authn: change staff password: %w", err)
		}
		if err := s.markSuccess(ctx, q, p.StaffID, true, clientKey, version, now); err != nil {
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
	if refused {
		return mismatch
	}
	return nil
}

// countPasswordFailure counts a wrong current password and ends the
// session when it locks the address or backs off the account.
func (s *StaffService) countPasswordFailure(ctx context.Context, q *authndb.Queries, p StaffPrincipal,
	accountFailures int32, src source, clientKey string, now time.Time,
) error {
	locked, backedOff, err := s.countFailure(ctx, q, p.StaffID, accountFailures, src, clientKey, now)
	if err != nil || (!locked && !backedOff) {
		return err
	}
	if err := q.RevokeStaffSession(ctx, authndb.RevokeStaffSessionParams{Now: now, Reason: reasonLocked, ID: p.SessionID}); err != nil {
		return fmt.Errorf("authn: revoke staff session: %w", err)
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

// StaffSourceRetention is how long an address state is kept after its
// last change once its lock has ended; a known-good source is kept until
// KnownSourceWindow after its last success (ADR-0025 §7).
const StaffSourceRetention = 24 * time.Hour

// Purge hard-deletes staff sessions that ended more than
// EndedSessionRetention ago, with their refresh tokens, and address
// states past their retention.
func (s *StaffService) Purge(ctx context.Context) (sessions, sources int64, err error) {
	now := s.clock.Now()
	if sessions, err = s.reads.PurgeStaffSessions(ctx, now.Add(-EndedSessionRetention)); err != nil {
		return 0, 0, fmt.Errorf("authn: purge staff sessions: %w", err)
	}
	sources, err = s.reads.PurgeStaffLoginSources(ctx, authndb.PurgeStaffLoginSourcesParams{
		Before: now.Add(-StaffSourceRetention), KnownBefore: now.Add(-KnownSourceWindow),
	})
	if err != nil {
		return sessions, 0, fmt.Errorf("authn: purge staff address states: %w", err)
	}
	return sessions, sources, nil
}
