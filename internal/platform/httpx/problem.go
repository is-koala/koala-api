package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// ProblemTypeBase prefixes the RFC 9457 "type" URI; the suffix is the code
// in kebab case (plan §3.4).
const ProblemTypeBase = "https://api.iskoala.com/problems/"

// ContentTypeProblem is the RFC 9457 media type.
const ContentTypeProblem = "application/problem+json"

// Problem is an RFC 9457 problem document with Koala's extension members
// code, requestId and errors. There is no "detail": clients render text
// from code (plan §4.1-3).
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Code      apperr.Code    `json:"code"`
	Instance  string         `json:"instance,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
	Errors    []ProblemField `json:"errors,omitempty"`
}

// ProblemField is one entry of Problem.Errors.
type ProblemField struct {
	Pointer string      `json:"pointer"`
	Code    apperr.Code `json:"code"`
}

// StatusOf returns the HTTP status for an error kind. It is the only
// kind-to-status mapping in the service.
func StatusOf(k apperr.Kind) int {
	switch k {
	case apperr.KindInvalid:
		return http.StatusBadRequest
	case apperr.KindUnauthenticated:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	case apperr.KindUpstream:
		return http.StatusBadGateway
	case apperr.KindPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case apperr.KindInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// NewProblem builds the problem document for status and code on request r.
func NewProblem(r *http.Request, status int, code apperr.Code) Problem {
	return Problem{
		Type:      ProblemTypeBase + strings.ReplaceAll(strings.ToLower(string(code)), "_", "-"),
		Title:     http.StatusText(status),
		Status:    status,
		Code:      code,
		Instance:  r.URL.Path,
		RequestID: RequestIDFrom(r.Context()),
	}
}

// WriteError renders err as a problem document. Server-side failures
// (5xx) are logged with their cause; the cause never reaches the client.
//
// A *http.MaxBytesError (body over BodyLimit) becomes 413
// REQUEST_BODY_TOO_LARGE, so handlers can return the decode error as is.
// This also holds when the error is wrapped as KindInvalid, as a generic
// "bad body" wrapper would do: an oversized body is never a 400. Any other
// *apperr.Error kind around it is kept.
func WriteError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var ae *apperr.Error
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) && (!errors.As(err, &ae) || ae.Kind == apperr.KindInvalid) {
		err = apperr.Wrap(err, apperr.KindPayloadTooLarge, CodeBodyTooLarge)
	}
	e := apperr.From(err)
	kind, code := e.Kind, e.Code
	if code == "" || kind == apperr.KindInternal {
		// An error without a code is a programming error; internal codes
		// are not part of the contract.
		kind, code = apperr.KindInternal, apperr.CodeInternal
	}
	status := StatusOf(kind)
	if status >= http.StatusInternalServerError {
		logger.ErrorContext(r.Context(), "request failed",
			slog.String("kind", kind.String()),
			slog.String("code", string(code)),
			slog.Any("error", err))
	}

	p := NewProblem(r, status, code)
	for _, f := range e.Fields {
		p.Errors = append(p.Errors, ProblemField(f))
	}
	WriteProblem(w, logger, r, p)
}

// WriteProblem writes p with the problem media type.
func WriteProblem(w http.ResponseWriter, logger *slog.Logger, r *http.Request, p Problem) {
	body, err := json.Marshal(p)
	if err != nil {
		// Problem has only string and int fields; this cannot happen.
		logger.ErrorContext(r.Context(), "marshal problem", slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ContentTypeProblem)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	if _, err := w.Write(body); err != nil {
		logger.DebugContext(r.Context(), "write problem", slog.Any("error", err))
	}
}
