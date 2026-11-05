// Package meta serves the contract's helper endpoints. POST /v1/meta/ping
// echoes a validated body; it shows that a request passes the spec's
// validation and the generated strict server end to end (ADR-0019). It is
// not mounted in production and goes away with the first real operation.
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
