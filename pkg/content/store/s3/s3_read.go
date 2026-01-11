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
	"net"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// isRetryableError returns true if the error is transient and the operation should be retried.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Context errors are not retryable
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Network errors are retryable
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	// Check for AWS API errors
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()

		// Throttling errors - retryable
		if code == "Throttling" || code == "ThrottlingException" ||
			code == "RequestThrottled" || code == "SlowDown" ||
			code == "ProvisionedThroughputExceededException" {
			return true
		}

		// Server errors (5xx) - retryable
		if code == "InternalError" || code == "ServiceUnavailable" ||
			code == "ServiceException" || code == "InternalServiceException" {
			return true
		}

		// Not found, access denied, invalid request - not retryable
		if code == "NoSuchKey" || code == "NotFound" ||
			code == "AccessDenied" || code == "Forbidden" ||
			code == "InvalidRange" || code == "InvalidRequest" {
			return false
		}
	}

	// Check error message for common patterns
	errStr := err.Error()
	if strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "temporary failure") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "500") {
		return true
	}

	return false
}

// isNotFoundError returns true if the error indicates the object doesn't exist.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	// Check typed errors
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}

	// Check AWS API error code
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NoSuchKey" || code == "NotFound" || code == "404" {
			return true
		}
	}

	// Check error message for 404 patterns
	errStr := err.Error()
	return strings.Contains(errStr, "StatusCode: 404") ||
		strings.Contains(errStr, "NotFound") ||
		strings.Contains(errStr, "NoSuchKey")
}

// isInvalidRangeError returns true if the error indicates an invalid byte range.
func isInvalidRangeError(err error) bool {
	if err == nil {
		return false
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidRange"
	}

	return strings.Contains(err.Error(), "InvalidRange")
}

// calculateBackoff returns the backoff duration for a given attempt using the store's retry config.
func (s *S3ContentStore) calculateBackoff(attempt int) time.Duration {
	backoff := float64(s.retry.initialBackoff)
	for i := 0; i < attempt; i++ {
		backoff *= s.retry.backoffMultiplier
	}
	if backoff > float64(s.retry.maxBackoff) {
		backoff = float64(s.retry.maxBackoff)
	}
	return time.Duration(backoff)
}

// ReadContent returns a reader for the content identified by the given ID.
//
// This downloads the object from S3 and returns a reader for streaming the data.
// The caller is responsible for closing the returned ReadCloser.
//
// Retry Behavior:
// Transient errors (network issues, throttling, 5xx errors) are retried up to 3 times
// with exponential backoff. Not found (404) and access denied errors are not retried.
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
		if s.metrics != nil {
			s.metrics.ObserveOperation("ReadContent", time.Since(start), err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return nil, err
	}

	key := s.getObjectKey(id)

	var result *s3.GetObjectOutput
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("ReadContent: retrying", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, lastErr = s.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})

		if lastErr == nil {
			break
		}

		// Don't retry non-retryable errors
		if isNotFoundError(lastErr) {
			return nil, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}

		if !isRetryableError(lastErr) {
			break
		}

		logger.Debug("ReadContent: transient error", "attempt", attempt+1, "max_retries", s.retry.maxRetries+1, "key", key, "error", lastErr)
	}

	if lastErr != nil {
		return nil, fmt.Errorf("failed to get object from S3 after %d attempts: %w", s.retry.maxRetries+1, lastErr)
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
// Retry Behavior:
// Transient errors (network issues, throttling, 5xx errors) are retried up to 3 times
// with exponential backoff. Not found (404) and invalid range errors are not retried.
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
func (s *S3ContentStore) ReadAt(
	ctx context.Context,
	id metadata.ContentID,
	p []byte,
	offset uint64,
) (n int, err error) {
	ctx, span := telemetry.StartContentSpan(ctx, "read", string(id),
		telemetry.FSOffset(offset),
		telemetry.FSCount(uint32(len(p))),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.ObserveOperation("ReadAt", time.Since(start), err)
			if n > 0 {
				s.metrics.RecordBytes("read", int64(n))
			}
		}
		if err != nil {
			telemetry.RecordError(ctx, err)
		}
	}()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Empty buffer: nothing to read (follows io.ReaderAt semantics)
	if len(p) == 0 {
		return 0, nil
	}

	key := s.getObjectKey(id)

	// Build range request: "bytes=offset-end"
	// S3 range is inclusive, so end = offset + len(p) - 1
	end := offset + uint64(len(p)) - 1
	rangeStr := fmt.Sprintf("bytes=%d-%d", offset, end)

	var result *s3.GetObjectOutput
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("ReadAt: retrying", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key, "offset", offset)

			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, lastErr = s.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Range:  aws.String(rangeStr),
		})

		if lastErr == nil {
			break
		}

		// Don't retry non-retryable errors
		if isNotFoundError(lastErr) {
			return 0, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}

		if isInvalidRangeError(lastErr) {
			return 0, io.EOF
		}

		if !isRetryableError(lastErr) {
			break
		}

		logger.Debug("ReadAt: transient error", "attempt", attempt+1, "max_retries", s.retry.maxRetries+1, "key", key, "offset", offset, "error", lastErr)
	}

	if lastErr != nil {
		return 0, fmt.Errorf("failed to read from S3 after %d attempts: %w", s.retry.maxRetries+1, lastErr)
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
// Retry Behavior:
// Transient errors (network issues, throttling, 5xx errors) are retried up to 3 times
// with exponential backoff. Not found (404) errors are not retried.
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
func (s *S3ContentStore) GetContentSize(ctx context.Context, id metadata.ContentID) (size uint64, err error) {
	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.ObserveOperation("GetContentSize", time.Since(start), err)
		}
	}()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	key := s.getObjectKey(id)

	var result *s3.HeadObjectOutput
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("GetContentSize: retrying", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, lastErr = s.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})

		if lastErr == nil {
			break
		}

		// Don't retry non-retryable errors
		if isNotFoundError(lastErr) {
			return 0, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}

		if !isRetryableError(lastErr) {
			break
		}

		logger.Debug("GetContentSize: transient error", "attempt", attempt+1, "max_retries", s.retry.maxRetries+1, "key", key, "error", lastErr)
	}

	if lastErr != nil {
		return 0, fmt.Errorf("failed to head object after %d attempts: %w", s.retry.maxRetries+1, lastErr)
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
// Retry Behavior:
// Transient errors (network issues, throttling, 5xx errors) are retried up to 3 times
// with exponential backoff. Not found (404) errors are not retried but return (false, nil).
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
func (s *S3ContentStore) ContentExists(ctx context.Context, id metadata.ContentID) (exists bool, err error) {
	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.ObserveOperation("ContentExists", time.Since(start), err)
		}
	}()

	if err := ctx.Err(); err != nil {
		return false, err
	}

	key := s.getObjectKey(id)

	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("ContentExists: retrying", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(backoff):
			}
		}

		_, lastErr = s.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})

		if lastErr == nil {
			return true, nil
		}

		// Not found is not an error for existence check
		if isNotFoundError(lastErr) {
			return false, nil
		}

		if !isRetryableError(lastErr) {
			break
		}

		logger.Debug("ContentExists: transient error", "attempt", attempt+1, "max_retries", s.retry.maxRetries+1, "key", key, "error", lastErr)
	}

	return false, fmt.Errorf("failed to check object existence after %d attempts: %w", s.retry.maxRetries+1, lastErr)
}
