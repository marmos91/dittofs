// Package s3 provides an S3-backed RemoteStore implementation.
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// maxBlockReadSize is the fallback pre-allocation size for Get when
// ContentLength is absent (e.g., chunked transfer). Matches block.Size (8 MB).
const maxBlockReadSize = 8 * 1024 * 1024

// casPrefix is the CAS object-key prefix walked by Walk. Mirrors the
// block.FormatCASKey output ("cas/{hh}/{hh}/{hex}").
const casPrefix = "cas/"

// s3HTTPRequestTimeout bounds the entire HTTP request lifecycle (connect +
// request + body read). Prevents a hung S3 endpoint from pinning syncer
// goroutines indefinitely; without it Close()/DrainAllUploads cannot honour
// their drain deadlines.
const s3HTTPRequestTimeout = 2 * time.Minute

// Compile-time interface satisfaction check.
var _ remote.RemoteStore = (*Store)(nil)

// Config holds configuration for the S3 block store.
type Config struct {
	// Bucket is the S3 bucket name.
	Bucket string

	// Region is the AWS region (optional, uses SDK default if empty).
	Region string

	// Endpoint is the S3 endpoint URL (optional, for S3-compatible services).
	Endpoint string

	// AccessKey is the S3 access key ID (required).
	AccessKey string

	// SecretKey is the S3 secret access key (required).
	SecretKey string

	// KeyPrefix is prepended to all block keys (e.g., "blocks/").
	// Should end with "/" if non-empty.
	KeyPrefix string

	// MaxRetries is the maximum number of retry attempts for transient errors.
	MaxRetries int

	// ForcePathStyle forces path-style addressing (required for Localstack/MinIO).
	ForcePathStyle bool
}

// Store is an S3-backed implementation of remote.RemoteStore.
type Store struct {
	client    *s3.Client
	bucket    string
	keyPrefix string
	closed    bool
	mu        sync.RWMutex
}

// New creates a new S3 remote block store with an existing client.
func New(client *s3.Client, config Config) *Store {
	return &Store{
		client:    client,
		bucket:    config.Bucket,
		keyPrefix: config.KeyPrefix,
	}
}

// NewFromConfig creates a new S3 remote block store by creating an S3 client from config.
// This is the preferred constructor when you don't have an existing S3 client.
func NewFromConfig(ctx context.Context, config Config) (*Store, error) {
	if config.Bucket == "" {
		return nil, errors.New("s3 block store: bucket is required")
	}
	if config.AccessKey == "" || config.SecretKey == "" {
		return nil, errors.New("s3 block store: access_key_id and secret_access_key are required")
	}

	var opts []func(*awsconfig.LoadOptions) error

	if config.Region != "" {
		opts = append(opts, awsconfig.WithRegion(config.Region))
	}

	opts = append(opts, awsconfig.WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, ""),
	))

	// Configure HTTP client for parallel uploads. Pool size kept moderate
	// to limit memory overhead (~50 conns x 512KB buffers = ~25MB).
	httpTransport := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
		WriteBufferSize:       256 * 1024,
		ReadBufferSize:        256 * 1024,
		ExpectContinueTimeout: 0,
		ResponseHeaderTimeout: 60 * time.Second,
	}

	httpClient := &http.Client{
		Transport: httpTransport,
		Timeout:   s3HTTPRequestTimeout,
	}
	opts = append(opts, awsconfig.WithHTTPClient(httpClient))

	maxAttempts := config.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	opts = append(opts, awsconfig.WithRetryer(func() aws.Retryer {
		return retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = maxAttempts
			o.MaxBackoff = 30 * time.Second
			o.Retryables = append(o.Retryables, retry.RetryableHTTPStatusCode{
				Codes: map[int]struct{}{429: {}},
			})
		})
	}))

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)

	if config.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(normalizeEndpoint(config.Endpoint))
		})
	}

	if config.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return New(client, config), nil
}

// normalizeEndpoint prepends https:// when the endpoint does not already
// include a URI scheme. Endpoints that already contain a scheme (including
// non-HTTP ones like s3://) are returned as-is.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	// Look for "://" preceded by a valid URI scheme (RFC 3986: ALPHA *(ALPHA / DIGIT / "+" / "-" / ".")).
	// We cannot use url.Parse alone because it misinterprets "host:port" as scheme "host".
	if i := strings.Index(endpoint, "://"); i > 0 {
		scheme := endpoint[:i]
		if isValidScheme(scheme) {
			return endpoint
		}
	}
	return "https://" + endpoint
}

// ErrUnsafeEndpoint indicates a configured S3 endpoint resolves to an
// address that must not be dialed (cloud metadata, loopback, link-local,
// or private/internal hosts) — the classic SSRF pivot.
var ErrUnsafeEndpoint = errors.New("s3 block store: unsafe endpoint")

// ValidateEndpoint rejects S3 endpoints that point at addresses an attacker
// could use to pivot the server into the internal network (SSRF). It runs
// at config-create time, before any HealthCheck/HeadBucket fires, so a
// malicious "endpoint":"http://169.254.169.254/..." is refused before the
// S3 SDK ever issues a request.
//
// An empty endpoint is allowed (the SDK uses the real AWS endpoint, which
// is public). Otherwise the endpoint is normalized, its host is resolved,
// and EVERY resolved IP must be a public unicast address. We reject:
//   - unspecified / multicast / loopback addresses,
//   - link-local unicast (169.254.0.0/16, fe80::/10) — covers the cloud
//     metadata endpoint 169.254.169.254,
//   - private / unique-local ranges (RFC1918, fc00::/7) and other
//     non-global-unicast space.
//
// allowPrivate is an explicit opt-out (config key "allow_private_endpoint")
// for operators running MinIO/Localstack/co-located object stores on a
// private network; it still rejects link-local/metadata addresses.
func ValidateEndpoint(endpoint string, allowPrivate bool) error {
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(normalizeEndpoint(endpoint))
	if err != nil {
		return fmt.Errorf("%w: parse %q: %v", ErrUnsafeEndpoint, endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q must be http or https", ErrUnsafeEndpoint, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing host in %q", ErrUnsafeEndpoint, endpoint)
	}

	// Resolve to IPs. A literal IP resolves to itself; a hostname is looked
	// up so an attacker cannot smuggle 169.254.169.254 behind a DNS name.
	var ips []netip.Addr
	if addr, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{addr}
	} else {
		resolved, lerr := net.LookupIP(host)
		if lerr != nil {
			return fmt.Errorf("%w: resolve host %q: %v", ErrUnsafeEndpoint, host, lerr)
		}
		for _, ip := range resolved {
			if a, ok := netip.AddrFromSlice(ip); ok {
				ips = append(ips, a.Unmap())
			}
		}
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: host %q resolved to no addresses", ErrUnsafeEndpoint, host)
	}
	for _, ip := range ips {
		if err := checkEndpointIP(ip, host, allowPrivate); err != nil {
			return err
		}
	}
	return nil
}

// checkEndpointIP rejects a single resolved address that is unsafe to dial.
func checkEndpointIP(ip netip.Addr, host string, allowPrivate bool) error {
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("%w: host %q resolves to unspecified address", ErrUnsafeEndpoint, host)
	case ip.IsMulticast():
		return fmt.Errorf("%w: host %q resolves to multicast address", ErrUnsafeEndpoint, host)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 / fe80::/10 — covers the 169.254.169.254 metadata
		// endpoint. Never allowed, even under allowPrivate.
		return fmt.Errorf("%w: host %q resolves to link-local address %s (cloud metadata)", ErrUnsafeEndpoint, host, ip)
	}
	if allowPrivate {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || !ip.IsGlobalUnicast() {
		return fmt.Errorf("%w: host %q resolves to private/internal address %s (set allow_private_endpoint=true to permit)", ErrUnsafeEndpoint, host, ip)
	}
	return nil
}

// isValidScheme checks whether s is a valid URI scheme per RFC 3986.
func isValidScheme(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
			// always valid
		case '0' <= c && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return false // must start with a letter
			}
		default:
			return false
		}
	}
	return true
}

// checkClosed returns ErrStoreClosed if the store has been closed.
func (s *Store) checkClosed() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	return nil
}

// fullKey returns the full S3 key for a given (already-formatted) object key.
func (s *Store) fullKey(blockKey string) string {
	return s.keyPrefix + blockKey
}

// hashKey returns the full S3 key for a CAS content hash.
func (s *Store) hashKey(hash block.ContentHash) string {
	return s.fullKey(block.FormatCASKey(hash))
}

// Put writes data under the CAS-shaped key derived from hash. The
// x-amz-meta-content-hash header is stamped atomically with the PUT
// the AWS SDK normalizes the metadata key to lowercase and
// prepends "x-amz-meta-" on the wire, so we pass the bare key
// "content-hash". The header value is the canonical "blake3:{hex}" form
// via ContentHash.CASKey().
func (s *Store) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.hashKey(hash)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
		Metadata: map[string]string{
			"content-hash": hash.CASKey(),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}

	return nil
}

// Get reads a complete object from S3 by content hash. Returns raw bytes
// WITHOUT BLAKE3 verification — production CAS reads should use
// ReadBlockVerified.
func (s *Store) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.hashKey(hash)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return readResponseBody(resp.Body, resp.ContentLength, maxBlockReadSize)
}

// ReadBlockVerified GETs the object at the CAS-derived key and verifies
// the body's BLAKE3 hash matches expected before returning bytes
//
// Two-stage verification (fail-closed twice)
//  1. Header pre-check: if the response carries x-amz-meta-content-hash
//     and it does not match expected, return ErrCASContentMismatch
//     BEFORE reading any body bytes (saves a doomed body transfer).
//  2. Streaming recompute: every body byte feeds a BLAKE3 hasher; on
//     EOF the accumulated digest is compared to expected. Mismatch
//     returns ErrCASContentMismatch and the buffer is discarded.
//
// Both hash arguments are intentional: hash derives the canonical CAS
// key, while expected is the body BLAKE3 the caller is asserting.
func (s *Store) ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.hashKey(hash)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get: %w", err)
	}

	// header pre-check. AWS SDK lower-cases user metadata keys and
	// strips the x-amz-meta- prefix. Memory store mirror uses the same
	// "content-hash" key.
	if hdr, ok := resp.Metadata["content-hash"]; ok && hdr != expected.CASKey() {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: header %q != expected %q",
			block.ErrCASContentMismatch, hdr, expected.CASKey())
	}

	// streaming recompute. The verifier owns Close on resp.Body —
	// it surfaces ErrCASContentMismatch if the caller closes before EOF.
	reader := newVerifyingReader(resp.Body, expected)
	data, readErr := readAllVerified(reader, resp.ContentLength, maxBlockReadSize)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

// GetRange reads a byte range from a CAS object using S3 range requests.
func (s *Store) GetRange(ctx context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	// Validate bounds before issuing the range request so callers get a
	// stable sentinel instead of an opaque S3 protocol error from a
	// malformed Range header. A past-EOF offset cannot be detected here
	// without a HEAD, so S3 surfaces its native 416 for that case (the
	// conformance suite only requires "any error" for offset >= EOF).
	if offset < 0 {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}

	// Guard against offset+length-1 overflowing int64 (which would emit a
	// negative, malformed Range header). offset >= 0 and length > 0 here.
	if length > math.MaxInt64-offset {
		return nil, block.ErrInvalidSize
	}

	key := s.hashKey(hash)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get range: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return readResponseBody(resp.Body, resp.ContentLength, length)
}

// readResponseBody reads the full body from an S3 response.
// When contentLength is known, pre-allocates exactly; otherwise uses fallbackSize.
func readResponseBody(body io.ReadCloser, contentLength *int64, fallbackSize int64) ([]byte, error) {
	if contentLength != nil && *contentLength > 0 {
		data := make([]byte, *contentLength)
		_, err := io.ReadFull(body, data)
		if err != nil {
			return nil, fmt.Errorf("read s3 object body: %w", err)
		}
		return data, nil
	}

	buf := bytes.NewBuffer(make([]byte, 0, fallbackSize))
	_, err := buf.ReadFrom(body)
	if err != nil {
		return nil, fmt.Errorf("read s3 object body: %w", err)
	}
	return buf.Bytes(), nil
}

// Has reports whether the CAS object addressed by hash exists in the
// bucket. Implements the block.BlockStore contract.
// Implemented via HEAD for cost and latency reasons (a Get with
// Range: bytes=0-0 would still transfer one byte body).
func (s *Store) Has(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := s.checkClosed(); err != nil {
		return false, err
	}
	key := s.hashKey(hash)
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3 head: %w", err)
	}
	return true, nil
}

// Head returns block.Meta for the CAS object addressed by hash
// without transferring the body. Returns block.ErrChunkNotFound on
// missing keys (same convention as Get).
//
// The x-amz-meta-content-hash header is NOT echoed in the returned
// Meta — the lookup key (ContentHash) is the input, not output. The
// header is still consulted internally by ReadBlockVerified for
// defense-in-depth.
func (s *Store) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	if err := s.checkClosed(); err != nil {
		return block.Meta{}, err
	}

	key := s.hashKey(hash)
	resp, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return block.Meta{}, block.ErrChunkNotFound
		}
		return block.Meta{}, fmt.Errorf("s3 head: %w", err)
	}

	out := block.Meta{}
	if resp.ContentLength != nil {
		out.Size = *resp.ContentLength
	}
	if resp.LastModified != nil {
		out.LastModified = *resp.LastModified
	}
	return out, nil
}

// Delete removes the CAS object addressed by hash. Delete is idempotent
// S3's DeleteObject succeeds with 204 even when the key is absent.
func (s *Store) Delete(ctx context.Context, hash block.ContentHash) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.hashKey(hash)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}

	return nil
}

// Walk enumerates every CAS object in the store. Iterates S3 listing
// pages under the cas/ prefix, parses each key via block.ParseCASKey
// (skipping non-CAS keys), and dispatches the callback with the parsed
// ContentHash and the per-object block.Meta. Honors
// block.ErrStopWalk for clean early exit; any other callback error
// halts and is wrapped as "walk halted at %s: %w".
// Context cancellation aborts immediately.
func (s *Store) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	fullPrefix := s.fullKey(casPrefix)

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3 walk: %w", err)
		}
		for _, obj := range page.Contents {
			if err := ctx.Err(); err != nil {
				return err
			}
			rawKey := ""
			if obj.Key != nil {
				rawKey = *obj.Key
			}
			// Strip the configured keyPrefix so ParseCASKey sees the
			// canonical "cas/..." shape.
			parseKey := rawKey
			if s.keyPrefix != "" && strings.HasPrefix(parseKey, s.keyPrefix) {
				parseKey = parseKey[len(s.keyPrefix):]
			}
			hash, perr := block.ParseCASKey(parseKey)
			if perr != nil {
				continue // non-CAS keys (legacy / sentinel) are skipped
			}
			meta := block.Meta{}
			if obj.Size != nil {
				meta.Size = *obj.Size
			}
			if obj.LastModified != nil {
				meta.LastModified = *obj.LastModified
			}
			if cberr := fn(hash, meta); cberr != nil {
				if errors.Is(cberr, block.ErrStopWalk) {
					return nil
				}
				return fmt.Errorf("walk halted at %s: %w", hash, cberr)
			}
		}
	}

	return nil
}

// Close marks the store as closed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	return nil
}

// HealthCheck verifies the S3 bucket is accessible.
//
// This is the legacy error-returning probe used internally by the
// syncer's HealthMonitor. Public callers should prefer Healthcheck
// (note the lowercase 'c') which returns a structured [health.Report]
// and satisfies the [health.Checker] interface.
func (s *Store) HealthCheck(ctx context.Context) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}

	return nil
}

// Healthcheck implements [health.Checker]: it wraps the existing
// HealthCheck error probe in a [health.Report] with measured latency.
// HeadBucket is the same call the syncer's HealthMonitor uses for its
// periodic probe, so the result reflects exactly what the runtime
// considers "remote reachable".
func (s *Store) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	err := s.HealthCheck(ctx)
	return health.ReportFromError(err, time.Since(start))
}

// isNotFoundError checks if an error is an S3 not found error.
// Uses proper AWS SDK error types first, falls back to string matching
// for non-standard S3-compatible services (e.g., MinIO, Localstack).
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	// Check for the typed AWS SDK error first.
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	// Fallback: some S3-compatible services return non-standard errors.
	errStr := err.Error()
	return strings.Contains(errStr, "NoSuchKey") ||
		strings.Contains(errStr, "NotFound") ||
		strings.Contains(errStr, "404")
}
