// Package s3 provides an S3-backed RemoteStore implementation.
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// maxBlockReadSize is the fallback pre-allocation size for ReadBlock when
// ContentLength is absent (e.g., chunked transfer). Matches block.Size (8 MB).
const maxBlockReadSize = 8 * 1024 * 1024

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
		Timeout:   0,
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
	// Look for "://" preceded by a valid URI scheme (RFC 3986: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." )).
	// We cannot use url.Parse alone because it misinterprets "host:port" as scheme "host".
	if i := strings.Index(endpoint, "://"); i > 0 {
		scheme := endpoint[:i]
		if isValidScheme(scheme) {
			return endpoint
		}
	}
	return "https://" + endpoint
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
		return blockstore.ErrStoreClosed
	}
	return nil
}

// fullKey returns the full S3 key for a block key.
func (s *Store) fullKey(blockKey string) string {
	return s.keyPrefix + blockKey
}

// WriteBlock writes a single block to S3.
func (s *Store) WriteBlock(ctx context.Context, blockKey string, data []byte) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.fullKey(blockKey)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("s3 put object: %w", err)
	}

	return nil
}

// WriteBlockWithHash implements RemoteStore (BSCAS-06). Sets
// x-amz-meta-content-hash on the PutObject. The AWS SDK normalizes the
// metadata key to lowercase and prepends "x-amz-meta-" on the wire, so we
// pass the bare key "content-hash". The header value is the canonical
// "blake3:{hex}" form via ContentHash.CASKey().
func (s *Store) WriteBlockWithHash(ctx context.Context, blockKey string, hash blockstore.ContentHash, data []byte) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.fullKey(blockKey)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
		Metadata: map[string]string{
			"content-hash": hash.CASKey(),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put object with hash: %w", err)
	}

	return nil
}

// ReadBlock reads a complete block from S3.
func (s *Store) ReadBlock(ctx context.Context, blockKey string) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.fullKey(blockKey)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, blockstore.ErrBlockNotFound
		}
		return nil, fmt.Errorf("s3 get object: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return readResponseBody(resp.Body, resp.ContentLength, maxBlockReadSize)
}

// ReadBlockVerified GETs the object at blockKey and verifies the body's
// BLAKE3 hash matches expected before returning bytes (INV-06).
//
// Two-stage verification (D-19 + D-18, fail-closed twice):
//  1. Header pre-check: if the response carries x-amz-meta-content-hash
//     and it does not match expected, return ErrCASContentMismatch
//     BEFORE reading any body bytes (saves a doomed body transfer).
//  2. Streaming recompute: every body byte feeds a BLAKE3 hasher; on
//     EOF the accumulated digest is compared to expected. Mismatch
//     returns ErrCASContentMismatch and the buffer is discarded.
func (s *Store) ReadBlockVerified(ctx context.Context, blockKey string, expected blockstore.ContentHash) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.fullKey(blockKey)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, blockstore.ErrBlockNotFound
		}
		return nil, fmt.Errorf("s3 get object: %w", err)
	}

	// D-19 header pre-check. AWS SDK lower-cases user metadata keys and
	// strips the x-amz-meta- prefix. Memory store mirror uses the same
	// "content-hash" key (BSCAS-06).
	if hdr, ok := resp.Metadata["content-hash"]; ok && hdr != expected.CASKey() {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: header %q != expected %q",
			blockstore.ErrCASContentMismatch, hdr, expected.CASKey())
	}

	// D-18 streaming recompute. The verifier owns Close on resp.Body —
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

// ReadBlockRange reads a byte range from a block using S3 range requests.
func (s *Store) ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.fullKey(blockKey)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, blockstore.ErrBlockNotFound
		}
		return nil, fmt.Errorf("s3 get object range: %w", err)
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

// CopyBlock copies a block from source to destination key using S3 server-side copy.
func (s *Store) CopyBlock(ctx context.Context, srcKey, dstKey string) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	fullSrcKey := s.fullKey(srcKey)
	fullDstKey := s.fullKey(dstKey)

	copySource := s.bucket + "/" + fullSrcKey

	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		CopySource: aws.String(copySource),
		Key:        aws.String(fullDstKey),
	})
	if err != nil {
		if isNotFoundError(err) {
			return blockstore.ErrBlockNotFound
		}
		return fmt.Errorf("s3 copy object: %w", err)
	}

	return nil
}

// DeleteBlock removes a single block from S3.
func (s *Store) DeleteBlock(ctx context.Context, blockKey string) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.fullKey(blockKey)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete object: %w", err)
	}

	return nil
}

// DeleteByPrefix removes all blocks with a given prefix using batch delete.
func (s *Store) DeleteByPrefix(ctx context.Context, prefix string) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	fullPrefix := s.fullKey(prefix)

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3 list objects: %w", err)
		}

		if len(page.Contents) == 0 {
			continue
		}

		objects := make([]types.ObjectIdentifier, len(page.Contents))
		for i, obj := range page.Contents {
			objects[i] = types.ObjectIdentifier{Key: obj.Key}
		}

		_, err = s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: objects},
		})
		if err != nil {
			return fmt.Errorf("s3 delete objects: %w", err)
		}
	}

	return nil
}

// ListByPrefix lists all block keys with a given prefix.
func (s *Store) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	fullPrefix := s.fullKey(prefix)
	var keys []string

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := *obj.Key
			if s.keyPrefix != "" && strings.HasPrefix(key, s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}
			keys = append(keys, key)
		}
	}

	return keys, nil
}

// ListByPrefixWithMeta lists all objects under prefix and surfaces the
// per-object metadata (Key, Size, LastModified). Used by the GC sweep
// phase to apply the snapshot - GracePeriod TTL filter (D-05). The
// returned Key has the configured keyPrefix stripped so it matches
// ListByPrefix and the keys passed to DeleteBlock.
func (s *Store) ListByPrefixWithMeta(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	fullPrefix := s.fullKey(prefix)
	out := make([]remote.ObjectInfo, 0)

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list objects with meta: %w", err)
		}

		for _, obj := range page.Contents {
			key := ""
			if obj.Key != nil {
				key = *obj.Key
			}
			if s.keyPrefix != "" && strings.HasPrefix(key, s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}
			info := remote.ObjectInfo{Key: key}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.LastModified != nil {
				info.LastModified = *obj.LastModified
			}
			out = append(out, info)
		}
	}

	return out, nil
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
