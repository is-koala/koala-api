package media

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/media/mediadb"
	"github.com/is-koala/koala-api/internal/platform/imaging"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// row is a media row as every query of db/queries/media returns it. The
// queries name the same columns in the same order, so sqlc's per-query
// row types are one struct and convert to this one.
type row = mediadb.GetOwnMediaRow

// Media is a media row read back: its keys are parsed, its kind, status,
// type and rejection are the domain's types.
type Media struct {
	ID      uuid.UUID
	OwnerID uuid.UUID
	Kind    Kind
	Status  Status
	// UploadKey is the quarantine key the write address was signed for.
	UploadKey    storage.Key
	DeclaredSize int64
	CreatedAt    time.Time
	// CompletionRequestedAt is when the owner first asked for the
	// completion; nil until then.
	CompletionRequestedAt *time.Time
	// DecidedAt is when the job decided READY or REJECTED.
	DecidedAt *time.Time
	// ObjectKey, ContentType and Size are set on a READY row only.
	ObjectKey   storage.Key
	ContentType imaging.Type
	Size        *int64
	// Rejection is set on a REJECTED row only.
	Rejection Rejection
}

// fromRow reads a row. The keys go through storage.ParseKey, which is the
// one place a string from the database becomes a key (ADR-0030 Karar 2:
// ParseKey's input today is exactly this column, so it acts as the
// database value's validator). A row that does not read is an error, not
// a media: the database's CHECKs make it unreachable, and a handler that
// rendered it anyway would hand out a key the pipeline never wrote.
func fromRow(r row) (Media, error) {
	m := Media{
		ID: r.ID, OwnerID: r.OwnerID, Kind: Kind(r.Kind), Status: Status(r.Status),
		DeclaredSize: r.DeclaredSize, CreatedAt: r.CreatedAt,
		CompletionRequestedAt: r.CompletionRequestedAt, DecidedAt: r.DecidedAt, Size: r.SizeBytes,
	}
	if _, ok := m.Kind.Prefix(); !ok {
		return Media{}, fmt.Errorf("media %s: unknown kind %q", r.ID, r.Kind)
	}
	up, err := storage.ParseKey(r.UploadKey)
	if err != nil {
		return Media{}, fmt.Errorf("media %s: upload key: %w", r.ID, err)
	}
	m.UploadKey = up
	if r.ObjectKey != nil {
		obj, err := storage.ParseKey(*r.ObjectKey)
		if err != nil {
			return Media{}, fmt.Errorf("media %s: object key: %w", r.ID, err)
		}
		m.ObjectKey = obj
	}
	if r.ContentType != nil {
		m.ContentType = imaging.Type(*r.ContentType)
	}
	if r.Rejection != nil {
		m.Rejection = Rejection(*r.Rejection)
	}
	return m, nil
}

// queries returns the media queries on q (a transaction or the pool).
func queries(q mediadb.DBTX) *mediadb.Queries { return mediadb.New(q) }
