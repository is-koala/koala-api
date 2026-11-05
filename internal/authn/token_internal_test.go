package authn

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

var tokenEpoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

type tokenFixture struct {
	clock       *clock.Fake
	marketplace *TokenClass
	staff       *TokenClass
	mpRing      tokenkeys.Ring
}

func newTokenFixture(t *testing.T) tokenFixture {
	t.Helper()
	clk := clock.NewFake(tokenEpoch)
	mpRing, stRing := tokenkeys.Generate("mp"), tokenkeys.Generate("st")
	mp, err := NewTokenClass(AudienceMarketplace, mpRing, clk)
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewTokenClass(AudienceStaff, stRing, clk)
	if err != nil {
		t.Fatal(err)
	}
	return tokenFixture{clock: clk, marketplace: mp, staff: st, mpRing: mpRing}
}

// claims are a valid marketplace token's claims, to be broken one at a time.
func (f tokenFixture) claims() jwt.MapClaims {
	now := f.clock.Now()
	return jwt.MapClaims{
		"iss": Issuer,
		"sub": uuid.NewString(),
		"aud": []string{AudienceMarketplace},
		"exp": now.Add(AccessTokenTTL).Unix(),
		"nbf": now.Unix(),
		"iat": now.Unix(),
		"jti": uuid.NewString(),
		"sid": uuid.NewString(),
	}
}

// sign signs claims with the marketplace key under header changes.
func (f tokenFixture) sign(t *testing.T, claims jwt.MapClaims, header map[string]any) string {
	t.Helper()
	key, _ := f.mpRing.Signer()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = key.ID()
	tok.Header["typ"] = accessTokenType
	for k, v := range header {
		tok.Header[k] = v
	}
	s, err := tok.SignedString(key.Private())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func wantCode(t *testing.T, err error, code apperr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("verify succeeded, want %s", code)
	}
	if got := apperr.From(err); got.Code != code || got.Kind != apperr.KindUnauthenticated {
		t.Fatalf("error = %v, want 401 %s", err, code)
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	t.Parallel()

	f := newTokenFixture(t)
	sub, sid := uuid.New(), uuid.New()
	raw, exp, err := f.marketplace.issue(sub, sid, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(tokenEpoch.Add(AccessTokenTTL)) {
		t.Errorf("exp = %s, want 15 minutes after issue (K12)", exp)
	}
	got, err := f.marketplace.verify(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != sub || got.SessionID != sid {
		t.Errorf("verify() = %+v", got)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(raw, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header["alg"] != "EdDSA" || parsed.Header["typ"] != "at+jwt" || parsed.Header["kid"] != "mp-ephemeral" {
		t.Errorf("header = %v", parsed.Header)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims = %T", parsed.Claims)
	}
	for _, c := range []string{"iss", "aud", "exp", "nbf", "iat", "jti", "sub", "sid"} {
		if _, ok := claims[c]; !ok {
			t.Errorf("claim %s missing", c)
		}
	}
}

// §6.3 token tests: alg=none, alg confusion, wrong aud or iss, expiry,
// a token not yet valid, missing claims, an unknown key, another token
// type and a tampered signature are all refused.
func TestAccessTokenRejects(t *testing.T) {
	t.Parallel()

	f := newTokenFixture(t)
	mpKey, _ := f.mpRing.Signer()

	without := func(name string) jwt.MapClaims {
		c := f.claims()
		delete(c, name)
		return c
	}
	with := func(name string, v any) jwt.MapClaims {
		c := f.claims()
		c[name] = v
		return c
	}

	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, f.claims()).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	// HS256 keyed with the public key's bytes: the classic confusion when
	// a verifier lets the token pick the algorithm.
	hsTok := jwt.NewWithClaims(jwt.SigningMethodHS256, f.claims())
	hsTok.Header["kid"] = mpKey.ID()
	hsTok.Header["typ"] = accessTokenType
	hs, err := hsTok.SignedString([]byte(mpKey.Public()))
	if err != nil {
		t.Fatal(err)
	}
	valid := f.sign(t, f.claims(), nil)
	head, rest, _ := strings.Cut(valid, ".")
	payload, sig, _ := strings.Cut(rest, ".")
	tampered := head + "." + payload + "." + flipMiddle(sig)
	other := f.sign(t, with("sub", uuid.NewString()), nil)
	_, otherRest, _ := strings.Cut(other, ".")
	otherPayload, _, _ := strings.Cut(otherRest, ".")
	spliced := head + "." + otherPayload + "." + sig

	cases := map[string]string{
		"alg none":         none,
		"alg HS256":        hs,
		"wrong audience":   f.sign(t, with("aud", []string{AudienceStaff}), nil),
		"no audience":      f.sign(t, without("aud"), nil),
		"wrong issuer":     f.sign(t, with("iss", "iskoala.com"), nil),
		"no expiry":        f.sign(t, without("exp"), nil),
		"not yet valid":    f.sign(t, with("nbf", f.clock.Now().Add(time.Minute).Unix()), nil),
		"no nbf":           f.sign(t, without("nbf"), nil),
		"issued in future": f.sign(t, with("iat", f.clock.Now().Add(time.Minute).Unix()), nil),
		"no iat":           f.sign(t, without("iat"), nil),
		"no jti":           f.sign(t, without("jti"), nil),
		"jti not a uuid":   f.sign(t, with("jti", "1"), nil),
		"no session":       f.sign(t, without("sid"), nil),
		"subject not uuid": f.sign(t, with("sub", "42"), nil),
		"unknown kid":      f.sign(t, f.claims(), map[string]any{"kid": "mp-retired"}),
		"no kid":           f.sign(t, f.claims(), map[string]any{"kid": nil}),
		"plain JWT type":   f.sign(t, f.claims(), map[string]any{"typ": "JWT"}),
		"tampered sig":     tampered,
		"spliced payload":  spliced,
		"garbage":          "not.a.token",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := f.marketplace.verify(raw)
			wantCode(t, err, CodeTokenInvalid)
		})
	}

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, err := f.marketplace.verify("")
		wantCode(t, err, CodeTokenMissing)
	})
}

// A genuine token past its expiry says so; the client refreshes. A forged
// one never reaches the expiry check (AUTH-003: "the strict extractor
// keeps refusing an expired token").
func TestAccessTokenExpiry(t *testing.T) {
	t.Parallel()

	f := newTokenFixture(t)
	raw, _, err := f.marketplace.issue(uuid.New(), uuid.New(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(AccessTokenTTL - time.Second)
	if _, err := f.marketplace.verify(raw); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	f.clock.Advance(time.Second) // at exp the token is expired (valid while now < exp)
	_, err = f.marketplace.verify(raw)
	wantCode(t, err, CodeTokenExpired)

	f.clock.Advance(72 * time.Hour)
	head, rest, _ := strings.Cut(raw, ".")
	payload, sig, _ := strings.Cut(rest, ".")
	_, err = f.marketplace.verify(head + "." + payload + "." + flipMiddle(sig))
	wantCode(t, err, CodeTokenInvalid)
}

// AUTH-015, D5: the two classes do not cross. A staff token fails the
// marketplace class and a marketplace token the staff class: other keys
// and other audience.
func TestTokenClassesDoNotCross(t *testing.T) {
	t.Parallel()

	f := newTokenFixture(t)
	staffTok, _, err := f.staff.issue(uuid.New(), uuid.New(), 1, []string{amrPassword})
	if err != nil {
		t.Fatal(err)
	}
	mpTok, _, err := f.marketplace.issue(uuid.New(), uuid.New(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("AUTH-015/staff token on the marketplace", func(t *testing.T) {
		t.Parallel()
		_, err := f.marketplace.verify(staffTok)
		wantCode(t, err, CodeTokenInvalid)
	})
	t.Run("AUTH-015/marketplace token on the staff surface", func(t *testing.T) {
		t.Parallel()
		_, err := f.staff.verify(mpTok)
		wantCode(t, err, CodeTokenInvalid)
	})
	t.Run("AUTH-015/marketplace key with the staff audience", func(t *testing.T) {
		t.Parallel()
		c := f.claims()
		c["aud"] = []string{AudienceStaff}
		c["stv"] = 1
		_, err := f.staff.verify(f.sign(t, c, nil))
		wantCode(t, err, CodeTokenInvalid)
	})
	if _, err := f.staff.verify(staffTok); err != nil {
		t.Fatalf("staff token on the staff class: %v", err)
	}
}

// A class signs with its ring's first key and verifies a retiring key's
// tokens: rotation without logging anyone out.
func TestAccessTokenKeyRotation(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(tokenEpoch)
	oldRing := tokenkeys.Generate("mp")
	oldClass, err := NewTokenClass(AudienceMarketplace, oldRing, clk)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := oldClass.issue(uuid.New(), uuid.New(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := oldRing.Signer()
	newKey, _ := tokenkeys.Generate("mp").Signer()
	rotated, err := tokenkeys.Parse("mp-new:" + seedOf(newKey.Private()) + ",mp-ephemeral:" + seedOf(oldKey.Private()))
	if err != nil {
		t.Fatal(err)
	}
	newClass, err := NewTokenClass(AudienceMarketplace, rotated, clk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newClass.verify(raw); err != nil {
		t.Fatalf("token of the retiring key: %v", err)
	}
	fresh, _, err := newClass.issue(uuid.New(), uuid.New(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(fresh, jwt.MapClaims{})
	if err != nil || parsed.Header["kid"] != "mp-new" {
		t.Fatalf("new token kid = %v, %v; want mp-new", parsed.Header["kid"], err)
	}
	if _, err := oldClass.verify(fresh); err == nil {
		t.Error("a ring without the new key verified its token")
	}
}

func seedOf(k ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(k.Seed())
}

// flipMiddle changes one character in the middle of a base64url segment,
// where every bit is significant (the last character carries padding
// bits a lenient decoder ignores).
func flipMiddle(s string) string {
	b := []byte(s)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}
