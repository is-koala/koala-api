package app

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/log"
)

// gaugeValue reads a metric of reg without labels: a counter's or a gauge's
// value, -1 when it is not gathered.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name || len(mf.GetMetric()) != 1 {
			continue
		}
		m := mf.GetMetric()[0]
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
		return m.GetGauge().GetValue()
	}
	return -1
}

// While the database does not answer, every failed start of the workers
// is counted and koala_workers_up stays 0; stopping does not wait for a
// start that will never succeed (ADR-0025 §7).
func TestWorkersStartFailuresAreCountedAndStopReturns(t *testing.T) {
	t.Parallel()

	logger := log.New(&bytes.Buffer{}, slog.LevelInfo)
	database := lazyDB(t)
	client, err := newWorkerClient(database, testModulesOn(t, database, clock.Real{}), logger)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m, err := newWorkerMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorkers(t.Context(), client, logger, m, 10*time.Millisecond)

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for gaugeValue(t, reg, metricWorkerStartFailures) < 2 {
		select {
		case <-deadline.C:
			t.Fatalf("%s = %v after 10s, want at least 2", metricWorkerStartFailures, gaugeValue(t, reg, metricWorkerStartFailures))
		case <-tick.C:
		}
	}
	if up := gaugeValue(t, reg, metricWorkersUp); up != 0 {
		t.Errorf("%s = %v while the start fails, want 0", metricWorkersUp, up)
	}

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), workerStopTimeout)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		stop(stopCtx)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-stopCtx.Done():
		t.Fatal("stop did not return within workerStopTimeout")
	}
}
