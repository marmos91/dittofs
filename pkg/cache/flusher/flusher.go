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
)

// BackgroundFlusher monitors the cache and finalizes idle file uploads.
//
// In NFS, there's no "close" operation - the server never knows when a client
// is done writing to a file. The flusher solves this by detecting files that
// haven't been written to recently (idle files) and completing their upload
// to the content store.
//
// Lifecycle:
//   - Created via New() with cache, content store, and optional metadata store
//   - Started via Start() which spawns the background goroutine
//   - Stopped via Stop() which cancels the context and waits for completion
//
// Sweep Behavior:
//   - Runs periodically (default: every 10 seconds)
//   - Checks each cache entry in StateUploading for idle status
//   - A file is "idle" if no writes occurred for flushTimeout (default: 30s)
//   - Idle files are finalized in parallel (no limit on concurrency)
//
// Finalization:
//   - Validates cache size matches expected file size (if metadata store available)
//   - For incremental stores (S3): calls CompleteIncrementalWrite
//   - For small files: uploads via PutObject directly from cache
//   - Transitions entry from StateUploading to StateCached
//   - Aborts incomplete uploads (e.g., interrupted copies) if size mismatch detected
//
// Thread Safety:
//   - The flusher is safe for concurrent use
//   - Uses context cancellation for graceful shutdown
//   - Waits for all flush operations to complete before Stop() returns
type BackgroundFlusher struct {
	cache         cache.Cache          // Cache to monitor for idle files
	contentStore  content.ContentStore // Content store to finalize uploads to
	sweepInterval time.Duration        // How often to check for idle files
	flushTimeout  time.Duration        // How long a file must be idle before flushing

	ctx    context.Context    // Context for cancellation (created in Start)
	cancel context.CancelFunc // Cancel function to trigger shutdown
	wg     sync.WaitGroup     // Tracks the main run() goroutine for graceful shutdown
}

// Config holds configuration for the background flusher.
type Config struct {
	// SweepInterval is how often to check for idle files.
	// Default: 10 seconds. Lower values provide faster finalization but increase CPU usage.
	SweepInterval time.Duration

	// FlushTimeout is how long a file must be idle (no writes) before flushing.
	// Default: 30 seconds. This is the key timeout for detecting "done" files.
	// Lower values finalize faster but risk flushing files that are still being written
	// (e.g., slow network transfers). Higher values delay finalization.
	FlushTimeout time.Duration
}

// New creates a new background flusher.
//
// The flusher will not start until Start() is called.
//
// Parameters:
//   - c: The cache to monitor for idle files
//   - contentStore: The content store to finalize uploads to
//   - config: Optional configuration. If nil, defaults are used.
//
// Example:
//
//	f := flusher.New(cache, s3Store, &flusher.Config{
//	    FlushTimeout: 60 * time.Second, // Wait longer for slow uploads
//	})
//	f.Start(ctx)
//	defer f.Stop()
func New(
	c cache.Cache,
	contentStore content.ContentStore,
	config *Config,
) *BackgroundFlusher {
	sweepInterval := defaultSweepInterval
	flushTimeout := defaultFlushTimeout

	if config != nil {
		if config.SweepInterval > 0 {
			sweepInterval = config.SweepInterval
		}
		if config.FlushTimeout > 0 {
			flushTimeout = config.FlushTimeout
		}
	}

	return &BackgroundFlusher{
		cache:         c,
		contentStore:  contentStore,
		sweepInterval: sweepInterval,
		flushTimeout:  flushTimeout,
	}
}

// Start begins the background flusher goroutine.
//
// The flusher runs until Stop() is called or the parent context is cancelled.
// Start should only be called once. Calling Start multiple times without Stop
// will leak goroutines.
//
// The provided context is used as the parent for all flush operations.
// Cancelling it will trigger a graceful shutdown (equivalent to calling Stop).
func (f *BackgroundFlusher) Start(ctx context.Context) {
	f.ctx, f.cancel = context.WithCancel(ctx)

	logger.Info("Background flusher started: sweep_interval=%s flush_timeout=%s",
		f.sweepInterval, f.flushTimeout)

	f.wg.Add(1)
	go f.run()
}

// Stop gracefully stops the flusher.
//
// This cancels the context and blocks until the flusher goroutine has exited.
// Before exiting, the flusher performs one final sweep to flush any remaining
// idle files.
//
// Stop is safe to call multiple times. After the first call, subsequent calls
// return immediately.
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
//
// The sweep:
//  1. Collects all entries that meet the flush criteria (idle for flushTimeout)
//  2. Spawns a goroutine for each entry to flush in parallel
//  3. Waits for all flush operations to complete before returning
//
// Concurrency is unlimited - all eligible entries are flushed simultaneously.
// The S3 store's maxParallelUploads setting limits actual upload concurrency.
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

	logger.Debug("Flusher: found %d entries to flush", len(toFlush))

	// Flush all entries in parallel
	var flushWg sync.WaitGroup

	for _, id := range toFlush {
		// Check context before starting new flush
		select {
		case <-f.ctx.Done():
			break
		default:
		}

		flushWg.Add(1)

		go func(contentID metadata.ContentID) {
			defer flushWg.Done()

			if err := f.flush(contentID); err != nil {
				logger.Warn("Flusher: failed to flush %s: %v", contentID, err)
			}
		}(id)
	}

	flushWg.Wait()
}

// shouldFlush checks if an entry should be flushed.
//
// An entry is eligible for flushing when:
//   - It's in StateUploading (has been committed at least once)
//   - It's been idle for at least flushTimeout (no recent writes)
//   - No parts are currently being uploaded (for incremental stores)
//   - All cache data has been flushed (for non-incremental stores)
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
			if writeState.PartsWriting > 0 {
				logger.Debug("Flusher: skipping %s, parts still uploading: uploading=%d",
					id, writeState.PartsWriting)
				return false
			}
			// Incremental write in progress with no active writes - ready to finalize
			return true
		}
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
//
// For incremental stores (S3), this calls CompleteIncrementalWrite which:
//   - Small files: uploads via PutObject directly from cache
//   - Large files: uploads remaining parts + calls CompleteMultipartUpload
//
// After successful completion, the entry transitions to StateCached.
func (f *BackgroundFlusher) flush(id metadata.ContentID) error {
	logger.Debug("Flusher: flushing %s", id)

	cacheSize := f.cache.Size(id)

	// Complete any in-progress incremental write
	if incStore, ok := f.contentStore.(content.IncrementalWriteStore); ok {
		state := incStore.GetIncrementalWriteState(id)
		if state != nil {
			logger.Debug("Flusher: completing incremental write for %s (parts_written=%d, flushed=%d)",
				id, state.PartsWritten, state.TotalFlushed)
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
