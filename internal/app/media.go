package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/internal/media"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/jobs"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/platform/storage"
)

// mediaSweepInterval is how often the media sweep runs. Unfinished
// uploads are kept a day (ADR-0035 Karar 4), so hourly keeps them at about
// a day and an hour.
const mediaSweepInterval = time.Hour

// wireMedia builds the media module (ADR-0035): the object store from
// cfg's KOALA_OBJECT_* variables, an insert-only River client for the jobs
// the operations queue in their transactions, and the soft-delete catalog
// the deletions follow.
//
// Without an object store (local and test only; config.ServeStorageErrors
// refuses it elsewhere) the module still serves: a start answers 500 and
// names the missing configuration in the log, a read answers without an
// address.
func wireMedia(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	reg prometheus.Registerer,
) (*media.Service, *media.HTTP, error) {
	var store *storage.Store
	if cfg.Object.Configured() {
		s, err := storage.New(storage.Config{
			Endpoint: cfg.Object.Endpoint, Region: cfg.Object.Region, Bucket: cfg.Object.Bucket,
			AccessKeyID: cfg.Object.AccessKeyID, SecretAccessKey: cfg.Object.SecretKey.Reveal(),
			PathStyle: cfg.Object.PathStyle, PublicBaseURL: cfg.Object.PublicBaseURL,
		}, clk, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("wire media: %w", err)
		}
		store = s
	} else {
		logger.WarnContext(ctx, "object store not configured; media uploads answer 500")
	}
	queue, err := jobs.NewClient(database.Pool(), jobs.Config{Logger: logger})
	if err != nil {
		return nil, nil, fmt.Errorf("wire media: %w", err)
	}
	catalog, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		return nil, nil, fmt.Errorf("wire media: %w", err)
	}
	service, err := media.NewService(database, queue, store, softdelete.New(catalog), clk, logger, reg)
	if err != nil {
		return nil, nil, fmt.Errorf("wire media: %w", err)
	}
	return service, media.NewHTTP(service), nil
}
