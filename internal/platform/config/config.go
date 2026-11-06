// Package config reads the service configuration from the environment and
// validates it fail-closed (plan §3.11): a missing required variable, an
// unknown enum value, an unparsable value or an unknown KOALA_* variable stops
// the process before it serves traffic.
//
// Error messages name the variable but never echo its value, because later
// variables hold secrets.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/is-koala/koala-api/internal/platform/appversion"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

// Prefix is the namespace of every variable this service reads.
const Prefix = "KOALA_"

// Variable names.
const (
	VarEnv                  = "KOALA_ENV"
	VarHTTPAddr             = "KOALA_HTTP_ADDR"
	VarLogLevel             = "KOALA_LOG_LEVEL"
	VarShutdownDrainDelay   = "KOALA_SHUTDOWN_DRAIN_DELAY"
	VarShutdownTimeout      = "KOALA_SHUTDOWN_TIMEOUT"
	VarDatabaseURL          = "KOALA_DATABASE_URL"
	VarOwnerDatabaseURL     = "KOALA_OWNER_DATABASE_URL"
	VarMaintDatabaseURL     = "KOALA_MAINT_DATABASE_URL"
	VarBootstrapDatabaseURL = "KOALA_BOOTSTRAP_DATABASE_URL"
	VarMinAppVersion        = "KOALA_MIN_APP_VERSION"
	VarTrustedProxies       = "KOALA_TRUSTED_PROXIES"
	VarMetricsAddr          = "KOALA_METRICS_ADDR"
	VarJWTMarketplaceKeys   = "KOALA_JWT_MARKETPLACE_KEYS"
	VarJWTStaffKeys         = "KOALA_JWT_STAFF_KEYS"
	VarRealtimeOrigins      = "KOALA_REALTIME_ORIGINS"
	VarLoginPepper          = "KOALA_LOGIN_PEPPER"
)

// known lists every variable Load understands. A KOALA_* variable outside
// this list is a typo or a leftover and fails startup.
var known = []string{ //nolint:gochecknoglobals // read-only registry of names
	VarEnv, VarHTTPAddr, VarLogLevel, VarShutdownDrainDelay, VarShutdownTimeout,
	VarDatabaseURL, VarOwnerDatabaseURL, VarMaintDatabaseURL, VarBootstrapDatabaseURL,
	VarMinAppVersion, VarTrustedProxies, VarMetricsAddr,
	VarJWTMarketplaceKeys, VarJWTStaffKeys, VarRealtimeOrigins, VarLoginPepper,
}

// Env is the deployment environment.
type Env string

// Environments.
const (
	EnvLocal      Env = "local"
	EnvTest       Env = "test"
	EnvStaging    Env = "staging"
	EnvProduction Env = "production"
)

// Config is the validated service configuration.
type Config struct {
	Env      Env
	HTTPAddr string
	LogLevel slog.Level
	// ShutdownDrainDelay is how long /readyz reports 503 before the HTTP
	// server stops accepting connections, so the proxy can take the
	// instance out of rotation.
	ShutdownDrainDelay time.Duration
	// ShutdownTimeout bounds how long in-flight requests may finish. It is
	// at least MinShutdownTimeout.
	ShutdownTimeout time.Duration
	// The database logins (ADR-0017). Each is optional here; the command
	// that needs one requires it. In production each must use TLS
	// (sslmode=require, verify-ca or verify-full).
	//
	// DatabaseURL is the server's login, a member of koala_app only (serve).
	DatabaseURL Secret
	// OwnerDatabaseURL is a member of koala_owner (migrate, maint reindex).
	OwnerDatabaseURL Secret
	// MaintDatabaseURL is a member of koala_maint: staff management and
	// the audit purge (koala staff, koala maint audit-purge; ADR-0026).
	MaintDatabaseURL Secret
	// BootstrapDatabaseURL has CREATEROLE; it creates the group roles
	// (bootstrap).
	BootstrapDatabaseURL Secret
	// MinAppVersion is the oldest mobile build (X-Client-Platform IOS or
	// ANDROID) the API serves; older ones get 400 APP_VERSION_UNSUPPORTED
	// (ADR-0019, ADR-0021). nil when unset; serve requires it in
	// production, where an unset minimum would let every build through.
	MinAppVersion *appversion.Version
	// TrustedProxies are the reverse proxies (Traefik, a CDN) whose
	// X-Forwarded-For entries are believed when resolving the client
	// address for rate limits (ADR-0021). nil: the TCP peer is the
	// client. serve requires it in production, where the API is only
	// reached through the proxy; without it every client would share the
	// proxy's rate limit bucket.
	TrustedProxies []netip.Prefix
	// MetricsAddr is the internal listener of /metrics (plan §3.10). It is
	// never routed by the proxy.
	MetricsAddr string
	// JWTMarketplaceKeys and JWTStaffKeys are the Ed25519 key rings of the
	// two access token classes (ADR-0025, internal/platform/tokenkeys):
	// "kid:seed[,kid:seed…]", the first key signs. serve requires both in
	// staging and production (ServeAuthErrors); elsewhere it generates
	// ephemeral keys when they are unset. The rings must not share a key.
	JWTMarketplaceKeys Secret
	JWTStaffKeys       Secret
	// RealtimeOrigins are the web origins allowed to open the WebSocket
	// from a browser (ADR-0018 §4), comma-separated https://host[:port].
	// The realtime handler validates each when serve starts.
	RealtimeOrigins string
	// LoginPepper keys the HMAC that stands for a sign-in identifier in the
	// attempt log and the rate limit keys (ADR-0025 §5): unpadded base64url
	// of at least 32 random bytes. Required like the key rings.
	LoginPepper Secret
}

// Secret is a configuration value that must not reach logs or error
// messages, such as a database URL with its password. Formatting it with
// fmt or slog prints a placeholder; Reveal returns the value.
type Secret string

const redacted = "[redacted]"

// Reveal returns the secret value. Pass it only to the library that needs it.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer without the value.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

// GoString implements fmt.GoStringer without the value.
func (s Secret) GoString() string { return s.String() }

// LogValue implements slog.LogValuer without the value.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Defaults for optional variables.
const (
	DefaultHTTPAddr           = ":8080"
	DefaultShutdownDrainDelay = 5 * time.Second
	DefaultShutdownTimeout    = MinShutdownTimeout
	DefaultMetricsAddr        = ":9090"
)

// MinShutdownTimeout is the smallest accepted ShutdownTimeout. It equals
// httpx.MinShutdownTimeout (read-header timeout + request timeout + write
// margin = 35s): a connection accepted just before the listener closes must
// be able to send its headers, run until its own deadline and write the
// response instead of being cut off by the forced close. config does not
// import httpx; a compile-time check in httpx keeps the two values equal.
const MinShutdownTimeout = 35 * time.Second

// Load parses environ (os.Environ format, "KEY=value") and returns the
// configuration or an error listing every problem found.
func Load(environ []string) (Config, error) {
	vars := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}

	var errs []error
	for k := range vars {
		if strings.HasPrefix(k, Prefix) && !slices.Contains(known, k) {
			errs = append(errs, fmt.Errorf("%s: unknown variable", k))
		}
	}

	cfg := Config{
		HTTPAddr:           DefaultHTTPAddr,
		LogLevel:           slog.LevelInfo,
		ShutdownDrainDelay: DefaultShutdownDrainDelay,
		ShutdownTimeout:    DefaultShutdownTimeout,
		MetricsAddr:        DefaultMetricsAddr,
	}

	if v, ok := vars[VarEnv]; !ok || v == "" {
		errs = append(errs, fmt.Errorf("%s: required", VarEnv))
	} else if env, err := parseEnv(v); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", VarEnv, err))
	} else {
		cfg.Env = env
	}

	if v, ok := vars[VarHTTPAddr]; ok {
		if err := validateAddr(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", VarHTTPAddr, err))
		} else {
			cfg.HTTPAddr = v
		}
	}

	if v, ok := vars[VarLogLevel]; ok {
		if lvl, err := parseLevel(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", VarLogLevel, err))
		} else {
			cfg.LogLevel = lvl
		}
	}

	if v, ok := vars[VarShutdownDrainDelay]; ok {
		if d, err := parseDuration(v, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", VarShutdownDrainDelay, err))
		} else {
			cfg.ShutdownDrainDelay = d
		}
	}

	if v, ok := vars[VarShutdownTimeout]; ok {
		if d, err := parseDuration(v, false); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", VarShutdownTimeout, err))
		} else if d < MinShutdownTimeout {
			errs = append(errs, fmt.Errorf("%s: %w: must be at least %s (request timeout plus header and write margins)",
				VarShutdownTimeout, errInvalidValue, MinShutdownTimeout))
		} else {
			cfg.ShutdownTimeout = d
		}
	}

	for name, dst := range map[string]*Secret{
		VarDatabaseURL:          &cfg.DatabaseURL,
		VarOwnerDatabaseURL:     &cfg.OwnerDatabaseURL,
		VarMaintDatabaseURL:     &cfg.MaintDatabaseURL,
		VarBootstrapDatabaseURL: &cfg.BootstrapDatabaseURL,
	} {
		if v, ok := vars[name]; ok {
			if err := validateDatabaseURL(v, cfg.Env == EnvProduction); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			} else {
				*dst = Secret(v)
			}
		}
	}

	errs = append(errs, loadEdge(vars, &cfg)...)
	errs = append(errs, loadAuth(vars, &cfg)...)

	if len(errs) > 0 {
		slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

var errInvalidValue = errors.New("invalid value")

// loadEdge reads the variables of the HTTP edge: the client version gate,
// the trusted proxies and the metrics listener (ADR-0021).
func loadEdge(vars map[string]string, cfg *Config) []error {
	var errs []error

	if v, ok := vars[VarMinAppVersion]; ok {
		if ver, err := appversion.Parse(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w: want MAJOR.MINOR.PATCH such as 1.4.0", VarMinAppVersion, errInvalidValue))
		} else {
			cfg.MinAppVersion = &ver
		}
	}

	if v, ok := vars[VarTrustedProxies]; ok {
		if prefixes, err := parseProxies(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", VarTrustedProxies, err))
		} else {
			cfg.TrustedProxies = prefixes
		}
	}

	if v, ok := vars[VarMetricsAddr]; ok {
		if err := validateAddr(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", VarMetricsAddr, err))
		} else {
			cfg.MetricsAddr = v
		}
	}
	if samePort(cfg.MetricsAddr, cfg.HTTPAddr) {
		errs = append(errs, fmt.Errorf("%s: %w: must not use the port of %s", VarMetricsAddr, errInvalidValue, VarHTTPAddr))
	}
	return errs
}

// ServeEdgeErrors reports the edge variables serve needs (ADR-0021): the
// trusted proxies in staging and production, both behind Traefik, where
// without them every client would share the proxy's rate limit and lock
// buckets (ADR-0025 §6, fail closed); the minimum app version in
// production. Other commands (migrate, bootstrap) do not read them.
func (c Config) ServeEdgeErrors() error {
	if c.Env != EnvStaging && c.Env != EnvProduction {
		return nil
	}
	var errs []error
	if c.Env == EnvProduction && c.MinAppVersion == nil {
		errs = append(errs, fmt.Errorf("%s: required by serve in %s", VarMinAppVersion, c.Env))
	}
	if c.TrustedProxies == nil {
		errs = append(errs, fmt.Errorf("%s: required by serve in %s", VarTrustedProxies, c.Env))
	}
	return errors.Join(errs...)
}

// Shortest accepted trusted proxy prefixes. A shorter one (a /0, a whole
// /4) would trust X-Forwarded-For from most of the Internet, so any
// client could pick its own address.
const (
	minProxyBits4 = 8
	minProxyBits6 = 32
)

// parseProxies reads a comma-separated list of addresses and CIDR
// prefixes. A bare address is a single-host prefix; an IPv4-mapped IPv6
// prefix (::ffff:10.0.0.0/104) is stored as its IPv4 prefix, since client
// addresses are unmapped before the lookup.
func parseProxies(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for item := range strings.SplitSeq(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("%w: empty entry", errInvalidValue)
		}
		p, err := netip.ParsePrefix(item)
		if err != nil {
			a, aerr := netip.ParseAddr(item)
			if aerr != nil || a.Zone() != "" {
				return nil, fmt.Errorf("%w: want IP addresses or CIDR prefixes, comma-separated", errInvalidValue)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p != p.Masked() {
			return nil, fmt.Errorf("%w: prefix has host bits set; want a network such as 10.0.0.0/8", errInvalidValue)
		}
		if p.Addr().Is4In6() {
			bits := p.Bits() - 96
			if bits < 0 {
				return nil, fmt.Errorf("%w: an IPv4-mapped prefix needs at least /96", errInvalidValue)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), bits)
		}
		if tooWide(p) {
			return nil, fmt.Errorf("%w: prefix too wide; at least /%d for IPv4 and /%d for IPv6", errInvalidValue, minProxyBits4, minProxyBits6)
		}
		out = append(out, p)
	}
	return out, nil
}

// tooWide reports a prefix shorter than the minimum of its family.
func tooWide(p netip.Prefix) bool {
	if p.Addr().Is4() {
		return p.Bits() < minProxyBits4
	}
	return p.Bits() < minProxyBits6
}

// samePort reports whether two valid host:port addresses use one port.
func samePort(a, b string) bool {
	_, pa, errA := net.SplitHostPort(a)
	_, pb, errB := net.SplitHostPort(b)
	return errA == nil && errB == nil && pa == pb
}

// loadAuth reads the authentication variables (ADR-0025): the two token
// classes' key rings and the realtime origins. The rings are parsed here
// so a bad value stops every command, and kept as secrets.
func loadAuth(vars map[string]string, cfg *Config) []error {
	var errs []error
	if v, ok := vars[VarRealtimeOrigins]; ok {
		cfg.RealtimeOrigins = v
	}
	rings := map[string]tokenkeys.Ring{}
	for name, dst := range map[string]*Secret{
		VarJWTMarketplaceKeys: &cfg.JWTMarketplaceKeys,
		VarJWTStaffKeys:       &cfg.JWTStaffKeys,
	} {
		v, ok := vars[name]
		if !ok {
			continue
		}
		ring, err := tokenkeys.Parse(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w: %w", name, errInvalidValue, err))
			continue
		}
		rings[name] = ring
		*dst = Secret(v)
	}
	if v, ok := vars[VarLoginPepper]; ok {
		if _, err := tokenkeys.ParsePepper(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w: %w", VarLoginPepper, errInvalidValue, err))
		} else {
			cfg.LoginPepper = Secret(v)
		}
	}
	if m, ok := rings[VarJWTMarketplaceKeys]; ok && m.SharesKey(rings[VarJWTStaffKeys]) {
		errs = append(errs, fmt.Errorf("%s, %s: %w: the token classes must not share a key", VarJWTMarketplaceKeys, VarJWTStaffKeys, errInvalidValue))
	}
	return errs
}

// ServeLoginErrors reports the database logins serve must not see (least
// privilege, ADR-0026): the owner, maintenance and bootstrap logins belong
// to one-off commands. A server process that holds them could act as
// koala_owner or koala_maint if it were compromised, so serve refuses to
// start while any of them is in its environment, in every environment.
func (c Config) ServeLoginErrors() error {
	var errs []error
	for name, v := range map[string]Secret{
		VarOwnerDatabaseURL: c.OwnerDatabaseURL, VarMaintDatabaseURL: c.MaintDatabaseURL, VarBootstrapDatabaseURL: c.BootstrapDatabaseURL,
	} {
		if v != "" {
			errs = append(errs, fmt.Errorf("%s: %w: must not be set for serve; it belongs to the command that needs it", name, errInvalidValue))
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

// ServeAuthErrors reports the authentication settings serve cannot start
// without: in staging and production both signing key rings and the login
// pepper. Elsewhere an unset one is generated per process (tokens die on
// restart, attempt targets do not match across restarts).
func (c Config) ServeAuthErrors() error {
	if c.Env != EnvStaging && c.Env != EnvProduction {
		return nil
	}
	var errs []error
	for name, v := range map[string]Secret{
		VarJWTMarketplaceKeys: c.JWTMarketplaceKeys, VarJWTStaffKeys: c.JWTStaffKeys, VarLoginPepper: c.LoginPepper,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s: required by serve in %s", name, c.Env))
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

func parseEnv(v string) (Env, error) {
	switch e := Env(v); e {
	case EnvLocal, EnvTest, EnvStaging, EnvProduction:
		return e, nil
	default:
		return "", fmt.Errorf("%w: want one of local, test, staging, production", errInvalidValue)
	}
}

func parseLevel(v string) (slog.Level, error) {
	switch v {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%w: want one of debug, info, warn, error", errInvalidValue)
	}
}

func parseDuration(v string, allowZero bool) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%w: want a Go duration such as 5s", errInvalidValue)
	}
	if d < 0 || (d == 0 && !allowZero) {
		return 0, fmt.Errorf("%w: must be positive", errInvalidValue)
	}
	return d, nil
}

func validateAddr(v string) error {
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%w: want host:port such as :8080", errInvalidValue)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%w: port must be 1-65535", errInvalidValue)
	}
	return nil
}

// validateDatabaseURL checks the form; pgx parses the rest when the pool
// opens. A repeated connection parameter is rejected. Production requires
// TLS: without sslmode, or with disable, allow
// or prefer, a connection can fall back to plain text. The value is never
// echoed: it holds a password.
func validateDatabaseURL(v string, production bool) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("%w: want a postgres:// URL", errInvalidValue)
	}
	query := u.Query()
	for key, values := range query {
		// libpq and pgx disagree on which repeated value wins; a URL like
		// ?sslmode=disable&sslmode=require must not pass the check below
		// with one value and connect with the other.
		if len(values) > 1 {
			return fmt.Errorf("%w: connection parameter %q is repeated", errInvalidValue, key)
		}
	}
	if production {
		switch query.Get("sslmode") {
		case "require", "verify-ca", "verify-full":
		default:
			return fmt.Errorf("%w: production requires sslmode=require, verify-ca or verify-full", errInvalidValue)
		}
	}
	return nil
}
