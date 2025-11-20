// Package gc provides generic garbage collection for orphaned content.
//
// The garbage collector identifies and removes content that is no longer
// referenced by any metadata (orphaned content). This can occur due to:
//   - Server crashes during delete operations
//   - Failed delete operations
//   - Buffered deletions lost in memory
//   - Bugs in metadata/content coordination
//
// The collector is generic and works with any MetadataStore and ContentStore
// implementation that supports the required interfaces.
package gc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Collector performs periodic garbage collection on content stores.
//
// The collector runs in the background and periodically scans for orphaned
// content (content not referenced by metadata) and deletes it.
//
// Thread Safety: Safe for concurrent use.
type Collector struct {
	metadataStore metadata.MetadataStore
	contentStore  content.ContentStore
	config        Config
	stopCh        chan struct{}
	doneCh        chan struct{}
	startOnce     sync.Once
	stopOnce      sync.Once
}

// Config contains configuration for the garbage collector.
type Config struct {
	// Enabled controls whether garbage collection is active (default: true)
	Enabled bool

	// Interval is how often to run garbage collection (default: 24h)
	Interval time.Duration

	// Timeout is the maximum duration for a single GC run (default: 10m)
	// For large deployments with millions of files, increase this value.
	Timeout time.Duration

	// BatchSize is how many orphaned items to delete per batch (default: 1000)
	// S3 supports up to 1000 objects per DeleteObjects call
	BatchSize int

	// DryRun mode logs what would be deleted without actually deleting (default: false)
	// Useful for testing and validation
	DryRun bool
}

// NewCollector creates a new garbage collector.
//
// The collector will be initialized but not started. Call Start() to begin
// background garbage collection.
//
// Parameters:
//   - metadataStore: Metadata store to query referenced content
//   - contentStore: Content store to scan and delete orphaned content
//   - config: Garbage collection configuration
//
// Returns:
//   - *Collector: Initialized collector (not started)
//   - error: Returns error if stores don't support required interfaces
func NewCollector(
	metadataStore metadata.MetadataStore,
	contentStore content.ContentStore,
	config Config,
) (*Collector, error) {
	// Validate that content store supports garbage collection
	if _, ok := contentStore.(content.GarbageCollectableStore); !ok {
		return nil, fmt.Errorf("content store does not implement GarbageCollectableStore interface")
	}

	// Set defaults
	if config.Interval == 0 {
		config.Interval = 24 * time.Hour
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Minute
	}
	if config.BatchSize == 0 {
		config.BatchSize = 1000
	}

	return &Collector{
		metadataStore: metadataStore,
		contentStore:  contentStore,
		config:        config,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}, nil
}

// Start begins background garbage collection.
//
// This starts a goroutine that periodically runs garbage collection at the
// configured interval. The goroutine will run until Stop() is called.
//
// Safe to call multiple times (subsequent calls are no-ops).
func (c *Collector) Start() {
	if !c.config.Enabled {
		logger.Info("Garbage collection disabled")
		return
	}

	c.startOnce.Do(func() {
		logger.Info("Starting garbage collector: interval=%s batch_size=%d dry_run=%v",
			c.config.Interval, c.config.BatchSize, c.config.DryRun)

		go c.worker()
	})
}

// Stop stops the garbage collector and waits for it to finish.
//
// This signals the worker goroutine to stop and waits for it to complete
// any in-progress collection. Safe to call multiple times.
//
// Parameters:
//   - ctx: Context for timeout (garbage collection will be interrupted if context expires)
//
// Returns:
//   - error: Returns error if context expires before shutdown completes
func (c *Collector) Stop(ctx context.Context) error {
	if !c.config.Enabled {
		return nil
	}

	var stopErr error
	c.stopOnce.Do(func() {
		logger.Info("Stopping garbage collector...")

		// Signal stop
		close(c.stopCh)

		// Wait for worker to finish (with context timeout)
		select {
		case <-c.doneCh:
			logger.Info("Garbage collector stopped successfully")
			stopErr = nil
		case <-ctx.Done():
			logger.Warn("Garbage collector shutdown timeout")
			stopErr = ctx.Err()
		}
	})

	return stopErr
}

// RunNow triggers an immediate garbage collection run.
//
// This is useful for:
//   - Testing
//   - Manual triggers via admin API
//   - Initial cleanup on startup
//
// The method blocks until collection completes or context is cancelled.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - *Stats: Collection statistics
//   - error: Returns error if collection fails or context is cancelled
func (c *Collector) RunNow(ctx context.Context) (*Stats, error) {
	logger.Info("Running garbage collection (manual trigger)...")
	return c.collect(ctx)
}

// worker is the background goroutine that runs periodic garbage collection.
func (c *Collector) worker() {
	defer close(c.doneCh)

	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()

	logger.Info("Garbage collector worker started")

	for {
		select {
		case <-ticker.C:
			// Periodic collection with configured timeout
			ctx, cancel := context.WithTimeout(context.Background(), c.config.Timeout)
			stats, err := c.collect(ctx)
			cancel()

			if err != nil {
				logger.Error("Garbage collection failed: %v", err)
			} else {
				logger.Info("Garbage collection completed: %s", stats.Summary())
			}

		case <-c.stopCh:
			logger.Info("Garbage collector worker stopping...")
			return
		}
	}
}

// collect performs a single garbage collection run.
//
// This is the core GC algorithm:
//  1. Get all ContentIDs referenced by metadata
//  2. Get all ContentIDs in content store
//  3. Compute orphaned = existing - referenced
//  4. Batch delete orphaned content
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - *Stats: Collection statistics
//   - error: Returns error if collection fails
func (c *Collector) collect(ctx context.Context) (*Stats, error) {
	startTime := time.Now()
	stats := &Stats{
		StartTime: startTime,
	}

	// Type assertion is guaranteed to succeed due to validation in NewCollector
	gcStore := c.contentStore.(content.GarbageCollectableStore)

	logger.Info("GC: Phase 1 - Getting referenced content from metadata store...")

	// Phase 1: Get all ContentIDs referenced by metadata
	referenced, err := c.metadataStore.GetAllContentIDs(ctx)
	if err != nil {
		return stats, fmt.Errorf("failed to get referenced content: %w", err)
	}
	stats.ReferencedCount = uint64(len(referenced))

	logger.Info("GC: Found %d referenced content items", stats.ReferencedCount)

	// Build set for fast lookup
	referencedSet := make(map[metadata.ContentID]struct{}, len(referenced))
	for _, id := range referenced {
		referencedSet[id] = struct{}{}
	}

	logger.Info("GC: Phase 2 - Getting all content from content store...")

	// Phase 2: Get all ContentIDs in content store
	existing, err := gcStore.ListAllContent(ctx)
	if err != nil {
		return stats, fmt.Errorf("failed to list content: %w", err)
	}
	stats.ExistingCount = uint64(len(existing))

	logger.Info("GC: Found %d existing content items", stats.ExistingCount)

	// Phase 3: Compute orphaned content
	orphaned := make([]metadata.ContentID, 0, len(existing))
	for _, id := range existing {
		if _, isReferenced := referencedSet[id]; !isReferenced {
			orphaned = append(orphaned, id)
		}
	}
	stats.OrphanedCount = uint64(len(orphaned))

	if len(orphaned) == 0 {
		logger.Info("GC: No orphaned content found")
		stats.EndTime = time.Now()
		return stats, nil
	}

	logger.Info("GC: Found %d orphaned content items", stats.OrphanedCount)

	// Phase 3.5: Re-verify references before deletion
	// Race condition prevention: Content created between Phase 1 and Phase 2 could be
	// incorrectly identified as orphaned. We do ONE final verification here to catch
	// any content that was created after Phase 1.
	//
	// Trade-off: We accept that content created DURING Phase 4 (deletion) may have a
	// small race window, but it will be caught in the next GC cycle. This is much more
	// efficient than re-verifying for every batch (which would be O(n * num_batches)).
	logger.Info("GC: Phase 3.5 - Re-verifying references before deletion...")
	referencedNow, err := c.metadataStore.GetAllContentIDs(ctx)
	if err != nil {
		return stats, fmt.Errorf("failed to re-verify references: %w", err)
	}

	// Build set of currently referenced content
	referencedNowSet := make(map[metadata.ContentID]struct{}, len(referencedNow))
	for _, id := range referencedNow {
		referencedNowSet[id] = struct{}{}
	}

	// Filter orphaned list to only include items that are STILL orphaned
	safeToDelete := make([]metadata.ContentID, 0, len(orphaned))
	var skippedCount uint64
	for _, id := range orphaned {
		if _, isReferenced := referencedNowSet[id]; !isReferenced {
			safeToDelete = append(safeToDelete, id)
		} else {
			// Content is now referenced - skip deletion
			skippedCount++
		}
	}

	if skippedCount > 0 {
		logger.Info("GC: %d items became referenced during scan, skipping deletion", skippedCount)
	}

	if len(safeToDelete) == 0 {
		logger.Info("GC: All items are now referenced, nothing to delete")
		stats.EndTime = time.Now()
		return stats, nil
	}

	logger.Info("GC: Verified %d items are safe to delete", len(safeToDelete))

	if c.config.DryRun {
		logger.Info("GC: DRY RUN - Would delete %d items:", len(safeToDelete))
		for i, id := range safeToDelete {
			if i < 10 {
				logger.Info("  - %s", id)
			}
		}
		if len(safeToDelete) > 10 {
			logger.Info("  ... and %d more", len(safeToDelete)-10)
		}
		stats.EndTime = time.Now()
		return stats, nil
	}

	// Phase 4: Batch delete verified orphaned content
	logger.Info("GC: Phase 4 - Deleting %d verified orphaned items in batches of %d...",
		len(safeToDelete), c.config.BatchSize)

	for i := 0; i < len(safeToDelete); i += c.config.BatchSize {
		// Check for cancellation
		if err := ctx.Err(); err != nil {
			stats.EndTime = time.Now()
			return stats, err
		}

		end := i + c.config.BatchSize
		if end > len(safeToDelete) {
			end = len(safeToDelete)
		}

		batch := safeToDelete[i:end]

		failures, err := gcStore.DeleteBatch(ctx, batch)
		if err != nil {
			logger.Warn("GC: Batch delete failed: %v", err)
			stats.FailedCount += uint64(len(batch))
			continue
		}

		stats.DeletedCount += uint64(len(batch) - len(failures))
		stats.FailedCount += uint64(len(failures))

		for id, ferr := range failures {
			logger.Debug("GC: Failed to delete %s: %v", id, ferr)
		}

		logger.Debug("GC: Deleted batch %d-%d: %d succeeded, %d failed",
			i, end, len(batch)-len(failures), len(failures))
	}

	stats.EndTime = time.Now()

	logger.Info("GC: Completed - deleted %d items, %d failed, duration=%s",
		stats.DeletedCount, stats.FailedCount, stats.Duration())

	return stats, nil
}

// Stats contains statistics from a garbage collection run.
type Stats struct {
	StartTime       time.Time // When collection started
	EndTime         time.Time // When collection ended
	ReferencedCount uint64    // Number of ContentIDs referenced by metadata
	ExistingCount   uint64    // Number of ContentIDs in content store
	OrphanedCount   uint64    // Number of orphaned ContentIDs found
	DeletedCount    uint64    // Number of orphaned items successfully deleted
	FailedCount     uint64    // Number of orphaned items that failed to delete
}

// Duration returns the total collection duration.
func (s *Stats) Duration() time.Duration {
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

// Summary returns a human-readable summary of the collection.
func (s *Stats) Summary() string {
	return fmt.Sprintf("referenced=%d existing=%d orphaned=%d deleted=%d failed=%d duration=%s",
		s.ReferencedCount, s.ExistingCount, s.OrphanedCount,
		s.DeletedCount, s.FailedCount, s.Duration())
}
