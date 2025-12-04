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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// S3ContentStore implements ContentStore using Amazon S3 or S3-compatible storage.
//
// This implementation provides:
//   - Full ContentStore support (read, write, delete, truncate)
//   - ReadAtContentStore for efficient byte-range reads
//   - IncrementalWriteStore for parallel multipart uploads
//
// Path-Based Key Design:
//   - ContentID is the relative file path from share root
//   - Format: "shareName/path/to/file" (e.g., "export/docs/report.pdf")
//   - S3 bucket mirrors the actual filesystem structure
//   - Enables metadata reconstruction from S3 (disaster recovery)
//
// S3 Characteristics:
//   - Object storage (no true random access like filesystem)
//   - Supports range reads (ReadAt uses HTTP Range header)
//   - Multipart uploads for large files (>= partSize, default 5MB)
//   - Eventually consistent (depending on S3 configuration)
//
// Cache Integration:
//   - Optional cache can be injected via SetCache()
//   - Cache is used by IncrementalWriteStore for buffering writes
//   - When cache is nil, writes go directly to S3
//
// Thread Safety:
// This implementation is safe for concurrent use by multiple goroutines.
// Concurrent writes to the same ContentID may result in last-write-wins
// behavior due to S3's eventual consistency model.
type S3ContentStore struct {
	client    *s3.Client
	bucket    string
	keyPrefix string // Optional prefix for all keys
	partSize  uint64 // Size for multipart upload parts (default: 5MB)

	// Multipart upload state (per-instance)
	uploadSessions   map[string]*multipartUpload
	uploadSessionsMu sync.RWMutex

	// Cache for buffering writes (optional, injected via SetCache)
	cache cache.Cache

	// Max parallel part uploads (default: 4)
	maxParallelUploads uint

	// Retry configuration for transient errors
	retry retryConfig

	// Cached storage stats (avoids repeated S3 ListObjects calls)
	cachedStats struct {
		stats     content.StorageStats
		valid     bool          // True if stats have been computed at least once
		timestamp time.Time     // When stats were last computed
		ttl       time.Duration // How long to cache stats
		mu        sync.RWMutex
	}

	// Metrics
	metrics S3Metrics

	// Buffered deletion queue for batching delete operations
	deletionQueue struct {
		enabled         bool                 // Whether buffered deletion is enabled (default: true)
		queue           []metadata.ContentID // Pending deletions
		mu              sync.Mutex           // Protects queue
		flushInterval   time.Duration        // How often to batch process (default: 2s)
		batchSize       uint                 // Trigger flush when this many items queued (default: 100)
		shutdownTimeout time.Duration        // Max time to wait for worker to finish on shutdown (default: 60s)
		stopCh          chan struct{}        // Signal to stop background worker
		flushCh         chan struct{}        // Signal to trigger immediate flush
		doneCh          chan struct{}        // Signal when worker stopped
		closeOnce       sync.Once            // Ensures Close() only executes once
	}
}

// retryConfig holds retry settings for S3 operations.
type retryConfig struct {
	maxRetries        uint          // Maximum number of retry attempts (default: 3)
	initialBackoff    time.Duration // Initial backoff duration (default: 100ms)
	maxBackoff        time.Duration // Maximum backoff duration (default: 2s)
	backoffMultiplier float64       // Backoff multiplier (default: 2.0)
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

	// PartSize controls multipart upload behavior:
	// - Files smaller than PartSize use PutObject (single request)
	// - Files >= PartSize use multipart upload with parts of this size
	// - During incremental writes, UploadPart is called when buffer reaches PartSize
	// - Only the final part can be smaller than PartSize
	// Must be between 5MB and 5GB. Default: 5MB.
	PartSize uint64

	// MaxParallelUploads is the maximum number of concurrent part uploads (default: 4).
	// Higher values improve throughput for large files but use more memory and connections.
	MaxParallelUploads uint

	// StatsCacheTTL is the duration to cache storage stats (default: 5 minutes)
	// Set to 0 to use the default 5-minute TTL
	StatsCacheTTL time.Duration

	// Metrics is an optional metrics collector
	Metrics S3Metrics

	// BufferedDeletionEnabled enables buffered deletion for batch processing (default: false)
	// When enabled, Delete() calls are queued and processed in batches
	BufferedDeletionEnabled bool

	// DeletionFlushInterval is how often to flush pending deletions (default: 2s)
	// Set to 0 to use the default 2-second interval
	DeletionFlushInterval time.Duration

	// DeletionBatchSize triggers flush when this many items are queued (default: 100)
	// Set to 0 to use the default batch size of 100
	DeletionBatchSize uint

	// DeletionShutdownTimeout is the maximum time to wait for deletion worker to finish on shutdown (default: 60s)
	// For large deletion queues or slow S3 connections, increase this value
	// Set to 0 to use the default 60-second timeout
	DeletionShutdownTimeout time.Duration

	// Cache is an optional cache for buffering writes before flushing to S3.
	// When provided, enables efficient write buffering and reduces S3 API calls.
	// Can be nil if caching is not configured for this share.
	Cache cache.Cache

	// MaxRetries is the maximum number of retry attempts for transient errors (default: 3).
	// Set to 0 to disable retries.
	MaxRetries uint

	// InitialBackoff is the initial backoff duration before first retry (default: 100ms).
	// Subsequent retries use exponential backoff up to MaxBackoff.
	InitialBackoff time.Duration

	// MaxBackoff is the maximum backoff duration between retries (default: 2s).
	MaxBackoff time.Duration

	// BackoffMultiplier is the multiplier for exponential backoff (default: 2.0).
	// Each retry waits: min(InitialBackoff * (BackoffMultiplier ^ attempt), MaxBackoff)
	BackoffMultiplier float64
}

// NewS3ClientFromConfig creates an S3 client from configuration parameters.
// This is a helper function for creating S3 clients from YAML configuration.
func NewS3ClientFromConfig(
	ctx context.Context,
	endpoint,
	region,
	accessKeyID,
	secretAccessKey string,
	forcePathStyle bool,
) (*s3.Client, error) {
	// Build AWS config with credentials
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKeyID,
			secretAccessKey,
			"", // session token (empty for static credentials)
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client with options
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = forcePathStyle
	})

	return client, nil
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

	// Set defaults for partSize (used for both threshold and part size)
	partSize := cfg.PartSize
	if partSize == 0 {
		partSize = 5 * 1024 * 1024 // 5MB default (S3 minimum)
	}

	// Validate part size (S3 limits: 5MB to 5GB)
	if partSize < 5*1024*1024 {
		return nil, fmt.Errorf("part size must be at least 5MB, got %d bytes", partSize)
	}
	if partSize > 5*1024*1024*1024 {
		return nil, fmt.Errorf("part size must be at most 5GB, got %d bytes", partSize)
	}

	// Set default max parallel uploads
	maxParallelUploads := cfg.MaxParallelUploads
	if maxParallelUploads == 0 {
		maxParallelUploads = 4 // Default: 4 concurrent uploads
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

	// Apply deletion queue defaults
	deletionFlushInterval := cfg.DeletionFlushInterval
	if deletionFlushInterval == 0 {
		deletionFlushInterval = 2 * time.Second
	}
	deletionBatchSize := cfg.DeletionBatchSize
	if deletionBatchSize == 0 {
		deletionBatchSize = 100
	}
	deletionShutdownTimeout := cfg.DeletionShutdownTimeout
	if deletionShutdownTimeout == 0 {
		deletionShutdownTimeout = 60 * time.Second
	}

	// Apply retry config defaults
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3 // Default: 3 retries
	}
	initialBackoff := cfg.InitialBackoff
	if initialBackoff == 0 {
		initialBackoff = 100 * time.Millisecond // Default: 100ms
	}
	maxBackoff := cfg.MaxBackoff
	if maxBackoff == 0 {
		maxBackoff = 2 * time.Second // Default: 2s
	}
	backoffMultiplier := cfg.BackoffMultiplier
	if backoffMultiplier == 0 {
		backoffMultiplier = 2.0 // Default: 2x
	}

	store := &S3ContentStore{
		client:             cfg.Client,
		bucket:             cfg.Bucket,
		keyPrefix:          cfg.KeyPrefix,
		partSize:           partSize,
		maxParallelUploads: maxParallelUploads,
		uploadSessions:     make(map[string]*multipartUpload),
		metrics:            cfg.Metrics,
		cache:              cfg.Cache,
		retry: retryConfig{
			maxRetries:        maxRetries,
			initialBackoff:    initialBackoff,
			maxBackoff:        maxBackoff,
			backoffMultiplier: backoffMultiplier,
		},
	}

	// Initialize cached stats
	store.cachedStats.ttl = statsCacheTTL

	// Initialize deletion queue
	store.deletionQueue.enabled = cfg.BufferedDeletionEnabled
	store.deletionQueue.flushInterval = deletionFlushInterval
	store.deletionQueue.batchSize = deletionBatchSize
	store.deletionQueue.shutdownTimeout = deletionShutdownTimeout
	store.deletionQueue.queue = make([]metadata.ContentID, 0, deletionBatchSize)
	store.deletionQueue.stopCh = make(chan struct{})
	store.deletionQueue.flushCh = make(chan struct{}, 1)
	store.deletionQueue.doneCh = make(chan struct{})

	// Start background deletion worker if buffering is enabled
	if store.deletionQueue.enabled {
		go store.deletionWorker()
	}

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

// SetCache injects a cache into the S3ContentStore.
//
// This allows the registry to provide a shared cache after the content store
// has been created. The cache is used by IncrementalWriteStore methods to
// read buffered data and upload it to S3.
//
// Parameters:
//   - c: The cache to use for buffering writes (can be nil to disable)
func (s *S3ContentStore) SetCache(c cache.Cache) {
	s.cache = c
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
		if s.metrics != nil {
			s.metrics.ObserveOperation("GetStorageStats", time.Since(start), err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return nil, err
	}

	// Check cached stats first
	s.cachedStats.mu.RLock()
	if s.cachedStats.valid && time.Since(s.cachedStats.timestamp) < s.cachedStats.ttl {
		cached := s.cachedStats.stats
		s.cachedStats.mu.RUnlock()
		return &cached, nil
	}
	s.cachedStats.mu.RUnlock()

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

	// Update cached stats - check again to prevent race condition
	s.cachedStats.mu.Lock()
	// Double-check if another goroutine updated while we were computing
	if s.cachedStats.valid && time.Since(s.cachedStats.timestamp) < s.cachedStats.ttl {
		cached := s.cachedStats.stats
		s.cachedStats.mu.Unlock()
		return &cached, nil
	}
	s.cachedStats.stats = computedStats
	s.cachedStats.valid = true
	s.cachedStats.timestamp = time.Now()
	s.cachedStats.mu.Unlock()

	return &computedStats, nil
}
