package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"

	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/realtime"
)

// authPurgeInterval is how often the authentication purge runs. Rows it
// removes are retained for days (ADR-0025 §7), so hourly is plenty.
const authPurgeInterval = time.Hour

// workerStopTimeout bounds the wait for the workers to wind down after
// the HTTP drain; their context is canceled first, so it is short.
const workerStopTimeout = 5 * time.Second

// workerRetry is how long serve waits before trying again to start the
// workers when the database did not answer.
const workerRetry = 5 * time.Second

// authPurgeArgs is the periodic purge of the exemption class M2 tables of
// authentication: ended sessions with their refresh tokens, old attempts,
// staff address counters and expired realtime tickets.
type authPurgeArgs struct{}

// Kind is the job kind in River's tables.
func (authPurgeArgs) Kind() string { return "auth_purge" }

// authPurgeWorker runs the purge as koala_app.
type authPurgeWorker struct {
	river.WorkerDefaults[authPurgeArgs]

	users   *authn.Service
	staff   *authn.StaffService
	tickets *realtime.PostgresTickets
	clock   clock.Clock
	logger  *slog.Logger
}

// Work purges every table and logs what it removed.
func (w *authPurgeWorker) Work(ctx context.Context, _ *river.Job[authPurgeArgs]) error {
	sessions, attempts, errUsers := w.users.Purge(ctx)
	staffSessions, sources, errStaff := w.staff.Purge(ctx)
	tickets, errTickets := w.tickets.Purge(ctx, w.clock.Now())
	if err := errors.Join(errUsers, errStaff, errTickets); err != nil {
		return fmt.Errorf("auth purge: %w", err)
	}
	w.logger.InfoContext(ctx, "auth purge",
		slog.Int64("user_sessions", sessions), slog.Int64("login_attempts", attempts),
		slog.Int64("staff_sessions", staffSessions), slog.Int64("staff_login_sources", sources),
		slog.Int64("realtime_tickets", tickets))
	return nil
}

// newWorkerClient returns the River client of serve: the default queue and
// the periodic jobs. Periodic jobs are enqueued by the elected leader only,
// so each runs once per interval however many instances run (ADR-0010).
func newWorkerClient(database *db.DB, mods modules, logger *slog.Logger) (*jobs.Client, error) {
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, mods.purge); err != nil {
		return nil, fmt.Errorf("workers: %w", err)
	}
	client, err := jobs.NewClient(database.Pool(), jobs.Config{
		Logger:  logger,
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(authPurgeInterval),
				func() (river.JobArgs, *river.InsertOpts) { return authPurgeArgs{}, nil },
				&river.PeriodicJobOpts{ID: "auth_purge", RunOnStart: true}),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("workers: %w", err)
	}
	return client, nil
}

// Worker metrics (ADR-0025 §7). A start that keeps failing shows as
// koala_workers_up staying 0 while the failure counter grows.
const (
	metricWorkersUp           = "koala_workers_up"
	metricWorkerStartFailures = "koala_worker_start_failures_total"
)

// workerMetrics are the gauges and counters of the background workers.
type workerMetrics struct {
	up       prometheus.Gauge
	failures prometheus.Counter
}

func newWorkerMetrics(reg prometheus.Registerer) (*workerMetrics, error) {
	m := &workerMetrics{
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: metricWorkersUp,
			Help: "1 while the River client of serve runs, 0 before it started and after it stopped.",
		}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: metricWorkerStartFailures,
			Help: "Failed starts of the River client of serve; each is retried.",
		}),
	}
	for _, c := range []prometheus.Collector{m.up, m.failures} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("worker metrics: %w", err)
		}
	}
	return m, nil
}

// startWorkers starts client in the background and returns the function
// that stops it. Like the HTTP server, serve does not wait for the
// database: while it does not answer, the start is retried every retry
// and counted. The workers run on a context of their own, so a canceled
// serve context does not cut a job short before the HTTP drain ends.
//
// stop cancels that context first: a start still dialing the database
// returns at once instead of holding the shutdown, and a running job is
// canceled (the purge is idempotent; the next run finishes it). Then it
// waits, bounded by its context, for the client to wind down.
func startWorkers(ctx context.Context, client *jobs.Client, logger *slog.Logger, m *workerMetrics, retry time.Duration) func(context.Context) {
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	retryCtx, cancelRetry := context.WithCancel(ctx)
	done := make(chan bool, 1)
	go func() {
		for {
			err := client.Start(runCtx)
			if err == nil {
				m.up.Set(1)
				done <- true
				return
			}
			if runCtx.Err() != nil {
				done <- false
				return
			}
			m.failures.Inc()
			logger.WarnContext(retryCtx, "workers not started; retrying", slog.Any("error", err))
			t := time.NewTimer(retry)
			select {
			case <-retryCtx.Done():
				t.Stop()
				done <- false
				return
			case <-t.C:
			}
		}
	}()
	return func(stopCtx context.Context) {
		cancelRetry()
		cancelRun()
		started := <-done
		if !started {
			return
		}
		defer m.up.Set(0)
		select {
		case <-client.Stopped():
		case <-stopCtx.Done():
			logger.WarnContext(stopCtx, "workers did not stop in time", slog.Any("error", stopCtx.Err()))
		}
	}
}
