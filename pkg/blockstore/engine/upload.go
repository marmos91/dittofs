package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// syncLocalBlocks runs one claim+upload cycle for the periodic uploader
// tick path; SyncNow reuses claimBatch + uploadOne directly with a bounded
// pool.
func (m *Syncer) syncLocalBlocks(ctx context.Context) {
	if m.remoteStore == nil {
		return
	}

	// Flush queued FileBlock metadata so ListLocalBlocks can find recently flushed blocks.
	m.local.SyncFileBlocks(ctx)

	batch, err := m.claimBatch(ctx, m.config.ClaimBatchSize)
	if err != nil {
		logger.Warn("Periodic sync: claim batch failed", "error", err)
		return
	}
	if len(batch) == 0 {
		return
	}

	logger.Info("Periodic sync: claimed batch", "count", len(batch))

	for _, fb := range batch {
		if fb.LocalPath == "" {
			continue
		}
		if err := m.uploadOne(ctx, fb); err != nil {
			// Row stays in Syncing; janitor will requeue after ClaimTimeout.
			logger.Debug("Periodic uploadOne failed", "blockID", fb.ID, "error", err)
		}
	}
}

// syncFileBlock performs the Pending → Syncing → Remote transition for a
// single FileBlock. Deprecated: prefer claimBatch + uploadOne. Retained
// for direct test callers that exercise the post-PUT metadata error
// contract (the row stays Syncing on failure; the janitor owns the
// requeue per D-14).
func (m *Syncer) syncFileBlock(ctx context.Context, fb *blockstore.FileBlock) error {
	if fb.State != blockstore.BlockStatePending {
		return nil
	}
	fb.State = blockstore.BlockStateSyncing
	fb.LastSyncAttemptAt = time.Now()
	if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
		return fmt.Errorf("mark block %s syncing: %w", fb.ID, err)
	}
	return m.uploadOne(ctx, fb)
}

// uploadOne uploads a single Syncing block to CAS storage and flips it to
// Remote. Sole owner of the Syncing → Remote transition (D-15).
//
// INV-03: Remote is persisted ONLY after PUT-success AND metadata-txn
// success. On any failure the row stays Syncing — the janitor requeues it
// after ClaimTimeout, and CAS keys being content-defined make any
// re-upload byte-identical and idempotent. Orphan CAS objects from
// post-PUT meta failures are reaped by GC after the grace period.
func (m *Syncer) uploadOne(ctx context.Context, fb *blockstore.FileBlock) error {
	if fb.State != blockstore.BlockStateSyncing {
		return fmt.Errorf("uploadOne: expected BlockStateSyncing for %s, got %v", fb.ID, fb.State)
	}
	startTime := time.Now()

	data, err := os.ReadFile(fb.LocalPath)
	if err != nil {
		return fmt.Errorf("read local block %s at %s: %w", fb.ID, fb.LocalPath, err)
	}

	// BLAKE3-256 (Phase 10 D-08 amendment + BSCAS-03).
	h := blake3.Sum256(data)
	var hash blockstore.ContentHash
	copy(hash[:], h[:])

	// Pre-PUT dedup short-circuit (DEDUP-01 carried over from Phase 10).
	// If another block in the metadata store already holds this hash and is
	// confirmed Remote, we skip the PUT entirely and re-use its CAS key.
	//
	// Phase 11 WR-03: surface IncrementRefCount errors. The previous code
	// silently dropped them, which meant a transient metadata error left
	// the new fb pointing at the donor's CAS key without a balanced
	// refcount bump and with no observable signal. The dedup short-circuit
	// itself still leaks a refcount on the donor (no decrement path
	// reverses it on file delete) — see WR-03 follow-up; for now we at
	// least do not also swallow the error path.
	if existing, derr := m.fileBlockStore.FindFileBlockByHash(ctx, hash); derr == nil && existing != nil && existing.IsRemote() {
		if err := m.fileBlockStore.IncrementRefCount(ctx, existing.ID); err != nil {
			return fmt.Errorf("dedup increment refcount on donor %s: %w", existing.ID, err)
		}
		fb.Hash = hash
		fb.DataSize = uint32(len(data))
		fb.BlockStoreKey = existing.BlockStoreKey
		fb.State = blockstore.BlockStateRemote
		if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
			return fmt.Errorf("persist dedup block %s: %w", fb.ID, err)
		}
		logger.Debug("uploadOne dedup: hash already remote", "blockID", fb.ID, "key", fb.BlockStoreKey)
		return nil
	}

	// CAS PUT (BSCAS-01 + BSCAS-06). content-hash header is set inside
	// WriteBlockWithHash by the s3 store implementation.
	casKey := blockstore.FormatCASKey(hash)
	if err := m.remoteStore.WriteBlockWithHash(ctx, casKey, hash, data); err != nil {
		return fmt.Errorf("upload block %s to %s: %w", fb.ID, casKey, err)
	}

	// PUT succeeded; only NOW promote to Remote (INV-03 ordering).
	fb.Hash = hash
	fb.DataSize = uint32(len(data))
	fb.BlockStoreKey = casKey
	fb.State = blockstore.BlockStateRemote
	if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
		// S3 object exists; row stayed Syncing. GC + janitor will resolve
		// (D-11/D-14). INV-03 honored — Remote is not persisted.
		return fmt.Errorf("mark remote block %s: %w", fb.ID, err)
	}

	logger.Info("uploadOne complete",
		"blockID", fb.ID, "size", len(data), "key", casKey,
		"duration", time.Since(startTime))
	return nil
}

// uploadBlock uploads a single block from local store to remote store.
// Called by queue workers for block-level upload requests.
//
// Phase 11: now drives the CAS path for parity with uploadOne. The legacy
// FormatStoreKey path is gone from the upload write path. Reads of legacy
// objects (Phase 11 → Phase 14 dual-read window) are still serviced by
// the engine's read path resolver.
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

	h := blake3.Sum256(data)
	var hash blockstore.ContentHash
	copy(hash[:], h[:])
	casKey := blockstore.FormatCASKey(hash)
	if err := m.remoteStore.WriteBlockWithHash(ctx, casKey, hash, data); err != nil {
		return fmt.Errorf("upload block %s: %w", casKey, err)
	}
	return nil
}
