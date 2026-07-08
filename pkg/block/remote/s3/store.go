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
	"sync/atomic"
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

// s3HTTPRequestTimeout bounds the entire HTTP request lifecycle (connect +
// request + body read). Prevents a hung S3 endpoint from pinning syncer
// goroutines indefinitely; without it Close()/DrainAllUploads cannot honour
// their drain deadlines.
const s3HTTPRequestTimeout = 2 * time.Minute

// maxS3ConnsPerHost sizes the HTTP connection pool so it never caps the
// syncer's upload concurrency (#1407): it matches the maximum a user can pin
// via --parallel-uploads (validateParallelUploads allows up to 256) and so also
// covers the adaptive ceiling (engine.AdaptiveUploadCeiling, 64) plus concurrent
// downloads. 256 conns are created on demand, not preallocated. Defined locally
// rather than imported from engine to avoid a dependency cycle.
const maxS3ConnsPerHost = 256

// Compile-time interface satisfaction check.
var (
	_ remote.RemoteStore       = (*Store)(nil)
	_ remote.RemoteBlockStore  = (*Store)(nil)
	_ remote.ChunkReader       = (*Store)(nil)
	_ remote.ChunkSealer       = (*Store)(nil)
	_ block.DurabilityReporter = (*Store)(nil)
)

// blockObjectPrefix is the S3 object-key prefix walked by WalkBlocks
// (block.FormatBlockKey output, "blocks/<blockID>").
const blockObjectPrefix = "blocks/"

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

	// durable reports whether accepted bytes survive a crash/restart
	// (block.DurabilityReporter). S3 object storage is durable, so the type
	// default is true; set via SetDurable from the controlplane config.
	durable atomic.Bool
}

// New creates a new S3 remote block store with an existing client.
func New(client *s3.Client, config Config) *Store {
	s := &Store{
		client:    client,
		bucket:    config.Bucket,
		keyPrefix: config.KeyPrefix,
	}
	s.durable.Store(true)
	return s
}

// Durable reports whether accepted bytes survive a crash/restart
// (block.DurabilityReporter). S3 is durable object storage, so the type
// default is true.
func (s *Store) Durable() bool {
	return s.durable.Load()
}

// SetDurable overrides the type-default durability of this store, applied by
// the controlplane when the per-store config carries an explicit "durable".
func (s *Store) SetDurable(durable bool) {
	s.durable.Store(durable)
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

	// Configure HTTP client for parallel uploads. The pool must not cap the
	// syncer's upload window below its ceiling, or it becomes the hidden
	// bottleneck: the adaptive controller ramps to engine.AdaptiveUploadCeiling
	// (64) and downloads run concurrently on the same host, so size for both
	// (128 conns x ~512KB buffers ≈ 64MB worst case, created on demand). A
	// pinned --parallel-uploads above this still caps here (#1407 follow-up).
	httpTransport := &http.Transport{
		MaxIdleConns:        maxS3ConnsPerHost,
		MaxIdleConnsPerHost: maxS3ConnsPerHost,
		MaxConnsPerHost:     maxS3ConnsPerHost,
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

	// Blocks are BLAKE3-verified end-to-end (sealed by the carver, re-verified
	// on read), so the SDK's flexible-checksum layer is redundant work. At the
	// aws-sdk-go-v2 default (WhenSupported) every PutObject is wrapped in an
	// aws-chunked streaming-trailer encoding with a full CRC32 pass over each
	// ~16 MiB block — extra CPU and wire framing, and a known interop friction
	// point with non-AWS S3 endpoints. WhenRequired restores clean
	// Content-Length PUTs (the block body is a seekable *bytes.Reader, so its
	// length is known) and skips the redundant checksum work on PUT and GET.
	s3Opts = append(s3Opts, func(o *s3.Options) {
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

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

// endpointResolveTimeout bounds the DNS lookup ValidateEndpoint performs for a
// hostname endpoint so a hung resolver cannot stall the config-create path.
const endpointResolveTimeout = 5 * time.Second

// ValidateEndpoint rejects S3 endpoints that point at addresses an attacker
// could use to pivot the server into the internal network (SSRF). It runs
// at config-create time, before any HealthCheck/HeadBucket fires, so a
// malicious "endpoint":"http://169.254.169.254/..." is refused before the
// S3 SDK ever issues a request.
//
// An empty endpoint is allowed (the SDK uses the real AWS endpoint, which
// is public). Otherwise the endpoint is normalized, its host is resolved,
// and EVERY resolved IP is checked. We reject UNCONDITIONALLY:
//   - unspecified / multicast addresses,
//   - link-local unicast (169.254.0.0/16, fe80::/10) — covers the cloud
//     metadata endpoint 169.254.169.254.
//
// We additionally reject loopback, private / unique-local (RFC1918,
// fc00::/7), and other non-global-unicast space UNLESS allowPrivate is set.
// allowPrivate (config key "allow_private_endpoint") is an explicit opt-out
// for operators running MinIO/Localstack/co-located object stores on a
// private network; it still rejects the unconditional set above (so the
// metadata endpoint is never reachable).
//
// DNS resolution for a hostname endpoint is bounded by endpointResolveTimeout
// so a slow/hung resolver cannot stall the config-create path indefinitely.
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
		ctx, cancel := context.WithTimeout(context.Background(), endpointResolveTimeout)
		defer cancel()
		resolved, lerr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if lerr != nil {
			return fmt.Errorf("%w: resolve host %q: %v", ErrUnsafeEndpoint, host, lerr)
		}
		for _, ipa := range resolved {
			if a, ok := netip.AddrFromSlice(ipa.IP); ok {
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

// blockKey returns the full S3 key for a block object identified by blockID.
func (s *Store) blockKey(blockID string) string {
	return s.fullKey(block.FormatBlockKey(blockID))
}

// SealChunk implements remote.ChunkSealer as the identity transform. As a base
// store there is no per-chunk transform to apply — bodies are stored verbatim —
// so the wire bytes equal the plaintext. The compression/encryption decorators
// wrap this to seal their own layer. A defensive copy is returned so the carver
// may retain it independently of the caller's plaintext buffer. hash is unused
// at this layer.
func (s *Store) SealChunk(_ context.Context, _ block.ContentHash, plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	copy(out, plaintext)
	return out, nil
}

// ReadChunk reads the wire bytes [offset, offset+length) from the block object
// blocks/<blockID> via an S3 range request and returns them verbatim. As a base
// store there is no transform to invert and no verification here (the engine
// verifies the BLAKE3 after the decorator stack). Implements
// remote.ChunkReader; hash is unused at this layer. See GetRange for the
// bounds-validation rationale.
func (s *Store) ReadChunk(ctx context.Context, blockID string, offset, length int64, _ block.ContentHash) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}
	if length > math.MaxInt64-offset {
		return nil, block.ErrInvalidSize
	}

	key := s.blockKey(blockID)
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
		return nil, fmt.Errorf("s3 get block chunk: %w", err)
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

	// Content-Length absent or zero: grow the buffer as we read. Cap the eager
	// preallocation so a missing/lying header can't drive an oversized up-front
	// allocation from an attacker- or caller-supplied range length; ReadFrom
	// still grows the buffer to whatever the body actually contains.
	prealloc := fallbackSize
	if prealloc < 0 {
		prealloc = 0
	}
	if prealloc > maxFallbackPrealloc {
		prealloc = maxFallbackPrealloc
	}
	buf := bytes.NewBuffer(make([]byte, 0, prealloc))
	_, err := buf.ReadFrom(body)
	if err != nil {
		return nil, fmt.Errorf("read s3 object body: %w", err)
	}
	return buf.Bytes(), nil
}

// maxFallbackPrealloc bounds the up-front buffer reserved when an S3 response
// omits Content-Length. 1 MiB is large enough to absorb most single-chunk
// reads without reallocation, small enough that a bogus header cannot force a
// large speculative allocation.
const maxFallbackPrealloc = 1 << 20

// PutBlock writes the content of r under blocks/<blockID> via S3 PutObject.
// Implements remote.RemoteBlockStore. Idempotent: a second call overwrites
// silently. r is streamed directly to S3; the SDK uses chunked transfer
// encoding when ContentLength is not set.
func (s *Store) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	key := s.blockKey(blockID)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("s3 put block %s: %w", blockID, err)
	}
	return nil
}

// GetBlock returns the full bytes of the block object identified by blockID.
// Returns block.ErrChunkNotFound when the block is absent.
func (s *Store) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}
	key := s.blockKey(blockID)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get block %s: %w", blockID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponseBody(resp.Body, resp.ContentLength, maxBlockReadSize)
}

// GetBlockRange returns [offset, offset+length) bytes of the block object
// identified by blockID via an S3 ranged GET. Bounds semantics mirror
// GetRange: ErrInvalidOffset for a negative offset and ErrInvalidSize for
// non-positive length. A past-EOF offset cannot be detected here without a
// HEAD, so S3 surfaces a native error (416) instead; past-EOF length is
// clamped by S3 (partial content).
func (s *Store) GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}
	if length > math.MaxInt64-offset {
		return nil, block.ErrInvalidSize
	}
	key := s.blockKey(blockID)
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
		return nil, fmt.Errorf("s3 get block range %s: %w", blockID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponseBody(resp.Body, resp.ContentLength, length)
}

// DeleteBlock removes the block object keyed by blockID. Idempotent: S3's
// DeleteObject succeeds with 204 even when the key is absent.
func (s *Store) DeleteBlock(ctx context.Context, blockID string) error {
	if err := s.checkClosed(); err != nil {
		return err
	}
	key := s.blockKey(blockID)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete block %s: %w", blockID, err)
	}
	return nil
}

// WalkBlocks enumerates every block object in the store. Iterates S3 listing
// pages under the blocks/ prefix, strips the prefix to recover the blockID,
// and dispatches the callback with the blockID and block.Meta. Honors
// block.ErrStopWalk for clean early exit; any other callback error halts and
// is wrapped as "walk halted at <blockID>: %w". Context cancellation aborts.
func (s *Store) WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	fullPrefix := s.fullKey(blockObjectPrefix)

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
			return fmt.Errorf("s3 walk blocks: %w", err)
		}
		for _, obj := range page.Contents {
			if err := ctx.Err(); err != nil {
				return err
			}
			rawKey := ""
			if obj.Key != nil {
				rawKey = *obj.Key
			}
			// Strip keyPrefix so we see the canonical "blocks/<blockID>" shape,
			// then strip the "blocks/" prefix to recover the bare blockID.
			parseKey := rawKey
			if s.keyPrefix != "" && strings.HasPrefix(parseKey, s.keyPrefix) {
				parseKey = parseKey[len(s.keyPrefix):]
			}
			if !strings.HasPrefix(parseKey, blockObjectPrefix) {
				continue // skip non-block keys that share the prefix
			}
			blockID := parseKey[len(blockObjectPrefix):]
			if blockID == "" {
				continue // skip the prefix key itself if it were ever stored
			}
			meta := block.Meta{}
			if obj.Size != nil {
				meta.Size = *obj.Size
			}
			if obj.LastModified != nil {
				meta.LastModified = *obj.LastModified
			}
			if cberr := fn(blockID, meta); cberr != nil {
				if errors.Is(cberr, block.ErrStopWalk) {
					return nil
				}
				return fmt.Errorf("walk halted at %s: %w", blockID, cberr)
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
