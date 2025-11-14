// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains write operations for the S3 content store, including
// full content writes, WriteAt, truncation, and deletion.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

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
		defer func() { _ = reader.Close() }()

		existingData, err = io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("failed to read existing content: %w", err)
		}
	}

	// WARNING: For sparse writes (small data at large offsets), this creates a large
	// allocation with mostly zero-filled space. Use multipart uploads for large files
	// or avoid sparse write patterns with S3.
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
		// Truncate - handle edge case where newSize == 0
		if newSize == 0 {
			// Overwrite object with empty file
			_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
				Body:   bytes.NewReader([]byte{}),
			})
			if err != nil {
				return fmt.Errorf("failed to write empty object for truncate: %w", err)
			}
		} else {
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
			defer func() { _ = result.Body.Close() }()

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
		}
		// Extend - download existing and append zeros
		reader, err := s.ReadContent(ctx, id)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()

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
