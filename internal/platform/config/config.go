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
	"slices"
	"strconv"
	"strings"
	"time"
)

// Prefix is the namespace of every variable this service reads.
const Prefix = "KOALA_"

// Variable names.
const (
	VarEnv                = "KOALA_ENV"
	VarHTTPAddr           = "KOALA_HTTP_ADDR"
	VarLogLevel           = "KOALA_LOG_LEVEL"
	VarShutdownDrainDelay = "KOALA_SHUTDOWN_DRAIN_DELAY"
	VarShutdownTimeout    = "KOALA_SHUTDOWN_TIMEOUT"
)

// known lists every variable Load understands. A KOALA_* variable outside
// this list is a typo or a leftover and fails startup.
var known = []string{ //nolint:gochecknoglobals // read-only registry of names
	VarEnv, VarHTTPAddr, VarLogLevel, VarShutdownDrainDelay, VarShutdownTimeout,
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
}

// Defaults for optional variables.
const (
	DefaultHTTPAddr           = ":8080"
	DefaultShutdownDrainDelay = 5 * time.Second
	DefaultShutdownTimeout    = MinShutdownTimeout
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

	if len(errs) > 0 {
		slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

var errInvalidValue = errors.New("invalid value")

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
