package config_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/appversion"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{"KOALA_ENV=local", "PATH=/usr/bin"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := config.Config{
		Env:                config.EnvLocal,
		HTTPAddr:           config.DefaultHTTPAddr,
		LogLevel:           slog.LevelInfo,
		ShutdownDrainDelay: config.DefaultShutdownDrainDelay,
		ShutdownTimeout:    config.DefaultShutdownTimeout,
		MetricsAddr:        config.DefaultMetricsAddr,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

// testRing is a key ring value made for this test run; no key is written
// anywhere.
func testRing(t *testing.T, kid string) string {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	return kid + ":" + base64.RawURLEncoding.EncodeToString(seed)
}

func TestLoadReadsEveryVariable(t *testing.T) {
	t.Parallel()

	mpKeys, staffKeys, pepper := testRing(t, "mp-1"), testRing(t, "st-1"), tokenkeys.NewPepper()
	cfg, err := config.Load([]string{
		"KOALA_ENV=production",
		"KOALA_HTTP_ADDR=127.0.0.1:9000",
		"KOALA_LOG_LEVEL=warn",
		"KOALA_SHUTDOWN_DRAIN_DELAY=0s",
		"KOALA_SHUTDOWN_TIMEOUT=60s",
		"KOALA_DATABASE_URL=postgres://app@db.internal:5432/koala?sslmode=verify-full",
		"KOALA_OWNER_DATABASE_URL=postgresql://migrator@db.internal/koala?sslmode=require",
		"KOALA_MAINT_DATABASE_URL=postgresql://maint@db.internal/koala?sslmode=require",
		"KOALA_BOOTSTRAP_DATABASE_URL=postgres://admin@db.internal/postgres?sslmode=verify-ca",
		"KOALA_MIN_APP_VERSION=1.4.0",
		"KOALA_TRUSTED_PROXIES=10.0.0.0/8, 192.0.2.7,2001:db8::/32",
		"KOALA_METRICS_ADDR=:9100",
		"KOALA_JWT_MARKETPLACE_KEYS=" + mpKeys,
		"KOALA_JWT_STAFF_KEYS=" + staffKeys,
		"KOALA_REALTIME_ORIGINS=https://iskoala.com",
		"KOALA_LOGIN_PEPPER=" + pepper,
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	minVersion := appversion.MustParse("1.4.0")
	want := config.Config{
		Env:                  config.EnvProduction,
		HTTPAddr:             "127.0.0.1:9000",
		LogLevel:             slog.LevelWarn,
		ShutdownDrainDelay:   0,
		ShutdownTimeout:      60 * time.Second,
		DatabaseURL:          "postgres://app@db.internal:5432/koala?sslmode=verify-full",
		OwnerDatabaseURL:     "postgresql://migrator@db.internal/koala?sslmode=require",
		MaintDatabaseURL:     "postgresql://maint@db.internal/koala?sslmode=require",
		BootstrapDatabaseURL: "postgres://admin@db.internal/postgres?sslmode=verify-ca",
		MinAppVersion:        &minVersion,
		TrustedProxies: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("192.0.2.7/32"),
			netip.MustParsePrefix("2001:db8::/32"),
		},
		MetricsAddr:        ":9100",
		JWTMarketplaceKeys: config.Secret(mpKeys),
		JWTStaffKeys:       config.Secret(staffKeys),
		RealtimeOrigins:    "https://iskoala.com",
		LoginPepper:        config.Secret(pepper),
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ []string
		wantVar string
	}{
		{"env missing", nil, "KOALA_ENV: required"},
		{"env empty", []string{"KOALA_ENV="}, "KOALA_ENV: required"},
		{"env unknown", []string{"KOALA_ENV=prod"}, "KOALA_ENV: invalid value"},
		{"env wrong case", []string{"KOALA_ENV=Production"}, "KOALA_ENV: invalid value"},
		{"log level unknown", []string{"KOALA_ENV=local", "KOALA_LOG_LEVEL=verbose"}, "KOALA_LOG_LEVEL: invalid value"},
		{"log level offset", []string{"KOALA_ENV=local", "KOALA_LOG_LEVEL=INFO+2"}, "KOALA_LOG_LEVEL: invalid value"},
		{"addr without port", []string{"KOALA_ENV=local", "KOALA_HTTP_ADDR=localhost"}, "KOALA_HTTP_ADDR: invalid value"},
		{"addr port zero", []string{"KOALA_ENV=local", "KOALA_HTTP_ADDR=:0"}, "KOALA_HTTP_ADDR: invalid value"},
		{"addr port text", []string{"KOALA_ENV=local", "KOALA_HTTP_ADDR=:http"}, "KOALA_HTTP_ADDR: invalid value"},
		{"drain not a duration", []string{"KOALA_ENV=local", "KOALA_SHUTDOWN_DRAIN_DELAY=5"}, "KOALA_SHUTDOWN_DRAIN_DELAY: invalid value"},
		{"drain negative", []string{"KOALA_ENV=local", "KOALA_SHUTDOWN_DRAIN_DELAY=-1s"}, "KOALA_SHUTDOWN_DRAIN_DELAY: invalid value"},
		{"timeout zero", []string{"KOALA_ENV=local", "KOALA_SHUTDOWN_TIMEOUT=0s"}, "KOALA_SHUTDOWN_TIMEOUT: invalid value"},
		{"timeout below minimum", []string{"KOALA_ENV=local", "KOALA_SHUTDOWN_TIMEOUT=" + (config.MinShutdownTimeout - time.Second).String()}, "KOALA_SHUTDOWN_TIMEOUT: invalid value"},
		{"database URL not a URL", []string{"KOALA_ENV=local", "KOALA_DATABASE_URL=host=db user=app"}, "KOALA_DATABASE_URL: invalid value"},
		{"database URL wrong scheme", []string{"KOALA_ENV=local", "KOALA_DATABASE_URL=mysql://app@db/koala"}, "KOALA_DATABASE_URL: invalid value"},
		{"production database URL without sslmode", []string{"KOALA_ENV=production", "KOALA_DATABASE_URL=postgres://app@db/koala"}, "KOALA_DATABASE_URL: invalid value"},
		{"production owner URL with sslmode=prefer", []string{"KOALA_ENV=production", "KOALA_OWNER_DATABASE_URL=postgres://o@db/koala?sslmode=prefer"}, "KOALA_OWNER_DATABASE_URL: invalid value"},
		{"production bootstrap URL with sslmode=disable", []string{"KOALA_ENV=production", "KOALA_BOOTSTRAP_DATABASE_URL=postgres://a@db/postgres?sslmode=disable"}, "KOALA_BOOTSTRAP_DATABASE_URL: invalid value"},
		{"repeated sslmode", []string{"KOALA_ENV=production", "KOALA_DATABASE_URL=postgres://app@db/koala?sslmode=disable&sslmode=require"}, "KOALA_DATABASE_URL: invalid value"},
		{"repeated sslmode outside production", []string{"KOALA_ENV=local", "KOALA_DATABASE_URL=postgres://app@db/koala?sslmode=require&sslmode=disable"}, "KOALA_DATABASE_URL: invalid value"},
		{"repeated parameter", []string{"KOALA_ENV=local", "KOALA_OWNER_DATABASE_URL=postgres://o@db/koala?application_name=a&application_name=b"}, "KOALA_OWNER_DATABASE_URL: invalid value"},
		{"migrate URL wrong scheme", []string{"KOALA_ENV=local", "KOALA_OWNER_DATABASE_URL=http://db/koala"}, "KOALA_OWNER_DATABASE_URL: invalid value"},
		{"unknown koala variable", []string{"KOALA_ENV=local", "KOALA_LOGLEVEL=debug"}, "KOALA_LOGLEVEL: unknown variable"},
		{"minimum app version not semver", []string{"KOALA_ENV=local", "KOALA_MIN_APP_VERSION=1.4"}, "KOALA_MIN_APP_VERSION: invalid value"},
		{"trusted proxy not an address", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=traefik"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted proxy empty entry", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=10.0.0.1,,10.0.0.2"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted proxy with host bits", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=10.0.0.1/8"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted proxy /0", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=0.0.0.0/0"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted IPv6 /0", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=::/0"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted proxy too wide", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=10.0.0.0/7"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted IPv6 too wide", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=2001::/16"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted mapped prefix too wide", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=::ffff:0.0.0.0/96"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"trusted proxy with a zone", []string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=fe80::1%eth0"}, "KOALA_TRUSTED_PROXIES: invalid value"},
		{"metrics addr without port", []string{"KOALA_ENV=local", "KOALA_METRICS_ADDR=localhost"}, "KOALA_METRICS_ADDR: invalid value"},
		{"metrics on the API port", []string{"KOALA_ENV=local", "KOALA_HTTP_ADDR=:9000", "KOALA_METRICS_ADDR=127.0.0.1:9000"}, "KOALA_METRICS_ADDR: invalid value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(tt.environ)
			if err == nil {
				t.Fatalf("Load() = %+v, want error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantVar) {
				t.Fatalf("Load() error = %q, want it to contain %q", err, tt.wantVar)
			}
			if !reflect.DeepEqual(cfg, config.Config{}) {
				t.Fatalf("Load() returned a partial config on error: %+v", cfg)
			}
		})
	}
}

// An IPv4-mapped prefix is stored as its IPv4 prefix: client addresses
// are unmapped before they are looked up.
func TestTrustedProxiesUnmapIPv4(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{"KOALA_ENV=local", "KOALA_TRUSTED_PROXIES=::ffff:10.0.0.0/104,::ffff:192.0.2.7"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.7/32")}
	if !reflect.DeepEqual(cfg.TrustedProxies, want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
}

// serve needs the edge variables: the trusted proxies in staging and
// production, the minimum app version in production; other commands and
// other environments need neither.
func TestServeEdgeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		environ []string
		want    []string
	}{
		{[]string{"KOALA_ENV=production"}, []string{"KOALA_MIN_APP_VERSION: required", "KOALA_TRUSTED_PROXIES: required"}},
		{[]string{"KOALA_ENV=production", "KOALA_MIN_APP_VERSION=1.0.0"}, []string{"KOALA_TRUSTED_PROXIES: required"}},
		{[]string{"KOALA_ENV=production", "KOALA_MIN_APP_VERSION=0.0.0", "KOALA_TRUSTED_PROXIES=10.0.0.0/8"}, nil},
		{[]string{"KOALA_ENV=staging"}, []string{"KOALA_TRUSTED_PROXIES: required by serve in staging"}},
		{[]string{"KOALA_ENV=staging", "KOALA_TRUSTED_PROXIES=10.0.0.0/8"}, nil},
		{[]string{"KOALA_ENV=local"}, nil},
	}
	for _, tt := range tests {
		cfg, err := config.Load(tt.environ)
		if err != nil {
			t.Fatalf("Load(%v) error = %v", tt.environ, err)
		}
		err = cfg.ServeEdgeErrors()
		if len(tt.want) == 0 {
			if err != nil {
				t.Errorf("%v: ServeEdgeErrors() = %v, want nil", tt.environ, err)
			}
			continue
		}
		for _, w := range tt.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%v: ServeEdgeErrors() = %v, want it to mention %q", tt.environ, err, w)
			}
		}
	}
}

func TestLoadAcceptsMinShutdownTimeout(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{"KOALA_ENV=local", "KOALA_SHUTDOWN_TIMEOUT=" + config.MinShutdownTimeout.String()})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ShutdownTimeout != config.MinShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, config.MinShutdownTimeout)
	}
}

func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Load([]string{"KOALA_LOG_LEVEL=loud", "KOALA_TYPO=1"})
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	for _, want := range []string{"KOALA_ENV: required", "KOALA_LOG_LEVEL: invalid value", "KOALA_TYPO: unknown variable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadNeverEchoesValues(t *testing.T) {
	t.Parallel()

	const secretish = "s3cr3t-value-that-must-not-leak"
	_, err := config.Load([]string{
		"KOALA_ENV=" + secretish,
		"KOALA_LOG_LEVEL=" + secretish,
		"KOALA_HTTP_ADDR=" + secretish,
		"KOALA_SHUTDOWN_TIMEOUT=" + secretish,
		"KOALA_DATABASE_URL=" + secretish,
		"KOALA_OWNER_DATABASE_URL=%zz" + secretish,
		"KOALA_BOOTSTRAP_DATABASE_URL=postgres://a:" + secretish + "@db/postgres?sslmode=disable",
		"KOALA_UNKNOWN=" + secretish,
	})
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if strings.Contains(err.Error(), secretish) {
		t.Fatalf("error echoes a value: %q", err)
	}
}

// TestSecretIsNotFormatted guards against a database password reaching a
// log line or a test failure message through %v of the whole Config.
func TestSecretIsNotFormatted(t *testing.T) {
	t.Parallel()

	const password = "s3cr3t-password"
	cfg, err := config.Load([]string{"KOALA_ENV=local", "KOALA_DATABASE_URL=postgres://app:" + password + "@db/koala"})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", slog.Any("url", cfg.DatabaseURL))
	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"%s":   fmt.Sprintf("url=%s", cfg.DatabaseURL),
		"slog": logs.String(),
	} {
		if strings.Contains(out, password) {
			t.Errorf("%s prints the password: %s", name, out)
		}
	}
	if !strings.Contains(cfg.DatabaseURL.Reveal(), password) {
		t.Error("Reveal() lost the value")
	}
}

// TestServeAuthErrors: staging and production need both key rings for
// serve; elsewhere an unset ring is generated per process.
func TestServeAuthErrors(t *testing.T) {
	t.Parallel()

	for _, env := range []config.Env{config.EnvLocal, config.EnvTest} {
		if err := (config.Config{Env: env}).ServeAuthErrors(); err != nil {
			t.Errorf("%s: ServeAuthErrors() = %v, want nil", env, err)
		}
	}
	for _, env := range []config.Env{config.EnvStaging, config.EnvProduction} {
		err := (config.Config{Env: env}).ServeAuthErrors()
		for _, v := range []string{config.VarJWTMarketplaceKeys, config.VarJWTStaffKeys, config.VarLoginPepper} {
			if err == nil || !strings.Contains(err.Error(), v+": required by serve") {
				t.Errorf("%s: ServeAuthErrors() = %v, want an error naming %s", env, err, v)
			}
		}
		ok := config.Config{Env: env, JWTMarketplaceKeys: "set", JWTStaffKeys: "set", LoginPepper: "set"}
		if err := ok.ServeAuthErrors(); err != nil {
			t.Errorf("%s with both rings: ServeAuthErrors() = %v", env, err)
		}
	}
}

// TestLoadRejectsBadKeyRings: a malformed ring and two classes sharing a
// key stop every command; the error never echoes a seed.
func TestLoadRejectsBadKeyRings(t *testing.T) {
	t.Parallel()

	shared := testRing(t, "k1")
	cases := map[string]struct {
		environ []string
		want    string
	}{
		"marketplace malformed": {[]string{"KOALA_ENV=local", "KOALA_JWT_MARKETPLACE_KEYS=k1"}, "KOALA_JWT_MARKETPLACE_KEYS: invalid value"},
		"staff short seed":      {[]string{"KOALA_ENV=local", "KOALA_JWT_STAFF_KEYS=k1:AAAA"}, "KOALA_JWT_STAFF_KEYS: invalid value"},
		"zero seed": {
			[]string{"KOALA_ENV=local", "KOALA_JWT_STAFF_KEYS=k1:" + strings.Repeat("A", 43)},
			"KOALA_JWT_STAFF_KEYS: invalid value",
		},
		"pepper too short": {[]string{"KOALA_ENV=local", "KOALA_LOGIN_PEPPER=c2hvcnQ"}, "KOALA_LOGIN_PEPPER: invalid value"},
		"pepper not random": {
			[]string{"KOALA_ENV=local", "KOALA_LOGIN_PEPPER=" + strings.Repeat("A", 43)},
			"KOALA_LOGIN_PEPPER: invalid value",
		},
		"classes share a key": {
			[]string{"KOALA_ENV=local", "KOALA_JWT_MARKETPLACE_KEYS=" + shared, "KOALA_JWT_STAFF_KEYS=" + shared},
			"must not share a key",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(tc.environ)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), shared) {
				t.Errorf("error %q echoes a key", err)
			}
		})
	}
}

// objectEnviron is a complete object store configuration, for the tests
// that take away one variable at a time.
func objectEnviron(env string) []string {
	return []string{
		"KOALA_ENV=" + env,
		config.VarObjectEndpoint + "=https://objects.example.com",
		config.VarObjectRegion + "=eu-central-1",
		config.VarObjectBucket + "=koala-media",
		config.VarObjectAccessKeyID + "=test-access-key-id",
		config.VarObjectSecretKey + "=test-secret-access-key",
		config.VarObjectPathStyle + "=true",
		config.VarObjectPublicBaseURL + "=https://cdn.example.com",
	}
}

func TestLoadReadsTheObjectStore(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(objectEnviron("production"))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	want := config.ObjectStore{
		Endpoint:      "https://objects.example.com",
		Region:        "eu-central-1",
		Bucket:        "koala-media",
		AccessKeyID:   "test-access-key-id",
		SecretKey:     "test-secret-access-key",
		PathStyle:     true,
		PublicBaseURL: "https://cdn.example.com",
	}
	if !reflect.DeepEqual(cfg.Object, want) {
		t.Fatalf("Object = %+v, want %+v", cfg.Object, want)
	}
	if !cfg.Object.Configured() {
		t.Error("Configured() = false for a complete configuration")
	}
	if err := cfg.ServeStorageErrors(); err != nil {
		t.Errorf("ServeStorageErrors() = %v, want nil", err)
	}
}

// TestObjectSecretNeverPrints is the log rule: the secret access key is
// a Secret, so every way the configuration reaches a log prints a
// placeholder instead of the value.
func TestObjectSecretNeverPrints(t *testing.T) {
	t.Parallel()

	const secret = "test-secret-access-key"
	cfg, err := config.Load(objectEnviron("production"))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if rendered := fmt.Sprintf(verb, cfg); strings.Contains(rendered, secret) {
			t.Errorf("a Config formatted with %s holds the secret access key", verb)
		}
	}
	if got := cfg.Object.SecretKey.Reveal(); got != secret {
		t.Errorf("Reveal() = %q, want the value", got)
	}
}

func TestLoadRejectsBadObjectValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ []string
		wantVar string
	}{
		{"bucket too short", []string{"KOALA_ENV=local", config.VarObjectBucket + "=ab"}, config.VarObjectBucket},
		{"bucket upper case", []string{"KOALA_ENV=local", config.VarObjectBucket + "=Koala-Media"}, config.VarObjectBucket},
		{"bucket with a slash", []string{"KOALA_ENV=local", config.VarObjectBucket + "=koala/media"}, config.VarObjectBucket},
		{"bucket with a traversal", []string{"KOALA_ENV=local", config.VarObjectBucket + "=../koala"}, config.VarObjectBucket},
		{"bucket leading dash", []string{"KOALA_ENV=local", config.VarObjectBucket + "=-koala"}, config.VarObjectBucket},
		{"bucket trailing dot", []string{"KOALA_ENV=local", config.VarObjectBucket + "=koala."}, config.VarObjectBucket},
		{"bucket too long", []string{"KOALA_ENV=local", config.VarObjectBucket + "=" + strings.Repeat("k", 64)}, config.VarObjectBucket},
		{"endpoint is not a URL", []string{"KOALA_ENV=local", config.VarObjectEndpoint + "=objects.example.com"}, config.VarObjectEndpoint},
		{"endpoint with credentials", []string{"KOALA_ENV=local", config.VarObjectEndpoint + "=https://k:s@objects.example.com"}, config.VarObjectEndpoint},
		{"endpoint with a query", []string{"KOALA_ENV=local", config.VarObjectEndpoint + "=https://objects.example.com?x=1"}, config.VarObjectEndpoint},
		{"endpoint plain text in production", []string{"KOALA_ENV=production", config.VarObjectEndpoint + "=http://objects.example.com"}, config.VarObjectEndpoint},
		{"public base URL plain text in production", []string{"KOALA_ENV=production", config.VarObjectPublicBaseURL + "=http://cdn.example.com"}, config.VarObjectPublicBaseURL},
		{"public base URL with a fragment", []string{"KOALA_ENV=local", config.VarObjectPublicBaseURL + "=https://cdn.example.com/#f"}, config.VarObjectPublicBaseURL},
		{"path style is not a boolean", []string{"KOALA_ENV=local", config.VarObjectPathStyle + "=yes"}, config.VarObjectPathStyle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(tt.environ)
			if err == nil {
				t.Fatalf("Load() = %+v, want error", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantVar) {
				t.Fatalf("Load() error = %q, want it to mention %s", err, tt.wantVar)
			}
			if !reflect.DeepEqual(cfg, config.Config{}) {
				t.Fatalf("Load() returned a partial config on error: %+v", cfg)
			}
			if strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "koala-media") {
				t.Errorf("Load() error echoes a value: %q", err)
			}
		})
	}
}

// TestObjectPlainTextEndpointIsFineOutsideProduction keeps a local MinIO
// workable: http is only refused in production.
func TestObjectPlainTextEndpointIsFineOutsideProduction(t *testing.T) {
	t.Parallel()

	for _, env := range []string{"local", "test", "staging"} {
		environ := []string{"KOALA_ENV=" + env, config.VarObjectEndpoint + "=http://127.0.0.1:9000"}
		if _, err := config.Load(environ); err != nil {
			t.Errorf("Load() in %s = %v, want nil", env, err)
		}
	}
}

// TestServeStorageErrors is the fail-closed rule of ADR-0030 Karar 7: a
// partly configured store stops serve everywhere, a missing one stops it
// where the upload endpoints are live, and the public base URL is
// optional.
func TestServeStorageErrors(t *testing.T) {
	t.Parallel()

	without := func(env, drop string) []string {
		var out []string
		for _, kv := range objectEnviron(env) {
			if name, _, _ := strings.Cut(kv, "="); name == drop {
				continue
			}
			out = append(out, kv)
		}
		return out
	}

	tests := []struct {
		name    string
		environ []string
		want    []string
	}{
		{"complete in production", objectEnviron("production"), nil},
		{"complete in local", objectEnviron("local"), nil},
		{"nothing set in local", []string{"KOALA_ENV=local"}, nil},
		{"nothing set in test", []string{"KOALA_ENV=test"}, nil},
		{
			"nothing set in staging",
			[]string{"KOALA_ENV=staging"},
			[]string{
				config.VarObjectRegion + ": required by serve in staging",
				config.VarObjectBucket + ": required by serve in staging",
				config.VarObjectAccessKeyID + ": required by serve in staging",
				config.VarObjectSecretKey + ": required by serve in staging",
			},
		},
		{
			"nothing set in production",
			[]string{"KOALA_ENV=production"},
			[]string{config.VarObjectBucket + ": required by serve in production"},
		},
		{
			"bucket missing in local",
			without("local", config.VarObjectBucket),
			[]string{config.VarObjectBucket + ": required once any KOALA_OBJECT_* variable is set"},
		},
		{
			"secret missing in production",
			without("production", config.VarObjectSecretKey),
			[]string{config.VarObjectSecretKey + ": required once any KOALA_OBJECT_* variable is set"},
		},
		{
			"region missing in test",
			without("test", config.VarObjectRegion),
			[]string{config.VarObjectRegion + ": required once any KOALA_OBJECT_* variable is set"},
		},
		{
			"access key missing in staging",
			without("staging", config.VarObjectAccessKeyID),
			[]string{config.VarObjectAccessKeyID + ": required once any KOALA_OBJECT_* variable is set"},
		},
		{"endpoint missing: AWS's own endpoint is used", without("production", config.VarObjectEndpoint), nil},
		{"public base URL missing: no object gets a public address", without("production", config.VarObjectPublicBaseURL), nil},
		{"path style missing: virtual hosted addressing", without("production", config.VarObjectPathStyle), nil},
		{
			"only the path style flag, in local",
			[]string{"KOALA_ENV=local", config.VarObjectPathStyle + "=true"},
			[]string{config.VarObjectBucket + ": required once any KOALA_OBJECT_* variable is set"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(tt.environ)
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			err = cfg.ServeStorageErrors()
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("ServeStorageErrors() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ServeStorageErrors() = nil, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("ServeStorageErrors() = %v, want it to mention %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "test-secret-access-key") {
				t.Errorf("ServeStorageErrors() echoes the secret: %v", err)
			}
		})
	}
}
