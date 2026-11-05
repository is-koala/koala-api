package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/httpx"
)

// readerFromRecorder is a response writer with its own ReadFrom, like
// net/http's, whose ReadFrom is the sendfile path.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalls int
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalls++
	return io.Copy(r.ResponseRecorder, src)
}

// plainWriter implements http.ResponseWriter and nothing else: no
// io.ReaderFrom, no Flush.
type plainWriter struct {
	header http.Header
	body   strings.Builder
	status int
}

func (p *plainWriter) Header() http.Header { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return p.body.Write(b)
}
func (p *plainWriter) WriteHeader(code int) { p.status = code }

func serveRecovered(w http.ResponseWriter, h http.HandlerFunc) (panicked any) {
	logger, _ := newLogger()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/file", http.NoBody)
	return recovered(func() { httpx.Recover(logger)(h).ServeHTTP(w, req) })
}

// io.Copy through Recover keeps the underlying writer's ReadFrom (the
// sendfile path), and a panic after the copy aborts the response: the
// status line has been sent, so a 500 can no longer replace it.
func TestRecoverKeepsReadFromAndCountsItAsWritten(t *testing.T) {
	t.Parallel()

	w := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	v := serveRecovered(w, func(w http.ResponseWriter, _ *http.Request) {
		// A reader without WriteTo, like a file or a pipe, so io.Copy takes
		// the writer's ReadFrom.
		if _, err := io.Copy(w, struct{ io.Reader }{strings.NewReader("file body")}); err != nil {
			t.Errorf("copy: %v", err)
		}
		panic("after the copy")
	})
	if w.readFromCalls != 1 {
		t.Fatalf("underlying ReadFrom called %d times, want 1", w.readFromCalls)
	}
	if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", v)
	}
	if w.Body.String() != "file body" || w.Code != http.StatusOK {
		t.Fatalf("response %d %q, want the copied body and no 500", w.Code, w.Body)
	}
}

// Without ReadFrom underneath, io.Copy falls back to Write instead of
// calling the wrapper's ReadFrom again.
func TestRecoverReadFromFallsBackToWrite(t *testing.T) {
	t.Parallel()

	w := &plainWriter{header: http.Header{}}
	v := serveRecovered(w, func(w http.ResponseWriter, _ *http.Request) {
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Error("the wrapper does not offer ReadFrom")
			return
		}
		if n, err := rf.ReadFrom(strings.NewReader("plain body")); err != nil || n != 10 {
			t.Errorf("ReadFrom = %d, %v", n, err)
		}
	})
	if v != nil {
		t.Fatalf("panicked: %v", v)
	}
	if w.body.String() != "plain body" || w.status != http.StatusOK {
		t.Fatalf("response %d %q", w.status, w.body.String())
	}
}

// http.ResponseController reaches the underlying Flush through the
// wrapper's FlushError; a flushed response counts as written.
func TestRecoverFlushErrorFlushesAndCommits(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	v := serveRecovered(w, func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush() = %v", err)
		}
		panic("after the flush")
	})
	if !w.Flushed {
		t.Fatal("the underlying writer was not flushed")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", v)
	}
}

// A writer that cannot flush reports it through FlushError instead of
// failing silently.
func TestRecoverFlushErrorReportsUnsupported(t *testing.T) {
	t.Parallel()

	w := &plainWriter{header: http.Header{}}
	var flushErr error
	serveRecovered(w, func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
		w.WriteHeader(http.StatusNoContent)
	})
	if !errors.Is(flushErr, http.ErrNotSupported) {
		t.Fatalf("Flush() = %v, want http.ErrNotSupported", flushErr)
	}
}
