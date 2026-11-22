package authn

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricLockouts counts locks and backoffs as they start (ADR-0025 §5–6).
const MetricLockouts = "koala_auth_lockouts_total"

// MetricAccountMailsHeld counts account link mails the mail budget held
// back (ADR-0032 Karar 6), by the bound that held them.
const MetricAccountMailsHeld = "koala_account_mails_held_total"

// Mail budget bounds, the values of the reason label.
const (
	heldMailbox = "mailbox" // one mailbox's cooldown or hourly budget (registration)
	heldAccount = "account" // one account's cooldown or hourly budget (reset)
	heldGlobal  = "global"  // the hourly cap of registration mails over every address
)

// Lock scopes, the values of the scope label.
const (
	scopeTarget  = "target"  // marketplace: a sign-in target, every 100 failures
	scopeAddress = "address" // staff: an (account, client address) pair
	scopeBackoff = "backoff" // staff: the account-wide backoff
)

// Metrics are the authentication counters. Labels come from fixed sets.
type Metrics struct {
	lockouts *prometheus.CounterVec
	held     *prometheus.CounterVec
}

// NewMetrics registers the counters on reg, every label pair at zero.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		lockouts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricLockouts,
			Help: "Sign-in locks and backoffs that started, by principal kind and scope.",
		}, []string{"principal", "scope"}),
		held: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricAccountMailsHeld,
			Help: "Account link mails the mail budget held back, by the bound that held them.",
		}, []string{"reason"}),
	}
	for _, c := range []prometheus.Collector{m.lockouts, m.held} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("authn metrics: %w", err)
		}
	}
	for _, r := range []string{heldMailbox, heldAccount, heldGlobal} {
		m.held.WithLabelValues(r)
	}
	m.lockouts.WithLabelValues("user", scopeTarget)
	m.lockouts.WithLabelValues("staff", scopeAddress)
	m.lockouts.WithLabelValues("staff", scopeBackoff)
	return m, nil
}

func (m *Metrics) lockout(principal, scope string) {
	m.lockouts.WithLabelValues(principal, scope).Inc()
}

func (m *Metrics) mailHeld(reason string) {
	m.held.WithLabelValues(reason).Inc()
}
