package address

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/integrations/googlemaps"
)

// Metric names (ADR-0039 Karar 7). Every label value comes from a finite
// set: no id, no query, no coordinate.
const (
	// MetricLookups counts the lookup budget's reservations by unit and
	// outcome.
	MetricLookups = "koala_address_lookups_total"
	// MetricLookupCap is the service's daily cap, so an alert can compare
	// the day's units with it.
	MetricLookupCap = "koala_address_lookup_cap"
	// MetricProviderCalls counts the map provider's calls by call and
	// outcome.
	MetricProviderCalls = "koala_address_provider_calls_total"
)

// Reservation outcomes, the values of the outcome label of MetricLookups.
const (
	outcomeReserved      = "reserved"
	outcomeUserLimited   = "user_limited"
	outcomeClientLimited = "client_limited"
	outcomeCapped        = "capped"
)

// Provider calls and their outcomes beyond googlemaps' reasons.
const (
	callAutocomplete = "autocomplete"
	callDetails      = "details"
	callReverse      = "reverse"
	callBounds       = "bounds"
	outcomeOK        = "ok"
	outcomeNoResult  = "no_result"
	outcomeUnwired   = "not_configured"
)

// Metrics are the address search's counters.
type Metrics struct {
	lookups *prometheus.CounterVec
	capped  prometheus.Gauge
	calls   *prometheus.CounterVec
}

// NewMetrics registers the counters on reg, every label pair at zero.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		lookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricLookups,
			Help: "Address lookup budget reservations by unit (SESSION, AUTOCOMPLETE, DETAILS, REVERSE, BOUNDS) and " +
				"outcome: reserved, user_limited, client_limited, capped (the service's daily cap).",
		}, []string{"unit", "outcome"}),
		capped: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: MetricLookupCap,
			Help: "The service's daily cap of billed address lookup units.",
		}),
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricProviderCalls,
			Help: "Map provider calls by call (autocomplete, details, reverse, bounds) and outcome.",
		}, []string{"call", "outcome"}),
	}
	for _, c := range []prometheus.Collector{m.lookups, m.capped, m.calls} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("address metrics: %w", err)
		}
	}
	for _, u := range []Unit{UnitSession, UnitAutocomplete, UnitDetails, UnitReverse, UnitBounds} {
		for _, o := range []string{outcomeReserved, outcomeUserLimited, outcomeClientLimited, outcomeCapped} {
			m.lookups.WithLabelValues(string(u), o)
		}
	}
	for _, c := range []string{callAutocomplete, callDetails, callReverse, callBounds} {
		for _, o := range []string{
			outcomeOK, outcomeNoResult, outcomeUnwired,
			googlemaps.ReasonQuota, googlemaps.ReasonDenied, googlemaps.ReasonInvalid, googlemaps.ReasonUpstream,
			googlemaps.ReasonTransport, googlemaps.ReasonUnreadable, googlemaps.ReasonUnavailable,
		} {
			m.calls.WithLabelValues(c, o)
		}
	}
	return m, nil
}

func (m *Metrics) reserved(u Unit, outcome string) {
	m.lookups.WithLabelValues(string(u), outcome).Inc()
}

func (m *Metrics) called(call, outcome string) { m.calls.WithLabelValues(call, outcome).Inc() }
