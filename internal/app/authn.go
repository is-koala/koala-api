package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/meta"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
	"github.com/is-koala/koala-api/internal/realtime"
)

// modules are the domain modules the router mounts.
type modules struct {
	users    *authn.Service
	staff    *authn.StaffService
	authHTTP *authn.HTTP
	hub      *realtime.Hub
	realtime *realtime.Handler
}

// strictServer composes the modules' parts of the generated strict server
// interface; each module implements its own operations.
type strictServer struct {
	*meta.Handler
	*authn.HTTP
}

// wireModules builds the modules on database. Authentication comes first:
// the realtime layer checks its sessions, and it closes a session's
// connections when authentication revokes the session (ADR-0022).
func wireModules(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger) (modules, error) {
	var hub *realtime.Hub
	onRevoke := func(_ context.Context, userID, sessionID uuid.UUID) {
		if hub != nil {
			hub.DisconnectSession(realtime.UserID(userID.String()), sessionID.String())
		}
	}
	users, staff, err := wireAuthn(ctx, cfg, database, clk, logger, onRevoke)
	if err != nil {
		return modules{}, err
	}
	sessions := realtimeSessions{users: users}
	hub, err = realtime.NewHub(noConversations{}, sessions, clk, logger, realtime.DefaultOptions())
	if err != nil {
		return modules{}, fmt.Errorf("wire realtime: %w", err)
	}
	tickets := realtime.NewTickets(realtime.NewPostgresTickets(database.Pool()), sessions, clk, logger)
	opts := realtime.DefaultHandlerOptions()
	opts.AllowedOrigins = splitList(cfg.RealtimeOrigins)
	handler, err := realtime.NewHandler(hub, tickets, realtimeAuth{users: users}, logger, opts)
	if err != nil {
		return modules{}, fmt.Errorf("wire realtime: %w", err)
	}
	return modules{users: users, staff: staff, authHTTP: authn.NewHTTP(users, staff), hub: hub, realtime: handler}, nil
}

// wireAuthn builds the two authentication services (ADR-0025). Each token
// class signs with its own ring. Outside staging and production a ring
// that is not configured is generated per process (config.ServeAuthErrors
// refuses that where it matters).
func wireAuthn(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	onRevoke authn.RevokeHook,
) (*authn.Service, *authn.StaffService, error) {
	// One argon2id hash per CPU at a time: each holds 19 MiB for tens of
	// milliseconds; more in parallel would only queue on the CPUs.
	passwords, err := authn.NewPasswords(authn.DefaultParams(), runtime.GOMAXPROCS(0))
	if err != nil {
		return nil, nil, fmt.Errorf("wire authn: %w", err)
	}
	classes := map[string]*authn.TokenClass{}
	for aud, secret := range map[string]config.Secret{
		authn.AudienceMarketplace: cfg.JWTMarketplaceKeys,
		authn.AudienceStaff:       cfg.JWTStaffKeys,
	} {
		ring, err := keyRing(ctx, aud, secret, logger)
		if err != nil {
			return nil, nil, err
		}
		if classes[aud], err = authn.NewTokenClass(aud, ring, clk); err != nil {
			return nil, nil, fmt.Errorf("wire authn: %w", err)
		}
	}
	users, err := authn.NewService(database, passwords, classes[authn.AudienceMarketplace], clk, logger, onRevoke)
	if err != nil {
		return nil, nil, fmt.Errorf("wire authn: %w", err)
	}
	staff, err := authn.NewStaffService(database, passwords, classes[authn.AudienceStaff], clk, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("wire authn: %w", err)
	}
	return users, staff, nil
}

func keyRing(ctx context.Context, aud string, secret config.Secret, logger *slog.Logger) (tokenkeys.Ring, error) {
	if secret == "" {
		logger.WarnContext(ctx, "token signing keys not configured; using an ephemeral key, tokens die with the process",
			slog.String("audience", aud))
		return tokenkeys.Generate(aud), nil
	}
	ring, err := tokenkeys.Parse(secret.Reveal())
	if err != nil {
		return tokenkeys.Ring{}, fmt.Errorf("wire authn: %s keys: %w", aud, err)
	}
	return ring, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// realtimeSessions is the realtime layer's session check on the user
// session table (realtime.Sessions, ADR-0022 §2).
type realtimeSessions struct {
	users *authn.Service
}

func (s realtimeSessions) Active(ctx context.Context, p realtime.Principal) (bool, error) {
	userID, err := uuid.Parse(string(p.UserID))
	if err != nil {
		return false, nil //nolint:nilerr // a principal that names no user is not an active session (contract: no error)
	}
	sessionID, err := uuid.Parse(p.SessionID)
	if err != nil {
		return false, nil //nolint:nilerr // as above
	}
	active, err := s.users.SessionActive(ctx, userID, sessionID, p.DeviceID)
	if err != nil {
		return false, fmt.Errorf("realtime session check: %w", err)
	}
	return active, nil
}

// realtimeAuth authenticates the ticket request like any marketplace
// request: bearer token, live session, the session's device.
type realtimeAuth struct {
	users *authn.Service
}

func (a realtimeAuth) Principal(r *http.Request) (realtime.Principal, error) {
	ctx := r.Context()
	p, err := a.users.Authenticate(ctx, authn.BearerToken(r), httpx.DeviceIDFrom(ctx))
	if err != nil {
		return realtime.Principal{}, fmt.Errorf("realtime ticket: %w", err)
	}
	return realtime.Principal{UserID: realtime.UserID(p.UserID.String()), SessionID: p.SessionID.String(), DeviceID: p.DeviceKey}, nil
}

// noConversations answers the realtime layer until messaging lands (Faz
// 4): nobody is a participant of anything, so typing is
// CONVERSATION_NOT_FOUND and no conversation event has a recipient.
type noConversations struct{}

func (noConversations) IsParticipant(context.Context, realtime.ConversationID, realtime.UserID) (bool, error) {
	return false, nil
}

func (noConversations) Participants(context.Context, realtime.ConversationID) ([]realtime.UserID, error) {
	return nil, nil
}
