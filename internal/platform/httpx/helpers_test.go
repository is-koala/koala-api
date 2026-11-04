package httpx_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/log"
)

// logSink is a goroutine-safe buffer of JSON log lines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records returns every log line decoded.
func (s *logSink) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(s.buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %q", err, sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// find returns the first record with the given message.
func (s *logSink) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	for _, r := range s.records(t) {
		if r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no log record %q", msg)
	return nil
}

func newLogger() (*slog.Logger, *logSink) {
	sink := &logSink{}
	return log.New(sink, slog.LevelDebug), sink
}
