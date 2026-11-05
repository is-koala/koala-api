package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/authn/authndb"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
)

// Retention of ended sessions and of the attempt log before the purge
// hard-deletes them (exemption class M2).
const (
	EndedSessionRetention = 30 * 24 * time.Hour
	LoginAttemptRetention = 90 * 24 * time.Hour
)

// PasswordHasher is what the services need of Passwords.
type PasswordHasher interface {
	Hash(ctx context.Context, password string) (string, error)
	Verify(ctx context.Context, stored, password string) (ok, rehash bool, err error)
	VerifyDummy(ctx context.Context, password string) error
}

// RevokeHook learns of each revoked session after the revoking
// transaction commits. The realtime hub closes the session's connections
// with it. It must not block.
type RevokeHook func(ctx context.Context, userID, sessionID uuid.UUID)

// Service authenticates marketplace users.
type Service struct {
	db        *db.DB
	reads     *authndb.Queries
	passwords PasswordHasher
	tokens    *TokenClass
	targets   targets
	clock     clock.Clock
	logger    *slog.Logger
	metrics   *Metrics
	onRevoke  RevokeHook
}

// NewService returns the marketplace authentication service. tokens is
// the marketplace class; pepper keys the sign-in targets
// (KOALA_LOGIN_PEPPER).
func NewService(database *db.DB, passwords PasswordHasher, tokens *TokenClass, pepper []byte, clk clock.Clock,
	logger *slog.Logger, metrics *Metrics, onRevoke RevokeHook,
) (*Service, error) {
	if tokens.audience != AudienceMarketplace {
		return nil, fmt.Errorf("authn: marketplace service needs the %s token class, got %s", AudienceMarketplace, tokens.audience)
	}
	if onRevoke == nil || metrics == nil {
		return nil, errors.New("authn: revoke hook and metrics are required")
	}
	if len(pepper) == 0 {
		return nil, errors.New("authn: login pepper is required")
	}
	return &Service{
		db: database, reads: authndb.New(database.Pool()), passwords: passwords, tokens: tokens,
		targets: targets{pepper: pepper}, clock: clk, logger: logger, metrics: metrics, onRevoke: onRevoke,
	}, nil
}

// LoginInput is a password sign-in. DeviceKey is the X-Device-Id and
// Platform the X-Client-Platform of the request.
type LoginInput struct {
	Email     string
	Password  string
	DeviceKey string
	Platform  string
}

// Target returns the keyed hash that stands for a sign-in identifier in
// the attempt log and the rate limit keys.
func (s *Service) Target(email string) []byte { return s.targets.of(normalizeEmail(email)) }

// Login signs a user in on a device. It replaces the device's open
// sessions (AUTH-002) and returns a new token pair.
//
// An unknown address, an account without a password, a wrong password and
// a locked target are the same error and pay the same cost: the lock
// check, one argon2id verification and one attempt row (AUTH-001,
// ADR-0025 §5). The device id is checked before anything is looked up.
func (s *Service) Login(ctx context.Context, in LoginInput) (TokenPair, error) {
	if err := checkDeviceKey(in.DeviceKey); err != nil {
		return TokenPair{}, err
	}
	email := normalizeEmail(in.Email)
	target := s.targets.of(email)
	mismatch := apperr.New(apperr.KindUnauthenticated, CodeEmailOrPasswordMismatch)
	now := s.clock.Now()

	failures, locked, err := s.lockState(ctx, target, now)
	if err != nil {
		return TokenPair{}, err
	}
	cred, err := s.reads.GetUserCredentials(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, fmt.Errorf("authn: load user credentials: %w", err)
	}
	var ok, rehash bool
	if found && cred.PasswordHash != nil && !locked {
		ok, rehash, err = s.passwords.Verify(ctx, *cred.PasswordHash, in.Password)
	} else {
		err = s.passwords.VerifyDummy(ctx, in.Password)
	}
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: verify password: %w", err)
	}
	if locked {
		if err := insertAttempt(ctx, s.reads, principalUser, target, outcomeLocked, now); err != nil {
			return TokenPair{}, err
		}
		return TokenPair{}, mismatch
	}
	if !ok {
		return TokenPair{}, s.fail(ctx, target, failures, now, mismatch)
	}
	if cred.Status != UserActive {
		return TokenPair{}, s.fail(ctx, target, failures, now, apperr.New(apperr.KindForbidden, CodeAccountSuspended))
	}
	var newHash string
	if rehash {
		if newHash, err = s.passwords.Hash(ctx, in.Password); err != nil {
			return TokenPair{}, fmt.Errorf("authn: rehash password: %w", err)
		}
	}

	var pair TokenPair
	var outcome error
	err = s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		u, err := q.LockUserForSignIn(ctx, cred.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = mismatch // deleted since the read above
			return insertAttempt(ctx, q, principalUser, target, outcomeFailed, now)
		}
		if err != nil {
			return fmt.Errorf("authn: lock user: %w", err)
		}
		if u.PasswordHash == nil || *u.PasswordHash != *cred.PasswordHash {
			// Changed since it was verified above: the password checked
			// was not the account's password at sign-in time.
			outcome = mismatch
			return insertAttempt(ctx, q, principalUser, target, outcomeFailed, now)
		}
		if u.Status != UserActive {
			outcome = apperr.New(apperr.KindForbidden, CodeAccountSuspended)
			return insertAttempt(ctx, q, principalUser, target, outcomeFailed, now)
		}
		if newHash != "" {
			if err := q.UpdateUserPasswordHash(ctx, authndb.UpdateUserPasswordHashParams{ID: u.ID, PasswordHash: &newHash}); err != nil {
				return fmt.Errorf("authn: store rehash: %w", err)
			}
		}
		deviceID, err := upsertDevice(ctx, q, u.ID, in, now)
		if err != nil {
			return err
		}
		replaced, err := q.RevokeDeviceSessions(ctx, authndb.RevokeDeviceSessionsParams{Now: now, DeviceID: deviceID})
		if err != nil {
			return fmt.Errorf("authn: replace device sessions: %w", err)
		}
		if pair, err = s.openSession(ctx, q, u.ID, deviceID, now); err != nil {
			return err
		}
		if err := insertAttempt(ctx, q, principalUser, target, outcomeSucceeded, now); err != nil {
			return err
		}
		tx.AfterCommit(func(ctx context.Context) {
			for _, sid := range replaced {
				s.onRevoke(ctx, u.ID, sid)
			}
		})
		return nil
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: sign in: %w", err)
	}
	if outcome != nil {
		return TokenPair{}, outcome
	}
	return pair, nil
}

// lockState reads the persistent failure counter of a target from the
// attempt log: its failures since the last success, and whether a lock is
// in force. Every UserLockThreshold failures start a lock of
// UserLockDuration from the failure that completed the hundred; refused
// attempts are logged as LOCKED and do not count, so they do not extend
// it.
func (s *Service) lockState(ctx context.Context, target []byte, now time.Time) (int64, bool, error) {
	n, err := s.reads.CountFailuresSinceSuccess(ctx, authndb.CountFailuresSinceSuccessParams{PrincipalKind: principalUser, TargetHash: target})
	if err != nil {
		return 0, false, fmt.Errorf("authn: count failures: %w", err)
	}
	if n < UserLockThreshold {
		return n, false, nil
	}
	start, err := s.reads.NthFailureSinceSuccess(ctx, authndb.NthFailureSinceSuccessParams{
		PrincipalKind: principalUser, TargetHash: target, Position: n/UserLockThreshold*UserLockThreshold - 1,
	})
	if err != nil {
		return 0, false, fmt.Errorf("authn: find lock start: %w", err)
	}
	return n, now.Before(start.Add(UserLockDuration)), nil
}

// fail logs a failed sign-in in its own statement (a transaction would
// roll it back with the request) and returns refusal. The failure that
// completes a hundred starts a lock: counted and logged.
func (s *Service) fail(ctx context.Context, target []byte, failures int64, now time.Time, refusal error) error {
	if err := insertAttempt(ctx, s.reads, principalUser, target, outcomeFailed, now); err != nil {
		return err
	}
	if (failures+1)%UserLockThreshold == 0 {
		s.metrics.lockout("user", scopeTarget)
		s.logger.WarnContext(ctx, "sign-in target locked after consecutive failures",
			slog.Int64("failures", failures+1), slog.Duration("lock", UserLockDuration))
	}
	return refusal
}

// upsertDevice returns the user's active device with in.DeviceKey,
// creating it on first sign-in. The caller holds the user's row lock, so
// two sign-ins of one user cannot both insert.
func upsertDevice(ctx context.Context, q *authndb.Queries, userID uuid.UUID, in LoginInput, now time.Time) (uuid.UUID, error) {
	platform := devicePlatform(in.Platform)
	id, err := q.GetDeviceForUpdate(ctx, authndb.GetDeviceForUpdateParams{UserID: userID, DeviceKey: in.DeviceKey})
	switch {
	case err == nil:
		if err := q.TouchDevice(ctx, authndb.TouchDeviceParams{ID: id, LastSeenAt: now, Platform: platform}); err != nil {
			return uuid.Nil, fmt.Errorf("authn: touch device: %w", err)
		}
		return id, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, fmt.Errorf("authn: load device: %w", err)
	}
	id = newID()
	err = q.InsertDevice(ctx, authndb.InsertDeviceParams{
		ID: id, UserID: userID, DeviceKey: in.DeviceKey, Platform: platform, CreatedAt: now, LastSeenAt: now,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("authn: insert device: %w", err)
	}
	return id, nil
}

// openSession starts a session with its first refresh token and an
// access token.
func (s *Service) openSession(ctx context.Context, q *authndb.Queries, userID, deviceID uuid.UUID, now time.Time) (TokenPair, error) {
	sid := newID()
	end := now.Add(UserSessionMaxTTL)
	err := q.InsertUserSession(ctx, authndb.InsertUserSessionParams{
		ID: sid, UserID: userID, DeviceID: deviceID, CreatedAt: now, LastSeenAt: now, ExpiresAt: end,
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: insert session: %w", err)
	}
	return s.issuePair(ctx, q, userID, sid, newOpaque(), now, end)
}

// issuePair stores refresh as a new refresh token of session sid and
// signs an access token.
func (s *Service) issuePair(ctx context.Context, q *authndb.Queries, userID, sid uuid.UUID, refresh string, now, sessionEnd time.Time) (TokenPair, error) {
	refreshExp := refreshExpiry(now, sessionEnd, UserRefreshIdleTTL)
	err := q.InsertUserRefreshToken(ctx, authndb.InsertUserRefreshTokenParams{
		ID: newID(), SessionID: sid, TokenHash: hashOpaque(refresh), CreatedAt: now, ExpiresAt: refreshExp,
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: insert refresh token: %w", err)
	}
	access, accessExp, err := s.tokens.issue(userID, sid, 0, nil)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken: access, AccessTokenExpiresAt: accessExp,
		RefreshToken: refresh, RefreshTokenExpiresAt: refreshExp, SessionID: sid,
	}, nil
}

// Refresh rotates a refresh token (K12, OAuth 2.0 Security BCP §4.14):
// the presented token is consumed and its successor issued. The session
// comes from the refresh token itself, so an expired access token is not
// needed (AUTH-003).
//
// A consumed token presented again is reuse and revokes the session, the
// token's family, with one exception (ADR-0025 §3): within
// RefreshGraceWindow of the rotation, from the session's own device, while
// the successor is still unused, the client is retrying, and it gets the
// same successor again with a new access token. A session asked from
// another device than its own is revoked (AUTH-004), and so is one past
// its idle or absolute limit (AUTH-005). Those revocations commit although
// the request fails.
func (s *Service) Refresh(ctx context.Context, refreshToken, deviceKey string) (TokenPair, error) {
	if err := checkDeviceKey(deviceKey); err != nil {
		return TokenPair{}, err
	}
	if !wellFormedOpaque(refreshToken) {
		return TokenPair{}, apperr.New(apperr.KindUnauthenticated, CodeRefreshTokenInvalid)
	}
	now := s.clock.Now()
	var pair TokenPair
	var outcome error
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := authndb.New(tx)
		row, err := q.GetUserRefreshTokenForUpdate(ctx, hashOpaque(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = apperr.New(apperr.KindUnauthenticated, CodeRefreshTokenInvalid)
			return nil
		}
		if err != nil {
			return fmt.Errorf("authn: load refresh token: %w", err)
		}
		end := func(reason string, code apperr.Code) error {
			outcome = apperr.New(apperr.KindUnauthenticated, code)
			return s.revokeIn(ctx, tx, q, row.UserID, row.SessionID, reason, now)
		}
		live := row.DeviceKey != nil && row.UserStatus != nil && *row.UserStatus == UserActive
		switch {
		case row.RevokedAt != nil:
			outcome = apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
			return nil
		case row.ConsumedAt != nil:
			if live && *row.DeviceKey == deviceKey && !now.After(row.ConsumedAt.Add(RefreshGraceWindow)) {
				retry, ok, err := s.retrySuccessor(ctx, q, refreshToken, row.SuccessorSalt, row.UserID, row.SessionID, now)
				if err != nil || ok {
					pair = retry
					return err
				}
			}
			s.logger.WarnContext(ctx, "refresh token reused; session revoked",
				slog.String("session_id", row.SessionID.String()))
			return end(reasonRefreshReused, CodeRefreshTokenReused)
		case !live:
			return end(reasonInactive, CodeSessionInactive)
		case *row.DeviceKey != deviceKey:
			s.logger.WarnContext(ctx, "refresh from another device; session revoked",
				slog.String("session_id", row.SessionID.String()))
			return end(reasonDeviceMismatch, CodeSessionDeviceMismatch)
		case !now.Before(row.ExpiresAt) || !now.Before(row.SessionExpiresAt):
			return end(reasonExpired, CodeRefreshTokenExpired)
		}
		salt := newSalt()
		err = q.ConsumeUserRefreshToken(ctx, authndb.ConsumeUserRefreshTokenParams{Now: now, SuccessorSalt: salt, ID: row.ID})
		if err != nil {
			return fmt.Errorf("authn: consume refresh token: %w", err)
		}
		if err := q.TouchUserSession(ctx, authndb.TouchUserSessionParams{Now: now, ID: row.SessionID, SeenBefore: now}); err != nil {
			return fmt.Errorf("authn: touch session: %w", err)
		}
		pair, err = s.issuePair(ctx, q, row.UserID, row.SessionID, successorOf(refreshToken, salt), now, row.SessionExpiresAt)
		return err
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("authn: refresh: %w", err)
	}
	if outcome != nil {
		return TokenPair{}, outcome
	}
	return pair, nil
}

// retrySuccessor answers a retry inside the grace window: the successor
// of token, if it is still unused and unexpired, with a new access token.
// ok is false when the retry does not qualify and is reuse.
func (s *Service) retrySuccessor(ctx context.Context, q *authndb.Queries, token string, salt []byte, userID, sid uuid.UUID, now time.Time) (TokenPair, bool, error) {
	if len(salt) == 0 {
		return TokenPair{}, false, nil
	}
	successor := successorOf(token, salt)
	next, err := q.GetUserRefreshTokenForUpdate(ctx, hashOpaque(successor))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, false, nil
	}
	if err != nil {
		return TokenPair{}, false, fmt.Errorf("authn: load successor: %w", err)
	}
	if next.ConsumedAt != nil || !now.Before(next.ExpiresAt) || !now.Before(next.SessionExpiresAt) {
		return TokenPair{}, false, nil
	}
	access, accessExp, err := s.tokens.issue(userID, sid, 0, nil)
	if err != nil {
		return TokenPair{}, false, err
	}
	return TokenPair{
		AccessToken: access, AccessTokenExpiresAt: accessExp,
		RefreshToken: successor, RefreshTokenExpiresAt: next.ExpiresAt, SessionID: sid,
	}, true, nil
}

// revokeIn closes a session inside tx and tells the hook after commit.
func (s *Service) revokeIn(ctx context.Context, tx db.Querier, q *authndb.Queries, userID, sid uuid.UUID, reason string, now time.Time) error {
	n, err := q.RevokeUserSession(ctx, authndb.RevokeUserSessionParams{Now: now, Reason: reason, ID: sid})
	if err != nil {
		return fmt.Errorf("authn: revoke session: %w", err)
	}
	if n > 0 {
		tx.AfterCommit(func(ctx context.Context) { s.onRevoke(ctx, userID, sid) })
	}
	return nil
}

// revoke closes a session outside a transaction.
func (s *Service) revoke(ctx context.Context, userID, sid uuid.UUID, reason string) error {
	n, err := s.reads.RevokeUserSession(ctx, authndb.RevokeUserSessionParams{Now: s.clock.Now(), Reason: reason, ID: sid})
	if err != nil {
		return fmt.Errorf("authn: revoke session: %w", err)
	}
	if n > 0 {
		s.onRevoke(ctx, userID, sid)
	}
	return nil
}

// Authenticate checks a marketplace request: the bearer access token, then
// the session in one indexed query (ADR-0015 §2): it exists, is open and
// unexpired, belongs to the token's user, its user and device are active,
// and the request comes from its device. A request from another device
// revokes the session (AUTH-004).
func (s *Service) Authenticate(ctx context.Context, bearer, deviceKey string) (UserPrincipal, error) {
	tok, err := s.tokens.verify(bearer)
	if err != nil {
		return UserPrincipal{}, err
	}
	if err := checkDeviceKey(deviceKey); err != nil {
		return UserPrincipal{}, err
	}
	st, err := s.reads.GetUserSessionState(ctx, tok.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserPrincipal{}, apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
	}
	if err != nil {
		return UserPrincipal{}, fmt.Errorf("authn: load session: %w", err)
	}
	now := s.clock.Now()
	if !userSessionLive(st, tok.Subject, now) {
		return UserPrincipal{}, apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
	}
	if *st.DeviceKey != deviceKey {
		s.logger.WarnContext(ctx, "request from another device; session revoked",
			slog.String("session_id", tok.SessionID.String()))
		if err := s.revoke(ctx, tok.Subject, tok.SessionID, reasonDeviceMismatch); err != nil {
			return UserPrincipal{}, err
		}
		return UserPrincipal{}, apperr.New(apperr.KindUnauthenticated, CodeSessionDeviceMismatch)
	}
	if now.Sub(st.LastSeenAt) >= touchInterval {
		err := s.reads.TouchUserSession(ctx, authndb.TouchUserSessionParams{Now: now, ID: tok.SessionID, SeenBefore: now.Add(-touchInterval)})
		if err != nil {
			// Activity bookkeeping; the request itself is authenticated.
			s.logger.WarnContext(ctx, "touch session failed", slog.Any("error", err))
		}
	}
	return UserPrincipal{UserID: tok.Subject, SessionID: tok.SessionID, DeviceKey: deviceKey}, nil
}

// userSessionLive is the session check minus the device comparison.
func userSessionLive(st authndb.GetUserSessionStateRow, userID uuid.UUID, now time.Time) bool {
	return st.UserID == userID && st.RevokedAt == nil && now.Before(st.ExpiresAt) &&
		st.DeviceKey != nil && st.UserStatus != nil && *st.UserStatus == UserActive
}

// SessionActive reports whether the session is open on deviceKey for
// userID. It is the realtime layer's session check (ADR-0022 §2): every
// "no" is (false, nil); an error only means the answer is unknown.
func (s *Service) SessionActive(ctx context.Context, userID, sessionID uuid.UUID, deviceKey string) (bool, error) {
	st, err := s.reads.GetUserSessionState(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("authn: load session: %w", err)
	}
	return userSessionLive(st, userID, s.clock.Now()) && *st.DeviceKey == deviceKey, nil
}

// Logout closes the caller's session.
func (s *Service) Logout(ctx context.Context, p UserPrincipal) error {
	return s.revoke(ctx, p.UserID, p.SessionID, reasonLogout)
}

// ListSessions returns the caller's open sessions, newest first.
func (s *Service) ListSessions(ctx context.Context, p UserPrincipal) ([]Session, error) {
	rows, err := s.reads.ListOpenUserSessions(ctx, authndb.ListOpenUserSessionsParams{UserID: p.UserID, Now: s.clock.Now()})
	if err != nil {
		return nil, fmt.Errorf("authn: list sessions: %w", err)
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, Session{
			ID: r.ID, Platform: deref(r.Platform), CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
			ExpiresAt: r.ExpiresAt, Current: r.ID == p.SessionID,
		})
	}
	return out, nil
}

// GetSession returns one of the caller's open sessions. Another user's,
// a closed and a missing session are the same 404 (AUTH-016).
func (s *Service) GetSession(ctx context.Context, p UserPrincipal, id uuid.UUID) (Session, error) {
	r, err := s.reads.GetOpenUserSession(ctx, authndb.GetOpenUserSessionParams{ID: id, UserID: p.UserID, Now: s.clock.Now()})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, apperr.New(apperr.KindNotFound, CodeSessionNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("authn: load session: %w", err)
	}
	return Session{
		ID: r.ID, Platform: deref(r.Platform), CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		ExpiresAt: r.ExpiresAt, Current: r.ID == p.SessionID,
	}, nil
}

// RevokeSession closes one of the caller's open sessions; another user's
// session is the same 404 as a missing one and stays open (AUTH-016).
func (s *Service) RevokeSession(ctx context.Context, p UserPrincipal, id uuid.UUID) error {
	n, err := s.reads.RevokeOwnUserSession(ctx, authndb.RevokeOwnUserSessionParams{Now: s.clock.Now(), ID: id, UserID: p.UserID})
	if err != nil {
		return fmt.Errorf("authn: revoke session: %w", err)
	}
	if n == 0 {
		return apperr.New(apperr.KindNotFound, CodeSessionNotFound)
	}
	s.onRevoke(ctx, p.UserID, id)
	return nil
}

// RevokeOtherSessions closes every open session of the caller except the
// one making the request (AUTH-016).
func (s *Service) RevokeOtherSessions(ctx context.Context, p UserPrincipal) error {
	ids, err := s.reads.RevokeOtherUserSessions(ctx, authndb.RevokeOtherUserSessionsParams{Now: s.clock.Now(), UserID: p.UserID, KeepID: p.SessionID})
	if err != nil {
		return fmt.Errorf("authn: revoke other sessions: %w", err)
	}
	for _, id := range ids {
		s.onRevoke(ctx, p.UserID, id)
	}
	return nil
}

// Me returns the caller's identity core.
func (s *Service) Me(ctx context.Context, p UserPrincipal) (User, error) {
	u, err := s.reads.GetUser(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The session check passed a moment ago; the user went since.
		return User{}, apperr.New(apperr.KindUnauthenticated, CodeSessionInactive)
	}
	if err != nil {
		return User{}, fmt.Errorf("authn: load user: %w", err)
	}
	return User{ID: u.ID, Email: u.Email, Status: u.Status, CreatedAt: u.CreatedAt}, nil
}

// Purge hard-deletes sessions that ended more than EndedSessionRetention
// ago (with their refresh tokens and realtime tickets) and attempts older
// than LoginAttemptRetention. A periodic job calls it.
func (s *Service) Purge(ctx context.Context) (sessions, attempts int64, err error) {
	now := s.clock.Now()
	if sessions, err = s.reads.PurgeUserSessions(ctx, now.Add(-EndedSessionRetention)); err != nil {
		return 0, 0, fmt.Errorf("authn: purge sessions: %w", err)
	}
	if attempts, err = s.reads.PurgeLoginAttempts(ctx, now.Add(-LoginAttemptRetention)); err != nil {
		return sessions, 0, fmt.Errorf("authn: purge login attempts: %w", err)
	}
	return sessions, attempts, nil
}

func insertAttempt(ctx context.Context, q *authndb.Queries, kind string, target []byte, outcome string, now time.Time) error {
	err := q.InsertLoginAttempt(ctx, authndb.InsertLoginAttemptParams{
		ID: newID(), PrincipalKind: kind, TargetHash: target, Outcome: outcome, CreatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("authn: record login attempt: %w", err)
	}
	return nil
}

// checkDeviceKey refuses a request without a well-formed X-Device-Id
// before any credential is looked up (AUTH-002). The HTTP edge already
// checks the header (ADR-0021 §2); this keeps the service safe on its own.
func checkDeviceKey(k string) error {
	if ValidDeviceKey(k) {
		return nil
	}
	code := apperr.Code("FIELD_VALUE_INVALID")
	if k == "" {
		code = "FIELD_REQUIRED"
	}
	return apperr.New(apperr.KindInvalid, "REQUEST_INVALID").
		WithFields(apperr.FieldError{Parameter: "X-Device-Id", In: "HEADER", Code: code})
}

// devicePlatform keeps a known X-Client-Platform value (case-insensitive,
// ADR-0021) and drops anything else.
func devicePlatform(p string) *string {
	switch v := strings.ToUpper(p); v {
	case "IOS", "ANDROID", "WEB":
		return &v
	default:
		return nil
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// newID returns a UUIDv7 (plan §3.6-4).
func newID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}
