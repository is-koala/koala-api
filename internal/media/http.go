package media

import (
	"context"
	"net/http"
	"strings"

	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/idempotency"
	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/jsontime"
)

// HTTP implements the media operations of the generated strict server
// (api/openapi.yaml, tag media; ADR-0029 Karar 6, ADR-0035). Every one is
// the caller's own: the media is looked up by id and owner together, so
// another user's media is the same 404 as one that never existed.
type HTTP struct {
	service *Service
}

// NewHTTP returns the handlers.
func NewHTTP(service *Service) *HTTP {
	return &HTTP{service: service}
}

// StartMediaUpload starts an upload: a PENDING media and the quarantine
// key's write address.
func (h *HTTP) StartMediaUpload(ctx context.Context, req apiv1.StartMediaUploadRequestObject) (apiv1.StartMediaUploadResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	// The answer is built in the start's transaction and stored there for
	// the Idempotency-Key (ADR-0034 Karar 4): a retry whose first answer
	// was lost gets the same media and the same write address, and spends
	// neither budget again.
	var resp apiv1.StartMediaUpload201JSONResponse
	_, err := h.service.Start(ctx, p.UserID, Kind(req.Body.Kind), req.Body.SizeBytes,
		func(ctx context.Context, q db.Querier, started Started) error {
			location := "/v1/me/media/" + started.Media.ID.String()
			resp = apiv1.StartMediaUpload201JSONResponse{
				Body: apiv1.MediaUpload{
					Media:           render(started.Media, Address{}),
					UploadURL:       started.UploadURL,
					UploadExpiresAt: jsontime.From(started.ExpiresAt),
				},
				Headers: apiv1.StartMediaUpload201ResponseHeaders{Location: &location},
			}
			return idempotency.Commit(ctx, q, resp.VisitStartMediaUploadResponse)
		})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CompleteMediaUpload queues the completion of a PENDING upload and
// answers 202 with the media as it stands; the outcome is read from it.
func (h *HTTP) CompleteMediaUpload(ctx context.Context, req apiv1.CompleteMediaUploadRequestObject) (apiv1.CompleteMediaUploadResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	m, err := h.service.RequestCompletion(ctx, p.UserID, req.MediaID)
	if err != nil {
		return nil, err
	}
	addr, err := h.service.Address(ctx, m, false)
	if err != nil {
		return nil, err
	}
	return apiv1.CompleteMediaUpload202JSONResponse(render(m, addr)), nil
}

// GetMedia returns one of the caller's media, with its address by kind.
func (h *HTTP) GetMedia(ctx context.Context, req apiv1.GetMediaRequestObject) (apiv1.GetMediaResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	m, err := h.service.Get(ctx, p.UserID, req.MediaID)
	if err != nil {
		return nil, err
	}
	addr, err := h.service.Address(ctx, m, true)
	if err != nil {
		return nil, err
	}
	return apiv1.GetMedia200JSONResponse(render(m, addr)), nil
}

// DeleteMedia deletes one of the caller's media; its objects follow in a
// job.
func (h *HTTP) DeleteMedia(ctx context.Context, req apiv1.DeleteMediaRequestObject) (apiv1.DeleteMediaResponseObject, error) {
	p, ok := authn.UserFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if err := h.service.Delete(ctx, p.UserID, req.MediaID); err != nil {
		return nil, err
	}
	return apiv1.DeleteMedia204Response{}, nil
}

// StrictMiddleware returns the strict server middleware of the media
// operations: an answer that carries a wait (MEDIA_UPLOAD_DAILY_LIMIT)
// gets its Retry-After, which the contract's TooManyRequests promises.
func (h *HTTP) StrictMiddleware() apiv1.StrictMiddlewareFunc {
	return func(f apiv1.StrictHandlerFunc, _ string) apiv1.StrictHandlerFunc {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			resp, err := f(ctx, w, r, request)
			if wait, ok := RetryAfter(err); ok {
				httpx.SetRetryAfter(w, wait)
			}
			return resp, err
		}
	}
}

// render is the contract's Media of m with its address.
func render(m Media, addr Address) apiv1.Media {
	out := apiv1.Media{
		ID: m.ID, Kind: apiv1.MediaKind(m.Kind), Status: apiv1.MediaStatus(m.Status),
		CreatedAt: jsontime.From(m.CreatedAt), SizeBytes: m.Size,
	}
	if m.ContentType != "" {
		ct := ContentTypeValue(m.ContentType)
		out.ContentType = &ct
	}
	if code, ok := m.Rejection.Code(); ok {
		ec := apiv1.ErrorCode(code)
		out.RejectionCode = &ec
	}
	if addr.URL != "" {
		url := addr.URL
		out.URL = &url
	}
	if addr.ExpiresAt != nil {
		at := jsontime.From(*addr.ExpiresAt)
		out.URLExpiresAt = &at
	}
	return out
}

// ContentTypeValue is the contract's MediaContentType of a detected type:
// the media type in upper case with every separator an underscore (style
// guide §1). The rule is mechanical in both directions, which
// internal/app's TestMediaContentTypesMatchTheContract holds against the
// contract's enum (ADR-0031 Karar 2).
func ContentTypeValue(typ imaging.Type) apiv1.MediaContentType {
	value := strings.ToUpper(string(typ))
	for _, separator := range []string{"/", ".", "-", "+"} {
		value = strings.ReplaceAll(value, separator, "_")
	}
	return apiv1.MediaContentType(value)
}
