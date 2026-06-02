package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// syncLocalBlocks runs one mirror-loop pass for the periodic uploader
// tick body. The mirror-loop helper is shared with explicit Flush
// every CAS hash present locally but not yet marked synced is Put to
// remote and MarkSynced, with Put-then-Mark ordering for crash safety.
//
// Caller (periodicUploader) MUST hold the uploading atomic gate.
func (m *Syncer) syncLocalBlocks(ctx context.Context) {
	if m.remoteStore == nil {
		return
	}
	// Flush queued FileBlock metadata so subsequent passes see any
	// recently rolled-up chunks.
	m.local.SyncFileBlocks(ctx)

	if err := m.mirrorOnce(ctx); err != nil {
		switch {
		case ctx.Err() != nil || errors.Is(err, ErrClosed):
			// Shutdown paths (context cancelled, syncer closed) are
			// expected during graceful Close and stay at Debug.
			logger.Debug("Periodic mirror pass aborted during shutdown", "error", err)
		case errors.Is(err, block.ErrChunkLostBeforeMirror):
			// One or more pending hashes had no local bytes and were
			// retained for the next tick. mirrorOnce already drained every
			// healthy hash this pass, so this is non-fatal — swallow it as
			// an expected retry condition rather than a failure.
			logger.Info("Periodic mirror pass retained chunk(s) with missing local bytes for retry", "error", err)
		default:
			// A genuine remote upload failure (network, auth, quota, S3
			// 5xx, local bitrot) is unexpected: log at Warn so it is
			// visible before the next health-check interval.
			logger.Warn("Periodic mirror pass failed", "error", err)
		}
	}
}

// uploadBlock uploads a single block from the local store to the
// remote store. Wired into the SyncQueue's per-block upload worker
// path; not invoked from the Flush mirror loop. The mirror-loop world
// addresses pending uploads by hash via ListUnsynced, so the queue's
// per-block upload path is largely vestigial — kept as the queue's
// processUpload target until a follow-up retires the queue's upload
// channel entirely.
func (m *Syncer) uploadBlock(ctx context.Context, payloadID string, blockIdx uint64) error {
	if !m.canProcess(ctx) {
		return ErrClosed
	}
	if m.remoteStore == nil {
		return errors.New("no remote store configured")
	}
	// Drive the mirror loop opportunistically — any locally present
	// chunk for this payloadID that has not been marked synced will be
	// caught up. The blockIdx hint is ignored under hash-keyed CAS.
	if !m.uploading.CompareAndSwap(false, true) {
		return nil
	}
	defer m.uploading.Store(false)
	if err := m.mirrorOnce(ctx); err != nil {
		if errors.Is(err, block.ErrChunkLostBeforeMirror) {
			// Opportunistic drive-by drain: a retained-for-retry hash is
			// non-fatal here (the healthy hashes were still uploaded).
			return nil
		}
		return fmt.Errorf("upload block (payload=%s, idx=%d): %w", payloadID, blockIdx, err)
	}
	return nil
}
