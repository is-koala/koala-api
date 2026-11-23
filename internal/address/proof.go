package address

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

// The provider's proof (ADR-0039 Karar 10): a resolve and a reverse
// geocode answer, with our components, a short-lived token the server
// signs. An address is recorded as the provider's (AUTOCOMPLETE or
// REVERSE_GEOCODE, its place id, a PROVIDER coordinate) only with a valid
// one: the client declares nothing. It is Java's candidateToken without
// the server-side store: the claims travel in the token, an HMAC under a
// key derived from the login pepper keeps them honest.
//
// What it binds: the caller, the source, the place id, the city and the
// components that locate the address (district, thoroughfare, building
// number), the coordinate the answer gave, and an expiry (SessionTTL). The
// describing lines (neighborhood, site name, postal code) and the caller's
// own lines (floor, unit, block, directions) are not bound: the user may
// correct them and the address stays the provider's.

// proofKeyInfo is the HKDF context of the proof key: its own, so no other
// key derived from the pepper can stand in for it.
const proofKeyInfo = "koala address proof v1"

// proofVersion is the claims' format.
const proofVersion = 1

// proofClaims are what a proof binds.
type proofClaims struct {
	Version      int    `json:"v"`
	User         string `json:"u"`
	Source       string `json:"s"`
	PlaceID      string `json:"p,omitempty"`
	City         string `json:"c"`
	District     string `json:"d,omitempty"`
	Thoroughfare string `json:"t,omitempty"`
	Building     string `json:"b,omitempty"`
	Lat          string `json:"la,omitempty"`
	Lng          string `json:"lo,omitempty"`
	Expires      int64  `json:"e"`
}

// prover signs and opens proofs.
type prover struct {
	key []byte
}

func newProver(secret []byte) (prover, error) {
	if len(secret) < tokenkeys.PepperBytes {
		return prover{}, fmt.Errorf("address: the proof secret must have at least %d bytes", tokenkeys.PepperBytes)
	}
	key, err := hkdf.Key(sha256.New, secret, nil, proofKeyInfo, sha256.Size)
	if err != nil {
		return prover{}, fmt.Errorf("address: derive the proof key: %w", err)
	}
	return prover{key: key}, nil
}

func proofInvalid() error {
	return apperr.New(apperr.KindInvalid, CodeProofInvalid)
}

func formatCoordinate(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// claimsOf are the claims of a resolved value for user, valid until
// expires.
func claimsOf(r Resolved, user uuid.UUID, expires time.Time) proofClaims {
	c := proofClaims{
		Version: proofVersion, User: user.String(), Source: string(r.Source), PlaceID: deref(r.PlaceID),
		City: r.CityID.String(), Thoroughfare: deref(r.Thoroughfare), Building: deref(r.BuildingNumber),
		Expires: expires.Unix(),
	}
	if r.DistrictID != nil {
		c.District = r.DistrictID.String()
	}
	if r.Point != nil {
		c.Lat, c.Lng = formatCoordinate(r.Point.Lat), formatCoordinate(r.Point.Lng)
	}
	return c
}

// sign is the proof of c: base64url(claims) "." base64url(HMAC).
func (p prover) sign(c proofClaims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("address: encode the proof: %w", err)
	}
	m := hmac.New(sha256.New, p.key)
	_, _ = m.Write(payload) // a hash.Hash never fails to write
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(m.Sum(nil)), nil
}

// open checks a proof's signature, version, caller and expiry and returns
// its claims; any failure is ADDRESS_PROOF_INVALID.
func (p prover) open(token string, user uuid.UUID, now time.Time) (proofClaims, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return proofClaims{}, proofInvalid()
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(body)
	mac, err2 := enc.DecodeString(sig)
	if err := errors.Join(err1, err2); err != nil {
		return proofClaims{}, proofInvalid()
	}
	m := hmac.New(sha256.New, p.key)
	_, _ = m.Write(payload) // a hash.Hash never fails to write
	if !hmac.Equal(m.Sum(nil), mac) {
		return proofClaims{}, proofInvalid()
	}
	var c proofClaims
	if json.Unmarshal(payload, &c) != nil || c.Version != proofVersion || c.User != user.String() ||
		!now.Before(time.Unix(c.Expires, 0)) {
		return proofClaims{}, proofInvalid()
	}
	switch Source(c.Source) {
	case SourceAutocomplete, SourceReverse:
	default:
		return proofClaims{}, proofInvalid()
	}
	return c, nil
}

// storedSource is what a row records of its provenance: the replace
// compares the input with it.
type storedSource struct {
	city, district         uuid.UUID
	thoroughfare, building *string
	components             Source
	placeID                *string
	point                  *Point
	coordinate             *string
}

// provenance is what a written row records (ADR-0039 Karar 10).
type provenance struct {
	components Source
	placeID    *string
	coordinate *string
}

// provenanceOf decides the sources of a create (stored nil) or a replace.
// The rule works by field group:
//
//   - With a proof: it must be valid (signature, caller, expiry) and hold
//     the address: the same city, the same district when the provider
//     gave one, the same thoroughfare and building number, and no other
//     place id. Else ADDRESS_PROOF_INVALID. The components take the
//     proof's source and place id; the coordinate is PROVIDER only when it
//     is the resolve's own, else the caller's pin (a moved pin is the
//     caller's datum).
//   - Without a proof, a place id is refused unless it is the one the row
//     keeps (an echo). On a replace whose locating fields (city, district,
//     thoroughfare, building number) did not change the stored source and
//     place id stay: correcting a floor or a postal code does not turn the
//     provider's address into the caller's. A changed locating field is
//     another place: MANUAL, no place id. The coordinate keeps its stored
//     source while it is the stored point, and is USER_PIN once moved.
func (s *Service) provenanceOf(userID uuid.UUID, in Input, stored *storedSource, now time.Time) (provenance, error) {
	if in.Proof != nil {
		return s.proven(userID, in, now)
	}
	if in.ProviderPlaceID != nil && (stored == nil || stored.placeID == nil || *stored.placeID != *in.ProviderPlaceID) {
		return provenance{}, proofInvalid()
	}
	out := provenance{components: SourceManual}
	if stored != nil && stored.components != SourceManual && stored.city == in.CityID && stored.district == in.DistrictID &&
		deref(stored.thoroughfare) == deref(in.Thoroughfare) && deref(stored.building) == deref(in.BuildingNumber) {
		out.components, out.placeID = stored.components, stored.placeID
	}
	if in.Point != nil {
		pin := coordinateUserPin
		if stored != nil && stored.point != nil && *stored.point == *in.Point && stored.coordinate != nil {
			pin = *stored.coordinate
		}
		out.coordinate = &pin
	}
	return out, nil
}

// proven is the provenance of an input that carries a proof.
func (s *Service) proven(userID uuid.UUID, in Input, now time.Time) (provenance, error) {
	c, err := s.proofs.open(*in.Proof, userID, now)
	if err != nil {
		return provenance{}, err
	}
	if c.City != in.CityID.String() || (c.District != "" && c.District != in.DistrictID.String()) ||
		c.Thoroughfare != deref(in.Thoroughfare) || c.Building != deref(in.BuildingNumber) ||
		(in.ProviderPlaceID != nil && *in.ProviderPlaceID != c.PlaceID) {
		return provenance{}, proofInvalid()
	}
	out := provenance{components: Source(c.Source)}
	if c.PlaceID != "" {
		id := c.PlaceID
		out.placeID = &id
	} else {
		out.components = SourceManual
	}
	if in.Point != nil {
		pin := coordinateUserPin
		if Source(c.Source) == SourceAutocomplete && c.Lat == formatCoordinate(in.Point.Lat) && c.Lng == formatCoordinate(in.Point.Lng) {
			pin = coordinateProvider
		}
		out.coordinate = &pin
	}
	return out, nil
}
