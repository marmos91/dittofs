package offloader

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
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
	// Direct-write mode: all blocks go straight to Remote in the payload store.
	// No local blocks exist, so skip the expensive ListLocalBlocks scan.
	if m.cache.IsDirectWrite() {
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
	fmt.Sscanf(fb.ID[lastSlash+1:], "%d", &blockIdx)
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

// uploadRemainingBlocks uploads dirty blocks for a specific file.
// Used by Flush for immediate upload (ignoring UploadDelay).
func (m *Offloader) uploadRemainingBlocks(ctx context.Context, payloadID string) error {
	pending, err := m.cache.GetDirtyBlocks(ctx, payloadID)
	if err != nil {
		return fmt.Errorf("get dirty blocks: %w", err)
	}

	if len(pending) == 0 {
		return nil
	}

	logger.Info("Flush: syncing remaining blocks",
		"payloadID", payloadID, "blocks", len(pending))

	var wg sync.WaitGroup
	for _, blk := range pending {
		blockIdx := blk.BlockIndex

		hash := blk.Hash
		if hash == [32]byte{} {
			hash = sha256.Sum256(blk.Data[:blk.DataSize])
		}

		existing, findErr := m.fileBlockStore.FindFileBlockByHash(ctx, metadata.ContentHash(hash))
		if findErr == nil && existing != nil && existing.IsRemote() {
			_ = m.fileBlockStore.IncrementRefCount(ctx, existing.ID)
			m.cache.MarkBlockRemote(ctx, payloadID, blockIdx)
			m.trackBlockHash(payloadID, blockIdx, hash)
			continue
		}

		if !m.cache.MarkBlockSyncing(ctx, payloadID, blockIdx) {
			continue
		}

		uploadData := make([]byte, blk.DataSize)
		copy(uploadData, blk.Data[:blk.DataSize])

		wg.Add(1)
		m.uploadSem <- struct{}{}

		go func(data []byte, dataSize uint32, bi uint64, hash [32]byte) {
			defer func() {
				<-m.uploadSem
				wg.Done()
			}()

			storeKey := cache.FormatStoreKey(payloadID, bi)
			if err := m.blockStore.WriteBlock(ctx, storeKey, data); err != nil {
				logger.Error("Flush sync failed",
					"payloadID", payloadID, "storeKey", storeKey, "error", err)
				m.cache.MarkBlockLocal(ctx, payloadID, bi)
				return
			}

			m.handleUploadSuccess(ctx, payloadID, bi, hash, dataSize)
		}(uploadData, blk.DataSize, blockIdx, hash)
	}

	wg.Wait()
	return nil
}

// trackBlockHash records a block hash for finalization callback.
func (m *Offloader) trackBlockHash(payloadID string, blockIdx uint64, hash [32]byte) {
	state := m.getUploadState(payloadID)
	if state != nil {
		state.blocksMu.Lock()
		state.blockHashes[blockIdx] = hash
		state.blocksMu.Unlock()
	}
}

// uploadBlock uploads a single block from cache to block store.
// Called by queue workers for block-level upload requests.
func (m *Offloader) uploadBlock(ctx context.Context, payloadID string, blockIdx uint64) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
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
