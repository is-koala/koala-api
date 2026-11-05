package config_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/config"
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
	}
	if cfg != want {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load([]string{
		"KOALA_ENV=production",
		"KOALA_HTTP_ADDR=127.0.0.1:9000",
		"KOALA_LOG_LEVEL=warn",
		"KOALA_SHUTDOWN_DRAIN_DELAY=0s",
		"KOALA_SHUTDOWN_TIMEOUT=60s",
		"KOALA_DATABASE_URL=postgres://app@db.internal:5432/koala?sslmode=verify-full",
		"KOALA_OWNER_DATABASE_URL=postgresql://migrator@db.internal/koala?sslmode=require",
		"KOALA_BOOTSTRAP_DATABASE_URL=postgres://admin@db.internal/postgres?sslmode=verify-ca",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := config.Config{
		Env:                  config.EnvProduction,
		HTTPAddr:             "127.0.0.1:9000",
		LogLevel:             slog.LevelWarn,
		ShutdownDrainDelay:   0,
		ShutdownTimeout:      60 * time.Second,
		DatabaseURL:          "postgres://app@db.internal:5432/koala?sslmode=verify-full",
		OwnerDatabaseURL:     "postgresql://migrator@db.internal/koala?sslmode=require",
		BootstrapDatabaseURL: "postgres://admin@db.internal/postgres?sslmode=verify-ca",
	}
	if cfg != want {
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
			if cfg != (config.Config{}) {
				t.Fatalf("Load() returned a partial config on error: %+v", cfg)
			}
		})
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
