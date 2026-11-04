package httpx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// ReadinessCheck reports whether a dependency (for example the database
// pool, Faz 1) can serve traffic.
type ReadinessCheck func(ctx context.Context) error

// Health serves /livez and /readyz. Readiness turns 503 as soon as
// draining starts (plan §3.10) or any check fails.
type Health struct {
	logger   *slog.Logger
	checks   []ReadinessCheck
	draining atomic.Bool
}

// NewHealth returns a Health that is ready while all checks pass.
func NewHealth(logger *slog.Logger, checks ...ReadinessCheck) *Health {
	return &Health{logger: logger, checks: checks}
}

// StartDraining makes /readyz report 503 from now on.
func (h *Health) StartDraining() { h.draining.Store(true) }

// Live reports that the process is up. It has no dependencies on purpose:
// a failing database must not get the container restarted.
func (h *Health) Live(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, http.StatusOK, "ok")
}

// Ready reports whether this instance should receive traffic.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		h.write(w, r, http.StatusServiceUnavailable, "draining")
		return
	}
	for _, check := range h.checks {
		if err := check(r.Context()); err != nil {
			h.logger.WarnContext(r.Context(), "readiness check failed", slog.Any("error", err))
			h.write(w, r, http.StatusServiceUnavailable, "not ready")
			return
		}
	}
	h.write(w, r, http.StatusOK, "ok")
}

func (h *Health) write(w http.ResponseWriter, r *http.Request, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body+"\n"); err != nil {
		h.logger.DebugContext(r.Context(), "write health response", slog.Any("error", err))
	}
}
