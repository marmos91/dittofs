package offloader

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// maxUploadBatch limits how many blocks are uploaded per periodic tick.
// Each block read from disk is ~8MB. Sequential processing ensures only
// 1 block (~8MB) is in heap at a time.
const maxUploadBatch = 4

// uploadPendingBlocks scans FileBlockStore for local blocks not yet synced
// to remote, and uploads them sequentially. Called by the periodic syncer.
//
// Memory safety: ListLocalBlocks is called with limit=maxUploadBatch to
// avoid scanning and deserializing thousands of FileBlock entries from BadgerDB.
// The periodic syncer guards against overlapping ticks, so at most one
// instance of this function runs at a time.
func (m *Offloader) uploadPendingBlocks(ctx context.Context) {
	if m.blockStore == nil {
		return
	}

	pending, err := m.fileBlockStore.ListLocalBlocks(ctx, m.config.UploadDelay, maxUploadBatch)
	if err != nil {
		logger.Warn("Periodic sync: failed to list local blocks", "error", err)
		return
	}

	if len(pending) == 0 {
		return
	}

	logger.Info("Periodic sync: found local blocks", "count", len(pending))

	// Upload sequentially to minimize memory: only 1 block (~8MB) in memory at a time.
	for _, fb := range pending {
		if fb.CachePath == "" {
			continue
		}
		m.uploadFileBlock(ctx, fb)
	}
}

// uploadFileBlock reads a local block from cache, dedup-checks, and syncs to remote store.
func (m *Offloader) uploadFileBlock(ctx context.Context, fb *metadata.FileBlock) {
	if fb.State != metadata.BlockStateLocal {
		return
	}

	fb.State = metadata.BlockStateSyncing
	if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
		return
	}

	startTime := time.Now()

	data, err := os.ReadFile(fb.CachePath)
	if err != nil {
		logger.Warn("Sync: failed to read cache file",
			"blockID", fb.ID, "cachePath", fb.CachePath, "error", err)
		// Revert to Local so it can be retried
		fb.State = metadata.BlockStateLocal
		_ = m.fileBlockStore.PutFileBlock(ctx, fb)
		return
	}

	hash := sha256.Sum256(data)

	existing, err := m.fileBlockStore.FindFileBlockByHash(ctx, hash)
	if err == nil && existing != nil && existing.IsRemote() {
		_ = m.fileBlockStore.IncrementRefCount(ctx, existing.ID)
		fb.Hash = metadata.ContentHash(hash)
		fb.DataSize = uint32(len(data))
		fb.BlockStoreKey = existing.BlockStoreKey
		fb.State = metadata.BlockStateRemote
		_ = m.fileBlockStore.PutFileBlock(ctx, fb)
		logger.Debug("Sync dedup: block already exists", "blockID", fb.ID)
		return
	}

	lastSlash := strings.LastIndex(fb.ID, "/")
	payloadID := fb.ID[:lastSlash]
	var blockIdx uint64
	if _, err := fmt.Sscanf(fb.ID[lastSlash+1:], "%d", &blockIdx); err != nil {
		logger.Warn("Sync: failed to parse block index", "blockID", fb.ID, "error", err)
		fb.State = metadata.BlockStateLocal
		_ = m.fileBlockStore.PutFileBlock(ctx, fb)
		return
	}
	storeKey := cache.FormatStoreKey(payloadID, blockIdx)

	if err := m.blockStore.WriteBlock(ctx, storeKey, data); err != nil {
		logger.Error("Sync: upload to remote failed", "blockID", fb.ID, "error", err)
		// Revert to Local so it can be retried
		fb.State = metadata.BlockStateLocal
		_ = m.fileBlockStore.PutFileBlock(ctx, fb)
		return
	}

	fb.Hash = metadata.ContentHash(hash)
	fb.DataSize = uint32(len(data))
	fb.BlockStoreKey = storeKey
	fb.State = metadata.BlockStateRemote
	_ = m.fileBlockStore.PutFileBlock(ctx, fb)

	logger.Info("Sync complete",
		"blockID", fb.ID, "size", len(data), "duration", time.Since(startTime))
}

// uploadBlock uploads a single block from cache to block store.
// Called by queue workers for block-level upload requests.
func (m *Offloader) uploadBlock(ctx context.Context, payloadID string, blockIdx uint64) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}
	if m.blockStore == nil {
		return fmt.Errorf("no remote store configured")
	}

	data, _, err := m.cache.GetBlockData(ctx, payloadID, blockIdx)
	if err != nil {
		return fmt.Errorf("block not in cache: blockIdx=%d", blockIdx)
	}

	storeKey := cache.FormatStoreKey(payloadID, blockIdx)
	if err := m.blockStore.WriteBlock(ctx, storeKey, data); err != nil {
		return fmt.Errorf("upload block %s: %w", storeKey, err)
	}

	return nil
}
