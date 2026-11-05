package authn

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricLockouts counts locks and backoffs as they start (ADR-0025 §5–6).
const MetricLockouts = "koala_auth_lockouts_total"

// Lock scopes, the values of the scope label.
const (
	scopeTarget  = "target"  // marketplace: a sign-in target, every 100 failures
	scopeAddress = "address" // staff: an (account, client address) pair
	scopeBackoff = "backoff" // staff: the account-wide backoff
)

// Metrics are the authentication counters. Labels come from fixed sets.
type Metrics struct {
	lockouts *prometheus.CounterVec
}

// NewMetrics registers the counters on reg, every label pair at zero.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{lockouts: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricLockouts,
		Help: "Sign-in locks and backoffs that started, by principal kind and scope.",
	}, []string{"principal", "scope"})}
	if err := reg.Register(m.lockouts); err != nil {
		return nil, fmt.Errorf("authn metrics: %w", err)
	}
	m.lockouts.WithLabelValues("user", scopeTarget)
	m.lockouts.WithLabelValues("staff", scopeAddress)
	m.lockouts.WithLabelValues("staff", scopeBackoff)
	return m, nil
}

func (m *Metrics) lockout(principal, scope string) {
	m.lockouts.WithLabelValues(principal, scope).Inc()
}
