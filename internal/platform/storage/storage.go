package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Errors of the object store. They are plain sentinels, not apperr codes:
// the error codes of the media endpoints belong to the contract
// (api/components/problem.yaml) and the handler that answers, not to this
// package (ADR-0007, ADR-0030 Karar 7).
var (
	// ErrNotFound means the object is not in the bucket. The quarantine
	// case ("the client never wrote") reaches the caller as this error.
	ErrNotFound = errors.New("storage: object not found")
	// ErrTooLarge means the object is larger than the bound the caller
	// gave Get. It is returned before the body is buffered.
	ErrTooLarge = errors.New("storage: object larger than the read bound")
	// ErrConfig means the configuration cannot build a working client.
	// serve refuses to start on it (fail closed).
	ErrConfig = errors.New("storage: invalid configuration")
)

// Clock tells the time. It is declared here, by the consumer, and
// clock.Real satisfies it: a signed URL has to report when it expires and
// nothing else in this package needs the time.
type Clock interface {
	Now() time.Time
}

// Config is the validated object store configuration. It is built by
// internal/platform/config from the KOALA_OBJECT_* variables; this
// package re-checks it so that a caller cannot assemble a half
// configured Store by hand.
type Config struct {
	// Endpoint is the base URL of the S3 compatible API, such as a MinIO
	// or an R2 endpoint. Empty means AWS's own endpoint for Region.
	Endpoint string
	// Region is the signing region. S3 compatible stores that have no
	// regions still need one for the signature; R2 uses "auto".
	Region string
	// Bucket holds every object. One bucket, because the prefix carries
	// the access class (ADR-0029 Karar 6).
	Bucket string
	// AccessKeyID and SecretAccessKey are the static credentials. The
	// secret never reaches a log: it is held only inside the credentials
	// provider, and Store has no method that prints it.
	AccessKeyID     string
	SecretAccessKey string
	// PathStyle addresses the bucket as <endpoint>/<bucket>/<key> instead
	// of <bucket>.<endpoint>. MinIO and a local test endpoint need it.
	PathStyle bool
	// PublicBaseURL is the origin publicly readable objects are served
	// from (a CDN in front of the bucket). Empty means no object gets a
	// public URL.
	PublicBaseURL string
}

// Validate reports the first reason the configuration cannot serve.
func (c Config) Validate() error {
	var errs []error
	if c.Bucket == "" {
		errs = append(errs, errors.New("bucket is required"))
	}
	if c.Region == "" {
		errs = append(errs, errors.New("region is required"))
	}
	if c.AccessKeyID == "" {
		errs = append(errs, errors.New("access key id is required"))
	}
	if c.SecretAccessKey == "" {
		errs = append(errs, errors.New("secret access key is required"))
	}
	if c.Endpoint != "" {
		if err := validateAbsoluteURL(c.Endpoint); err != nil {
			errs = append(errs, fmt.Errorf("endpoint: %w", err))
		}
	}
	if c.PublicBaseURL != "" {
		if err := validateAbsoluteURL(c.PublicBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("public base URL: %w", err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrConfig, errors.Join(errs...))
}

// validateAbsoluteURL accepts an http or https origin with no query,
// fragment or user info. A credential in the URL would end up in every
// signed address the service hands out.
func validateAbsoluteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("want an http:// or https:// URL")
	case u.Host == "":
		return errors.New("want a host")
	case u.User != nil:
		return errors.New("must not carry credentials")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("must not carry a query or fragment")
	}
	return nil
}

// Store is the object store client. It is safe for concurrent use.
type Store struct {
	api      *s3.Client
	presign  *s3.PresignClient
	clock    Clock
	bucket   string
	publicer string // PublicBaseURL without a trailing slash; "" disables public URLs
}

// New returns a Store for cfg. httpClient may be nil, in which case the
// SDK's default transport is used; tests pass the fake's client.
func New(cfg Config, now Clock, httpClient *http.Client) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		return nil, fmt.Errorf("%w: clock is required", ErrConfig)
	}
	creds := aws.NewCredentialsCache(aws.CredentialsProviderFunc(
		func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     cfg.AccessKeyID,
				SecretAccessKey: cfg.SecretAccessKey,
				Source:          "koala/config",
			}, nil
		}))
	awsCfg := aws.Config{Region: cfg.Region, Credentials: creds}
	if httpClient != nil {
		awsCfg.HTTPClient = httpClient
	}
	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
		// The SDK's default is WhenSupported, which adds a CRC32 header
		// to every write and, for a body it cannot rewind, switches the
		// request to aws-chunked encoding. Koala talks to S3 compatible
		// stores, not only to AWS, and that default is the part of the
		// protocol they most often implement differently. WhenRequired
		// also makes a direct write and a presigned write behave the
		// same way: the presign path forces it (ADR-0030 Karar 1).
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &Store{
		api:      api,
		presign:  s3.NewPresignClient(api),
		clock:    now,
		bucket:   cfg.Bucket,
		publicer: strings.TrimSuffix(cfg.PublicBaseURL, "/"),
	}, nil
}

// Bucket is the bucket every key resolves in.
func (s *Store) Bucket() string { return s.bucket }

// SignedRequest is a presigned request: the address, the method the
// client has to use and the headers the signature covers.
type SignedRequest struct {
	URL       string
	Method    string
	Headers   http.Header
	ExpiresAt time.Time
}

// PresignPut returns the address the client writes the quarantine object
// to (ADR-0019 §9, ADR-0029 Karar 6).
//
// The signature covers the method, the bucket, the key and the expiry,
// and nothing else. It does not cover the content type or the size, and
// cannot: aws-sdk-go-v2 removes the Content-Type header from a presigned
// PutObject on purpose, and SigV4 only signs Content-Length when the
// request being signed carries a body, which a presign never does. The
// write address therefore bounds what the client may *name*, not what it
// may write; the size, the detected type and the canvas are checked at
// the complete step, against the object (ADR-0030 Karar 1).
func (s *Store) PresignPut(ctx context.Context, key Key, ttl time.Duration) (SignedRequest, error) {
	if err := checkSigned(key, ttl); err != nil {
		return SignedRequest{}, err
	}
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key.String()),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return SignedRequest{}, fmt.Errorf("storage: presign put %s: %w", key.Prefix(), err)
	}
	return s.signed(req.URL, req.Method, req.SignedHeader, ttl), nil
}

// Download is how a signed read address presents the object. The values
// become S3 response overrides, so the store echoes them back on the
// object the client fetches.
type Download struct {
	// ContentType is the type the server detected. It is sent instead of
	// whatever the object carries, so a stored type can never widen into
	// something a browser would execute.
	ContentType string
	// Attachment asks the browser to save the file instead of rendering
	// it. Identity documents and dispute evidence are always attachments
	// (Java's rule, kept).
	Attachment bool
	// NoStore asks every cache to keep nothing. Identity documents are
	// always no-store (ADR-0029 Karar 6).
	NoStore bool
}

// PresignGet returns a short lived read address for key.
//
// X-Content-Type-Options cannot be set through a presigned GET: S3's
// response overrides are a closed list and that header is not on it. The
// API's own 302 carries it, and the object is written with its content
// disposition already set, so a client that follows the redirect gets
// both headers (ADR-0030 Karar 1).
func (s *Store) PresignGet(ctx context.Context, key Key, ttl time.Duration, d Download) (SignedRequest, error) {
	if err := checkSigned(key, ttl); err != nil {
		return SignedRequest{}, err
	}
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key.String()),
	}
	if d.ContentType != "" {
		in.ResponseContentType = aws.String(d.ContentType)
	}
	if d.Attachment {
		in.ResponseContentDisposition = aws.String("attachment")
	}
	if d.NoStore {
		in.ResponseCacheControl = aws.String("no-store")
	}
	req, err := s.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return SignedRequest{}, fmt.Errorf("storage: presign get %s: %w", key.Prefix(), err)
	}
	return s.signed(req.URL, req.Method, req.SignedHeader, ttl), nil
}

// PublicURL returns the address a publicly readable object is served
// from, or false when the store serves none or the key's prefix is not
// public. identity-documents/ resolves to no URL at all, which is a
// contract rule (ADR-0029 Karar 6, IDN-007) that lives here so that no
// caller can get one by mistake.
func (s *Store) PublicURL(key Key) (string, bool) {
	if s.publicer == "" || key.Zero() {
		return "", false
	}
	if key.Prefix() != PrefixImages && key.Prefix() != PrefixPublic {
		return "", false
	}
	return s.publicer + "/" + key.String(), true
}

// ObjectInfo is what a head says about an object.
type ObjectInfo struct {
	// Size is the stored length in bytes.
	Size int64
	// ContentType is the type stored with the object. It is the type the
	// client declared on upload for a quarantine object, so it is
	// evidence, not a decision: the type is detected from the bytes
	// (ADR-0029 Karar 5).
	ContentType string
	// ETag is the store's entity tag, unquoted where the store quotes it.
	ETag string
}

// Head returns the object's metadata, or ErrNotFound.
func (s *Store) Head(ctx context.Context, key Key) (ObjectInfo, error) {
	if err := checkKey(key); err != nil {
		return ObjectInfo{}, err
	}
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key.String()),
	})
	if err != nil {
		return ObjectInfo{}, classify("head", key, err)
	}
	info := ObjectInfo{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		info.ContentType = *out.ContentType
	}
	if out.ETag != nil {
		info.ETag = strings.Trim(*out.ETag, `"`)
	}
	return info, nil
}

// Get reads the whole object into memory, refusing one over limit bytes.
//
// The bound is checked twice: against the length the store reports and
// again while reading, because a store that reports no length or lies
// about it would otherwise decide how much memory the process uses. The
// read is one byte over the bound on purpose, so "exactly limit" passes
// and "limit plus one" is caught without trusting the header.
func (s *Store) Get(ctx context.Context, key Key, limit int64) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: read bound must be positive", ErrConfig)
	}
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key.String()),
	})
	if err != nil {
		return nil, classify("get", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	if out.ContentLength != nil && *out.ContentLength > limit {
		return nil, fmt.Errorf("%w: %s is %d bytes, bound is %d", ErrTooLarge, key.Prefix(), *out.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("storage: read %s: %w", key.Prefix(), err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrTooLarge, key.Prefix(), limit)
	}
	return body, nil
}

// PutOptions is the metadata an object is written with.
type PutOptions struct {
	// ContentType is the type the server detected. It is the object's
	// stored type; the client's declaration never becomes it (ADR-0029
	// Karar 5, a deliberate departure from Java, which wrote the raw
	// header to both S3 and media.file_type).
	ContentType string
	// Attachment stores Content-Disposition: attachment, so the object is
	// saved and not rendered even when it is fetched directly.
	Attachment bool
	// CacheControl is stored as given; empty stores none.
	CacheControl string
}

// Put writes body at key.
func (s *Store) Put(ctx context.Context, key Key, body []byte, o PutOptions) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if o.ContentType == "" {
		return fmt.Errorf("%w: content type is required to write an object", ErrConfig)
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key.String()),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(o.ContentType),
		ContentLength: aws.Int64(int64(len(body))),
	}
	if o.Attachment {
		in.ContentDisposition = aws.String("attachment")
	}
	if o.CacheControl != "" {
		in.CacheControl = aws.String(o.CacheControl)
	}
	if _, err := s.api.PutObject(ctx, in); err != nil {
		return fmt.Errorf("storage: put %s: %w", key.Prefix(), err)
	}
	return nil
}

// Copy copies from to to, replacing the metadata with o.
//
// S3 has no rename, so a move is a copy followed by a delete and the two
// are not one operation. The caller owns the order and what a half
// finished move leaves behind; Move states that order in one place.
func (s *Store) Copy(ctx context.Context, from, to Key, o PutOptions) error {
	if err := checkKey(from); err != nil {
		return err
	}
	if err := checkKey(to); err != nil {
		return err
	}
	if o.ContentType == "" {
		return fmt.Errorf("%w: content type is required to copy an object", ErrConfig)
	}
	in := &s3.CopyObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(to.String()),
		// The source is "<bucket>/<key>". The key alphabet is closed and
		// holds no character S3 would need escaped, which ParseKey and
		// NewKey guarantee, so no escaping step can go wrong here.
		CopySource:        aws.String(s.bucket + "/" + from.String()),
		MetadataDirective: s3types.MetadataDirectiveReplace,
		ContentType:       aws.String(o.ContentType),
	}
	if o.Attachment {
		in.ContentDisposition = aws.String("attachment")
	}
	if o.CacheControl != "" {
		in.CacheControl = aws.String(o.CacheControl)
	}
	if _, err := s.api.CopyObject(ctx, in); err != nil {
		return classify("copy", from, err)
	}
	return nil
}

// Move copies from to to and then deletes from.
//
// The order is "write the destination, then drop the source", so an
// interruption leaves the object in the quarantine and not nowhere: the
// sweep removes an orphan in incoming/ after 24 hours, while a lost
// object would be unrecoverable. A delete that fails after a successful
// copy is reported separately through ErrSourceKept, so the caller can
// treat the move as done and still log the leftover.
func (s *Store) Move(ctx context.Context, from, to Key, o PutOptions) error {
	if err := s.Copy(ctx, from, to, o); err != nil {
		return err
	}
	if err := s.Delete(ctx, from); err != nil {
		return fmt.Errorf("%w: %w", ErrSourceKept, err)
	}
	return nil
}

// ErrSourceKept means a move copied the object but could not delete the
// source. The destination is there and correct; the source is a leftover
// for the sweep.
var ErrSourceKept = errors.New("storage: move copied but did not delete the source")

// Delete removes the object. Deleting an object that is not there
// succeeds, as S3 does: a retried cleanup must not fail.
func (s *Store) Delete(ctx context.Context, key Key) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if _, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key.String()),
	}); err != nil {
		if errors.Is(classify("delete", key, err), ErrNotFound) {
			return nil
		}
		return fmt.Errorf("storage: delete %s: %w", key.Prefix(), err)
	}
	return nil
}

// signed builds the result of a presign. The expiry is computed from the
// injected clock, so a test can pin it.
func (s *Store) signed(rawURL, method string, headers http.Header, ttl time.Duration) SignedRequest {
	return SignedRequest{
		URL:       rawURL,
		Method:    method,
		Headers:   headers.Clone(),
		ExpiresAt: s.clock.Now().Add(ttl).UTC(),
	}
}

// MaxSignedTTL bounds how long a signed address may live. SigV4 allows up
// to seven days; a Koala address is handed to one client for one upload
// or one download, so a long lived one is a mistake, not a feature.
const MaxSignedTTL = time.Hour

func checkSigned(key Key, ttl time.Duration) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if ttl <= 0 || ttl > MaxSignedTTL {
		return fmt.Errorf("%w: signed URL lifetime must be in (0, %s]", ErrConfig, MaxSignedTTL)
	}
	return nil
}

// checkKey refuses the zero Key. Every other Key came from NewKey or
// ParseKey and is well formed by construction, which EscapesPrefix
// restates for the reader and FuzzStorageKey proves.
func checkKey(key Key) error {
	if key.Zero() || key.EscapesPrefix() {
		return fmt.Errorf("%w: key", ErrKeyInvalid)
	}
	return nil
}

// classify turns an SDK error into ErrNotFound where the store said the
// object is not there, and wraps it otherwise. HeadObject answers 404
// with types.NotFound and GetObject with types.NoSuchKey; a plain 404
// from a store that models neither is caught by the status check.
func classify(op string, key Key, err error) error {
	var notFound *s3types.NotFound
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &notFound) || errors.As(err, &noSuchKey) || httpStatus(err) == http.StatusNotFound {
		return fmt.Errorf("%w: %s %s", ErrNotFound, op, key.Prefix())
	}
	return fmt.Errorf("storage: %s %s: %w", op, key.Prefix(), err)
}

// httpStatus returns the HTTP status an SDK error carries, or 0.
func httpStatus(err error) int {
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode()
	}
	return 0
}
