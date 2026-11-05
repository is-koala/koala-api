package authn

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/is-koala/koala-api/internal/platform/apperr"
	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

// Token class constants (ADR-0025). Each class has its own key ring and
// audience; a token of one class fails the other's verification on both.
const (
	// Issuer is the iss of every access token.
	Issuer = "https://api.iskoala.com"
	// AudienceMarketplace and AudienceStaff are the two classes' aud.
	AudienceMarketplace = "koala-marketplace"
	AudienceStaff       = "koala-staff"

	// accessTokenType is the JOSE typ of an access token (RFC 9068 §2.1):
	// it keeps any other JWT this service might sign from passing as one.
	accessTokenType = "at+jwt"
	// signingAlg is the only accepted alg. Pinning it closes "none" and
	// alg confusion (RFC 8725 §3.1).
	signingAlg = "EdDSA"

	// amrPassword is the amr value of a password sign-in (RFC 8176).
	amrPassword = "pwd"

	// ClockSkew is the tolerance on exp, nbf and iat (RFC 7519 §4.1.4
	// allows a small leeway): a token is valid until exp+ClockSkew and
	// from nbf-ClockSkew. It covers clocks of several instances.
	ClockSkew = 5 * time.Second
)

// accessClaims are an access token's claims: the registered ones plus the
// session id, and for staff the token version and the authentication
// methods.
type accessClaims struct {
	jwt.RegisteredClaims
	SessionID    string   `json:"sid"`
	TokenVersion int64    `json:"stv,omitempty"`
	AMR          []string `json:"amr,omitempty"`
}

// verifiedToken is what a valid access token says.
type verifiedToken struct {
	Subject      uuid.UUID
	SessionID    uuid.UUID
	TokenVersion int64
	AMR          []string
}

// TokenClass issues and verifies the access tokens of one class.
type TokenClass struct {
	audience string
	ring     tokenkeys.Ring
	clock    clock.Clock
	parser   *jwt.Parser
}

// NewTokenClass returns the class with audience aud, signing with ring's
// first key and verifying with any of its keys.
func NewTokenClass(aud string, ring tokenkeys.Ring, clk clock.Clock) (*TokenClass, error) {
	if aud != AudienceMarketplace && aud != AudienceStaff {
		return nil, fmt.Errorf("authn: unknown token audience %q", aud)
	}
	if ring.Empty() {
		return nil, fmt.Errorf("authn: %s: key ring is empty", aud)
	}
	return &TokenClass{
		audience: aud,
		ring:     ring,
		clock:    clk,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{signingAlg}),
			jwt.WithIssuer(Issuer),
			jwt.WithAudience(aud),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithTimeFunc(clk.Now),
			jwt.WithLeeway(ClockSkew),
		),
	}, nil
}

// issue signs an access token for subject in session sid. stv and amr are
// the staff claims; the marketplace passes 0 and nil.
func (c *TokenClass) issue(subject, sid uuid.UUID, stv int64, amr []string) (string, time.Time, error) {
	key, _ := c.ring.Signer()
	now := c.clock.Now().Truncate(time.Second)
	exp := now.Add(AccessTokenTTL)
	jti, err := uuid.NewV7()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: token id: %w", err)
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   subject.String(),
			Audience:  jwt.ClaimStrings{c.audience},
			ExpiresAt: jwt.NewNumericDate(exp),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti.String(),
		},
		SessionID:    sid.String(),
		TokenVersion: stv,
		AMR:          amr,
	})
	t.Header["kid"] = key.ID()
	t.Header["typ"] = accessTokenType
	signed, err := t.SignedString(key.Private())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("authn: sign access token: %w", err)
	}
	return signed, exp, nil
}

// verify checks raw completely: header (typ, alg, kid of this class's
// ring), signature, iss, aud, exp, nbf, iat, jti, sub and sid. A genuine
// token past its expiry is CodeTokenExpired; every other failure is
// CodeTokenInvalid, without saying which check failed.
func (c *TokenClass) verify(raw string) (verifiedToken, error) {
	if raw == "" {
		return verifiedToken{}, apperr.New(apperr.KindUnauthenticated, CodeTokenMissing)
	}
	invalid := apperr.New(apperr.KindUnauthenticated, CodeTokenInvalid)
	var claims accessClaims
	_, err := c.parser.ParseWithClaims(raw, &claims, c.key)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		// golang-jwt v5 validates claims only after the signature, so an
		// expired token here is a genuine one.
		return verifiedToken{}, apperr.New(apperr.KindUnauthenticated, CodeTokenExpired)
	case err != nil:
		return verifiedToken{}, invalid
	}
	if claims.NotBefore == nil || claims.IssuedAt == nil {
		return verifiedToken{}, invalid
	}
	if _, err := uuid.Parse(claims.ID); err != nil {
		return verifiedToken{}, invalid
	}
	sub, err := uuid.Parse(claims.Subject)
	if err != nil {
		return verifiedToken{}, invalid
	}
	sid, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return verifiedToken{}, invalid
	}
	return verifiedToken{Subject: sub, SessionID: sid, TokenVersion: claims.TokenVersion, AMR: claims.AMR}, nil
}

// key is the parser's key function: the token must declare this class's
// type and one of its key ids. The alg is pinned by the parser.
// Expiry, not-before and issued-at are checked with ClockSkew.
func (c *TokenClass) key(t *jwt.Token) (any, error) {
	if typ, _ := t.Header["typ"].(string); typ != accessTokenType {
		return nil, errors.New("unexpected token type")
	}
	kid, _ := t.Header["kid"].(string)
	pub, ok := c.ring.Verifier(kid)
	if !ok {
		return nil, errors.New("unknown key id")
	}
	return pub, nil
}
