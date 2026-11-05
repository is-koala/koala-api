// Package fakes holds test fakes of the external HTTP services Koala calls
// (plan §6.1 Faz 1): Iyzico and FCM. A fake is the HTTP server the real
// client talks to, started with httptest: point the client's base URL at
// it. It records every request and answers from a programmed scenario.
//
// The real clients come in Faz 5 (FCM) and Faz 6 (Iyzico); the fakes only
// model what those clients need from the wire. Only tests import this
// package (TestOnlyTestsImportFakes).
package fakes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
)

// maxBody bounds a request body; the services take small JSON. A larger
// body is refused with 413 and fails the test at cleanup.
const maxBody = 1 << 20

// Request is a request a fake received.
type Request struct {
	Method string
	Path   string // URL path without the query
	Query  url.Values
	Header http.Header
	Body   []byte
}

// Response is a programmed answer.
type Response struct {
	Status int // 0 means 200
	Header http.Header
	Body   []byte
	// Hang keeps the request open, without an answer, until the client
	// gives up or the fake closes: a timeout whose outcome the client
	// cannot know.
	Hang bool
	// Drop closes the connection without an answer: a network failure.
	Drop bool
}

// JSON returns a response with status and v encoded as JSON. It panics if
// v cannot be encoded, which is a mistake in the test.
func JSON(status int, v any) Response {
	body, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("fakes: encode response: %v", err))
	}
	return Response{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   body,
	}
}

// responder answers a request or declines with false.
type responder func(Request) (Response, bool)

// Server is the part every fake shares: the httptest server, the request
// log and the scenario. A request is answered by, in order:
//  1. the fake's checks (e.g. a bad signature is refused before anything
//     else, and consumes nothing);
//  2. the next response enqueued for its method and path;
//  3. the fake's default behavior, if it has one for that request.
//
// A request nothing answers gets 501 and fails the test at cleanup, as does
// any other problem a fake reports.
type Server struct {
	tb       testing.TB
	name     string
	srv      *httptest.Server
	done     chan struct{}
	check    responder
	fallback responder

	mu       sync.Mutex
	requests []Request
	queues   map[string][]Response
	problems []string
}

func newServer(tb testing.TB, name string, check, fallback responder) *Server {
	tb.Helper()
	s := &Server{
		tb:       tb,
		name:     name,
		done:     make(chan struct{}),
		check:    check,
		fallback: fallback,
		queues:   map[string][]Response{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	tb.Cleanup(s.close)
	return s
}

// URL is the base URL to give the client in place of the service's.
func (s *Server) URL() string { return s.srv.URL }

// Client returns an HTTP client for the fake.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Enqueue programs the next answers to method and path, used once each in
// order. When they run out the fake's default behavior applies again.
func (s *Server) Enqueue(method, path string, rs ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(method, path)
	s.queues[k] = append(s.queues[k], rs...)
}

// Requests returns every request received so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// RequestsTo returns the requests received for method and path.
func (s *Server) RequestsTo(method, path string) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func key(method, path string) string { return method + " " + path }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		s.report(fmt.Sprintf("%s fake: read body of %s %s: %v", s.name, r.Method, r.URL.Path, err))
		http.Error(w, "fakes: read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		s.report(fmt.Sprintf("%s fake: %s %s body is over %d bytes", s.name, r.Method, r.URL.Path, maxBody))
		http.Error(w, "fakes: body too large", http.StatusRequestEntityTooLarge)
		return
	}
	req := Request{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   body,
	}
	s.write(w, r, s.respond(req))
}

func (s *Server) respond(req Request) Response {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	if s.check != nil {
		if resp, ok := s.check(req); ok {
			return resp
		}
	}

	s.mu.Lock()
	k := key(req.Method, req.Path)
	if q := s.queues[k]; len(q) > 0 {
		s.queues[k] = q[1:]
		s.mu.Unlock()
		return q[0]
	}
	s.mu.Unlock()

	if s.fallback != nil {
		if resp, ok := s.fallback(req); ok {
			return resp
		}
	}

	s.report(fmt.Sprintf("%s fake: unexpected request %s", s.name, k))
	return JSON(http.StatusNotImplemented, map[string]string{
		"error": fmt.Sprintf("%s fake: no response programmed for %s", s.name, k),
	})
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, resp Response) {
	if resp.Hang {
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
		resp.Drop = true
	}
	if resp.Drop {
		// net/http closes the connection and logs nothing.
		panic(http.ErrAbortHandler)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// report records a problem that fails the test at cleanup. Reporting from
// the handler goroutine, not calling tb.Errorf there, keeps a problem
// visible even when it happens after the test body returned.
func (s *Server) report(problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, problem)
}

// close releases hanging requests, stops the server and fails the test
// for every problem reported.
func (s *Server) close() {
	close(s.done)
	s.srv.Close()
	s.mu.Lock()
	problems := slices.Clone(s.problems)
	s.mu.Unlock()
	for _, p := range problems {
		s.tb.Errorf("%s", p)
	}
}
