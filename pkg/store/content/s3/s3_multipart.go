// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains multipart upload operations for the S3 content store,
// enabling efficient uploads of large files in parallel parts.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

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
	ctx, span := telemetry.StartContentSpan(ctx, "begin_multipart_upload", string(id),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	if err := ctx.Err(); err != nil {
		telemetry.RecordError(ctx, err)
		return "", err
	}

	key := s.getObjectKey(id)

	start := time.Now()
	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})

	// Record metrics
	if s.metrics != nil {
		s.metrics.ObserveOperation("CreateMultipartUpload", time.Since(start), err)
	}

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

	// Record active upload
	if s.metrics != nil {
		s.metrics.RecordActiveUpload("s3", 1)
	}

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
	ctx, span := telemetry.StartContentSpan(ctx, "upload_part", string(id),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"),
		telemetry.FSCount(uint32(len(data))))
	defer span.End()

	if err := ctx.Err(); err != nil {
		telemetry.RecordError(ctx, err)
		return err
	}

	key := s.getObjectKey(id)
	dataSize := int64(len(data))

	start := time.Now()
	result, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
		Body:       bytes.NewReader(data),
	})

	// Record metrics
	if s.metrics != nil {
		s.metrics.ObserveOperation("UploadPart", time.Since(start), err)
		if err == nil {
			s.metrics.RecordBytes("UploadPart", dataSize)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}

	s.uploadSessionsMu.RLock()
	upload, ok := s.uploadSessions[uploadID]
	s.uploadSessionsMu.RUnlock()

	if !ok {
		return fmt.Errorf("upload session %s not found", uploadID)
	}

	// Lock before appending to prevent race condition
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
	ctx, span := telemetry.StartContentSpan(ctx, "complete_multipart_upload", string(id),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	if err := ctx.Err(); err != nil {
		telemetry.RecordError(ctx, err)
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

	start := time.Now()
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})

	// Record metrics
	if s.metrics != nil {
		s.metrics.ObserveOperation("CompleteMultipartUpload", time.Since(start), err)
	}

	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	s.uploadSessionsMu.Lock()
	delete(s.uploadSessions, uploadID)
	s.uploadSessionsMu.Unlock()

	// Record completed upload
	if s.metrics != nil {
		s.metrics.RecordActiveUpload("s3", -1)
	}

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

	start := time.Now()
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})

	// Record metrics (before error handling to capture all attempts)
	if s.metrics != nil {
		s.metrics.ObserveOperation("AbortMultipartUpload", time.Since(start), err)
	}

	if err != nil {
		// Ignore NoSuchUpload error (idempotent behavior)
		var noSuchUpload *types.NoSuchUpload
		if !errors.As(err, &noSuchUpload) {
			return fmt.Errorf("failed to abort multipart upload: %w", err)
		}
	}

	s.uploadSessionsMu.Lock()
	delete(s.uploadSessions, uploadID)
	s.uploadSessionsMu.Unlock()

	// Record aborted upload
	if s.metrics != nil {
		s.metrics.RecordActiveUpload("s3", -1)
	}

	return nil
}
