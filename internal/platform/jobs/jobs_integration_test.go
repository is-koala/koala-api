//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/jobs"
)

const waitTimeout = 30 * time.Second

type noteCreatedArgs struct {
	NoteID string `json:"note_id"`
}

func (noteCreatedArgs) Kind() string { return "example_note_created" }

type tickArgs struct{}

func (tickArgs) Kind() string { return "example_tick" }

type tickWorker struct {
	river.WorkerDefaults[tickArgs]
}

func (tickWorker) Work(context.Context, *river.Job[tickArgs]) error { return nil }

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestInsertTxCommitsWithTheData is the outbox guarantee of ADR-0010: the
// job and the data it is about commit together or not at all.
func TestInsertTxCommitsWithTheData(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	// Insert-only client: no queues, so it never works jobs and its jobs
	// stay in river_job for the assertions.
	client, err := jobs.NewClient(app.Pool(), jobs.Config{Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}

	errRollback := errors.New("roll back")
	for _, commit := range []bool{true, false} {
		ctx := t.Context()
		id := uuid.Must(uuid.NewV7())
		err := app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
			if _, err := q.Exec(ctx, "INSERT INTO active_example_notes (id, body) VALUES ($1, 'note')", id); err != nil {
				return err
			}
			if _, err := client.InsertTx(ctx, q.PgxTx(), noteCreatedArgs{NoteID: id.String()}, nil); err != nil {
				return err
			}
			if !commit {
				return errRollback
			}
			return nil
		})
		if commit && err != nil {
			t.Fatalf("InTx() = %v", err)
		}
		if !commit && !errors.Is(err, errRollback) {
			t.Fatalf("InTx() = %v, want the rollback error", err)
		}

		var notes, queued int
		err = app.Pool().QueryRow(ctx, `
			SELECT (SELECT count(*) FROM active_example_notes WHERE id = $1),
			       (SELECT count(*) FROM river_job WHERE kind = $2 AND args->>'note_id' = $3)`,
			id, noteCreatedArgs{}.Kind(), id.String()).Scan(&notes, &queued)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if commit {
			want = 1
		}
		if notes != want || queued != want {
			t.Fatalf("commit=%v: notes = %d, jobs = %d; want %d of each", commit, notes, queued, want)
		}
	}
}

// TestUniqueOptsSkipsDuplicates: the same (kind, args) inserted twice with
// UniqueOpts.ByArgs is enqueued once, also across transactions (plan §3.5:
// `(payment_id, operation)` must not be queued twice). It runs as
// koala_app, so it also proves the grants on River's unique index function.
func TestUniqueOptsSkipsDuplicates(t *testing.T) {
	t.Parallel()

	app := dbtest.New(t).Open(t, db.RoleApp)
	client, err := jobs.NewClient(app.Pool(), jobs.Config{Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	args := noteCreatedArgs{NoteID: uuid.Must(uuid.NewV7()).String()}
	opts := &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}}

	var skipped []bool
	for range 2 {
		err := app.InTx(ctx, func(ctx context.Context, q db.Querier) error {
			res, err := client.InsertTx(ctx, q.PgxTx(), args, opts)
			if err != nil {
				return err
			}
			skipped = append(skipped, res.UniqueSkippedAsDuplicate)
			return nil
		})
		if err != nil {
			t.Fatalf("InTx() = %v", err)
		}
	}
	if len(skipped) != 2 || skipped[0] || !skipped[1] {
		t.Fatalf("UniqueSkippedAsDuplicate = %v, want [false true]", skipped)
	}
	var queued int
	err = app.Pool().QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'note_id' = $2",
		args.Kind(), args.NoteID).Scan(&queued)
	if err != nil || queued != 1 {
		t.Fatalf("jobs = %d, err = %v; want 1", queued, err)
	}
}

// startClients starts one River client per login, as separate server
// instances would run, each on its own pool.
func startClients(t *testing.T, d dbtest.DB, ids []string) (map[string]*jobs.Client, map[string]dbtest.Login) {
	t.Helper()
	ctx := t.Context()
	clients := map[string]*jobs.Client{}
	logins := map[string]dbtest.Login{}
	for _, id := range ids {
		login := d.NewLogin(t, dbtest.LoginOptions{Roles: []string{db.RoleApp}})
		pool, err := db.Open(ctx, db.Config{URL: login.URL, Role: db.RoleApp, Logger: discard()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)

		workers := river.NewWorkers()
		river.AddWorker(workers, tickWorker{})
		client, err := jobs.NewClient(pool.Pool(), jobs.Config{
			ID:      id,
			Logger:  discard(),
			Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
			Workers: workers,
			PeriodicJobs: []*river.PeriodicJob{
				river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
					func() (river.JobArgs, *river.InsertOpts) { return tickArgs{}, nil },
					&river.PeriodicJobOpts{ID: "example_tick", RunOnStart: true}),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		if err := client.Start(runCtx); err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Cancel first: a client cut off from the database cannot stop
			// gracefully.
			cancel()
			stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer stopCancel()
			_ = client.Stop(stopCtx)
		})
		clients[id] = client
		logins[id] = login
	}
	return clients, logins
}

func leaderOf(ctx context.Context, app *db.DB) (string, error) {
	var id string
	err := app.Pool().QueryRow(ctx, "SELECT leader_id FROM river_leader").Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// waitForLeader waits for a periodic job to complete, checks that it was
// enqueued once and that there is one leader, and returns the leader and
// the other client.
func waitForLeader(t *testing.T, app *db.DB, ids []string) (leader, follower string) {
	t.Helper()
	ctx := t.Context()
	dbtest.Eventually(t, waitTimeout, "the periodic job to complete", func() (bool, error) {
		var done int
		err := app.Pool().QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = $1 AND state = 'completed'", tickArgs{}.Kind()).Scan(&done)
		return done > 0, err
	})

	var total, leaders int
	err := app.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM river_job WHERE kind = $1), (SELECT count(*) FROM river_leader)`,
		tickArgs{}.Kind()).Scan(&total, &leaders)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || leaders != 1 {
		t.Fatalf("periodic jobs enqueued = %d, leaders = %d; want 1 and 1", total, leaders)
	}
	leader, err = leaderOf(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	switch leader {
	case ids[0]:
		return leader, ids[1]
	case ids[1]:
		return leader, ids[0]
	default:
		t.Fatalf("leader_id = %q, not one of %v", leader, ids)
		return "", ""
	}
}

// TestPeriodicJobRunsOnTheLeaderOnly starts two clients, as two instances
// of the server would. River elects one leader; only the leader enqueues
// periodic jobs, so a RunOnStart job is enqueued once, not twice. When the
// leader stops gracefully it resigns and the other client takes over.
func TestPeriodicJobRunsOnTheLeaderOnly(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	app := d.Open(t, db.RoleApp)
	ids := []string{"instance-a", "instance-b"}
	clients, _ := startClients(t, d, ids)
	leader, follower := waitForLeader(t, app, ids)

	stopCtx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	if err := clients[leader].Stop(stopCtx); err != nil {
		t.Fatalf("stop leader: %v", err)
	}
	dbtest.Eventually(t, waitTimeout, "the follower to take over after a resignation", func() (bool, error) {
		id, err := leaderOf(t.Context(), app)
		return id == follower, err
	})
}

// TestLeaderCrashFailsOverAfterTTL: a leader that loses the database
// cannot resign. Its river_leader row expires after River's leader TTL
// (elect interval 5s + 10s padding in River v0.49) and the other client
// is elected.
func TestLeaderCrashFailsOverAfterTTL(t *testing.T) {
	t.Parallel()

	d := dbtest.New(t)
	app := d.Open(t, db.RoleApp)
	ids := []string{"instance-c", "instance-d"}
	_, logins := startClients(t, d, ids)
	leader, follower := waitForLeader(t, app, ids)

	d.Disconnect(t, logins[leader].User)
	dbtest.Eventually(t, 60*time.Second, "the follower to take over after the leader's TTL", func() (bool, error) {
		id, err := leaderOf(t.Context(), app)
		return id == follower, err
	})
}
