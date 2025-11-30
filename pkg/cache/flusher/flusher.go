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
// TODO: Make these configurable via config file
const (
	// defaultSweepInterval is how often the flusher checks for idle files.
	defaultSweepInterval = 10 * time.Second

	// defaultFlushTimeout is how long a file must be idle before flushing.
	// This is the key NFS async write timeout - files are considered "done"
	// when no writes have occurred for this duration.
	defaultFlushTimeout = 30 * time.Second
)

// BackgroundFlusher runs in the background to detect and flush idle files.
//
// The flusher:
//   - Runs periodically (default: every 10 seconds)
//   - Checks each cache entry for idle status
//   - Completes incremental uploads (S3 multipart) for fully flushed files
//   - Transitions entries from StateUploading to StateCached
type BackgroundFlusher struct {
	cache         cache.Cache
	contentStore  content.ContentStore
	sweepInterval time.Duration
	flushTimeout  time.Duration

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
}

// New creates a new background flusher.
//
// The flusher will not start until Start() is called.
//
// Parameters:
//   - c: The cache to monitor for idle files
//   - contentStore: The content store to flush uploads to
//   - config: Optional configuration (nil for defaults)
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
// The flusher will run until Stop() is called or the context is cancelled.
func (f *BackgroundFlusher) Start(ctx context.Context) {
	f.ctx, f.cancel = context.WithCancel(ctx)

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

// sweep checks all cache entries and flushes idle ones.
func (f *BackgroundFlusher) sweep() {
	threshold := time.Now().Add(-f.flushTimeout)

	for _, id := range f.cache.List() {
		// Check context in case we're shutting down
		select {
		case <-f.ctx.Done():
			return
		default:
		}

		state := f.cache.GetState(id)

		// Skip entries that don't need flushing
		switch state {
		case cache.StateNone, cache.StateCached, cache.StatePrefetching:
			continue
		case cache.StateBuffering:
			// Not yet in upload state - COMMIT will transition it
			continue
		case cache.StateUploading:
			// Candidate for flushing
		}

		// Check if file is idle (no recent writes)
		lastWrite := f.cache.LastWrite(id)
		if lastWrite.After(threshold) {
			continue
		}

		// Check if all data has been flushed from cache to content store
		cacheSize := f.cache.Size(id)
		flushedOffset := f.cache.GetFlushedOffset(id)
		if flushedOffset < cacheSize {
			logger.Debug("Flusher: skipping %s, unflushed cache data: flushed=%d size=%d",
				id, flushedOffset, cacheSize)
			continue
		}

		// For incremental stores, check if upload is still in progress
		// (has buffered data waiting to be uploaded to remote storage)
		if incStore, ok := f.contentStore.(content.IncrementalWriteStore); ok {
			if writeState := incStore.GetIncrementalWriteState(id); writeState != nil {
				if writeState.BufferedSize > 0 {
					logger.Debug("Flusher: skipping %s, incremental upload in progress: buffered=%d",
						id, writeState.BufferedSize)
					continue
				}
			}
		}

		// All checks passed - flush this entry
		if err := f.flush(id); err != nil {
			logger.Warn("Flusher: failed to flush %s: %v", id, err)
		}
	}
}

// flush completes the upload for a single cache entry.
func (f *BackgroundFlusher) flush(id metadata.ContentID) error {
	logger.Debug("Flusher: flushing %s", id)

	// Complete any in-progress incremental upload (S3 multipart, etc.)
	if incStore, ok := f.contentStore.(content.IncrementalWriteStore); ok {
		if state := incStore.GetIncrementalWriteState(id); state != nil {
			logger.Debug("Flusher: completing incremental write for %s (parts=%d, flushed=%d)",
				id, state.CurrentPartNumber-1, state.TotalFlushed)

			if err := incStore.CompleteIncrementalWrite(f.ctx, id); err != nil {
				return err
			}
		}
	}

	// Transition to cached state
	f.cache.SetState(id, cache.StateCached)

	logger.Info("Flusher: flushed %s", id)

	return nil
}
