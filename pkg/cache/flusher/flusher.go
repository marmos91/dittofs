// Package flusher implements background flushing of idle cache entries.
//
// In NFS, there's no "close" operation - the server never knows when a client
// is done writing. The flusher solves this by detecting files that haven't
// been written to recently and completing their upload to the content store.
package flusher

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Configuration defaults
const (
	// defaultSweepInterval is how often the flusher checks for idle files.
	defaultSweepInterval = 10 * time.Second

	// defaultFlushTimeout is how long a file must be idle before flushing.
	// This is the key NFS async write timeout - files are considered "done"
	// when no writes have occurred for this duration.
	defaultFlushTimeout = 30 * time.Second

	// defaultFlushPoolSize is the maximum number of files to flush in parallel.
	// This improves throughput when multiple files are ready to be finalized.
	defaultFlushPoolSize = 4
)

// BackgroundFlusher runs in the background to detect and flush idle files.
//
// The flusher:
//   - Runs periodically (default: every 10 seconds)
//   - Checks each cache entry for idle status
//   - Validates cache size matches expected file size before completing
//   - Completes incremental uploads (S3 multipart) for fully flushed files
//   - Aborts incomplete uploads (e.g., interrupted copies)
//   - Transitions entries from StateUploading to StateCached
//   - Processes multiple files in parallel for better throughput
type BackgroundFlusher struct {
	cache         cache.Cache
	contentStore  content.ContentStore
	metadataStore metadata.MetadataStore
	sweepInterval time.Duration
	flushTimeout  time.Duration
	flushPoolSize int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Config holds configuration for the flusher.
type Config struct {
	// SweepInterval is how often to check for idle files (default: 10s)
	SweepInterval time.Duration

	// FlushTimeout is how long a file must be idle before flushing (default: 30s)
	FlushTimeout time.Duration

	// FlushPoolSize is how many files to flush in parallel (default: 4)
	FlushPoolSize int
}

// New creates a new background flusher.
//
// The flusher will not start until Start() is called.
//
// Parameters:
//   - c: The cache to monitor for idle files
//   - contentStore: The content store to flush uploads to
//   - metadataStore: The metadata store to validate file sizes (optional, nil disables validation)
//   - config: Optional configuration (nil for defaults)
func New(
	c cache.Cache,
	contentStore content.ContentStore,
	metadataStore metadata.MetadataStore,
	config *Config,
) *BackgroundFlusher {
	sweepInterval := defaultSweepInterval
	flushTimeout := defaultFlushTimeout
	flushPoolSize := defaultFlushPoolSize

	if config != nil {
		if config.SweepInterval > 0 {
			sweepInterval = config.SweepInterval
		}
		if config.FlushTimeout > 0 {
			flushTimeout = config.FlushTimeout
		}
		if config.FlushPoolSize > 0 {
			flushPoolSize = config.FlushPoolSize
		}
	}

	return &BackgroundFlusher{
		cache:         c,
		contentStore:  contentStore,
		metadataStore: metadataStore,
		sweepInterval: sweepInterval,
		flushTimeout:  flushTimeout,
		flushPoolSize: flushPoolSize,
	}
}

// Start begins the background flusher goroutine.
//
// The flusher will run until Stop() is called or the context is cancelled.
func (f *BackgroundFlusher) Start(ctx context.Context) {
	f.ctx, f.cancel = context.WithCancel(ctx)

	logger.Info("Background flusher started: sweep_interval=%s, flush_timeout=%s, pool_size=%d",
		f.sweepInterval, f.flushTimeout, f.flushPoolSize)

	f.wg.Add(1)
	go f.run()
}

// Stop gracefully stops the flusher.
//
// This blocks until the flusher goroutine has exited.
func (f *BackgroundFlusher) Stop() {
	if f.cancel != nil {
		f.cancel()
	}
	f.wg.Wait()
}

// run is the main flusher loop.
func (f *BackgroundFlusher) run() {
	defer f.wg.Done()

	ticker := time.NewTicker(f.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-f.ctx.Done():
			// Shutdown requested - do one final sweep
			f.sweep()
			return
		case <-ticker.C:
			f.sweep()
		}
	}
}

// sweep checks all cache entries and flushes idle ones in parallel.
func (f *BackgroundFlusher) sweep() {
	threshold := time.Now().Add(-f.flushTimeout)

	// Collect entries that need flushing
	var toFlush []metadata.ContentID
	for _, id := range f.cache.List() {
		// Check context in case we're shutting down
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		if f.shouldFlush(id, threshold) {
			toFlush = append(toFlush, id)
		}
	}

	if len(toFlush) == 0 {
		return
	}

	logger.Debug("Flusher: found %d entries to flush (pool size: %d)", len(toFlush), f.flushPoolSize)

	// Process entries in parallel using a worker pool
	var flushWg sync.WaitGroup
	semaphore := make(chan struct{}, f.flushPoolSize)

	for _, id := range toFlush {
		// Check context before starting new flush
		select {
		case <-f.ctx.Done():
			break
		default:
		}

		flushWg.Add(1)
		semaphore <- struct{}{} // Acquire slot

		go func(contentID metadata.ContentID) {
			defer flushWg.Done()
			defer func() { <-semaphore }() // Release slot

			if err := f.flush(contentID); err != nil {
				logger.Warn("Flusher: failed to flush %s: %v", contentID, err)
			}
		}(id)
	}

	flushWg.Wait()
}

// shouldFlush checks if an entry should be flushed.
func (f *BackgroundFlusher) shouldFlush(id metadata.ContentID, threshold time.Time) bool {
	state := f.cache.GetState(id)

	// Skip entries that don't need flushing
	switch state {
	case cache.StateNone, cache.StateCached, cache.StatePrefetching:
		return false
	case cache.StateBuffering:
		// Not yet in upload state - COMMIT will transition it
		return false
	case cache.StateUploading:
		// Candidate for flushing
	}

	// Check if file is idle (no recent writes)
	lastWrite := f.cache.LastWrite(id)
	if lastWrite.After(threshold) {
		return false
	}

	// For incremental stores, check upload state
	if incStore, ok := f.contentStore.(content.IncrementalWriteStore); ok {
		if writeState := incStore.GetIncrementalWriteState(id); writeState != nil {
			// Check if any parts are still being uploaded
			if writeState.PartsUploading > 0 {
				logger.Debug("Flusher: skipping %s, parts still uploading: uploading=%d",
					id, writeState.PartsUploading)
				return false
			}
			// Multipart upload in progress with no active uploads - ready to finalize
			return true
		}
		// No write state means small file (< partSize) - ready to upload via PutObject
		// CompleteIncrementalWrite will handle this case
		return true
	}

	// For non-incremental stores, check if all data has been flushed from cache
	cacheSize := f.cache.Size(id)
	flushedOffset := f.cache.GetFlushedOffset(id)
	if flushedOffset < cacheSize {
		logger.Debug("Flusher: skipping %s, unflushed cache data: flushed=%d size=%d",
			id, flushedOffset, cacheSize)
		return false
	}

	return true
}

// flush completes the upload for a single cache entry.
func (f *BackgroundFlusher) flush(id metadata.ContentID) error {
	logger.Debug("Flusher: flushing %s", id)

	cacheSize := f.cache.Size(id)

	// Validate file completeness if metadata store is available
	// This prevents completing uploads for interrupted copies
	if f.metadataStore != nil {
		expectedSize, err := f.getExpectedFileSize(id)
		if err != nil {
			logger.Warn("Flusher: cannot get expected size for %s, skipping: %v", id, err)
			return nil // Skip this entry, don't complete or abort
		}

		if cacheSize != expectedSize {
			logger.Warn("Flusher: incomplete file detected %s: cache_size=%d expected_size=%d, aborting upload",
				id, cacheSize, expectedSize)

			// Abort the incomplete upload
			if incStore, ok := f.contentStore.(content.IncrementalWriteStore); ok {
				if err := incStore.AbortIncrementalWrite(f.ctx, id); err != nil {
					logger.Error("Flusher: failed to abort incomplete upload %s: %v", id, err)
				}
			}

			// Remove from cache since it's incomplete
			f.cache.Remove(id)

			return nil
		}
	}

	// Complete any in-progress incremental upload (S3 multipart or PutObject for small files)
	if incStore, ok := f.contentStore.(content.IncrementalWriteStore); ok {
		state := incStore.GetIncrementalWriteState(id)
		if state != nil {
			logger.Debug("Flusher: completing incremental write for %s (parts_uploaded=%d, flushed=%d)",
				id, state.PartsUploaded, state.TotalFlushed)
		} else {
			logger.Debug("Flusher: completing small file write for %s", id)
		}

		// CompleteIncrementalWrite handles both:
		// - Small files (no session): PutObject directly from cache
		// - Large files: upload remaining parts + CompleteMultipartUpload
		if err := incStore.CompleteIncrementalWrite(f.ctx, id, f.cache); err != nil {
			return err
		}
	}

	// Transition to cached state
	f.cache.SetState(id, cache.StateCached)

	logger.Info("Flusher: flushed %s (size=%d)", id, cacheSize)

	return nil
}

// getExpectedFileSize looks up the expected file size from metadata.
// The ContentID is typically the file path (e.g., "export/path/to/file").
func (f *BackgroundFlusher) getExpectedFileSize(id metadata.ContentID) (uint64, error) {
	// ContentID is the file path - we need to find the file by content ID
	// This requires iterating or having a reverse lookup
	// For now, use a simple approach: the metadata store should have a way to look this up

	file, err := f.metadataStore.GetFileByContentID(f.ctx, id)
	if err != nil {
		return 0, err
	}

	return file.Size, nil
}
