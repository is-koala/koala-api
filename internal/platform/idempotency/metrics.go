package idempotency

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricRequests counts the requests the store handled, by operation and
// result.
const MetricRequests = "koala_idempotency_requests_total"

// Results, the values of the result label.
const (
	// ResultCommitted: a success the operation stored in its own
	// transaction.
	ResultCommitted = "committed"
	// ResultUnbound: a success the operation did not store in its own
	// transaction; the middleware stored it afterwards, not atomically. A
	// wiring bug: the operation's handler does not call Commit inside its
	// transaction (alert KoalaIdempotencyUnbound).
	ResultUnbound = "unbound"
	// ResultStored: a permanent 4xx, stored after the handler.
	ResultStored = "stored"
	// ResultReleased: a 5xx or a transient 4xx; the key is free again.
	ResultReleased = "released"
	// ResultReplayed: the stored response was sent again.
	ResultReplayed = "replayed"
	// ResultReused: 422, the key was used with another request.
	ResultReused = "key_reused"
	// ResultInUse: 409, a request with the key was running.
	ResultInUse = "key_in_use"
	// ResultTakenOver: a key claimed from an attempt whose lease ran out.
	ResultTakenOver = "taken_over"
)

func results() []string {
	return []string{
		ResultCommitted, ResultUnbound, ResultStored, ResultReleased,
		ResultReplayed, ResultReused, ResultInUse, ResultTakenOver,
	}
}

type metrics struct {
	requests *prometheus.CounterVec
}

// newMetrics registers the counter on reg, every (operation, result) pair
// at zero.
func newMetrics(reg prometheus.Registerer, operations []string) (*metrics, error) {
	m := &metrics{requests: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricRequests,
		Help: "Requests the Idempotency-Key store handled, by operation and result.",
	}, []string{"operation", "result"})}
	for _, op := range operations {
		for _, r := range results() {
			m.requests.WithLabelValues(op, r)
		}
	}
	if err := reg.Register(m.requests); err != nil {
		return nil, fmt.Errorf("idempotency metrics: %w", err)
	}
	return m, nil
}

func (m *metrics) inc(operation, result string) {
	m.requests.WithLabelValues(operation, result).Inc()
}
