package transfer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
)

// RecoveryStats holds statistics about the recovery process.
type RecoveryStats struct {
	FilesScanned   int
	SlicesFound    int
	SlicesUploaded int
	SlicesFailed   int
	BytesUploaded  int64
}

// RecoverUnflushedSlices scans the cache for pending slices and uploads them to the block store.
// Called on startup to ensure crash recovery.
//
// This is safe to call even if there are no pending slices - it will return quickly.
func (m *TransferManager) RecoverUnflushedSlices(ctx context.Context) (*RecoveryStats, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	if m.cache == nil {
		return nil, fmt.Errorf("no cache configured")
	}

	stats := &RecoveryStats{}

	// Get all file handles from cache
	fileHandles := m.cache.ListFiles()
	stats.FilesScanned = len(fileHandles)

	if len(fileHandles) == 0 {
		logger.Info("Recovery: no cached files found")
		return stats, nil
	}

	logger.Info("Recovery: scanning cached files", "count", len(fileHandles))

	// Process files with bounded parallelism
	var wg sync.WaitGroup
	sem := make(chan struct{}, m.config.ParallelUploads)
	var uploadedCount, failedCount int64
	var bytesUploaded int64

	for _, handle := range fileHandles {
		// Get pending (dirty) slices for this file
		pending, err := m.cache.GetDirtySlices(ctx, handle)
		if err != nil {
			logger.Debug("Recovery: no dirty slices for file", "error", err)
			continue
		}

		if len(pending) == 0 {
			continue
		}

		stats.SlicesFound += len(pending)
		logger.Info("Recovery: found pending slices",
			"file", handle,
			"slices", len(pending))

		// Upload each pending slice
		for _, slice := range pending {
			wg.Add(1)
			sem <- struct{}{}

			go func(fileHandle string, s cache.PendingSlice) {
				defer func() {
					<-sem
					wg.Done()
				}()

				// Use file handle as content ID (they're the same in our model)
				payloadID := fileHandle

				// Upload slice as blocks
				blockRefs, err := m.uploadSliceAsBlocks(ctx, "", payloadID, s)
				if err != nil {
					logger.Error("Recovery: failed to upload slice",
						"file", fileHandle,
						"sliceID", s.ID,
						"error", err)
					atomic.AddInt64(&failedCount, 1)
					return
				}

				// Mark as flushed in cache
				if err := m.cache.MarkSliceFlushed(ctx, fileHandle, s.ID, blockRefs); err != nil {
					logger.Error("Recovery: failed to mark slice flushed",
						"file", fileHandle,
						"sliceID", s.ID,
						"error", err)
					atomic.AddInt64(&failedCount, 1)
					return
				}

				atomic.AddInt64(&uploadedCount, 1)
				atomic.AddInt64(&bytesUploaded, int64(len(s.Data)))

				logger.Debug("Recovery: uploaded slice",
					"file", fileHandle,
					"sliceID", s.ID,
					"bytes", len(s.Data))
			}(handle, slice)
		}
	}

	wg.Wait()

	stats.SlicesUploaded = int(uploadedCount)
	stats.SlicesFailed = int(failedCount)
	stats.BytesUploaded = bytesUploaded

	logger.Info("Recovery: completed",
		"files", stats.FilesScanned,
		"slicesFound", stats.SlicesFound,
		"slicesUploaded", stats.SlicesUploaded,
		"slicesFailed", stats.SlicesFailed,
		"bytesUploaded", stats.BytesUploaded)

	if stats.SlicesFailed > 0 {
		return stats, fmt.Errorf("recovery failed for %d slices", stats.SlicesFailed)
	}

	return stats, nil
}
