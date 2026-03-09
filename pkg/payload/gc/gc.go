package gc

import (
	"context"
	"os"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// BlockSize is the size of a single block (8MB), used for byte estimation.
const BlockSize = 8 * 1024 * 1024

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

	for payloadID, blockKeys := range blocksByPayload {
		if ctx.Err() != nil {
			logger.Info("GC: cancelled", "processed", stats.OrphanFiles)
			return stats
		}

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

		metaStore, err := reconciler.GetMetadataStoreForShare(shareName)
		if err != nil {
			logger.Debug("GC: share not found, treating as orphan",
				"shareName", shareName,
				"payloadID", payloadID)
			// Fall through to delete blocks
		} else {
			_, err = metaStore.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
			if err == nil {
				continue
			}
		}

		stats.OrphanFiles++
		stats.OrphanBlocks += len(blockKeys)
		stats.BytesReclaimed += int64(len(blockKeys)) * int64(BlockSize)

		logger.Info("GC: found orphan blocks",
			"payloadID", payloadID,
			"blockCount", len(blockKeys),
			"dryRun", options.DryRun)

		if !options.DryRun {
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

		if options.MaxOrphansPerShare > 0 && stats.OrphanFiles >= options.MaxOrphansPerShare {
			logger.Info("GC: reached max orphans limit", "limit", options.MaxOrphansPerShare)
			break
		}

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

// CollectUnreferenced removes FileBlocks with RefCount=0 and their associated
// block store objects and cache files. This is the FileBlock-based GC that
// complements the block-store-scan-based CollectGarbage.
//
// Unreferenced blocks occur when:
//   - A file is deleted but its blocks are shared with other files (RefCount decremented)
//   - Dedup replaces a pending block with an existing one (old block drops to RefCount=0)
//
// Parameters:
//   - ctx: Context for cancellation
//   - fileBlockStore: FileBlockStore to query for unreferenced blocks
//   - blockStore: Block store for deleting uploaded objects (may be nil to skip)
//   - batchSize: Max blocks to process per invocation (0 = 100)
//   - dryRun: If true, only count without deleting
//
// Returns the number of blocks cleaned up and any error.
func CollectUnreferenced(
	ctx context.Context,
	fileBlockStore metadata.FileBlockStore,
	blockStore store.BlockStore,
	batchSize int,
	dryRun bool,
) (int, error) {
	if batchSize <= 0 {
		batchSize = 100
	}

	unreferenced, err := fileBlockStore.ListUnreferenced(ctx, batchSize)
	if err != nil {
		return 0, err
	}

	if len(unreferenced) == 0 {
		return 0, nil
	}

	logger.Info("GC: found unreferenced blocks", "count", len(unreferenced), "dryRun", dryRun)

	cleaned := 0
	for _, fb := range unreferenced {
		if ctx.Err() != nil {
			break
		}

		if dryRun {
			cleaned++
			continue
		}

		if fb.BlockStoreKey != "" && blockStore != nil {
			if err := blockStore.DeleteBlock(ctx, fb.BlockStoreKey); err != nil {
				logger.Warn("GC: failed to delete block from store",
					"blockID", fb.ID, "key", fb.BlockStoreKey, "error", err)
			}
		}

		if fb.CachePath != "" {
			if err := os.Remove(fb.CachePath); err != nil && !os.IsNotExist(err) {
				logger.Warn("GC: failed to delete cache file",
					"blockID", fb.ID, "path", fb.CachePath, "error", err)
			}
		}

		if err := fileBlockStore.DeleteFileBlock(ctx, fb.ID); err != nil {
			logger.Warn("GC: failed to delete file block",
				"blockID", fb.ID, "error", err)
			continue
		}

		cleaned++
	}

	logger.Info("GC: unreferenced cleanup complete",
		"cleaned", cleaned, "dryRun", dryRun)

	return cleaned, nil
}

// parsePayloadIDFromBlockKey extracts payloadID from a block key.
//
// Block key format: {payloadID}/block-{N}
// Example: "export/documents/report.pdf/block-0" -> "export/documents/report.pdf"
//
// Returns empty string if format is invalid.
func parsePayloadIDFromBlockKey(blockKey string) string {
	idx := strings.Index(blockKey, "/block-")
	if idx <= 0 {
		return ""
	}
	return blockKey[:idx]
}

// parseShareName extracts the share name from a payloadID.
// PayloadID format: "shareName/path/to/file"
// Returns empty string if format is invalid.
func parseShareName(payloadID string) string {
	payloadID = strings.TrimPrefix(payloadID, "/")
	if payloadID == "" {
		return ""
	}

	share, _, found := strings.Cut(payloadID, "/")
	if !found {
		return payloadID // No separator: "export" (file at root of share)
	}
	return share
}
