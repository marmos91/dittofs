// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains the main types, configuration, constructor, and helper methods
// for the S3 content store implementation.
package s3

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// S3ContentStore implements ContentStore using Amazon S3 or S3-compatible storage.
//
// This implementation provides:
//   - Full WritableContentStore support (read, write, delete)
//   - StreamingContentStore support for efficient uploads/downloads
//   - MultipartContentStore support for large files (>5MB)
//   - GarbageCollectableStore support for cleanup
//
// Path-Based Key Design:
//   - ContentID is the relative file path from share root
//   - Format: "shareName/path/to/file" (e.g., "export/docs/report.pdf")
//   - No leading "/" and no ":content" suffix
//   - S3 bucket mirrors the actual filesystem structure
//   - Enables metadata reconstruction from S3 (disaster recovery)
//   - Human-readable and inspectable S3 bucket contents
//
// S3 Characteristics:
//   - Object storage (no true random access like filesystem)
//   - Supports range reads (for partial reads)
//   - Multipart uploads for large files
//   - Eventually consistent (depending on S3 configuration)
//   - High durability and availability
//
// Implementation Details:
//   - WriteAt is implemented using read-modify-write for small files
//   - For large files, consider using multipart uploads directly
//   - No local caching (every read hits S3)
//   - Supports custom endpoint for S3-compatible storage (Cubbit DS3, etc.)
//
// Thread Safety:
// This implementation is safe for concurrent use by multiple goroutines.
// Concurrent writes to the same ContentID may result in last-write-wins
// behavior due to S3's eventual consistency model.
type S3ContentStore struct {
	client    *s3.Client
	bucket    string
	keyPrefix string // Optional prefix for all keys
	partSize  int64  // Size for multipart upload parts (default: 10MB)

	// Multipart upload state (per-instance)
	uploadSessions   map[string]*multipartUpload
	uploadSessionsMu sync.RWMutex

	// Storage stats cache (stores value instead of pointer to eliminate cloning)
	statsCache struct {
		stats     content.StorageStats
		hasStats  bool
		timestamp time.Time
		ttl       time.Duration
		mu        sync.RWMutex
	}

	// Metrics
	metrics S3Metrics
}

// S3ContentStoreConfig contains configuration for S3 content store.
type S3ContentStoreConfig struct {
	// Client is the configured S3 client
	Client *s3.Client

	// Bucket is the S3 bucket name
	Bucket string

	// KeyPrefix is an optional prefix for all object keys
	// Example: "dittofs/content/" results in keys like "dittofs/content/abc123"
	KeyPrefix string

	// PartSize is the size of each part for multipart uploads (default: 10MB)
	// Must be between 5MB and 5GB
	PartSize int64

	// StatsCacheTTL is the duration to cache storage stats (default: 5 minutes)
	// Set to 0 to use the default 5-minute TTL
	StatsCacheTTL time.Duration

	// Metrics is an optional metrics collector
	Metrics S3Metrics
}

// NewS3ContentStore creates a new S3-based content store.
//
// This initializes the S3 client and verifies bucket access. The bucket must
// already exist - this function does not create it.
//
// Context Cancellation:
// This operation checks the context before verifying bucket access.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - cfg: S3 configuration
//
// Returns:
//   - *S3ContentStore: Initialized S3 content store
//   - error: Returns error if bucket access fails or context is cancelled
func NewS3ContentStore(ctx context.Context, cfg S3ContentStoreConfig) (*S3ContentStore, error) {
	// ========================================================================
	// Step 1: Check context before S3 operations
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Validate configuration
	// ========================================================================

	if cfg.Client == nil {
		return nil, fmt.Errorf("S3 client is required")
	}

	if cfg.Bucket == "" {
		return nil, fmt.Errorf("bucket name is required")
	}

	// Set defaults
	partSize := cfg.PartSize
	if partSize == 0 {
		partSize = 10 * 1024 * 1024 // 10MB default
	}

	// Validate part size (S3 limits: 5MB to 5GB)
	if partSize < 5*1024*1024 {
		return nil, fmt.Errorf("part size must be at least 5MB, got %d bytes", partSize)
	}
	if partSize > 5*1024*1024*1024 {
		return nil, fmt.Errorf("part size must be at most 5GB, got %d bytes", partSize)
	}

	// ========================================================================
	// Step 3: Verify bucket access
	// ========================================================================

	_, err := cfg.Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(cfg.Bucket),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to access bucket %q: %w", cfg.Bucket, err)
	}

	// Set default stats cache TTL
	statsCacheTTL := cfg.StatsCacheTTL
	if statsCacheTTL == 0 {
		statsCacheTTL = 5 * time.Minute // Default: 5 minutes
	}

	// Set default metrics (no-op if not provided)
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = noopMetrics{}
	}

	store := &S3ContentStore{
		client:         cfg.Client,
		bucket:         cfg.Bucket,
		keyPrefix:      cfg.KeyPrefix,
		partSize:       partSize,
		uploadSessions: make(map[string]*multipartUpload),
		metrics:        metrics,
	}

	// Initialize stats cache
	store.statsCache.ttl = statsCacheTTL

	return store, nil
}

// getObjectKey returns the full S3 object key for a given content ID.
//
// Design Decision: Path-Based Keys
// ---------------------------------
// The ContentID is used directly as the S3 object key (with optional prefix).
// This means the S3 bucket mirrors the actual file structure, enabling:
//   - Easy inspection of S3 contents
//   - Metadata reconstruction from S3 (disaster recovery)
//   - Simple migration and backup strategies
//   - Human-readable S3 bucket structure
//
// ContentID Format:
//
//	The metadata store generates ContentID as: "shareName/path/to/file"
//	- No leading "/" (relative path)
//	- No ":content" suffix
//	- Share name included as root prefix
//
// Example:
//
//	ContentID:  "export/documents/report.pdf"
//	Key Prefix: "dittofs/"
//	S3 Key:     "dittofs/export/documents/report.pdf"
//
// Parameters:
//   - id: Content identifier (share-relative path)
//
// Returns:
//   - string: Full S3 object key
func (s *S3ContentStore) getObjectKey(id metadata.ContentID) string {
	// Use ContentID directly as the key (it should be the full file path)
	key := string(id)

	if s.keyPrefix != "" {
		return s.keyPrefix + key
	}

	return key
}

// GetStorageStats returns statistics about S3 storage.
//
// Note: For S3, storage stats are expensive to compute (requires listing all
// objects and summing sizes). This implementation returns approximate stats.
//
// For production use, consider:
//   - Using S3 CloudWatch metrics
//   - Maintaining stats in metadata store
//   - Caching stats with TTL
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - *content.StorageStats: Storage statistics
//   - error: Returns error for S3 failures or context cancellation
func (s *S3ContentStore) GetStorageStats(ctx context.Context) (stats *content.StorageStats, err error) {
	start := time.Now()
	defer func() {
		s.metrics.ObserveOperation("GetStorageStats", time.Since(start), err)
	}()

	if err = ctx.Err(); err != nil {
		return nil, err
	}

	// Check cache first
	s.statsCache.mu.RLock()
	if s.statsCache.hasStats && time.Since(s.statsCache.timestamp) < s.statsCache.ttl {
		cached := s.statsCache.stats
		s.statsCache.mu.RUnlock()
		return &cached, nil
	}
	s.statsCache.mu.RUnlock()

	// Cache miss or expired - compute stats
	var totalSize uint64
	var objectCount uint64

	prefix := s.keyPrefix
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			if obj.Size != nil {
				totalSize += uint64(*obj.Size)
			}
			objectCount++
		}
	}

	// S3 has effectively unlimited storage
	const maxUint64 = ^uint64(0)

	averageSize := uint64(0)
	if objectCount > 0 {
		averageSize = totalSize / objectCount
	}

	computedStats := content.StorageStats{
		TotalSize:     maxUint64,
		UsedSize:      totalSize,
		AvailableSize: maxUint64,
		ContentCount:  objectCount,
		AverageSize:   averageSize,
	}

	// Update cache - check again to prevent race condition
	s.statsCache.mu.Lock()
	// Double-check if another goroutine updated the cache while we were computing
	if s.statsCache.hasStats && time.Since(s.statsCache.timestamp) < s.statsCache.ttl {
		cached := s.statsCache.stats
		s.statsCache.mu.Unlock()
		return &cached, nil
	}
	s.statsCache.stats = computedStats
	s.statsCache.hasStats = true
	s.statsCache.timestamp = time.Now()
	s.statsCache.mu.Unlock()

	return &computedStats, nil
}
