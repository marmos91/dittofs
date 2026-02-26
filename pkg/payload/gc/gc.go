package gc

import (
	"context"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// BlockSize is the size of a single block (4MB), used for byte estimation.
const BlockSize = block.Size

// ============================================================================
// Types
// ============================================================================

// Stats holds statistics about the garbage collection run.
type Stats struct {
	SharesScanned  int   // Number of shares processed
	BlocksScanned  int   // Total blocks examined
	OrphanFiles    int   // Files with orphan blocks (no metadata)
	OrphanBlocks   int   // Total orphan blocks deleted
	BytesReclaimed int64 // Estimated bytes freed (block count * BlockSize)
	Errors         int   // Non-fatal errors encountered
}

// Options configures the garbage collection behavior.
type Options struct {
	// SharePrefix limits GC to shares matching this prefix.
	// Empty string means scan all blocks (no prefix filter).
	SharePrefix string

	// DryRun if true, only reports orphans without deleting.
	DryRun bool

	// MaxOrphansPerShare stops processing after finding this many orphan files.
	// 0 means unlimited.
	MaxOrphansPerShare int

	// ProgressCallback is called periodically with progress updates.
	// May be nil.
	ProgressCallback func(stats Stats)
}

// MetadataReconciler provides access to metadata operations for reconciliation.
// This interface is implemented by the Registry.
type MetadataReconciler interface {
	// GetMetadataStoreForShare returns the metadata store for a given share name.
	GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error)
}

// ============================================================================
// Main GC Function
// ============================================================================

// CollectGarbage scans the block store and removes orphan blocks.
//
// Orphan blocks are blocks that exist in the block store but have no
// corresponding metadata. This can happen when:
//   - File deletion fails after metadata is removed but before blocks are deleted
//   - Server crashes during file deletion
//
// The function is safe to run during normal operation because metadata is
// always created BEFORE blocks are written:
//
//	CREATE -> PayloadID assigned -> PutFile(metadata) -> WRITE -> blocks uploaded
//
// Parameters:
//   - ctx: Context for cancellation
//   - blockStore: The block store to scan
//   - reconciler: Interface to check metadata existence (typically Registry)
//   - options: GC configuration (nil uses defaults)
//
// Returns:
//   - *Stats: Summary of GC actions
func CollectGarbage(
	ctx context.Context,
	blockStore store.BlockStore,
	reconciler MetadataReconciler,
	options *Options,
) *Stats {
	stats := &Stats{}

	if options == nil {
		options = &Options{}
	}

	// List all blocks (with optional prefix filter)
	blocks, err := blockStore.ListByPrefix(ctx, options.SharePrefix)
	if err != nil {
		logger.Error("GC: failed to list blocks", "error", err)
		stats.Errors++
		return stats
	}

	if len(blocks) == 0 {
		logger.Debug("GC: no blocks found")
		return stats
	}

	logger.Info("GC: scanning blocks", "count", len(blocks), "prefix", options.SharePrefix)

	// Group blocks by payloadID
	blocksByPayload := make(map[string][]string)
	for _, blockKey := range blocks {
		stats.BlocksScanned++

		payloadID := parsePayloadIDFromBlockKey(blockKey)
		if payloadID == "" {
			logger.Warn("GC: invalid block key format", "blockKey", blockKey)
			stats.Errors++
			continue
		}

		blocksByPayload[payloadID] = append(blocksByPayload[payloadID], blockKey)
	}

	logger.Info("GC: found unique files", "count", len(blocksByPayload))

	// Track shares we've seen for stats
	sharesSeen := make(map[string]bool)

	// Check each payloadID for metadata existence
	for payloadID, blockKeys := range blocksByPayload {
		// Check for context cancellation
		if ctx.Err() != nil {
			logger.Info("GC: cancelled", "processed", stats.OrphanFiles)
			return stats
		}

		// Extract share name for metadata lookup
		shareName := "/" + parseShareName(payloadID)
		if shareName == "/" {
			logger.Warn("GC: invalid payloadID format", "payloadID", payloadID)
			stats.Errors++
			continue
		}

		if !sharesSeen[shareName] {
			sharesSeen[shareName] = true
			stats.SharesScanned++
		}

		// Get metadata store for this share
		metaStore, err := reconciler.GetMetadataStoreForShare(shareName)
		if err != nil {
			// Share might not exist (blocks from deleted share)
			logger.Debug("GC: share not found, treating as orphan",
				"shareName", shareName,
				"payloadID", payloadID)
			// Fall through to delete blocks
		} else {
			// Check if file exists in metadata
			_, err = metaStore.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
			if err == nil {
				// File exists, blocks are valid
				continue
			}
			// File not found - blocks are orphans
		}

		// Found orphan blocks
		stats.OrphanFiles++
		stats.OrphanBlocks += len(blockKeys)
		stats.BytesReclaimed += int64(len(blockKeys)) * int64(BlockSize)

		logger.Info("GC: found orphan blocks",
			"payloadID", payloadID,
			"blockCount", len(blockKeys),
			"dryRun", options.DryRun)

		// Delete orphan blocks (unless dry run)
		if !options.DryRun {
			// Use DeleteByPrefix for efficient batch deletion
			prefix := payloadID + "/"
			if err := blockStore.DeleteByPrefix(ctx, prefix); err != nil {
				logger.Error("GC: failed to delete orphan blocks",
					"payloadID", payloadID,
					"error", err)
				stats.Errors++
			} else {
				logger.Info("GC: deleted orphan blocks",
					"payloadID", payloadID,
					"blockCount", len(blockKeys))
			}
		}

		// Check max orphans limit
		if options.MaxOrphansPerShare > 0 && stats.OrphanFiles >= options.MaxOrphansPerShare {
			logger.Info("GC: reached max orphans limit", "limit", options.MaxOrphansPerShare)
			break
		}

		// Progress callback
		if options.ProgressCallback != nil {
			options.ProgressCallback(*stats)
		}
	}

	logger.Info("GC: complete",
		"sharesScanned", stats.SharesScanned,
		"blocksScanned", stats.BlocksScanned,
		"orphanFiles", stats.OrphanFiles,
		"orphanBlocks", stats.OrphanBlocks,
		"bytesReclaimed", stats.BytesReclaimed,
		"dryRun", options.DryRun,
		"errors", stats.Errors)

	return stats
}

// ============================================================================
// Helpers
// ============================================================================

// parsePayloadIDFromBlockKey extracts payloadID from a block key.
//
// Block key format: {payloadID}/chunk-{N}/block-{N}
// Example: "export/documents/report.pdf/chunk-0/block-0" -> "export/documents/report.pdf"
//
// Returns empty string if format is invalid.
func parsePayloadIDFromBlockKey(blockKey string) string {
	idx := strings.Index(blockKey, "/chunk-")
	if idx <= 0 {
		return ""
	}
	return blockKey[:idx]
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
