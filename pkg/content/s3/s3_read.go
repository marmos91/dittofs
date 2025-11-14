// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains read operations for the S3 content store, including
// full content reads, range reads, size queries, and existence checks.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

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
func (s *S3ContentStore) ReadContent(ctx context.Context, id metadata.ContentID) (rc io.ReadCloser, err error) {
	start := time.Now()
	defer func() {
		s.metrics.ObserveOperation("ReadContent", time.Since(start), err)
	}()

	if err = ctx.Err(); err != nil {
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
			err = fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
			return nil, err
		}
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}

	// Wrap the body to track bytes read
	return &metricsReadCloser{
		ReadCloser: result.Body,
		metrics:    s.metrics,
		operation:  "read",
	}, nil
}

// ReadAt reads data from the specified offset without downloading the entire object.
//
// This uses S3 byte-range requests to efficiently read portions of large files.
// This is significantly more efficient than downloading the entire file when only
// a small portion is needed (e.g., NFS READ operations).
//
// Context Cancellation:
// The S3 GetObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//   - p: Buffer to read into
//   - offset: Byte offset to start reading from
//
// Returns:
//   - n: Number of bytes read
//   - error: Returns error if content not found, read fails, or context is cancelled
//     Returns io.EOF if offset is at or beyond end of content
func (s *S3ContentStore) ReadAt(ctx context.Context, id metadata.ContentID, p []byte, offset int64) (n int, err error) {
	start := time.Now()
	defer func() {
		s.metrics.ObserveOperation("ReadAt", time.Since(start), err)
		if n > 0 {
			s.metrics.RecordBytes("read", int64(n))
		}
	}()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if len(p) == 0 {
		return 0, nil
	}

	key := s.getObjectKey(id)

	// Build range request: "bytes=offset-end"
	// S3 range is inclusive, so end = offset + len(p) - 1
	end := offset + int64(len(p)) - 1
	rangeStr := fmt.Sprintf("bytes=%d-%d", offset, end)

	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeStr),
	})
	if err != nil {
		// Check if object doesn't exist
		var notFound *types.NoSuchKey
		if errors.As(err, &notFound) {
			return 0, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}

		// S3 returns InvalidRange error code for invalid ranges
		// This typically happens when offset is beyond the file size
		// Check if the error indicates an invalid range (offset beyond file size)
		if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "InvalidRange") {
			return 0, io.EOF
		}

		return 0, fmt.Errorf("failed to read from S3: %w", err)
	}
	defer func() { _ = result.Body.Close() }()

	// Read the data
	n, err = io.ReadFull(result.Body, p)
	if err == io.ErrUnexpectedEOF {
		// This happens if the object is smaller than requested range
		// Return what we got and no error (like io.ReaderAt)
		return n, nil
	}

	return n, err
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
