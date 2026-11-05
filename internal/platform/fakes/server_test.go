package fakes_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/is-koala/koala-api/internal/platform/fakes"
)

func TestServerAnswersInOrderAndRecords(t *testing.T) {
	t.Parallel()
	rec := &recordingTB{}
	t.Cleanup(func() { rec.finish() })
	f := fakes.NewFCM(rec, "koala-test")
	f.Enqueue(http.MethodGet, "/scenario",
		fakes.JSON(http.StatusOK, map[string]int{"n": 1}),
		fakes.Response{Status: http.StatusAccepted, Header: http.Header{"X-Fake": {"2"}}, Body: []byte("second")},
	)

	if status, body := call(t, f.Client(), http.MethodGet, f.URL()+"/scenario?page=1", nil, ""); status != http.StatusOK || decode(t, body)["n"] != float64(1) {
		t.Fatalf("first: %d %s", status, body)
	}
	if status, body := call(t, f.Client(), http.MethodGet, f.URL()+"/scenario", nil, ""); status != http.StatusAccepted || string(body) != "second" {
		t.Fatalf("second: %d %s", status, body)
	}
	// Out of programmed answers and no default: 501, and the test fails.
	if status, _ := call(t, f.Client(), http.MethodGet, f.URL()+"/scenario", nil, ""); status != http.StatusNotImplemented {
		t.Fatalf("third: %d, want 501", status)
	}

	reqs := f.RequestsTo(http.MethodGet, "/scenario")
	if len(reqs) != 3 || reqs[0].Query.Get("page") != "1" {
		t.Fatalf("recorded %+v", reqs)
	}
	errs := rec.finish()
	if len(errs) != 1 || !strings.Contains(errs[0], "unexpected request GET /scenario") {
		t.Fatalf("reported at cleanup: %q", errs)
	}
}

func TestServerHang(t *testing.T) {
	t.Parallel()
	rec := &recordingTB{}
	t.Cleanup(func() { rec.finish() })
	f := fakes.NewFCM(rec, "koala-test")
	f.Enqueue(http.MethodPost, "/hang", fakes.Response{Hang: true}, fakes.Response{Hang: true})

	// The client gives up: its deadline, not an answer, ends the call.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := callErr(ctx, f.Client(), f.URL()+"/hang"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung call: %v, want the client's deadline", err)
	}

	// A call still hanging when the fake closes is released, not leaked.
	done := make(chan error, 1)
	go func() { done <- callErr(context.WithoutCancel(t.Context()), f.Client(), f.URL()+"/hang") }()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.RequestsTo(http.MethodPost, "/hang")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second call never arrived")
		}
		time.Sleep(time.Millisecond)
	}
	if errs := rec.finish(); len(errs) != 0 {
		t.Fatalf("reported: %q", errs)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hanging call got an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the fake did not release the hanging call")
	}
}

func TestServerDrop(t *testing.T) {
	t.Parallel()
	f := fakes.NewFCM(t, "koala-test")
	f.Enqueue(http.MethodPost, "/drop", fakes.Response{Drop: true})
	if err := callErr(t.Context(), f.Client(), f.URL()+"/drop"); err == nil {
		t.Fatal("dropped call succeeded")
	}
}

// TestOnlyTestsImportFakes keeps the fakes out of the server: no non-test
// file in the module imports this package.
func TestOnlyTestsImportFakes(t *testing.T) {
	t.Parallel()
	const pkg = "github.com/is-koala/koala-api/internal/platform/fakes"
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	fset := token.NewFileSet()
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == pkg && filepath.Dir(path) != filepath.Join(root, "internal", "platform", "fakes") {
				t.Errorf("%s imports %s; only tests may", path, pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("checked only %d files; is %s the module root?", checked, root)
	}
}
