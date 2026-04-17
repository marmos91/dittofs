package gc

import (
	"context"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockSize is the size of a single block (8MB), used for byte estimation.
const BlockSize = blockstore.BlockSize

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

	// BackupHold is consulted once per CollectGarbage run. PayloadIDs
	// returned by HeldPayloadIDs are treated as live even when no
	// metadata references them. nil disables the hold check.
	//
	// See Phase 5 CONTEXT.md D-11.
	BackupHold BackupHoldProvider
}

// MetadataReconciler provides access to metadata operations for reconciliation.
// This interface is implemented by the Registry.
type MetadataReconciler interface {
	// GetMetadataStoreForShare returns the metadata store for a given share name.
	GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error)
}

// BackupHoldProvider returns the set of PayloadIDs that are "held"
// by retained backup manifests and must NOT be treated as orphans
// even if no live metadata references them. Implementations compute
// the set at GC time by unioning PayloadIDSet fields from every
// succeeded BackupRecord's manifest.
//
// A nil BackupHoldProvider passed via Options disables the hold
// check (pre-Phase-5 behavior / tests without backup infra).
//
// See Phase 5 CONTEXT.md D-11, D-12, D-13 and Pitfall #3.
type BackupHoldProvider interface {
	HeldPayloadIDs(ctx context.Context) (map[metadata.PayloadID]struct{}, error)
}

// StaticBackupHold wraps a pre-resolved hold set as a BackupHoldProvider.
// Used by callers that resolve the hold eagerly (e.g. Runtime.RunBlockGC,
// which fails hard if the hold query errors — see Phase 5 SAFETY-01) to
// inject the already-known set into CollectGarbage without re-querying.
func StaticBackupHold(held map[metadata.PayloadID]struct{}) BackupHoldProvider {
	return staticHold(held)
}

type staticHold map[metadata.PayloadID]struct{}

func (h staticHold) HeldPayloadIDs(context.Context) (map[metadata.PayloadID]struct{}, error) {
	return h, nil
}

// CollectGarbage scans the remote store and removes orphan blocks.
//
// Orphan blocks are blocks that exist in the remote store but have no
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
//   - remoteStore: The remote store to scan
//   - reconciler: Interface to check metadata existence (typically Registry)
//   - options: GC configuration (nil uses defaults)
//
// Returns:
//   - *Stats: Summary of GC actions
func CollectGarbage(
	ctx context.Context,
	remoteStore remote.RemoteStore,
	reconciler MetadataReconciler,
	options *Options,
) *Stats {
	stats := &Stats{}

	if options == nil {
		options = &Options{}
	}

	// List all blocks (with optional prefix filter)
	blocks, err := remoteStore.ListByPrefix(ctx, options.SharePrefix)
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

	// Phase-5 D-11: compute the hold set at the start of the run. Errors
	// are logged and swallowed (fail-open: under-hold slightly rather
	// than abort GC).
	var heldSet map[metadata.PayloadID]struct{}
	if options.BackupHold != nil {
		held, err := options.BackupHold.HeldPayloadIDs(ctx)
		if err != nil {
			logger.Warn("GC: backup hold provider failed, proceeding without hold",
				"error", err)
		} else {
			heldSet = held
			logger.Info("GC: backup hold computed",
				"payloadIDs", len(heldSet))
		}
	}

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

		// Phase-5 D-11: consult the backup hold set before accounting this
		// payload as orphan. Held PayloadIDs are referenced by at least one
		// retained backup manifest and must be preserved.
		if heldSet != nil {
			if _, isHeld := heldSet[metadata.PayloadID(payloadID)]; isHeld {
				logger.Info("GC: holding orphan for backup",
					"payloadID", payloadID,
					"shareName", shareName)
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
			if err := remoteStore.DeleteByPrefix(ctx, prefix); err != nil {
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
