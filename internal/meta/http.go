// Package meta serves the contract's helper endpoints on /v1/meta/ping:
// POST echoes a validated body, PATCH reports what a JSON Merge Patch
// changes. They show that a request passes the spec's validation and the
// generated strict server end to end (ADR-0019). They are not mounted in
// production and go away with the first real operation.
package meta

import (
	"context"

	"github.com/is-koala/koala-api/internal/apiv1"
)

// Handler implements the meta operations of apiv1.StrictServerInterface.
type Handler struct{}

// NewHandler returns the meta handler.
func NewHandler() *Handler {
	return &Handler{}
}

// Ping returns the request body. The validation middleware has already
// checked it against the spec, so there is nothing left to check here.
func (*Handler) Ping(_ context.Context, req apiv1.PingRequestObject) (apiv1.PingResponseObject, error) {
	return apiv1.Ping200JSONResponse{Amount: apiv1.Money(req.Body.Amount), Note: req.Body.Note}, nil
}

// PatchPing reports which members of the merge patch set a value and which
// cleared one (RFC 7396: an absent member is unchanged, null clears it).
func (*Handler) PatchPing(_ context.Context, req apiv1.PatchPingRequestObject) (apiv1.PatchPingResponseObject, error) {
	res := apiv1.PatchPing200JSONResponse{Set: []string{}, Cleared: []string{}}
	if req.Body.Amount != nil {
		res.Set = append(res.Set, "amount")
	}
	switch {
	case req.Body.Note.IsNull():
		res.Cleared = append(res.Cleared, "note")
	case req.Body.Note.IsSpecified():
		res.Set = append(res.Set, "note")
	}
	return res, nil
}
