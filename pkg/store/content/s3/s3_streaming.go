// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains streaming operations for the S3 content store, including
// streaming readers and writers with automatic multipart upload support.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// OpenWriter returns a writer for streaming content writes.
//
// The returned writer uses multipart uploads for large files to avoid buffering
// the entire content in memory. Data is uploaded in parts as it's written, with
// automatic part management.
//
// Write Behavior:
//   - Small writes are buffered until partSize is reached
//   - Once a part is full, it's uploaded to S3 in the background
//   - Close() finalizes the multipart upload or does a simple PutObject for small content
//
// Memory Usage:
//   - Maximum memory = ~1x partSize (single buffer) plus any temporary allocations during upload
//   - Default partSize = 10MB, so ~10MB max memory per writer
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
		store:    s,
		ctx:      ctx,
		id:       id,
		buffer:   &bytes.Buffer{},
		partSize: s.partSize,
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
// It automatically uses multipart uploads for large files.
type s3Writer struct {
	store      *S3ContentStore
	ctx        context.Context
	id         metadata.ContentID
	buffer     *bytes.Buffer
	partSize   int64
	uploadID   string
	partNum    int
	totalBytes int64
	err        error
}

func (w *s3Writer) Write(p []byte) (n int, err error) {
	if w.err != nil {
		return 0, w.err
	}

	n, err = w.buffer.Write(p)
	if err != nil {
		w.err = err
		return n, err
	}

	w.totalBytes += int64(n)

	// If buffer has reached partSize, upload a part
	if int64(w.buffer.Len()) >= w.partSize {
		if err := w.uploadPart(); err != nil {
			w.err = err
			return n, err
		}
	}

	return n, nil
}

func (w *s3Writer) uploadPart() error {
	if w.buffer.Len() == 0 {
		return nil
	}

	// Start multipart upload on first part
	if w.uploadID == "" {
		uploadID, err := w.store.BeginMultipartUpload(w.ctx, w.id)
		if err != nil {
			return fmt.Errorf("failed to begin multipart upload: %w", err)
		}
		w.uploadID = uploadID
	}

	w.partNum++
	// Make a copy of the buffer data since we'll reset the buffer
	// and the underlying slice may be reused
	data := append([]byte(nil), w.buffer.Bytes()...)

	// Upload the part
	if err := w.store.UploadPart(w.ctx, w.id, w.uploadID, w.partNum, data); err != nil {
		// Abort the multipart upload on error with timeout to prevent hangs
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.store.AbortMultipartUpload(abortCtx, w.id, w.uploadID)
		return fmt.Errorf("failed to upload part %d: %w", w.partNum, err)
	}

	// Record metrics
	w.store.metrics.RecordBytes("write", int64(len(data)))

	// Clear buffer for next part
	w.buffer.Reset()

	return nil
}

func (w *s3Writer) Close() error {
	start := time.Now()
	defer func() {
		w.store.metrics.ObserveOperation("OpenWriter.Close", time.Since(start), w.err)
	}()

	if w.err != nil {
		// If there was an error during writes, abort any in-progress upload with timeout
		if w.uploadID != "" {
			abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = w.store.AbortMultipartUpload(abortCtx, w.id, w.uploadID)
		}
		return w.err
	}

	// If we never started a multipart upload, use simple PutObject
	if w.uploadID == "" {
		data := w.buffer.Bytes()
		err := w.store.WriteContent(w.ctx, w.id, data)
		if err != nil {
			w.err = err
			return err
		}
		w.store.metrics.RecordBytes("write", int64(len(data)))
		return nil
	}

	// Upload final part if there's remaining data
	if w.buffer.Len() > 0 {
		err := w.uploadPart()
		if err != nil {
			w.err = err
			return err
		}
	}

	// Complete the multipart upload
	// Complete the multipart upload using the tracked parts in the upload session.
	// Pass nil for partNumbers to use all parts tracked in the session
	err := w.store.CompleteMultipartUpload(w.ctx, w.id, w.uploadID, nil)
	if err != nil {
		w.err = err
		// Abort with timeout to prevent hangs
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.store.AbortMultipartUpload(abortCtx, w.id, w.uploadID)
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	return nil
}
