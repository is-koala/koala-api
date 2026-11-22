package media

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricSwept counts what the media sweep did with each media it listed
// (ADR-0035 Karar 4 and 8), by result.
const MetricSwept = "koala_media_swept_total"

// Sweep results, the values of the result label.
const (
	// sweptDeleted: soft-deleted, its objects' deletion queued.
	sweptDeleted = "deleted"
	// sweptAdopted: a READY media a consumer points at without Bind; the
	// sweep bound it. A consumer bug, alerted on.
	sweptAdopted = "adopted"
	// sweptKept: its retirement failed and it was left. Alerted on.
	sweptKept = "kept"
)

// sweepMetrics are the media sweep's counters.
type sweepMetrics struct {
	results *prometheus.CounterVec
}

// newSweepMetrics registers the counters on reg, every result at zero.
func newSweepMetrics(reg prometheus.Registerer) (*sweepMetrics, error) {
	m := &sweepMetrics{results: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricSwept,
		Help: "Media the sweep listed, by result: deleted, adopted (bound by the sweep because a consumer " +
			"points at it without media.Bind) and kept (its retirement failed).",
	}, []string{"result"})}
	if err := reg.Register(m.results); err != nil {
		return nil, fmt.Errorf("media metrics: %w", err)
	}
	for _, r := range []string{sweptDeleted, sweptAdopted, sweptKept} {
		m.results.WithLabelValues(r)
	}
	return m, nil
}

func (m *sweepMetrics) swept(result string) {
	m.results.WithLabelValues(result).Inc()
}
