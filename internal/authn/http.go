package authn

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/jsontime"
)

// HTTP implements the authentication operations of the generated strict
// server (api/openapi.yaml, tags auth and admin-auth) and the account
// operations of marketplace users (registration, password reset and
// change; ADR-0029 Karar 2 and 4).
type HTTP struct {
	users    *Service
	staff    *StaffService
	accounts *Accounts
}

// NewHTTP returns the handlers.
func NewHTTP(users *Service, staff *StaffService, accounts *Accounts) *HTTP {
	return &HTTP{users: users, staff: staff, accounts: accounts}
}

// errNoPrincipal means a protected handler ran without the middleware's
// principal: a wiring bug, answered 500.
var errNoPrincipal = errors.New("authn: handler reached without an authenticated principal")

// LoginUser signs a user in on the request's device.
func (h *HTTP) LoginUser(ctx context.Context, req apiv1.LoginUserRequestObject) (apiv1.LoginUserResponseObject, error) {
	pair, err := h.users.Login(ctx, LoginInput{
		Email: req.Body.Email, Password: req.Body.Password,
		DeviceKey: httpx.DeviceIDFrom(ctx), Platform: string(httpx.ClientPlatformFrom(ctx)),
	})
	if err != nil {
		return nil, err
	}
	return apiv1.LoginUser200JSONResponse(tokenPair(pair)), nil
}

// RefreshUserSession rotates a refresh token.
func (h *HTTP) RefreshUserSession(ctx context.Context, req apiv1.RefreshUserSessionRequestObject) (apiv1.RefreshUserSessionResponseObject, error) {
	pair, err := h.users.Refresh(ctx, req.Body.RefreshToken, httpx.DeviceIDFrom(ctx))
	if err != nil {
		return nil, err
	}
	return apiv1.RefreshUserSession200JSONResponse(tokenPair(pair)), nil
}

// LogoutUser closes the caller's session.
func (h *HTTP) LogoutUser(ctx context.Context, _ apiv1.LogoutUserRequestObject) (apiv1.LogoutUserResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.users.Logout(ctx, p); err != nil {
		return nil, err
	}
	return apiv1.LogoutUser204Response{}, nil
}

// GetMe returns the caller's identity core.
func (h *HTTP) GetMe(ctx context.Context, _ apiv1.GetMeRequestObject) (apiv1.GetMeResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	u, err := h.users.Me(ctx, p)
	if err != nil {
		return nil, err
	}
	return apiv1.GetMe200JSONResponse{
		ID: u.ID, Email: u.Email, Status: apiv1.MeStatus(u.Status), CreatedAt: jsontime.From(u.CreatedAt),
	}, nil
}

// ListMySessions lists the caller's open sessions in one page.
func (h *HTTP) ListMySessions(ctx context.Context, _ apiv1.ListMySessionsRequestObject) (apiv1.ListMySessionsResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	sessions, err := h.users.ListSessions(ctx, p)
	if err != nil {
		return nil, err
	}
	items := make([]apiv1.Session, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, session(s))
	}
	return apiv1.ListMySessions200JSONResponse{Items: items}, nil
}

// GetMySession returns one of the caller's open sessions.
func (h *HTTP) GetMySession(ctx context.Context, req apiv1.GetMySessionRequestObject) (apiv1.GetMySessionResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	s, err := h.users.GetSession(ctx, p, req.SessionID)
	if err != nil {
		return nil, err
	}
	return apiv1.GetMySession200JSONResponse(session(s)), nil
}

// RevokeMySession closes one of the caller's open sessions.
func (h *HTTP) RevokeMySession(ctx context.Context, req apiv1.RevokeMySessionRequestObject) (apiv1.RevokeMySessionResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.users.RevokeSession(ctx, p, req.SessionID); err != nil {
		return nil, err
	}
	return apiv1.RevokeMySession204Response{}, nil
}

// RevokeMyOtherSessions closes every other open session of the caller.
func (h *HTTP) RevokeMyOtherSessions(ctx context.Context, _ apiv1.RevokeMyOtherSessionsRequestObject) (apiv1.RevokeMyOtherSessionsResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.users.RevokeOtherSessions(ctx, p); err != nil {
		return nil, err
	}
	return apiv1.RevokeMyOtherSessions204Response{}, nil
}

// LoginStaff signs a staff member in.
func (h *HTTP) LoginStaff(ctx context.Context, req apiv1.LoginStaffRequestObject) (apiv1.LoginStaffResponseObject, error) {
	pair, err := h.staff.Login(ctx, req.Body.Email, req.Body.Password, ClientKey(httpx.ClientIPFrom(ctx)))
	if err != nil {
		return nil, err
	}
	return apiv1.LoginStaff200JSONResponse(staffTokenPair(pair)), nil
}

// RefreshStaffSession rotates a staff refresh token.
func (h *HTTP) RefreshStaffSession(ctx context.Context, req apiv1.RefreshStaffSessionRequestObject) (apiv1.RefreshStaffSessionResponseObject, error) {
	pair, err := h.staff.Refresh(ctx, req.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	return apiv1.RefreshStaffSession200JSONResponse(staffTokenPair(pair)), nil
}

// LogoutStaff ends every token of the caller's account.
func (h *HTTP) LogoutStaff(ctx context.Context, _ apiv1.LogoutStaffRequestObject) (apiv1.LogoutStaffResponseObject, error) {
	p, ok := StaffFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.staff.Logout(ctx, p, ClientKey(httpx.ClientIPFrom(ctx))); err != nil {
		return nil, err
	}
	return apiv1.LogoutStaff204Response{}, nil
}

// ChangeStaffPassword changes the caller's password.
func (h *HTTP) ChangeStaffPassword(ctx context.Context, req apiv1.ChangeStaffPasswordRequestObject) (apiv1.ChangeStaffPasswordResponseObject, error) {
	p, ok := StaffFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.staff.ChangePassword(ctx, p, req.Body.CurrentPassword, req.Body.NewPassword, ClientKey(httpx.ClientIPFrom(ctx))); err != nil {
		return nil, err
	}
	return apiv1.ChangeStaffPassword204Response{}, nil
}

// StartRegistration queues the first step of a registration; the answer
// does not say whether the address has an account (USR-001).
func (h *HTTP) StartRegistration(ctx context.Context, req apiv1.StartRegistrationRequestObject) (apiv1.StartRegistrationResponseObject, error) {
	err := h.accounts.StartRegistration(ctx, RegistrationRequest{
		Email: req.Body.Email, FirstName: req.Body.FirstName, LastName: req.Body.LastName, Employer: req.Body.Employer,
	})
	if err != nil {
		return nil, err
	}
	return apiv1.StartRegistration202Response{}, nil
}

// VerifyRegistration turns a registration link into a registration token.
func (h *HTTP) VerifyRegistration(ctx context.Context, req apiv1.VerifyRegistrationRequestObject) (apiv1.VerifyRegistrationResponseObject, error) {
	g, err := h.accounts.VerifyRegistration(ctx, req.Body.VerificationToken)
	if err != nil {
		return nil, err
	}
	return apiv1.VerifyRegistration200JSONResponse{
		RegistrationToken: g.Token, ExpiresAt: jsontime.From(g.ExpiresAt), Employer: g.Employer,
	}, nil
}

// CompleteRegistration creates the user and signs it in on the request's
// device.
func (h *HTTP) CompleteRegistration(ctx context.Context, req apiv1.CompleteRegistrationRequestObject) (apiv1.CompleteRegistrationResponseObject, error) {
	consents := make([]ConsentDecision, 0, len(req.Body.Consents))
	for _, c := range req.Body.Consents {
		consents = append(consents, ConsentDecision{DocumentCode: c.DocumentCode, Version: c.Version, Granted: c.Granted})
	}
	pair, err := h.accounts.CompleteRegistration(ctx, RegistrationCompletion{
		Token: req.Body.RegistrationToken, Password: req.Body.Password, Consents: consents,
		DeviceKey: httpx.DeviceIDFrom(ctx), Platform: string(httpx.ClientPlatformFrom(ctx)),
		ClientIP: httpx.ClientIPFrom(ctx), UserAgent: userAgentFrom(ctx),
	})
	if err != nil {
		return nil, err
	}
	location := "/v1/me"
	return apiv1.CompleteRegistration201JSONResponse{
		Body:    tokenPair(pair),
		Headers: apiv1.CompleteRegistration201ResponseHeaders{Location: &location},
	}, nil
}

// RequestPasswordReset queues a reset request; the answer does not say
// whether the address has an account (USR-018).
func (h *HTTP) RequestPasswordReset(ctx context.Context, req apiv1.RequestPasswordResetRequestObject) (apiv1.RequestPasswordResetResponseObject, error) {
	if err := h.accounts.RequestPasswordReset(ctx, req.Body.Email); err != nil {
		return nil, err
	}
	return apiv1.RequestPasswordReset202Response{}, nil
}

// CompletePasswordReset sets a new password with a reset token and ends
// every session.
func (h *HTTP) CompletePasswordReset(ctx context.Context, req apiv1.CompletePasswordResetRequestObject) (apiv1.CompletePasswordResetResponseObject, error) {
	err := h.accounts.CompletePasswordReset(ctx, PasswordResetCompletion{
		Token: req.Body.ResetToken, Password: req.Body.NewPassword,
		DeviceKey: httpx.DeviceIDFrom(ctx), Platform: string(httpx.ClientPlatformFrom(ctx)),
	})
	if err != nil {
		return nil, err
	}
	return apiv1.CompletePasswordReset204Response{}, nil
}

// ChangeMyPassword changes the caller's password; the caller's session
// stays.
func (h *HTTP) ChangeMyPassword(ctx context.Context, req apiv1.ChangeMyPasswordRequestObject) (apiv1.ChangeMyPasswordResponseObject, error) {
	p, ok := UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	err := h.accounts.ChangePassword(ctx, p, PasswordChange{
		Current: req.Body.CurrentPassword, New: req.Body.NewPassword, Confirmation: req.Body.NewPasswordConfirmation,
	})
	if err != nil {
		return nil, err
	}
	return apiv1.ChangeMyPassword204Response{}, nil
}

func tokenPair(p TokenPair) apiv1.TokenPair {
	return apiv1.TokenPair{
		AccessToken: p.AccessToken, AccessTokenExpiresAt: jsontime.From(p.AccessTokenExpiresAt),
		RefreshToken: p.RefreshToken, RefreshTokenExpiresAt: jsontime.From(p.RefreshTokenExpiresAt),
		SessionID: p.SessionID,
	}
}

func staffTokenPair(p StaffTokenPair) apiv1.StaffTokenPair {
	return apiv1.StaffTokenPair{
		AccessToken: p.AccessToken, AccessTokenExpiresAt: jsontime.From(p.AccessTokenExpiresAt),
		RefreshToken: p.RefreshToken, RefreshTokenExpiresAt: jsontime.From(p.RefreshTokenExpiresAt),
		SessionID: p.SessionID, MustChangePassword: p.MustChangePassword,
	}
}

func session(s Session) apiv1.Session {
	platform := apiv1.SessionPlatform(s.Platform)
	if !platform.Valid() {
		platform = apiv1.SessionPlatformUNKNOWN
	}
	return apiv1.Session{
		ID: s.ID, Platform: platform, CreatedAt: jsontime.From(s.CreatedAt),
		LastSeenAt: jsontime.From(s.LastSeenAt), ExpiresAt: jsontime.From(s.ExpiresAt), Current: s.Current,
	}
}

// TargetLimiter is the module-key part of the HTTP rate limiter
// (httpx.RateLimiter.Allow, ADR-0021 §5).
type TargetLimiter interface {
	Allow(group string, kind httpx.KeyKind, key string) (bool, time.Duration)
}

// StrictMiddleware returns the strict server middleware of the
// authentication operations (ADR-0025 §5, AUTH-018):
//
//   - A sign-in takes a token from two KeyCustom buckets: one per (target,
//     client address) in pairGroup and one per target from any address in
//     accountGroup. The target is the keyed hash of the address, so one
//     account hammered behind an office NAT does not lock out the next,
//     and many addresses together cannot try one account fast. A
//     known-good source of the account (a marketplace device it signed in
//     on recently, a staff address it signed in from) is let past an empty
//     account bucket, so an attacker cannot keep the user out by draining
//     it; its own (target, address) bucket still applies.
//   - A refresh takes one from the bucket of its hashed token, a staff
//     password change from the member's, both in pairGroup.
//   - A registration start and a password reset request take a token from
//     the two buckets of their target mailbox (the address without its
//     sub-address, ADR-0032 Karar 6), like a sign-in but without the
//     known-source escape: the e-mail bomb defense (the mail budget of the
//     job is the second line).
//   - The steps that present a mailed or issued token (registration
//     verify and complete, reset complete) take one from the bucket of the
//     hashed token, an in-app password change from the user's; all in
//     pairGroup.
//   - Token responses are not cached (RFC 6749 §5.1).
//
// The address bucket of the root chain applies on top.
func (h *HTTP) StrictMiddleware(limiter TargetLimiter, pairGroup, accountGroup string) apiv1.StrictMiddlewareFunc {
	return func(f apiv1.StrictHandlerFunc, _ string) apiv1.StrictHandlerFunc {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			keys, tokens := h.rateLimitKeys(ctx, request)
			if tokens {
				w.Header().Set("Cache-Control", "no-store")
			}
			if _, ok := request.(apiv1.CompleteRegistrationRequestObject); ok {
				// The consent records keep it (ADR-0032 Karar 8).
				ctx = context.WithValue(ctx, userAgentKey{}, r.UserAgent())
			}
			for _, k := range keys {
				group := pairGroup
				if k.account {
					group = accountGroup
				}
				ok, wait := limiter.Allow(group, httpx.KeyCustom, k.key)
				if ok {
					continue
				}
				if k.known != nil {
					known, err := k.known(ctx)
					if err != nil {
						return nil, err
					}
					if known {
						continue
					}
				}
				return nil, httpx.RateLimitedError(w, wait)
			}
			return f(ctx, w, r, request)
		}
	}
}

// userAgentKey carries the request's User-Agent to a handler that records
// it.
type userAgentKey struct{}

// userAgentFrom returns the User-Agent the strict middleware put on ctx.
func userAgentFrom(ctx context.Context) string {
	ua, _ := ctx.Value(userAgentKey{}).(string)
	return ua
}

type limitKey struct {
	key     string
	account bool
	// known, on an account bucket, reports whether the request comes from
	// a known-good source of the account, which an empty bucket lets past.
	known func(context.Context) (bool, error)
}

// rateLimitKeys returns the rate limit keys of an authentication request
// (none for other operations) and whether its response carries tokens.
func (h *HTTP) rateLimitKeys(ctx context.Context, request any) ([]limitKey, bool) {
	addr := ClientKey(httpx.ClientIPFrom(ctx))
	signIn := func(prefix string, target []byte, known func(context.Context) (bool, error)) []limitKey {
		t := prefix + hex.EncodeToString(target)
		return []limitKey{{key: t + "@" + addr}, {key: t, account: true, known: known}}
	}
	switch req := request.(type) {
	case apiv1.LoginUserRequestObject:
		email, device := req.Body.Email, httpx.DeviceIDFrom(ctx)
		return signIn("login:user:", h.users.Target(email), func(ctx context.Context) (bool, error) {
			return h.users.KnownDevice(ctx, email, device)
		}), true
	case apiv1.LoginStaffRequestObject:
		email := req.Body.Email
		return signIn("login:staff:", h.staff.Target(email), func(ctx context.Context) (bool, error) {
			return h.staff.KnownSource(ctx, email, addr)
		}), true
	case apiv1.RefreshUserSessionRequestObject:
		return []limitKey{{key: "refresh:user:" + hex.EncodeToString(hashOpaque(req.Body.RefreshToken))}}, true
	case apiv1.RefreshStaffSessionRequestObject:
		return []limitKey{{key: "refresh:staff:" + hex.EncodeToString(hashOpaque(req.Body.RefreshToken))}}, true
	case apiv1.ChangeStaffPasswordRequestObject:
		if p, ok := StaffFrom(ctx); ok {
			return []limitKey{{key: "password:staff:" + p.StaffID.String()}}, false
		}
	case apiv1.StartRegistrationRequestObject:
		return signIn("registration:", h.users.MailboxTarget(req.Body.Email), nil), true
	case apiv1.RequestPasswordResetRequestObject:
		return signIn("password-reset:", h.users.MailboxTarget(req.Body.Email), nil), true
	case apiv1.VerifyRegistrationRequestObject:
		return []limitKey{{key: "registration-link:" + hex.EncodeToString(hashOpaque(req.Body.VerificationToken))}}, true
	case apiv1.CompleteRegistrationRequestObject:
		return []limitKey{{key: "registration-token:" + hex.EncodeToString(hashOpaque(req.Body.RegistrationToken))}}, true
	case apiv1.CompletePasswordResetRequestObject:
		return []limitKey{{key: "reset-token:" + hex.EncodeToString(hashOpaque(req.Body.ResetToken))}}, true
	case apiv1.ChangeMyPasswordRequestObject:
		if p, ok := UserFrom(ctx); ok {
			return []limitKey{{key: "password:user:" + p.UserID.String()}}, false
		}
	}
	return nil, false
}
