package offloader

import (
	"context"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// RecoverUnflushedBlocks scans the cache for pending blocks and starts background uploads.
// Called on startup to ensure crash recovery.
//
// This is non-blocking - uploads are enqueued to the background transfer queue.
// Returns immediately after scanning and enqueueing work.
//
// The returned RecoveryStats includes RecoveredFileSizes which maps payloadID to actual
// file size from recovered WAL data. Consumers MUST use this to reconcile metadata:
//
//	stats := offloader.RecoverUnflushedBlocks(ctx)
//	for payloadID, actualSize := range stats.RecoveredFileSizes {
//	    metadataSize := getMetadataSize(payloadID)
//	    if metadataSize > actualSize {
//	        // Metadata has stale size from CommitWrite before crash
//	        truncateMetadata(payloadID, actualSize)
//	    }
//	}
//
// This reconciliation is necessary because WAL logs individual block writes.
// If a crash occurs after metadata update but before WAL persistence,
// metadata may have been updated with a larger size before crash.
func (m *Offloader) RecoverUnflushedBlocks(ctx context.Context) *RecoveryStats {
	stats := &RecoveryStats{
		RecoveredFileSizes: make(map[string]uint64),
	}

	if m.cache == nil {
		logger.Debug("Recovery: no cache configured")
		return stats
	}

	// Get all files with their actual recovered sizes
	fileSizes := m.cache.ListFilesWithSizes()
	stats.FilesScanned = len(fileSizes)
	stats.RecoveredFileSizes = fileSizes

	if len(fileSizes) == 0 {
		logger.Info("Recovery: no cached files found")
		return stats
	}

	logger.Info("Recovery: scanning cached files", "count", len(fileSizes))

	// Start background flush for each file with dirty data
	for payloadID, recoveredSize := range fileSizes {
		pending, _ := m.cache.GetDirtyBlocks(ctx, payloadID)
		if len(pending) == 0 {
			continue
		}

		blockCount := len(pending)
		stats.BlocksFound += blockCount

		// Calculate bytes for stats
		for _, b := range pending {
			stats.BytesPending += int64(b.DataSize)
		}

		logger.Info("Recovery: uploading recovered blocks",
			"payloadID", payloadID,
			"blocks", blockCount,
			"recoveredSize", recoveredSize)

		// Upload remaining blocks in background goroutine
		go func(pID string) {
			if err := m.uploadRemainingBlocks(ctx, pID); err != nil {
				logger.Error("Recovery: failed to upload recovered blocks",
					"payloadID", pID,
					"error", err)
			}
		}(payloadID)
	}

	logger.Info("Recovery: background flushes started",
		"files", stats.FilesScanned,
		"blocksFound", stats.BlocksFound,
		"bytesPending", stats.BytesPending)

	return stats
}

// MetadataReconciler provides access to metadata operations for reconciliation.
// This interface is implemented by the Registry.
type MetadataReconciler interface {
	// GetMetadataStoreForShare returns the metadata store for a given share name.
	GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error)
}

// ReconciliationStats holds statistics about the metadata reconciliation.
type ReconciliationStats struct {
	FilesChecked   int   // Number of files compared
	FilesTruncated int   // Number of files truncated to match recovered size
	BytesTruncated int64 // Total bytes removed from metadata
	Errors         int   // Number of errors encountered (non-fatal)
}

// ReconcileMetadata compares recovered file sizes with metadata and truncates
// metadata where necessary.
//
// This should be called AFTER RecoverUnflushedBlocks and AFTER metadata stores
// are registered. It fixes metadata inconsistencies caused by crashes that
// occurred after CommitWrite but before data was flushed to block store.
//
// Background:
// WAL only logs NEW blocks for performance (not extended blocks). If a crash
// occurs after:
//  1. Data written to cache (extending an existing block)
//  2. CommitWrite called (metadata updated with new size)
//  3. BUT before WAL persistence of the extended data
//
// Then on recovery, the metadata will show a larger file size than the actual
// recovered data. This function truncates metadata to match reality.
//
// Parameters:
//   - ctx: Context for cancellation
//   - reconciler: Interface to access metadata stores (typically the Registry)
//   - recoveredSizes: Map from payloadID to actual recovered size (from RecoveryStats)
//
// Returns:
//   - *ReconciliationStats: Summary of reconciliation actions
//
// Example:
//
//	stats := offloader.RecoverUnflushedBlocks(ctx)
//	reconStats := offloader.ReconcileMetadata(ctx, registry, stats.RecoveredFileSizes)
//	logger.Info("Reconciliation complete", "truncated", reconStats.FilesTruncated)
func ReconcileMetadata(
	ctx context.Context,
	reconciler MetadataReconciler,
	recoveredSizes map[string]uint64,
) *ReconciliationStats {
	stats := &ReconciliationStats{}

	if len(recoveredSizes) == 0 {
		logger.Debug("Reconciliation: no recovered files to reconcile")
		return stats
	}

	logger.Info("Reconciliation: starting metadata reconciliation", "files", len(recoveredSizes))

	for payloadID, recoveredSize := range recoveredSizes {
		stats.FilesChecked++

		// Parse shareName from payloadID (format: "shareName/path/to/file")
		shareName := parseShareName(payloadID)
		if shareName == "" {
			logger.Warn("Reconciliation: invalid payloadID format",
				"payloadID", payloadID)
			stats.Errors++
			continue
		}

		// Get metadata store for this share
		metaStore, err := reconciler.GetMetadataStoreForShare("/" + shareName)
		if err != nil {
			logger.Warn("Reconciliation: cannot find metadata store",
				"shareName", shareName,
				"error", err)
			stats.Errors++
			continue
		}

		// Find file by PayloadID
		file, err := metaStore.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
		if err != nil {
			// File may not exist in metadata (e.g., deleted before crash)
			logger.Debug("Reconciliation: file not found in metadata",
				"payloadID", payloadID,
				"error", err)
			continue
		}

		// Check if metadata size is larger than recovered size
		// FileAttr is embedded in File, so Size is accessed directly
		metadataSize := file.Size
		if metadataSize <= recoveredSize {
			// Metadata is consistent or smaller - no action needed
			continue
		}

		// Metadata has stale size - truncate it
		logger.Info("Reconciliation: truncating stale metadata",
			"payloadID", payloadID,
			"metadataSize", metadataSize,
			"recoveredSize", recoveredSize,
			"diff", metadataSize-recoveredSize)

		// Update file size and save using PutFile
		file.Size = recoveredSize
		err = metaStore.PutFile(ctx, file)
		if err != nil {
			logger.Error("Reconciliation: failed to truncate metadata",
				"payloadID", payloadID,
				"error", err)
			stats.Errors++
			continue
		}

		stats.FilesTruncated++
		stats.BytesTruncated += int64(metadataSize - recoveredSize)
	}

	logger.Info("Reconciliation: complete",
		"filesChecked", stats.FilesChecked,
		"filesTruncated", stats.FilesTruncated,
		"bytesTruncated", stats.BytesTruncated,
		"errors", stats.Errors)

	return stats
}

// parseShareName extracts the share name from a payloadID.
// PayloadID format: "shareName/path/to/file"
// Returns empty string if format is invalid.
func parseShareName(payloadID string) string {
	if payloadID == "" {
		return ""
	}
	// Remove leading slash if present
	payloadID = strings.TrimPrefix(payloadID, "/")

	// Find first path separator
	idx := strings.Index(payloadID, "/")
	if idx <= 0 {
		// No separator or starts with separator - return entire string as share name
		// This handles cases like "export" (file at root of share)
		if idx == 0 {
			return ""
		}
		return payloadID
	}

	return payloadID[:idx]
}
