package address

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is-koala/koala-api/internal/address/addressdb"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/httpx"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
	"github.com/is-koala/koala-api/internal/profilesync"
)

// table is the soft-deleted table of the addresses.
const table = "addresses"

// userActive is the status of a user who may change their addresses.
const userActive = "ACTIVE"

// Service serves the caller's addresses, the profile's primary address
// link and the address search.
type Service struct {
	db       *db.DB
	reads    *addressdb.Queries
	deleter  *softdelete.Deleter
	clock    clock.Clock
	search   *search
	logger   *slog.Logger
	metrics  *Metrics
	provider provider
}

// Config is what the service needs besides its dependencies.
type Config struct {
	// Places matches the provider's names to the reference rows.
	Places *Places
	// Area is the service area of the search.
	Area ServiceArea
	// Budget is the lookup budget; a zero bound takes its default.
	Budget Budget
}

// NewService returns the service on database. p is the map provider; nil
// when none is configured (local and test without a key), and then every
// search answers ADDRESS_PROVIDER_UNAVAILABLE.
func NewService(database *db.DB, p provider, deleter *softdelete.Deleter, clk clock.Clock, cfg Config,
	logger *slog.Logger, metrics *Metrics,
) (*Service, error) {
	if database == nil || deleter == nil || clk == nil || cfg.Places == nil || logger == nil || metrics == nil {
		return nil, errors.New("address: the service needs a database, a deleter, a clock, the places, a logger and metrics")
	}
	if len(cfg.Area.Cities()) == 0 {
		return nil, errors.New("address: the service needs a service area")
	}
	s := &Service{
		db: database, reads: addressdb.New(database.Pool()), deleter: deleter, clock: clk, logger: logger,
		metrics: metrics, provider: p,
	}
	s.search = newSearch(s, cfg)
	return s, nil
}

// sessionInactive is the answer to a caller whose user is gone or
// suspended: the session check's own code (ADR-0035 Karar 2).
func sessionInactive() error {
	return apperr.New(apperr.KindUnauthenticated, authn.CodeSessionInactive)
}

func addressNotFound() error {
	return apperr.New(apperr.KindNotFound, CodeAddressNotFound)
}

// origin is where a profile change came from, for its sync intent.
func origin(ctx context.Context, p authn.UserPrincipal) profilesync.Origin {
	return profilesync.Origin{RequestID: httpx.RequestIDFrom(ctx), SessionID: p.SessionID, DeviceKey: p.DeviceKey}
}

// lockOwner locks the caller's active row FOR UPDATE and returns its
// primary address; a user gone or suspended is the session's 401.
func lockOwner(ctx context.Context, q *addressdb.Queries, userID uuid.UUID) (uuid.NullUUID, error) {
	row, err := q.LockOwner(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.Status != userActive) {
		return uuid.NullUUID{}, sessionInactive()
	}
	if err != nil {
		return uuid.NullUUID{}, fmt.Errorf("address: lock the user: %w", err)
	}
	return row.AddressID, nil
}

// setPrimary is the one writer of the profile's primary address link
// (users.address_id, ADR-0039 Karar 5): it writes the link and the
// profile sync intent of the change in q's transaction, Java's
// UserManager.java:536 publish (ADR-0036 Karar 8). The caller holds the
// user's row and, for an address, the address's.
func setPrimary(ctx context.Context, q *addressdb.Queries, tx db.Querier, userID uuid.UUID, id uuid.NullUUID,
	o profilesync.Origin, now time.Time,
) error {
	if err := q.SetPrimaryAddress(ctx, addressdb.SetPrimaryAddressParams{ID: userID, AddressID: id}); err != nil {
		return fmt.Errorf("address: write the primary address: %w", err)
	}
	if err := profilesync.Record(ctx, tx, userID, o, now); err != nil {
		return fmt.Errorf("address: record the profile change: %w", err)
	}
	return nil
}

// LinkPrimary points the profile of the caller at one of their addresses,
// or clears the link (id nil), in the profile patch's transaction
// (ADR-0039 Karar 5). The patch holds the caller's row (users.LockProfile)
// and calls this only when the link changes. Another user's address and a
// deleted one are ADDRESS_NOT_FOUND. The address's row is locked FOR
// SHARE: a concurrent deletion of it waits and then finds the link (the
// restrict edge), or deleted it first and the address is not found.
func (s *Service) LinkPrimary(ctx context.Context, tx db.Querier, p authn.UserPrincipal, id *uuid.UUID, now time.Time) error {
	q := addressdb.New(tx)
	link := uuid.NullUUID{}
	if id != nil {
		_, err := q.OwnAddress(ctx, addressdb.OwnAddressParams{ID: *id, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return addressNotFound()
		}
		if err != nil {
			return fmt.Errorf("address: lock the address: %w", err)
		}
		link = uuid.NullUUID{UUID: *id, Valid: true}
	}
	return setPrimary(ctx, q, tx, p.UserID, link, origin(ctx, p), now)
}

// Create adds an address for the caller. commit runs last in the
// transaction with the new address: the handler stores its answer for the
// Idempotency-Key there (ADR-0034 Karar 4).
//
// Lock order: the caller's row FOR UPDATE first (lockOwner), so concurrent
// creates cannot pass the cap (AddressesPerUser) together and the primary
// link is written under the lock the profile patch takes; then
// softdelete.LockActiveParent on the same row, which the update lock
// already covers. The first address of a user without a primary one
// becomes primary (ADR-0039 Karar 5).
func (s *Service) Create(ctx context.Context, p authn.UserPrincipal, in Input,
	commit func(ctx context.Context, q db.Querier, a Address) error,
) (Address, error) {
	in = in.normalized()
	id, now := uuid.Must(uuid.NewV7()), s.clock.Now()
	var out Address
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		primary, err := lockOwner(ctx, q, p.UserID)
		if err != nil {
			return err
		}
		err = softdelete.LockActiveParent(ctx, tx, "users", p.UserID)
		if errors.Is(err, softdelete.ErrNotActive) {
			return sessionInactive()
		}
		if err != nil {
			return fmt.Errorf("address: lock the user: %w", err)
		}
		n, err := q.CountAddresses(ctx, p.UserID)
		if err != nil {
			return fmt.Errorf("address: count the addresses: %w", err)
		}
		if n >= AddressesPerUser {
			return apperr.New(apperr.KindConflict, CodeAddressLimitReached)
		}
		if err := checkReferences(ctx, q, in, uuid.Nil, uuid.Nil); err != nil {
			return err
		}
		lat, lng := coordinates(in.Point)
		err = q.InsertAddress(ctx, addressdb.InsertAddressParams{
			ID: id, UserID: p.UserID, CityID: in.CityID, DistrictID: in.DistrictID,
			Neighborhood: in.Neighborhood, Thoroughfare: in.Thoroughfare, BuildingNumber: in.BuildingNumber,
			BuildingName: in.BuildingName, Floor: in.Floor, UnitNumber: in.UnitNumber, PostalCode: in.PostalCode,
			AdditionalDirections: in.AdditionalDirections, ProviderPlaceID: in.ProviderPlaceID,
			Latitude: lat, Longitude: lng, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("address: insert the address: %w", err)
		}
		if !primary.Valid {
			if err := setPrimary(ctx, q, tx, p.UserID, uuid.NullUUID{UUID: id, Valid: true}, origin(ctx, p), now); err != nil {
				return err
			}
		}
		a, err := load(ctx, q, id, p.UserID)
		if err != nil {
			return err
		}
		out = a
		return commit(ctx, tx, a)
	})
	if err != nil {
		return Address{}, fmt.Errorf("address: create an address: %w", err)
	}
	return out, nil
}

// Replace rewrites one of the caller's addresses. The address is found
// first: another user's is the 404 of a missing one whatever the body
// says, so no reference check can tell it apart. A retired city or
// district the address names already may stay.
func (s *Service) Replace(ctx context.Context, p authn.UserPrincipal, id uuid.UUID, in Input) (Address, error) {
	in = in.normalized()
	now := s.clock.Now()
	var out Address
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		cur, err := q.LockAddress(ctx, addressdb.LockAddressParams{ID: id, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return addressNotFound()
		}
		if err != nil {
			return fmt.Errorf("address: lock the address: %w", err)
		}
		if err := checkReferences(ctx, q, in, cur.CityID, cur.DistrictID); err != nil {
			return err
		}
		lat, lng := coordinates(in.Point)
		err = q.ReplaceAddress(ctx, addressdb.ReplaceAddressParams{
			ID: id, UserID: p.UserID, CityID: in.CityID, DistrictID: in.DistrictID,
			Neighborhood: in.Neighborhood, Thoroughfare: in.Thoroughfare, BuildingNumber: in.BuildingNumber,
			BuildingName: in.BuildingName, Floor: in.Floor, UnitNumber: in.UnitNumber, PostalCode: in.PostalCode,
			AdditionalDirections: in.AdditionalDirections, ProviderPlaceID: in.ProviderPlaceID,
			Latitude: lat, Longitude: lng, UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("address: replace the address: %w", err)
		}
		a, err := load(ctx, q, id, p.UserID)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	if err != nil {
		return Address{}, fmt.Errorf("address: replace an address: %w", err)
	}
	return out, nil
}

// Delete soft-deletes one of the caller's addresses. Another user's, a
// deleted and a missing one are the same 404. The primary address's link
// is cleared first, in the same transaction, with its sync intent; no
// other address becomes primary (ADR-0039 Karar 5). Lock order is the
// profile patch's: the caller's row, then the address.
func (s *Service) Delete(ctx context.Context, p authn.UserPrincipal, id uuid.UUID) error {
	now := s.clock.Now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx db.Querier) error {
		q := addressdb.New(tx)
		primary, err := lockOwner(ctx, q, p.UserID)
		if err != nil {
			return err
		}
		_, err = q.LockAddress(ctx, addressdb.LockAddressParams{ID: id, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return addressNotFound()
		}
		if err != nil {
			return fmt.Errorf("address: lock the address: %w", err)
		}
		if primary.Valid && primary.UUID == id {
			if err := setPrimary(ctx, q, tx, p.UserID, uuid.NullUUID{}, origin(ctx, p), now); err != nil {
				return err
			}
		}
		_, err = s.deleter.Delete(ctx, tx, softdelete.DeleteRequest{
			Table: table, ID: id,
			Actor:  softdelete.Actor{Kind: softdelete.ActorUser, ID: p.UserID},
			Reason: softdelete.ReasonUserRequest, At: now,
		})
		if errors.Is(err, softdelete.ErrNotActive) {
			return addressNotFound()
		}
		if err != nil {
			return fmt.Errorf("address: delete the address: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("address: delete an address: %w", err)
	}
	return nil
}

// Get returns one of the caller's addresses.
func (s *Service) Get(ctx context.Context, p authn.UserPrincipal, id uuid.UUID) (Address, error) {
	a, err := load(ctx, s.reads, id, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Address{}, addressNotFound()
	}
	if err != nil {
		return Address{}, err
	}
	return a, nil
}

// Page is one page of the caller's addresses and the cursors around it.
type Page struct {
	Items      []Address
	NextCursor *string
	PrevCursor *string
}

// List returns a page of the caller's addresses, the newest first. A user
// has at most AddressesPerUser, so all are read and the page is cut here
// on the same keyset (the id) a query would use. A malformed cursor is
// CURSOR_INVALID.
func (s *Service) List(ctx context.Context, p authn.UserPrincipal, limit int, cursor string) (Page, error) {
	var at *listCursor
	if cursor != "" {
		c, err := decodeCursor(cursor)
		if err != nil {
			return Page{}, err
		}
		at = &c
	}
	rows, err := s.reads.ListAddresses(ctx, p.UserID)
	if err != nil {
		return Page{}, fmt.Errorf("address: list the addresses: %w", err)
	}
	all := make([]Address, 0, len(rows))
	for i := range rows {
		all = append(all, addressOf(addressdb.GetAddressRow(rows[i])))
	}
	return pageOf(all, limit, at), nil
}

// pageOf cuts one page of limit addresses out of all, in list order, after
// (older) or before (newer) the cursor's position, which need not be an
// address that still exists.
func pageOf(all []Address, limit int, at *listCursor) Page {
	start, end := 0, min(len(all), limit)
	if at != nil {
		before, after := 0, 0
		for i := range all {
			if precedes(all[i].ID, at.id) {
				before++
			}
			if precedes(at.id, all[i].ID) {
				after++
			}
		}
		if at.newer {
			end = before
			start = max(0, end-limit)
		} else {
			start = len(all) - after
			end = min(len(all), start+limit)
		}
	}
	page := Page{Items: all[start:end]}
	if len(page.Items) == 0 {
		return page
	}
	if start > 0 {
		page.PrevCursor = listCursor{newer: true, id: page.Items[0].ID}.encode()
	}
	if end < len(all) {
		page.NextCursor = listCursor{id: page.Items[len(page.Items)-1].ID}.encode()
	}
	return page
}

// The keyset cursor of the list: its direction and the id, base64url
// under a version prefix; opaque to the client.
const (
	cursorPrefix = "adr1"
	cursorOlder  = "n"
	cursorNewer  = "p"
)

type listCursor struct {
	newer bool
	id    uuid.UUID
}

func (c listCursor) encode() *string {
	dir := cursorOlder
	if c.newer {
		dir = cursorNewer
	}
	s := base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + "|" + dir + "|" + c.id.String()))
	return &s
}

func decodeCursor(s string) (listCursor, error) {
	invalid := apperr.New(apperr.KindInvalid, authn.CodeCursorInvalid)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return listCursor{}, invalid
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != cursorPrefix || (parts[1] != cursorOlder && parts[1] != cursorNewer) {
		return listCursor{}, invalid
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return listCursor{}, invalid
	}
	return listCursor{newer: parts[1] == cursorNewer, id: id}, nil
}

// checkReferences finds the city and the district under it. A retired row
// cannot be chosen anew, but an address keeps the one it names already
// (M4, ADR-0036's rule). A district of another city is
// REFERENCE_ITEM_NOT_FOUND, as a missing one is; the composite foreign key
// holds the same in the database.
func checkReferences(ctx context.Context, q *addressdb.Queries, in Input, keptCity, keptDistrict uuid.UUID) error {
	_, err := q.FindCity(ctx, addressdb.FindCityParams{ID: in.CityID, AllowRetired: keptCity != uuid.Nil && keptCity == in.CityID})
	if err != nil {
		return referenceError(err)
	}
	_, err = q.FindDistrictOf(ctx, addressdb.FindDistrictOfParams{
		ID: in.DistrictID, CityID: in.CityID, AllowRetired: keptDistrict != uuid.Nil && keptDistrict == in.DistrictID,
	})
	if err != nil {
		return referenceError(err)
	}
	return nil
}

func referenceError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(apperr.KindNotFound, CodeReferenceNotFound)
	}
	return fmt.Errorf("address: find the reference row: %w", err)
}

func coordinates(p *Point) (lat, lng *float64) {
	if p == nil {
		return nil, nil
	}
	la, ln := p.Lat, p.Lng
	return &la, &ln
}

// load is one of the user's addresses; pgx.ErrNoRows when there is none.
func load(ctx context.Context, q *addressdb.Queries, id, userID uuid.UUID) (Address, error) {
	row, err := q.GetAddress(ctx, addressdb.GetAddressParams{ID: id, UserID: userID})
	if err != nil {
		return Address{}, fmt.Errorf("address: load the address: %w", err)
	}
	return addressOf(row), nil
}

func addressOf(r addressdb.GetAddressRow) Address {
	a := Address{
		ID:       r.ID,
		City:     Reference{ID: r.CityID, Name: r.CityName},
		District: Reference{ID: r.DistrictID, Name: r.DistrictName},
		Lines: Lines{
			Neighborhood: r.Neighborhood, Thoroughfare: r.Thoroughfare, BuildingNumber: r.BuildingNumber,
			BuildingName: r.BuildingName, Floor: r.Floor, UnitNumber: r.UnitNumber, PostalCode: r.PostalCode,
			AdditionalDirections: r.AdditionalDirections,
		},
		ProviderPlaceID: r.ProviderPlaceID,
		CreatedAt:       r.CreatedAt,
	}
	if r.Latitude != nil && r.Longitude != nil {
		a.Point = &Point{Lat: *r.Latitude, Lng: *r.Longitude}
	}
	return a
}
