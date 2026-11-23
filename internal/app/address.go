package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"

	dbsrc "github.com/is-koala/koala-api/db"
	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/address"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/integrations/googlemaps"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// wireAddress builds the address module (ADR-0039) with the map provider
// cfg's driver and key name: the Google client, the canned driver, or none
// (local and test without a key: the search answers
// ADDRESS_PROVIDER_UNAVAILABLE and the start says why;
// config.ServeMapsErrors refuses that in staging and production).
func wireAddress(ctx context.Context, cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	reg prometheus.Registerer,
) (*address.Service, *address.HTTP, error) {
	switch {
	case cfg.Maps.Driver == config.MapsDriverFake:
		logger.WarnContext(ctx, "map provider is the fake driver; address search answers canned places")
		return wireAddressWith(cfg, database, clk, logger, reg, googlemaps.Canned{})
	case cfg.Maps.APIKey != "":
		client, err := googlemaps.New(googlemaps.Config{APIKey: cfg.Maps.APIKey.Reveal()})
		if err != nil {
			return nil, nil, fmt.Errorf("wire address: %w", err)
		}
		return wireAddressWith(cfg, database, clk, logger, reg, client)
	default:
		logger.WarnContext(ctx, "map provider not configured; address search answers 502")
		return wireAddressWith(cfg, database, clk, logger, reg, nil)
	}
}

// wireAddressWith builds the address module on provider p (nil: none):
// the service area checked against the reference data the binary carries
// (an unknown city stops the server) and the lookup budget.
func wireAddressWith(cfg config.Config, database *db.DB, clk clock.Clock, logger *slog.Logger,
	reg prometheus.Registerer, p address.Provider,
) (*address.Service, *address.HTTP, error) {
	data, err := referencedata.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("wire address: %w", err)
	}
	places, err := address.NewPlaces(data)
	if err != nil {
		return nil, nil, fmt.Errorf("wire address: %w", err)
	}
	area, err := address.NewServiceArea(places, cfg.Maps.ServiceArea())
	if err != nil {
		return nil, nil, fmt.Errorf("wire address: %s: %w", config.VarServiceAreaCities, err)
	}
	catalog, err := softdelete.ParseCatalog(dbsrc.SoftDeleteCatalog())
	if err != nil {
		return nil, nil, fmt.Errorf("wire address: %w", err)
	}
	metrics, err := address.NewMetrics(reg)
	if err != nil {
		return nil, nil, fmt.Errorf("wire address: %w", err)
	}
	budget := address.Budget{
		PerUser: cfg.Maps.LookupsPerUserDay, PerClient: cfg.Maps.LookupsPerIPHour, PerDay: cfg.Maps.LookupsPerDay,
	}
	svc, err := address.NewService(database, p, softdelete.New(catalog), clk,
		address.Config{Places: places, Area: area, Budget: budget}, logger, metrics)
	if err != nil {
		return nil, nil, fmt.Errorf("wire address: %w", err)
	}
	return svc, address.NewHTTP(svc), nil
}

// addressPurgeInterval is how often the address purge runs: sessions live
// 15 minutes and the ledger a day, so hourly keeps both small.
const addressPurgeInterval = time.Hour

// addressPurgeArgs is the periodic purge of the address module's M2
// tables: expired search sessions and the lookup ledger past its longest
// window (ADR-0039).
type addressPurgeArgs struct{}

// Kind is the job kind in River's tables.
func (addressPurgeArgs) Kind() string { return "address_purge" }

// addressPurgeWorker runs the purge as koala_app.
type addressPurgeWorker struct {
	river.WorkerDefaults[addressPurgeArgs]

	service *address.Service
	logger  *slog.Logger
}

// Work purges both tables and logs how many rows it removed.
func (w *addressPurgeWorker) Work(ctx context.Context, _ *river.Job[addressPurgeArgs]) error {
	sessions, lookups, err := w.service.Purge(ctx)
	if err != nil {
		return fmt.Errorf("address purge: %w", err)
	}
	w.logger.InfoContext(ctx, "address purge",
		slog.Int64("address_search_sessions", sessions), slog.Int64("address_lookups", lookups))
	return nil
}

// The address operations, forwarded to address.HTTP: its type is named
// HTTP like authn's, so it cannot be embedded next to it.

func (s strictServer) ListMyAddresses(ctx context.Context, req apiv1.ListMyAddressesRequestObject) (apiv1.ListMyAddressesResponseObject, error) {
	return s.address.ListMyAddresses(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) CreateMyAddress(ctx context.Context, req apiv1.CreateMyAddressRequestObject) (apiv1.CreateMyAddressResponseObject, error) {
	return s.address.CreateMyAddress(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) GetMyAddress(ctx context.Context, req apiv1.GetMyAddressRequestObject) (apiv1.GetMyAddressResponseObject, error) {
	return s.address.GetMyAddress(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ReplaceMyAddress(ctx context.Context, req apiv1.ReplaceMyAddressRequestObject) (apiv1.ReplaceMyAddressResponseObject, error) {
	return s.address.ReplaceMyAddress(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) DeleteMyAddress(ctx context.Context, req apiv1.DeleteMyAddressRequestObject) (apiv1.DeleteMyAddressResponseObject, error) {
	return s.address.DeleteMyAddress(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) StartAddressSearchSession(ctx context.Context, req apiv1.StartAddressSearchSessionRequestObject) (apiv1.StartAddressSearchSessionResponseObject, error) {
	return s.address.StartAddressSearchSession(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ListAddressSuggestions(ctx context.Context, req apiv1.ListAddressSuggestionsRequestObject) (apiv1.ListAddressSuggestionsResponseObject, error) {
	return s.address.ListAddressSuggestions(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ResolveAddressCandidate(ctx context.Context, req apiv1.ResolveAddressCandidateRequestObject) (apiv1.ResolveAddressCandidateResponseObject, error) {
	return s.address.ResolveAddressCandidate(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}

func (s strictServer) ReverseLookupAddress(ctx context.Context, req apiv1.ReverseLookupAddressRequestObject) (apiv1.ReverseLookupAddressResponseObject, error) {
	return s.address.ReverseLookupAddress(ctx, req) //nolint:wrapcheck // forwarding: the handler's error is the response
}
