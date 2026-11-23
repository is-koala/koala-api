//go:build integration

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/address"
	"github.com/is-koala/koala-api/internal/authn"
	"github.com/is-koala/koala-api/internal/platform/config"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// Reference rows of db/referencedata the test template carries.
var (
	istanbulCity    = uuid.MustParse("e3eecc83-7fa1-4405-bc61-04dd36656ebd")
	kadikoyDistrict = uuid.MustParse("464757c9-1827-499d-8b58-562758181579")
	sisliDistrict   = uuid.MustParse("3f3a88b0-947d-4bd9-9fc0-1fe45f5c27fb")
	ankaraCity      = uuid.MustParse("453a1765-7a7f-4f30-b6e7-7922b5eb9883")
	cankayaDistrict = uuid.MustParse("e80b80eb-790a-4df4-b73a-405922f5a27e")
	adiyamanCity    = uuid.MustParse("94b50acc-2e84-4850-b2ff-b5a0e661bee6")
	adiyamanMerkez  = uuid.MustParse("95bc3fc8-1260-4c44-9498-b3ca06367da4")
)

// addressBody is an AddressInput in İstanbul with a coordinate.
func addressBody(district uuid.UUID, thoroughfare string) string {
	return `{"cityId":"` + istanbulCity.String() + `","districtId":"` + district.String() +
		`","thoroughfare":"` + thoroughfare + `","buildingNumber":"12","unitNumber":"7","neighborhood":"Caferağa",` +
		`"postalCode":"34710","providerPlaceId":"ChIJ-test","coordinates":{"latitude":40.987654321,"longitude":29.025432109}}`
}

// addressAnswer is the part of an Address the tests read.
type addressAnswer struct {
	ID          uuid.UUID `json:"id"`
	DisplayLine string    `json:"displayLine"`
	City        struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	} `json:"city"`
	District struct {
		ID uuid.UUID `json:"id"`
	} `json:"district"`
	Coordinates *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Precision string  `json:"precision"`
	} `json:"coordinates"`
}

type addressPage struct {
	Items      []addressAnswer `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

// primaryAddress is the profile's addressId.
func (a *authApp) primaryAddress(t *testing.T, bearer string) *string {
	t.Helper()
	rec := a.call(t, http.MethodGet, "/v1/me/profile", bearer, "")
	wantStatus(t, rec, http.StatusOK, "")
	return decodeBody[struct {
		AddressID *string `json:"addressId"`
	}](t, rec).AddressID
}

// createAddress posts an address with a fresh Idempotency-Key.
func (a *authApp) createAddress(t *testing.T, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return a.postKeyedAs(t, "/v1/me/addresses", "adr-"+uuid.NewString(), body, bearer)
}

// syncIntentAt is the user's latest profile sync intent time: the open
// intent's updated_at moves with every recorded change.
func (a *authApp) syncIntentAt(t *testing.T, user uuid.UUID) string {
	t.Helper()
	return a.text(t, "SELECT coalesce(max(updated_at)::text, '') FROM user_profile_sync_intents WHERE user_id = $1", user)
}

// The address operations over HTTP (ADR-0039): the stored coordinate is
// the one sent, at full precision, and the owner sees it exact (MED-009);
// the shown line is our components' (MED-008); the first address becomes
// primary, the second does not; the list is newest first; another user's
// address is the 404 of a missing one, byte for byte.
func TestAddressesOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "adres@example.com")
	user := a.userID(t, "adres@example.com")

	rec := a.createAddress(t, tok, addressBody(kadikoyDistrict, "Moda Caddesi"))
	wantStatus(t, rec, http.StatusCreated, "")
	first := decodeBody[addressAnswer](t, rec)
	if loc := rec.Header().Get("Location"); loc != "/v1/me/addresses/"+first.ID.String() {
		t.Errorf("Location = %q", loc)
	}
	if first.DisplayLine != "Moda Caddesi 12/7, Caferağa, Kadıköy, İstanbul" {
		t.Errorf("MED-008: displayLine = %q, want our components' line", first.DisplayLine)
	}
	if c := first.Coordinates; c == nil || c.Latitude != 40.987654321 || c.Longitude != 29.025432109 || c.Precision != "EXACT" {
		t.Errorf("MED-009: the owner sees %+v, want the exact coordinate", first.Coordinates)
	}
	// MED-009: the stored coordinate is the one sent, never rounded.
	if got := a.text(t, "SELECT latitude::text || ',' || longitude::text FROM active_addresses WHERE id = $1", first.ID); got != "40.987654321,29.025432109" {
		t.Errorf("MED-009: stored %s", got)
	}
	if p := a.primaryAddress(t, tok); p == nil || *p != first.ID.String() {
		t.Fatalf("the first address is not primary: %v", p)
	}

	rec = a.createAddress(t, tok, addressBody(sisliDistrict, "Halaskargazi Caddesi"))
	wantStatus(t, rec, http.StatusCreated, "")
	second := decodeBody[addressAnswer](t, rec)
	if p := a.primaryAddress(t, tok); p == nil || *p != first.ID.String() {
		t.Fatalf("the second address took the primary link: %v", p)
	}

	rec = a.call(t, http.MethodGet, "/v1/me/addresses", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	page := decodeBody[addressPage](t, rec)
	if len(page.Items) != 2 || page.Items[0].ID != second.ID || page.Items[1].ID != first.ID {
		t.Fatalf("list = %s, want newest first", rec.Body)
	}
	rec = a.call(t, http.MethodGet, "/v1/me/addresses?limit=1", tok, "")
	page = decodeBody[addressPage](t, rec)
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("page of one = %s", rec.Body)
	}
	rec = a.call(t, http.MethodGet, "/v1/me/addresses?limit=1&cursor="+*page.NextCursor, tok, "")
	if page = decodeBody[addressPage](t, rec); len(page.Items) != 1 || page.Items[0].ID != first.ID {
		t.Fatalf("next page = %s", rec.Body)
	}
	wantStatus(t, a.call(t, http.MethodGet, "/v1/me/addresses?cursor=bozuk", tok, ""), http.StatusBadRequest, string(authn.CodeCursorInvalid))

	rec = a.call(t, http.MethodPut, "/v1/me/addresses/"+second.ID.String(), tok, addressBody(kadikoyDistrict, "Bahariye Caddesi"))
	wantStatus(t, rec, http.StatusOK, "")
	if got := decodeBody[addressAnswer](t, rec); got.District.ID != kadikoyDistrict || !strings.HasPrefix(got.DisplayLine, "Bahariye Caddesi") {
		t.Errorf("replace = %s", rec.Body)
	}

	// Another user's address is a missing one, byte for byte.
	other := a.addSignedIn(t, "baskasi@example.com")
	missing := "/v1/me/addresses/" + uuid.NewString()
	for _, m := range []struct{ method, body string }{
		{http.MethodGet, ""}, {http.MethodPut, addressBody(kadikoyDistrict, "X")}, {http.MethodDelete, ""},
	} {
		theirs := a.call(t, m.method, "/v1/me/addresses/"+first.ID.String(), other, m.body)
		none := a.call(t, m.method, missing, other, m.body)
		wantStatus(t, theirs, http.StatusNotFound, string(address.CodeAddressNotFound))
		if strings.ReplaceAll(theirs.Body.String(), first.ID.String(), "") != strings.ReplaceAll(none.Body.String(), strings.TrimPrefix(missing, "/v1/me/addresses/"), "") {
			t.Errorf("%s: another user's %s, a missing one's %s", m.method, theirs.Body, none.Body)
		}
	}

	// Deleting the primary address clears the link in its transaction,
	// with the sync intent; the other address does not become primary.
	before := a.syncIntentAt(t, user)
	a.clock.Advance(time.Minute)
	wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/addresses/"+first.ID.String(), tok, ""), http.StatusNoContent, "")
	if p := a.primaryAddress(t, tok); p != nil {
		t.Errorf("the deleted primary address is still linked: %s", *p)
	}
	if after := a.syncIntentAt(t, user); after == before {
		t.Errorf("deleting the primary address recorded no sync intent (%s)", after)
	}
	wantStatus(t, a.call(t, http.MethodGet, "/v1/me/addresses/"+first.ID.String(), tok, ""), http.StatusNotFound, string(address.CodeAddressNotFound))
	// A non-primary address goes without touching the profile.
	before = a.syncIntentAt(t, user)
	a.clock.Advance(time.Minute)
	wantStatus(t, a.call(t, http.MethodDelete, "/v1/me/addresses/"+second.ID.String(), tok, ""), http.StatusNoContent, "")
	if after := a.syncIntentAt(t, user); after != before {
		t.Errorf("deleting a non-primary address recorded a sync intent")
	}
}

// A district of another city, a missing city and a malformed postal code
// are refused before anything is written; the cap holds at ten.
func TestAddressRulesOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "kural@example.com")
	// Çankaya is Ankara's, not İstanbul's.
	wantStatus(t, a.createAddress(t, tok, addressBody(cankayaDistrict, "X")), http.StatusNotFound, string(address.CodeReferenceNotFound))
	noCity := strings.Replace(addressBody(kadikoyDistrict, "X"), istanbulCity.String(), uuid.NewString(), 1)
	wantStatus(t, a.createAddress(t, tok, noCity), http.StatusNotFound, string(address.CodeReferenceNotFound))
	badPostal := strings.Replace(addressBody(kadikoyDistrict, "X"), `"34710"`, `"3471A"`, 1)
	wantStatus(t, a.createAddress(t, tok, badPostal), http.StatusBadRequest, "REQUEST_INVALID")
	// An address entered by hand does not depend on the service area:
	// Ankara is accepted (ADR-0039 Karar 3).
	ankara := strings.Replace(addressBody(cankayaDistrict, "Atatürk Bulvarı"), istanbulCity.String(), ankaraCity.String(), 1)
	wantStatus(t, a.createAddress(t, tok, ankara), http.StatusCreated, "")
	for i := 1; i < address.AddressesPerUser; i++ {
		wantStatus(t, a.createAddress(t, tok, addressBody(kadikoyDistrict, "Sokak "+strconv.Itoa(i))), http.StatusCreated, "")
	}
	wantStatus(t, a.createAddress(t, tok, addressBody(kadikoyDistrict, "Fazla")), http.StatusConflict, string(address.CodeAddressLimitReached))
}

// The profile's primary address link (ADR-0039 Karar 5): the caller's own
// address links, another user's and a missing one are ADDRESS_NOT_FOUND,
// null clears it, and the change records the sync intent. The link is no
// door past the #23 rules: a patch that also sends an e-mail still needs
// the password, and a refused patch links nothing.
func TestProfilePrimaryAddressLink(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "birincil@example.com")
	user := a.userID(t, "birincil@example.com")
	first := decodeBody[addressAnswer](t, a.createAddress(t, tok, addressBody(kadikoyDistrict, "Moda Caddesi")))
	second := decodeBody[addressAnswer](t, a.createAddress(t, tok, addressBody(sisliDistrict, "Rumeli Caddesi")))
	other := a.addSignedIn(t, "yabanci@example.com")
	theirs := decodeBody[addressAnswer](t, a.createAddress(t, other, addressBody(kadikoyDistrict, "Yabancı Sokak")))

	for _, id := range []string{theirs.ID.String(), uuid.NewString()} {
		wantStatus(t, a.patchProfile(t, tok, `{"addressId":"`+id+`"}`), http.StatusNotFound, string(address.CodeAddressNotFound))
	}
	if p := a.primaryAddress(t, tok); p == nil || *p != first.ID.String() {
		t.Fatalf("a refused link moved the primary address: %v", p)
	}

	before := a.syncIntentAt(t, user)
	a.clock.Advance(time.Minute)
	rec := a.patchProfile(t, tok, `{"addressId":"`+second.ID.String()+`"}`)
	wantStatus(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"addressId":"`+second.ID.String()+`"`) {
		t.Fatalf("patch answer = %s", rec.Body)
	}
	if after := a.syncIntentAt(t, user); after == before {
		t.Error("a primary address change recorded no sync intent (Java's UserManager.java:536)")
	}

	// The #23 rules: the e-mail needs the password, whatever else the
	// patch sends, and a wrong one links nothing.
	wantStatus(t, a.patchProfile(t, tok, `{"addressId":"`+first.ID.String()+`","email":"yeni@example.com"}`),
		http.StatusBadRequest, "REQUEST_INVALID")
	wantStatus(t, a.patchProfile(t, tok, `{"addressId":"`+first.ID.String()+`","email":"yeni@example.com","currentPassword":"yanlis-parola-1"}`),
		http.StatusForbidden, string(authn.CodeCurrentMismatch))
	if p := a.primaryAddress(t, tok); p == nil || *p != second.ID.String() {
		t.Fatalf("a refused patch moved the primary address: %v", p)
	}

	wantStatus(t, a.patchProfile(t, tok, `{"addressId":null}`), http.StatusOK, "")
	if p := a.primaryAddress(t, tok); p != nil {
		t.Errorf("null did not clear the link: %s", *p)
	}
	// Clearing it deletes no address.
	wantStatus(t, a.call(t, http.MethodGet, "/v1/me/addresses/"+second.ID.String(), tok, ""), http.StatusOK, "")
}

// The account's own deletion takes its addresses along, its primary one
// included: the restrict edge users.address_id does not hold it back,
// because the user is marked first (ADR-0039, the media links' pattern).
func TestAccountDeletionTakesItsAddresses(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "giden@example.com")
	id := a.userID(t, "giden@example.com")
	wantStatus(t, a.createAddress(t, tok, addressBody(kadikoyDistrict, "Moda Caddesi")), http.StatusCreated, "")
	cat := dbtest.SoftDeleteCatalog(t)
	err := a.db.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		_, err := softdelete.New(cat).Delete(ctx, q, softdelete.DeleteRequest{
			Table: "users", ID: id, Actor: softdelete.Actor{Kind: softdelete.ActorSystem},
			Reason: softdelete.ReasonAccountDeletion, At: a.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("the account's deletion: %v", err)
	}
	if n := a.text(t, "SELECT count(*)::text FROM active_addresses WHERE user_id = $1", id); n != "0" {
		t.Errorf("%s addresses outlived their user", n)
	}
}

// suggestions is the part of an AddressSuggestionList the tests read.
type suggestions struct {
	Items []struct {
		CandidateID   string `json:"candidateId"`
		PrimaryText   string `json:"primaryText"`
		SecondaryText string `json:"secondaryText"`
	} `json:"items"`
}

type resolvedAnswer struct {
	CityID          uuid.UUID  `json:"cityId"`
	DistrictID      *uuid.UUID `json:"districtId"`
	Neighborhood    *string    `json:"neighborhood"`
	Thoroughfare    *string    `json:"thoroughfare"`
	PostalCode      *string    `json:"postalCode"`
	ProviderPlaceID *string    `json:"providerPlaceId"`
	Latitude        *float64   `json:"latitude"`
	Longitude       *float64   `json:"longitude"`
}

// startSearch opens a search session and returns its suggestions path.
func (a *authApp) startSearch(t *testing.T, bearer string) string {
	t.Helper()
	rec := a.call(t, http.MethodPost, "/v1/me/address-search-sessions", bearer, "")
	wantStatus(t, rec, http.StatusCreated, "")
	loc := rec.Header().Get("Location")
	if !strings.HasSuffix(loc, "/suggestions") {
		t.Fatalf("Location = %q", loc)
	}
	return loc
}

// The search on the fake driver (ADR-0039): suggestions, a resolve into
// our components, the session's end; MED-008 (only the place id and our
// components come back, the provider's lines are not kept) and MED-010
// (another user's, an expired and an ended session are the same 404).
func TestAddressSearchOverHTTP(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "arayan@example.com")
	path := a.startSearch(t, tok)

	rec := a.call(t, http.MethodGet, path+"?query=Mo", tok, "")
	wantStatus(t, rec, http.StatusBadRequest, "REQUEST_INVALID") // the contract's minLength
	rec = a.call(t, http.MethodGet, path+"?query=%20%20Mo%20%20", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	if got := decodeBody[suggestions](t, rec); len(got.Items) != 0 {
		t.Errorf("a query of two letters reached the provider: %s", rec.Body)
	}
	rec = a.call(t, http.MethodGet, path+"?query=moda", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	got := decodeBody[suggestions](t, rec)
	if len(got.Items) != 1 || got.Items[0].CandidateID != "c_0" || got.Items[0].PrimaryText != "Moda Caddesi No:12" {
		t.Fatalf("suggestions = %s", rec.Body)
	}
	// MED-008: the session keeps the place ids, nothing of the lines.
	if kept := a.text(t, "SELECT candidates::text FROM address_search_sessions WHERE id = $1",
		strings.TrimSuffix(strings.TrimPrefix(path, "/v1/me/address-search-sessions/"), "/suggestions")); kept != "{canned-moda-caddesi-12}" {
		t.Errorf("MED-008: the session keeps %s", kept)
	}

	// MED-010: another user's session is a missing one.
	other := a.addSignedIn(t, "kapan@example.com")
	theirs := a.call(t, http.MethodGet, path+"?query=moda", other, "")
	missing := a.call(t, http.MethodGet, "/v1/me/address-search-sessions/"+uuid.NewString()+"/suggestions?query=moda", other, "")
	wantStatus(t, theirs, http.StatusNotFound, string(address.CodeSessionNotFound))
	if theirs.Body.String() != missing.Body.String() {
		t.Errorf("MED-010: another user's session %s, a missing one %s", theirs.Body, missing.Body)
	}
	wantStatus(t, a.call(t, http.MethodPost, strings.TrimSuffix(path, "/suggestions")+"/candidates/c_0/resolve", other, ""),
		http.StatusNotFound, string(address.CodeSessionNotFound))
	// A candidate the session never offered is the session's 404.
	wantStatus(t, a.call(t, http.MethodPost, strings.TrimSuffix(path, "/suggestions")+"/candidates/c_9/resolve", tok, ""),
		http.StatusNotFound, string(address.CodeSessionNotFound))

	rec = a.call(t, http.MethodPost, strings.TrimSuffix(path, "/suggestions")+"/candidates/c_0/resolve", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	r := decodeBody[resolvedAnswer](t, rec)
	if r.CityID != istanbulCity || r.DistrictID == nil || *r.DistrictID != kadikoyDistrict ||
		r.Neighborhood == nil || *r.Neighborhood != "Caferağa" || r.PostalCode == nil || *r.PostalCode != "34710" ||
		r.ProviderPlaceID == nil || *r.ProviderPlaceID != "canned-moda-caddesi-12" || r.Latitude == nil {
		t.Fatalf("resolved = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "Türkiye") || strings.Contains(rec.Body.String(), "Kadıköy/İstanbul") {
		t.Errorf("MED-008: the provider's text came back: %s", rec.Body)
	}
	// The resolve ended the session.
	ended := a.call(t, http.MethodGet, path+"?query=moda", tok, "")
	wantStatus(t, ended, http.StatusNotFound, string(address.CodeSessionNotFound))

	// MED-010: an expired session is a missing one too.
	path = a.startSearch(t, tok)
	a.clock.Advance(address.SessionTTL)
	expired := a.call(t, http.MethodGet, path+"?query=moda", tok, "")
	missingOwn := a.call(t, http.MethodGet, "/v1/me/address-search-sessions/"+uuid.NewString()+"/suggestions?query=moda", tok, "")
	wantStatus(t, expired, http.StatusNotFound, string(address.CodeSessionNotFound))
	if expired.Body.String() != missingOwn.Body.String() {
		t.Errorf("MED-010: an expired session %s, a missing one %s", expired.Body, missingOwn.Body)
	}

	// The resolved value posts as it is (the city's address).
	body := `{"cityId":"` + r.CityID.String() + `","districtId":"` + r.DistrictID.String() + `","neighborhood":"` + *r.Neighborhood +
		`","thoroughfare":"` + *r.Thoroughfare + `","postalCode":"` + *r.PostalCode + `","providerPlaceId":"` + *r.ProviderPlaceID +
		`","coordinates":{"latitude":` + strconv.FormatFloat(*r.Latitude, 'f', -1, 64) + `,"longitude":` + strconv.FormatFloat(*r.Longitude, 'f', -1, 64) + `}}`
	wantStatus(t, a.createAddress(t, tok, body), http.StatusCreated, "")
}

// The service area (ADR-0039 Karar 3) is the configured list of cities:
// a suggestion outside it is not offered under the restriction derived
// from the listed cities' bounds, and its resolve is 422; widening the
// list to two cities offers and resolves both, and the restriction is the
// union of their bounds. The reverse lookup answers the caller's pin and
// refuses a point outside the area.
func TestServiceAreaIsTheConfiguredCities(t *testing.T) {
	t.Parallel()

	only := newAuthApp(t, unlimitedGroups()) // the default: İstanbul
	tok := only.addSignedIn(t, "alan@example.com")
	path := only.startSearch(t, tok)
	if got := decodeBody[suggestions](t, only.call(t, http.MethodGet, path+"?query="+url.QueryEscape("Atatürk Bulvarı"), tok, "")); len(got.Items) != 0 {
		t.Errorf("MED-013: outside the area, offered %+v", got.Items)
	}
	rec := only.call(t, http.MethodPost, "/v1/me/address-lookups/reverse", tok, `{"latitude":39.9208,"longitude":32.8541}`)
	wantStatus(t, rec, http.StatusUnprocessableEntity, string(address.CodeOutsideServiceArea))
	rec = only.call(t, http.MethodPost, "/v1/me/address-lookups/reverse", tok, `{"latitude":40.98765,"longitude":29.02545}`)
	wantStatus(t, rec, http.StatusOK, "")
	if r := decodeBody[resolvedAnswer](t, rec); r.Latitude == nil || *r.Latitude != 40.98765 || *r.Longitude != 29.02545 ||
		r.DistrictID == nil || *r.DistrictID != kadikoyDistrict {
		t.Errorf("MED-013: reverse = %s, want the caller's pin and Kadıköy", rec.Body)
	}
	rec = only.call(t, http.MethodPost, "/v1/me/address-lookups/reverse", tok, `{"latitude":0,"longitude":0}`)
	wantStatus(t, rec, http.StatusUnprocessableEntity, string(address.CodeOutsideServiceArea))

	two := newAuthAppWith(t, unlimitedGroups(), config.Maps{
		Driver: config.MapsDriverFake, ServiceAreaCities: []uuid.UUID{istanbulCity, ankaraCity},
	})
	tok = two.addSignedIn(t, "iki@example.com")
	path = two.startSearch(t, tok)
	got := decodeBody[suggestions](t, two.call(t, http.MethodGet, path+"?query="+url.QueryEscape("Atatürk Bulvarı"), tok, ""))
	if len(got.Items) != 1 || !strings.Contains(got.Items[0].SecondaryText, "Ankara") {
		t.Fatalf("two cities: offered %+v, want Ankara's place", got.Items)
	}
	rec = two.call(t, http.MethodPost, strings.TrimSuffix(path, "/suggestions")+"/candidates/"+got.Items[0].CandidateID+"/resolve", tok, "")
	wantStatus(t, rec, http.StatusOK, "")
	if r := decodeBody[resolvedAnswer](t, rec); r.CityID != ankaraCity || r.DistrictID == nil || *r.DistrictID != cankayaDistrict {
		t.Errorf("resolved = %s, want Çankaya, Ankara", rec.Body)
	}
	// Both cities' bounds were looked up once, as the service's own units.
	if n := two.text(t, "SELECT count(*)::text FROM address_lookups WHERE unit = 'BOUNDS'"); n != "2" {
		t.Errorf("bounds lookups = %s, want one per city", n)
	}
}

// The provider's "Merkez" is the city's MERKEZ district (ADR-0039 Karar 2).
func TestCentralDistrictMatches(t *testing.T) {
	t.Parallel()
	a := newAuthAppWith(t, unlimitedGroups(), config.Maps{
		Driver: config.MapsDriverFake, ServiceAreaCities: []uuid.UUID{adiyamanCity},
	})
	tok := a.addSignedIn(t, "merkez@example.com")
	rec := a.call(t, http.MethodPost, "/v1/me/address-lookups/reverse", tok, `{"latitude":37.7648,"longitude":38.2786}`)
	wantStatus(t, rec, http.StatusOK, "")
	if r := decodeBody[resolvedAnswer](t, rec); r.CityID != adiyamanCity || r.DistrictID == nil || *r.DistrictID != adiyamanMerkez {
		t.Errorf("reverse = %s, want MERKEZ of Adıyaman", rec.Body)
	}
}

// The lookup budget (ADR-0039 Karar 7) over HTTP: the caller's daily
// budget, then the client address's hourly budget, then the service's
// cap, counted on billed units. A caller over their own budget spends
// nothing of the address's: a second caller behind the same address still
// gets its share (the order's red side: were the address counted first,
// the refused requests would have taken its units). The cap answers 502,
// whoever asks.
func TestLookupBudgetOrder(t *testing.T) {
	t.Parallel()
	a := newAuthAppWith(t, unlimitedGroups(), config.Maps{
		Driver: config.MapsDriverFake, LookupsPerUserDay: 2, LookupsPerIPHour: 4, LookupsPerDay: 6,
	})
	start := func(addr, bearer string) int {
		return a.callFrom(t, addr, http.MethodPost, "/v1/me/address-search-sessions", bearer, "").Code
	}
	u1 := a.addSignedIn(t, "butce1@example.com")
	u2 := a.addSignedIn(t, "butce2@example.com")
	u3 := a.addSignedIn(t, "butce3@example.com")
	u4 := a.addSignedIn(t, "butce4@example.com")
	const nat, home = "198.51.100.7", "203.0.113.9"

	for i := range 2 {
		if c := start(nat, u1); c != http.StatusCreated {
			t.Fatalf("u1 start %d = %d", i, c)
		}
	}
	for range 5 {
		rec := a.callFrom(t, nat, http.MethodPost, "/v1/me/address-search-sessions", u1, "")
		wantStatus(t, rec, http.StatusTooManyRequests, string(address.CodeLookupLimitReached))
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("a spent user budget answered without Retry-After")
		}
	}
	// u1's refusals took nothing of the address: u2 has the other two.
	for i := range 2 {
		if c := start(nat, u2); c != http.StatusCreated {
			t.Fatalf("u2 start %d = %d: a refused caller spent the address's budget", i, c)
		}
	}
	// The address's hourly budget is spent now, for anyone behind it.
	wantStatus(t, a.callFrom(t, nat, http.MethodPost, "/v1/me/address-search-sessions", u3, ""),
		http.StatusTooManyRequests, string(address.CodeLookupLimitReached))
	// From another address u3 goes on until the service's cap (6).
	for i := range 2 {
		if c := start(home, u3); c != http.StatusCreated {
			t.Fatalf("u3 start %d = %d", i, c)
		}
	}
	wantStatus(t, a.callFrom(t, home, http.MethodPost, "/v1/me/address-search-sessions", u4, ""),
		http.StatusBadGateway, string(address.CodeProviderUnavailable))
	if n := a.text(t, "SELECT count(*)::text FROM address_lookups"); n != "6" {
		t.Errorf("ledger rows = %s, want the six reserved units", n)
	}
	// The cap counts every unit alike: a reverse lookup is refused too.
	wantStatus(t, a.callFrom(t, home, http.MethodPost, "/v1/me/address-lookups/reverse", u4, `{"latitude":40.98765,"longitude":29.02545}`),
		http.StatusBadGateway, string(address.CodeProviderUnavailable))
	// A day later the window has moved on (a fresh sign-in: the access
	// tokens of a day ago have expired).
	a.clock.Advance(address.UserWindow + time.Second)
	if c := start(home, a.addSignedIn(t, "butce5@example.com")); c != http.StatusCreated {
		t.Errorf("after the window: %d", c)
	}
}

// A session asks the provider at most SuggestionRequestsPerSession times:
// the bound of what an abandoned session costs (ADR-0039 Karar 7).
func TestSessionSuggestionBound(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "yazan@example.com")
	path := a.startSearch(t, tok)
	for i := range address.SuggestionRequestsPerSession {
		wantStatus(t, a.call(t, http.MethodGet, path+"?query=moda"+strconv.Itoa(i), tok, ""), http.StatusOK, "")
	}
	wantStatus(t, a.call(t, http.MethodGet, path+"?query=moda", tok, ""), http.StatusTooManyRequests, string(address.CodeLookupLimitReached))
}

// Without a provider (local and test without a key) the search answers
// 502 and spends nothing.
func TestSearchWithoutProvider(t *testing.T) {
	t.Parallel()
	a := newAuthAppWith(t, unlimitedGroups(), config.Maps{Driver: config.MapsDriverGoogle})
	tok := a.addSignedIn(t, "anahtarsiz@example.com")
	wantStatus(t, a.call(t, http.MethodPost, "/v1/me/address-search-sessions", tok, ""), http.StatusBadGateway, string(address.CodeProviderUnavailable))
	wantStatus(t, a.call(t, http.MethodPost, "/v1/me/address-lookups/reverse", tok, `{"latitude":40.98,"longitude":29.02}`),
		http.StatusBadGateway, string(address.CodeProviderUnavailable))
	if n := a.text(t, "SELECT count(*)::text FROM address_lookups"); n != "0" {
		t.Errorf("MED-013: ledger rows = %s, want none", n)
	}
}

// The cap of AddressesPerUser holds under concurrency: the count and the
// insert happen under the caller's row lock (lockOwner, FOR UPDATE), so
// racing creates cannot pass it together. Without the lock, more than one
// of the racers would land.
func TestAddressLimitHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	a := newAuthApp(t, unlimitedGroups())
	tok := a.addSignedIn(t, "yaris@example.com")
	for i := range address.AddressesPerUser - 1 {
		wantStatus(t, a.createAddress(t, tok, addressBody(kadikoyDistrict, "Sokak "+strconv.Itoa(i))), http.StatusCreated, "")
	}
	const racers = 6
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			codes[i] = a.createAddress(t, tok, addressBody(kadikoyDistrict, "Yarış "+strconv.Itoa(i))).Code
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Errorf("a racing create answered %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("%d racing creates passed the cap, want 1 (%v)", created, codes)
	}
	if n := a.text(t, "SELECT count(*)::text FROM active_addresses a JOIN active_users u ON u.id = a.user_id WHERE u.email = $1",
		"yaris@example.com"); n != strconv.Itoa(address.AddressesPerUser) {
		t.Fatalf("%s addresses, want %d", n, address.AddressesPerUser)
	}
}

// The service's cap holds under concurrency: every reservation counts and
// spends under one advisory lock (LockLookups), so racing starts of
// different users from different addresses cannot pass it together.
func TestLookupCapHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	const limit = 3
	a := newAuthAppWith(t, unlimitedGroups(), config.Maps{Driver: config.MapsDriverFake, LookupsPerDay: limit})
	const racers = 8
	toks := make([]string, racers)
	for i := range racers {
		toks[i] = a.addSignedIn(t, "tavan"+strconv.Itoa(i)+"@example.com")
	}
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			codes[i] = a.callFrom(t, "198.51.100."+strconv.Itoa(10+i), http.MethodPost, "/v1/me/address-search-sessions", toks[i], "").Code
		})
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusBadGateway:
		default:
			t.Errorf("a racing start answered %d", c)
		}
	}
	if created != limit {
		t.Fatalf("%d racing starts passed, want the cap %d (%v)", created, limit, codes)
	}
}
