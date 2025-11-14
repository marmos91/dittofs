package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
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

	return &S3ContentStore{
		client:         cfg.Client,
		bucket:         cfg.Bucket,
		keyPrefix:      cfg.KeyPrefix,
		partSize:       partSize,
		uploadSessions: make(map[string]*multipartUpload),
	}, nil
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

// ============================================================================
// ContentStore Interface Implementation
// ============================================================================

// ReadContent returns a reader for the content identified by the given ID.
//
// This downloads the object from S3 and returns a reader for streaming the data.
// The caller is responsible for closing the returned ReadCloser.
//
// Context Cancellation:
// The S3 GetObject operation respects context cancellation. If the context is
// cancelled during download, the reader will return an error.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//
// Returns:
//   - io.ReadCloser: Reader for the content (must be closed by caller)
//   - error: Returns error if content not found, download fails, or context is cancelled
func (s *S3ContentStore) ReadContent(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := s.getObjectKey(id)

	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Check if object doesn't exist
		var notFound *types.NoSuchKey
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}

	return result.Body, nil
}

// GetContentSize returns the size of the content in bytes.
//
// This performs a HEAD request to S3 to retrieve object metadata without
// downloading the content.
//
// Context Cancellation:
// The S3 HeadObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - uint64: Size of the content in bytes
//   - error: Returns error if content not found, request fails, or context is cancelled
func (s *S3ContentStore) GetContentSize(ctx context.Context, id metadata.ContentID) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	key := s.getObjectKey(id)

	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NoSuchKey
		if errors.As(err, &notFound) {
			return 0, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}
		return 0, fmt.Errorf("failed to head object: %w", err)
	}

	if result.ContentLength == nil {
		return 0, fmt.Errorf("content length not available for %s", id)
	}

	return uint64(*result.ContentLength), nil
}

// ContentExists checks if content with the given ID exists in S3.
//
// This performs a HEAD request to check object existence without downloading.
//
// Context Cancellation:
// The S3 HeadObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to check
//
// Returns:
//   - bool: True if content exists, false otherwise
//   - error: Returns error for S3 failures or context cancellation (not for non-existent objects)
func (s *S3ContentStore) ContentExists(ctx context.Context, id metadata.ContentID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	key := s.getObjectKey(id)

	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NoSuchKey
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check object existence: %w", err)
	}

	return true, nil
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
func (s *S3ContentStore) GetStorageStats(ctx context.Context) (*content.StorageStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var totalSize uint64
	var objectCount uint64

	prefix := s.keyPrefix
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
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

	return &content.StorageStats{
		TotalSize:     maxUint64,
		UsedSize:      totalSize,
		AvailableSize: maxUint64,
		ContentCount:  objectCount,
		AverageSize:   averageSize,
	}, nil
}

// ============================================================================
// WritableContentStore Interface Implementation
// ============================================================================

// WriteAt writes data at the specified offset.
//
// For S3, this is implemented using read-modify-write:
//  1. If offset is 0: use PutObject directly
//  2. Otherwise: download existing object, modify, and re-upload
//
// WARNING: This is inefficient for large objects. For better performance with
// large files, use multipart upload APIs directly or write at offset 0.
//
// Context Cancellation:
// S3 operations respect context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Data to write
//   - offset: Byte offset where writing begins
//
// Returns:
//   - error: Returns error if write fails or context is cancelled
func (s *S3ContentStore) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	// Simple case - writing at offset 0
	if offset == 0 {
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(data),
		})
		if err != nil {
			return fmt.Errorf("failed to write object to S3: %w", err)
		}
		return nil
	}

	// Write at offset > 0 requires read-modify-write
	existingData := []byte{}
	exists, err := s.ContentExists(ctx, id)
	if err != nil {
		return err
	}

	if exists {
		reader, err := s.ReadContent(ctx, id)
		if err != nil {
			return err
		}
		defer reader.Close()

		existingData, err = io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("failed to read existing content: %w", err)
		}
	}

	// Extend existing data if needed
	requiredSize := offset + int64(len(data))
	if int64(len(existingData)) < requiredSize {
		newData := make([]byte, requiredSize)
		copy(newData, existingData)
		existingData = newData
	}

	// Write new data at offset
	copy(existingData[offset:], data)

	// Upload modified content
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(existingData),
	})
	if err != nil {
		return fmt.Errorf("failed to write modified object to S3: %w", err)
	}

	return nil
}

// Truncate changes the size of the content.
//
// For S3, this requires downloading the object, truncating/extending it, and re-uploading.
// This is inefficient for large objects.
//
// Context Cancellation:
// S3 operations respect context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - newSize: New size in bytes
//
// Returns:
//   - error: Returns error if truncate fails or context is cancelled
func (s *S3ContentStore) Truncate(ctx context.Context, id metadata.ContentID, newSize uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	exists, err := s.ContentExists(ctx, id)
	if err != nil {
		return err
	}

	if !exists {
		return fmt.Errorf("truncate failed for %s: %w", id, content.ErrContentNotFound)
	}

	currentSize, err := s.GetContentSize(ctx, id)
	if err != nil {
		return err
	}

	// No-op if size is already correct
	if currentSize == newSize {
		return nil
	}

	key := s.getObjectKey(id)

	if newSize < currentSize {
		// Truncate - download only the portion we need
		rangeStr := fmt.Sprintf("bytes=0-%d", newSize-1)
		result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Range:  aws.String(rangeStr),
		})
		if err != nil {
			return fmt.Errorf("failed to get object for truncate: %w", err)
		}
		defer result.Body.Close()

		data, err := io.ReadAll(result.Body)
		if err != nil {
			return fmt.Errorf("failed to read object for truncate: %w", err)
		}

		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(data),
		})
		if err != nil {
			return fmt.Errorf("failed to write truncated object: %w", err)
		}
	} else {
		// Extend - download existing and append zeros
		reader, err := s.ReadContent(ctx, id)
		if err != nil {
			return err
		}
		defer reader.Close()

		existingData, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("failed to read existing content: %w", err)
		}

		newData := make([]byte, newSize)
		copy(newData, existingData)

		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(newData),
		})
		if err != nil {
			return fmt.Errorf("failed to write extended object: %w", err)
		}
	}

	return nil
}

// Delete removes content from S3.
//
// This operation is idempotent - deleting non-existent content returns nil.
//
// Context Cancellation:
// The S3 DeleteObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to delete
//
// Returns:
//   - error: Returns error for S3 failures or context cancellation (not for non-existent objects)
func (s *S3ContentStore) Delete(ctx context.Context, id metadata.ContentID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object from S3: %w", err)
	}

	return nil
}

// WriteContent writes the entire content in one operation.
//
// This uses S3 PutObject for uploading the complete content.
//
// Context Cancellation:
// The S3 PutObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Complete content data
//
// Returns:
//   - error: Returns error if write fails or context is cancelled
func (s *S3ContentStore) WriteContent(ctx context.Context, id metadata.ContentID, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to write content to S3: %w", err)
	}

	return nil
}

// ============================================================================
// StreamingContentStore Interface Implementation
// ============================================================================

// OpenWriter returns a writer for streaming content writes.
//
// The returned writer buffers data in memory and uploads to S3 when closed.
// For very large files, consider using multipart uploads instead.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - io.WriteCloser: Writer for streaming writes (must be closed)
//   - error: Returns error if writer cannot be created
func (s *S3ContentStore) OpenWriter(ctx context.Context, id metadata.ContentID) (io.WriteCloser, error) {
	return &s3Writer{
		store:  s,
		ctx:    ctx,
		id:     id,
		buffer: &bytes.Buffer{},
	}, nil
}

// OpenReader returns a reader for streaming content reads.
//
// This is identical to ReadContent() for S3.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//
// Returns:
//   - io.ReadCloser: Reader for streaming reads (must be closed)
//   - error: Returns error if content not found or context is cancelled
func (s *S3ContentStore) OpenReader(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error) {
	return s.ReadContent(ctx, id)
}

// s3Writer implements io.WriteCloser for streaming writes to S3.
type s3Writer struct {
	store  *S3ContentStore
	ctx    context.Context
	id     metadata.ContentID
	buffer *bytes.Buffer
}

func (w *s3Writer) Write(p []byte) (n int, err error) {
	return w.buffer.Write(p)
}

func (w *s3Writer) Close() error {
	return w.store.WriteContent(w.ctx, w.id, w.buffer.Bytes())
}

// ============================================================================
// MultipartContentStore Interface Implementation
// ============================================================================

// multipartUpload tracks state for a multipart upload session.
type multipartUpload struct {
	uploadID       string
	completedParts []types.CompletedPart
	mu             sync.Mutex
}

// BeginMultipartUpload initiates a multipart upload session.
//
// This creates an S3 multipart upload and returns an upload ID for subsequent
// part uploads.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - string: Upload ID for this multipart upload session
//   - error: Returns error if upload cannot be initiated
func (s *S3ContentStore) BeginMultipartUpload(ctx context.Context, id metadata.ContentID) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	key := s.getObjectKey(id)

	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create multipart upload: %w", err)
	}

	uploadID := *result.UploadId

	s.uploadSessionsMu.Lock()
	s.uploadSessions[uploadID] = &multipartUpload{
		uploadID:       uploadID,
		completedParts: make([]types.CompletedPart, 0),
	}
	s.uploadSessionsMu.Unlock()

	return uploadID, nil
}

// UploadPart uploads one part of a multipart upload.
//
// Parts can be uploaded in parallel. Part numbers must be unique (1-10000).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - uploadID: Upload ID from BeginMultipartUpload
//   - partNumber: Part number (1-10000, must be unique)
//   - data: Part data
//
// Returns:
//   - error: Returns error if upload fails
func (s *S3ContentStore) UploadPart(ctx context.Context, id metadata.ContentID, uploadID string, partNumber int, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	result, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
		Body:       bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}

	s.uploadSessionsMu.RLock()
	upload, ok := s.uploadSessions[uploadID]
	s.uploadSessionsMu.RUnlock()

	if !ok {
		return fmt.Errorf("upload session %s not found", uploadID)
	}

	upload.mu.Lock()
	upload.completedParts = append(upload.completedParts, types.CompletedPart{
		ETag:       result.ETag,
		PartNumber: aws.Int32(int32(partNumber)),
	})
	upload.mu.Unlock()

	return nil
}

// CompleteMultipartUpload finalizes a multipart upload.
//
// This assembles all uploaded parts into the final content.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - uploadID: Upload ID from BeginMultipartUpload
//   - partNumbers: Ordered list of part numbers to assemble
//
// Returns:
//   - error: Returns error if completion fails
func (s *S3ContentStore) CompleteMultipartUpload(ctx context.Context, id metadata.ContentID, uploadID string, partNumbers []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.uploadSessionsMu.RLock()
	upload, ok := s.uploadSessions[uploadID]
	s.uploadSessionsMu.RUnlock()

	if !ok {
		return fmt.Errorf("upload session %s not found", uploadID)
	}

	upload.mu.Lock()
	completedParts := make([]types.CompletedPart, len(upload.completedParts))
	copy(completedParts, upload.completedParts)
	upload.mu.Unlock()

	// Sort parts by part number
	sort.Slice(completedParts, func(i, j int) bool {
		return *completedParts[i].PartNumber < *completedParts[j].PartNumber
	})

	key := s.getObjectKey(id)

	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	s.uploadSessionsMu.Lock()
	delete(s.uploadSessions, uploadID)
	s.uploadSessionsMu.Unlock()

	return nil
}

// AbortMultipartUpload cancels an in-progress multipart upload.
//
// This operation is idempotent.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - uploadID: Upload ID from BeginMultipartUpload
//
// Returns:
//   - error: Returns error for S3 failures or context cancellation
func (s *S3ContentStore) AbortMultipartUpload(ctx context.Context, id metadata.ContentID, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		// Check for NoSuchUpload error (idempotent behavior)
		var noSuchUpload *types.NoSuchUpload
		if errors.As(err, &noSuchUpload) {
			// Ignore - upload doesn't exist
		} else {
			return fmt.Errorf("failed to abort multipart upload: %w", err)
		}
	}

	s.uploadSessionsMu.Lock()
	delete(s.uploadSessions, uploadID)
	s.uploadSessionsMu.Unlock()

	return nil
}

// ============================================================================
// GarbageCollectableStore Interface Implementation
// ============================================================================

// ListAllContent returns all content IDs in the S3 bucket.
//
// This lists all objects with the configured key prefix and returns their
// content IDs (which are the full file paths).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - []metadata.ContentID: List of all content IDs (file paths)
//   - error: Returns error for S3 failures or context cancellation
func (s *S3ContentStore) ListAllContent(ctx context.Context) ([]metadata.ContentID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var contentIDs []metadata.ContentID

	prefix := s.keyPrefix
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}

			// Remove prefix to get content ID (which is the file path)
			key := *obj.Key
			if s.keyPrefix != "" && len(key) > len(s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}

			contentIDs = append(contentIDs, metadata.ContentID(key))
		}
	}

	return contentIDs, nil
}

// DeleteBatch removes multiple content items in one operation.
//
// S3 supports batch deletes of up to 1000 objects at a time. This implementation
// automatically chunks larger batches.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - ids: Content identifiers to delete
//
// Returns:
//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
//   - error: Returns error for catastrophic failures or context cancellation
func (s *S3ContentStore) DeleteBatch(ctx context.Context, ids []metadata.ContentID) (map[metadata.ContentID]error, error) {
	failures := make(map[metadata.ContentID]error)

	// S3 allows max 1000 objects per delete request
	const maxBatchSize = 1000

	for i := 0; i < len(ids); i += maxBatchSize {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(ids); j++ {
				failures[ids[j]] = ctx.Err()
			}
			return failures, ctx.Err()
		}

		end := i + maxBatchSize
		if end > len(ids) {
			end = len(ids)
		}

		batch := ids[i:end]

		// Build delete objects input
		objects := make([]types.ObjectIdentifier, len(batch))
		for j, id := range batch {
			key := s.getObjectKey(id)
			objects[j] = types.ObjectIdentifier{
				Key: aws.String(key),
			}
		}

		// Execute batch delete
		result, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{
				Objects: objects,
				Quiet:   aws.Bool(false),
			},
		})
		if err != nil {
			for _, id := range batch {
				failures[id] = err
			}
			continue
		}

		// Check for individual errors
		for _, deleteErr := range result.Errors {
			if deleteErr.Key == nil {
				continue
			}

			// Find the ContentID for this key (remove prefix to get path)
			key := *deleteErr.Key
			if s.keyPrefix != "" && len(key) > len(s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}

			id := metadata.ContentID(key)
			errMsg := "unknown error"
			if deleteErr.Code != nil && deleteErr.Message != nil {
				errMsg = fmt.Sprintf("%s: %s", *deleteErr.Code, *deleteErr.Message)
			}
			failures[id] = fmt.Errorf("%s", errMsg)
		}
	}

	return failures, nil
}
