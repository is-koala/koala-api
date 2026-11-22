package idempotency

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/is-koala/koala-api/internal/platform/tokenkeys"
)

// HKDF contexts of the keys derived from the server secret and from a
// request (ADR-0034 Karar 3). Each use has its own, so no key can stand
// in for another.
const (
	requestHashInfo = "koala-api idempotency request hash v1"
	sealSaltInfo    = "koala-api idempotency response salt v1"
	responseKeyInfo = "koala-api idempotency response key v1"
)

// keys are the server-side keys of the store, derived from the secret.
type keys struct {
	requestHash []byte // HMAC key of the request hash
	sealSalt    []byte // HKDF salt of the per-request response key
}

func deriveKeys(secret []byte) (keys, error) {
	if len(secret) < tokenkeys.PepperBytes {
		return keys{}, fmt.Errorf("idempotency: the secret must have at least %d bytes", tokenkeys.PepperBytes)
	}
	hashKey, err := hkdf.Key(sha256.New, secret, nil, requestHashInfo, sha256.Size)
	if err != nil {
		return keys{}, fmt.Errorf("idempotency: derive the request hash key: %w", err)
	}
	salt, err := hkdf.Key(sha256.New, secret, nil, sealSaltInfo, sha256.Size)
	if err != nil {
		return keys{}, fmt.Errorf("idempotency: derive the response salt: %w", err)
	}
	return keys{requestHash: hashKey, sealSalt: salt}, nil
}

// hash is the stored request hash of a canonical request: an HMAC under a
// server key, so a leaked table does not confirm a guessed body.
func (k keys) hash(canonical []byte) []byte {
	m := hmac.New(sha256.New, k.requestHash)
	_, _ = m.Write(canonical) // a hash.Hash never fails to write
	return m.Sum(nil)
}

// aead is the cipher of one request's response. Its key comes from the
// canonical request itself, salted with a server key: the database alone
// does not open a response, and neither does the secret without the
// request (completeRegistration's carries a 256-bit token and the
// password).
func (k keys) aead(canonical []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, canonical, k.sealSalt, responseKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("idempotency: derive the response key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("idempotency: response cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("idempotency: response cipher: %w", err)
	}
	return gcm, nil
}

// seal encrypts plaintext for the request canonical, bound to the row by
// aad: nonce ‖ ciphertext ‖ tag.
func (k keys) seal(canonical, aad, plaintext []byte) ([]byte, error) {
	gcm, err := k.aead(canonical)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error (Go 1.24+)
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// errUnsealable is a stored response the request cannot open.
var errUnsealable = errors.New("idempotency: the stored response does not open with this request")

// open decrypts what seal returned for the same request and row.
func (k keys) open(canonical, aad, sealed []byte) ([]byte, error) {
	gcm, err := k.aead(canonical)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errUnsealable
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errUnsealable
	}
	return plain, nil
}
