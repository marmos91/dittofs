// Package cache provides write caching implementations for DittoFS.
//
// This file contains the auto-flush decorator that wraps any WriteCache
// implementation to add timeout-based automatic flushing.
package cache

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// FlushCallback is called when cached content needs to be flushed to storage.
//
// This callback is provided by the protocol adapter (e.g., NFS handler) and
// typically calls ContentStore.FlushWrites() if the store supports it.
//
// The callback design keeps the cache and content store interfaces completely
// separated - the cache doesn't need to know about ContentStore at all.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to flush
//
// Returns:
//   - error: Returns error if flush fails, nil on success
type FlushCallback func(ctx context.Context, id metadata.ContentID) error

// AutoFlushWriteCache wraps a WriteCache with automatic timeout-based flushing.
//
// This decorator is critical for platforms like macOS that don't send COMMIT
// operations. Without auto-flush, small files would remain in cache indefinitely.
//
// Design:
//   - Wraps any WriteCache implementation (decorator pattern)
//   - Uses callback to flush content (keeps interfaces separated)
//   - Background worker periodically checks for stale writes
//   - Flushes content IDs that haven't been written to within the timeout
//   - Graceful shutdown via Close()
//
// Thread Safety:
// This implementation is safe for concurrent use by multiple goroutines.
type AutoFlushWriteCache struct {
	cache         WriteCache    // Wrapped cache implementation
	flushCallback FlushCallback // Callback to flush content to storage
	timeout       time.Duration // Idle timeout before flushing

	// Worker state
	checkInterval time.Duration // How often to check for stale writes
	stopCh        chan struct{} // Signal to stop the worker
	doneCh        chan struct{} // Signal when worker has stopped
	startOnce     sync.Once     // Ensures Start() only runs once
	stopOnce      sync.Once     // Ensures Stop() only runs once
	ctx           context.Context
	cancelFunc    context.CancelFunc
}

// AutoFlushConfig contains configuration for auto-flush behavior.
type AutoFlushConfig struct {
	// Timeout is the idle duration after which cached writes are flushed
	// Default: 30 seconds (critical for macOS compatibility)
	Timeout time.Duration

	// CheckInterval is how often the worker checks for stale writes
	// Default: 10 seconds (balance between responsiveness and CPU usage)
	CheckInterval time.Duration
}

// NewAutoFlushWriteCache creates a new auto-flush decorator around a WriteCache.
//
// The decorator starts a background worker that periodically checks for cached
// content that hasn't been written to within the timeout period and flushes it.
//
// Important: You must call Start() to begin auto-flushing, and Close() for
// graceful shutdown.
//
// Parameters:
//   - cache: The underlying WriteCache to wrap
//   - flushCallback: Function to call when flushing content
//   - config: Auto-flush configuration (timeouts, intervals)
//
// Returns:
//   - *AutoFlushWriteCache: The decorated cache with auto-flush capability
func NewAutoFlushWriteCache(
	cache WriteCache,
	flushCallback FlushCallback,
	config AutoFlushConfig,
) *AutoFlushWriteCache {
	// Set defaults
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second // Default: 30 seconds
	}
	if config.CheckInterval == 0 {
		config.CheckInterval = 10 * time.Second // Default: 10 seconds
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &AutoFlushWriteCache{
		cache:         cache,
		flushCallback: flushCallback,
		timeout:       config.Timeout,
		checkInterval: config.CheckInterval,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
		ctx:           ctx,
		cancelFunc:    cancel,
	}
}

// Start begins the auto-flush background worker.
//
// This method is idempotent - calling it multiple times has no effect.
// The worker will run until Stop() or Close() is called.
func (a *AutoFlushWriteCache) Start() {
	a.startOnce.Do(func() {
		go a.worker()
	})
}

// Stop gracefully stops the auto-flush worker.
//
// This method is idempotent - calling it multiple times has no effect.
// It waits for the worker to finish its current cycle before returning.
func (a *AutoFlushWriteCache) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
		<-a.doneCh // Wait for worker to finish
		a.cancelFunc()
	})
}

// worker is the background goroutine that periodically checks for stale writes.
func (a *AutoFlushWriteCache) worker() {
	defer close(a.doneCh)

	ticker := time.NewTicker(a.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopCh:
			// Shutdown requested - do one final flush before exiting
			a.flushStaleContent(a.ctx)
			return

		case <-ticker.C:
			// Periodic check for stale content
			a.flushStaleContent(a.ctx)
		}
	}
}

// flushStaleContent identifies and flushes content that hasn't been written to
// within the timeout period.
func (a *AutoFlushWriteCache) flushStaleContent(ctx context.Context) {
	now := time.Now()

	// Get all cached content IDs
	contentIDs := a.cache.List()

	for _, id := range contentIDs {
		// Check if context was cancelled
		if ctx.Err() != nil {
			return
		}

		// Check if this content is stale
		lastWrite := a.cache.LastWrite(id)
		if lastWrite.IsZero() {
			// No write timestamp - skip
			continue
		}

		idleTime := now.Sub(lastWrite)
		if idleTime < a.timeout {
			// Not stale yet - skip
			continue
		}

		// Content is stale - flush it
		if err := a.flushCallback(ctx, id); err != nil {
			// Log error but continue with other content IDs
			// Note: We can't import logger here to avoid circular dependencies,
			// so errors are silently ignored. The callback should handle logging.
			continue
		}

		// Successfully flushed - reset cache entry
		// Note: We don't remove from cache, just reset to allow future writes
		// The callback (FlushWrites) will call cache.Reset() after successful flush
	}
}

// WriteAt delegates to the wrapped cache.
func (a *AutoFlushWriteCache) WriteAt(id metadata.ContentID, data []byte, offset int64) error {
	return a.cache.WriteAt(id, data, offset)
}

// ReadAt delegates to the wrapped cache.
func (a *AutoFlushWriteCache) ReadAt(id metadata.ContentID, buf []byte, offset int64) (int, error) {
	return a.cache.ReadAt(id, buf, offset)
}

// ReadAll delegates to the wrapped cache.
func (a *AutoFlushWriteCache) ReadAll(id metadata.ContentID) ([]byte, error) {
	return a.cache.ReadAll(id)
}

// Size delegates to the wrapped cache.
func (a *AutoFlushWriteCache) Size(id metadata.ContentID) int64 {
	return a.cache.Size(id)
}

// Reset delegates to the wrapped cache.
func (a *AutoFlushWriteCache) Reset(id metadata.ContentID) error {
	return a.cache.Reset(id)
}

// ResetAll delegates to the wrapped cache.
func (a *AutoFlushWriteCache) ResetAll() error {
	return a.cache.ResetAll()
}

// LastWrite delegates to the wrapped cache.
func (a *AutoFlushWriteCache) LastWrite(id metadata.ContentID) time.Time {
	return a.cache.LastWrite(id)
}

// List delegates to the wrapped cache.
func (a *AutoFlushWriteCache) List() []metadata.ContentID {
	return a.cache.List()
}

// Close stops the auto-flush worker and closes the wrapped cache.
//
// This ensures graceful shutdown:
//   - Stops the background worker
//   - Performs one final flush of stale content
//   - Closes the underlying cache
func (a *AutoFlushWriteCache) Close() error {
	// Stop the worker (waits for completion)
	a.Stop()

	// Close the underlying cache
	return a.cache.Close()
}
