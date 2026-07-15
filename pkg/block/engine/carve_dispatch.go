package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// carveBlockSize returns the configured target block size, falling back to the
// default when unset. Retained for the cas→blocks migration repacker.
func (m *Syncer) carveBlockSize() int64 {
	if m.config.BlockCarveBytes > 0 {
		return m.config.BlockCarveBytes
	}
	return DefaultBlockCarveBytes
}

// carveDispatcher is the background carve loop. Every UploadInterval it asks the
// journal-backed local store to pack its eligible dirty ranges into remote
// blocks (journal.Carve applies its own age/size batching gate). The journal
// serializes carve per shard internally, so the dispatcher adds no lock of its
// own. It never triggers journal.GC — dead-byte GC and FileChunk-refcount reap
// are the engine's own concern (gc_block.go), kept off this loop.
//
// Runs only when a remote and the carve substrate are wired (carveActive) and
// not in ManualSync mode (where Flush/SyncNow are the sole carve drivers).
func (m *Syncer) carveDispatcher(ctx context.Context) {
	logger.Info("Carve dispatcher started")
	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !m.canProcess(ctx) {
				return
			}
			if !m.carveActive.Load() || !m.IsRemoteHealthy() {
				continue
			}
			res, err := m.local.Carve(ctx, journal.CarveOptions{})
			if err != nil {
				m.uploadErrWindow.Add(1)
				m.failedSyncs.Add(1)
				logger.Warn("carve dispatcher: carve pass failed", "error", err)
				continue
			}
			if res.BytesCarved > 0 {
				// Feed the adaptive-upload goodput sample and the lifetime
				// completed-sync counter (blocks committed this pass).
				m.uploadedBytesWindow.Add(res.BytesCarved)
				m.completedSyncs.Add(int64(res.BlocksWritten))
			}
		}
	}
}

// newBlockID returns a fresh, unguessable block object key. crypto/rand keeps it
// collision-free under concurrent carvers (unlike a timestamp) and unrelated to
// the block's content hash, so a re-carve after a crash always targets a new
// object.
func newBlockID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("carve: generate block id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
