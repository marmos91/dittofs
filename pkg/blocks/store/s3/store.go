// Package s3 provides an S3-backed block store implementation.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/blocks/store"
)

// Config holds configuration for the S3 block store.
type Config struct {
	// Bucket is the S3 bucket name.
	Bucket string

	// Region is the AWS region (optional, uses SDK default if empty).
	Region string

	// Endpoint is the S3 endpoint URL (optional, for S3-compatible services).
	Endpoint string

	// KeyPrefix is prepended to all block keys (e.g., "blocks/").
	// Should end with "/" if non-empty.
	KeyPrefix string

	// MaxRetries is the maximum number of retry attempts for transient errors.
	MaxRetries int

	// ForcePathStyle forces path-style addressing (required for Localstack/MinIO).
	ForcePathStyle bool
}

// Store is an S3-backed implementation of store.BlockStore.
type Store struct {
	client    *s3.Client
	bucket    string
	keyPrefix string
	closed    bool
	mu        sync.RWMutex
}

// New creates a new S3 block store with an existing client.
func New(client *s3.Client, config Config) *Store {
	return &Store{
		client:    client,
		bucket:    config.Bucket,
		keyPrefix: config.KeyPrefix,
	}
}

// NewFromConfig creates a new S3 block store by creating an S3 client from config.
// This is the preferred constructor when you don't have an existing S3 client.
func NewFromConfig(ctx context.Context, config Config) (*Store, error) {
	// Build AWS SDK config options
	var opts []func(*awsconfig.LoadOptions) error

	if config.Region != "" {
		opts = append(opts, awsconfig.WithRegion(config.Region))
	}

	// Load AWS configuration
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Build S3 client options
	var s3Opts []func(*s3.Options)

	if config.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(config.Endpoint)
		})
	}

	if config.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	// Create S3 client
	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return New(client, config), nil
}

// fullKey returns the full S3 key for a block key.
func (s *Store) fullKey(blockKey string) string {
	return s.keyPrefix + blockKey
}

// WriteBlock writes a single block to S3.
func (s *Store) WriteBlock(ctx context.Context, blockKey string, data []byte) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return store.ErrStoreClosed
	}
	s.mu.RUnlock()

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

// ReadBlock reads a complete block from S3.
func (s *Store) ReadBlock(ctx context.Context, blockKey string) ([]byte, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrStoreClosed
	}
	s.mu.RUnlock()

	key := s.fullKey(blockKey)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, store.ErrBlockNotFound
		}
		return nil, fmt.Errorf("s3 get object: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read s3 object body: %w", err)
	}

	return data, nil
}

// ReadBlockRange reads a byte range from a block using S3 range requests.
func (s *Store) ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrStoreClosed
	}
	s.mu.RUnlock()

	key := s.fullKey(blockKey)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, store.ErrBlockNotFound
		}
		return nil, fmt.Errorf("s3 get object range: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read s3 object body: %w", err)
	}

	return data, nil
}

// DeleteBlock removes a single block from S3.
func (s *Store) DeleteBlock(ctx context.Context, blockKey string) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return store.ErrStoreClosed
	}
	s.mu.RUnlock()

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
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return store.ErrStoreClosed
	}
	s.mu.RUnlock()

	fullPrefix := s.fullKey(prefix)

	// List all objects with the prefix
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

		// Batch delete (up to 1000 per call)
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
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrStoreClosed
	}
	s.mu.RUnlock()

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
			// Strip the key prefix to return the block key
			key := *obj.Key
			if s.keyPrefix != "" && strings.HasPrefix(key, s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}
			keys = append(keys, key)
		}
	}

	return keys, nil
}

// Close marks the store as closed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	return nil
}

// HealthCheck verifies the S3 bucket is accessible.
// Performs a HeadBucket call to check connectivity and permissions.
func (s *Store) HealthCheck(ctx context.Context) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return store.ErrStoreClosed
	}
	s.mu.RUnlock()

	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}

	return nil
}

// isNotFoundError checks if an error is an S3 not found error.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	// Check for NoSuchKey error
	errStr := err.Error()
	return strings.Contains(errStr, "NoSuchKey") ||
		strings.Contains(errStr, "NotFound") ||
		strings.Contains(errStr, "404")
}

// Ensure Store implements store.BlockStore.
var _ store.BlockStore = (*Store)(nil)
