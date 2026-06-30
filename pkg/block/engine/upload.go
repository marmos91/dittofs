package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
)

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
	m.ensureGate()
	ok, err := m.gate.acquireExclusive(ctx, false)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer m.gate.releaseExclusive()
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
