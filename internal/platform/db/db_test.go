package db_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/db"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestOpenNeverEchoesTheURL(t *testing.T) {
	t.Parallel()

	const password = "s3cr3t-password"
	_, err := db.Open(t.Context(), db.Config{
		URL: "postgres://app:" + password + "@db:notaport/koala", Role: db.RoleApp, Logger: discard(),
	})
	if err == nil {
		t.Fatal("Open() error = nil for an invalid URL")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("error echoes the password: %v", err)
	}
}

func TestOpenRequiresLogger(t *testing.T) {
	t.Parallel()

	if _, err := db.Open(t.Context(), db.Config{URL: "postgres://app@127.0.0.1:1/koala", Role: db.RoleApp}); err == nil {
		t.Fatal("Open() without Logger = nil error")
	}
}

// TestReadyFailsWhenDatabaseIsDown: Open is lazy, Ready reports the outage.
func TestReadyFailsWhenDatabaseIsDown(t *testing.T) {
	t.Parallel()

	// Port 1 on loopback: nothing listens, the connection is refused.
	database, err := db.Open(t.Context(), db.Config{
		URL: "postgres://app@127.0.0.1:1/koala?sslmode=disable", Role: db.RoleApp, Logger: discard(),
	})
	if err != nil {
		t.Fatalf("Open() error = %v; Open must not connect", err)
	}
	t.Cleanup(database.Close)

	if err := database.Ready(t.Context()); err == nil {
		t.Fatal("Ready() = nil with no database")
	}
}
