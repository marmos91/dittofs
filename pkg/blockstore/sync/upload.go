package sync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// maxUploadBatch limits how many blocks are uploaded per periodic tick.
// Each block read from disk is ~8MB. Sequential processing ensures only
// 1 block (~8MB) is in heap at a time.
const maxUploadBatch = 4

// revertToLocal reverts a FileBlock to Local state so the periodic syncer retries it.
func (m *Syncer) revertToLocal(ctx context.Context, fb *blockstore.FileBlock) {
	fb.State = blockstore.BlockStateLocal
	_ = m.fileBlockStore.PutFileBlock(ctx, fb)
}

// syncLocalBlocks scans FileBlockStore for local blocks not yet synced
// to remote, and uploads them sequentially. Called by the periodic syncer.
//
// Memory safety: ListLocalBlocks is called with limit=maxUploadBatch to
// avoid scanning and deserializing thousands of FileBlock entries from BadgerDB.
// The periodic syncer guards against overlapping ticks, so at most one
// instance of this function runs at a time.
func (m *Syncer) syncLocalBlocks(ctx context.Context) {
	if m.remoteStore == nil {
		return
	}

	// Flush queued FileBlock metadata so ListLocalBlocks can find recently flushed blocks.
	m.local.SyncFileBlocks(ctx)

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
	// Individual block failures are already logged inside syncFileBlock; the
	// periodic uploader intentionally continues so a bad block doesn't starve
	// the queue.
	for _, fb := range pending {
		if fb.LocalPath == "" {
			continue
		}
		_ = m.syncFileBlock(ctx, fb)
	}
}

// syncFileBlock reads a local block from the local store, dedup-checks, and
// syncs to remote store. Returns nil on success (including the dedup fast path)
// or an error describing why the block was not uploaded. The block's state is
// always reverted to Local before a non-nil error is returned so the next
// drain/tick can retry it.
func (m *Syncer) syncFileBlock(ctx context.Context, fb *blockstore.FileBlock) error {
	if fb.State != blockstore.BlockStateLocal {
		return nil
	}

	fb.State = blockstore.BlockStateSyncing
	if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
		return fmt.Errorf("mark block %s syncing: %w", fb.ID, err)
	}

	startTime := time.Now()

	data, err := os.ReadFile(fb.LocalPath)
	if err != nil {
		logger.Warn("Sync: failed to read local store file",
			"blockID", fb.ID, "localPath", fb.LocalPath, "error", err)
		m.revertToLocal(ctx, fb)
		return fmt.Errorf("read local block %s: %w", fb.ID, err)
	}

	hash := sha256.Sum256(data)

	existing, err := m.fileBlockStore.FindFileBlockByHash(ctx, hash)
	if err == nil && existing != nil && existing.IsRemote() {
		_ = m.fileBlockStore.IncrementRefCount(ctx, existing.ID)
		fb.Hash = blockstore.ContentHash(hash)
		fb.DataSize = uint32(len(data))
		fb.BlockStoreKey = existing.BlockStoreKey
		fb.State = blockstore.BlockStateRemote
		_ = m.fileBlockStore.PutFileBlock(ctx, fb)
		logger.Debug("Sync dedup: block already exists", "blockID", fb.ID)
		return nil
	}

	lastSlash := strings.LastIndex(fb.ID, "/")
	payloadID := fb.ID[:lastSlash]
	blockIdx, err := strconv.ParseUint(fb.ID[lastSlash+1:], 10, 64)
	if err != nil {
		logger.Warn("Sync: failed to parse block index", "blockID", fb.ID, "error", err)
		m.revertToLocal(ctx, fb)
		return fmt.Errorf("parse block index for %s: %w", fb.ID, err)
	}
	storeKey := blockstore.FormatStoreKey(payloadID, blockIdx)

	if err := m.remoteStore.WriteBlock(ctx, storeKey, data); err != nil {
		logger.Error("Sync: upload to remote failed", "blockID", fb.ID, "error", err)
		m.revertToLocal(ctx, fb)
		return fmt.Errorf("upload block %s: %w", fb.ID, err)
	}

	fb.Hash = blockstore.ContentHash(hash)
	fb.DataSize = uint32(len(data))
	fb.BlockStoreKey = storeKey
	fb.State = blockstore.BlockStateRemote
	_ = m.fileBlockStore.PutFileBlock(ctx, fb)

	logger.Info("Sync complete",
		"blockID", fb.ID, "size", len(data), "duration", time.Since(startTime))
	return nil
}

// uploadBlock uploads a single block from local store to remote store.
// Called by queue workers for block-level upload requests.
func (m *Syncer) uploadBlock(ctx context.Context, payloadID string, blockIdx uint64) error {
	if !m.canProcess(ctx) {
		return ErrClosed
	}
	if m.remoteStore == nil {
		return errors.New("no remote store configured")
	}

	data, _, err := m.local.GetBlockData(ctx, payloadID, blockIdx)
	if err != nil {
		return fmt.Errorf("block not in local store (blockIdx=%d): %w", blockIdx, err)
	}

	storeKey := blockstore.FormatStoreKey(payloadID, blockIdx)
	if err := m.remoteStore.WriteBlock(ctx, storeKey, data); err != nil {
		return fmt.Errorf("upload block %s: %w", storeKey, err)
	}

	return nil
}
