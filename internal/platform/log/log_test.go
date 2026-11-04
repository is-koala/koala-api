package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/log"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not JSON: %v: %q", err, buf.String())
	}
	return m
}

func TestContextAttributesAreLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, slog.LevelInfo)
	ctx := log.WithAttrs(context.Background(), slog.String("request_id", "req-1"))
	ctx = log.WithAttrs(ctx, slog.String("user_id", "u-1"))

	logger.InfoContext(ctx, "hello")

	m := decode(t, &buf)
	if m["request_id"] != "req-1" || m["user_id"] != "u-1" {
		t.Fatalf("context attributes missing: %v", m)
	}
	if m["msg"] != "hello" || m["level"] != "INFO" {
		t.Fatalf("unexpected record: %v", m)
	}
}

func TestWithAttrsDoesNotLeakIntoParentContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, slog.LevelInfo)
	parent := log.WithAttrs(context.Background(), slog.String("request_id", "req-1"))
	_ = log.WithAttrs(parent, slog.String("user_id", "u-1"))

	logger.InfoContext(parent, "parent")

	if _, ok := decode(t, &buf)["user_id"]; ok {
		t.Fatal("child attribute leaked into parent context")
	}
}

func TestLevelFiltersRecords(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, slog.LevelWarn)
	logger.InfoContext(context.Background(), "dropped")

	if buf.Len() != 0 {
		t.Fatalf("info record written at warn level: %q", buf.String())
	}
}

func TestDerivedLoggerKeepsContextAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, slog.LevelInfo).With(slog.String("component", "http"))
	ctx := log.WithAttrs(context.Background(), slog.String("request_id", "req-2"))

	logger.InfoContext(ctx, "derived")

	m := decode(t, &buf)
	if m["component"] != "http" || m["request_id"] != "req-2" {
		t.Fatalf("derived logger lost attributes: %v", m)
	}
}
