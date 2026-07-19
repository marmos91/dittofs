package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
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
			m.carvePass(ctx)
		}
	}
}

// carvePass packs every file with local data into remote blocks, carving files
// concurrently so multiple blocks are uploaded at once. A single sequential
// pass (one file, one block, one PutBlock at a time) leaves the uplink almost
// idle — the block-upload latency, not the link or CPU, caps throughput.
//
// Concurrency is bounded by the adaptive upload window: the loop acquires
// uploadLimiter before starting each file's carve and releases it when that
// file's blocks are committed, so at most Limit() carves — and thus block
// PUTs — are in flight. Acquiring the window is also what lets the goodput
// controller observe real in-flight concurrency (TakePeak) and ramp the window;
// without it the window is never consumed and stays pinned at the floor. Files
// in one shard still serialize on the journal's internal carve lock, so the
// concurrency here overlaps distinct shards' upload latency.
func (m *Syncer) carvePass(ctx context.Context) {
	files := m.local.ListFiles(ctx)
	if len(files) == 0 {
		return
	}
	// stopCh is not observed once blocked inside uploadLimiter.Acquire or a
	// file's Carve, so derive a pass context that a stop cancels — otherwise a
	// shutdown while the window is full (or a carve is stuck on a slow PutBlock)
	// would hang the dispatcher until the slot frees.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-m.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	var wg sync.WaitGroup
	for _, id := range files {
		stop := false
		select {
		case <-ctx.Done():
			stop = true
		case <-m.stopCh:
			stop = true
		default:
		}
		if stop {
			break
		}
		if m.uploadLimiter != nil {
			// Blocks here when the window is full, throttling both concurrency
			// and goroutine spawn to the current limit; released by the worker.
			if err := m.uploadLimiter.Acquire(ctx); err != nil {
				break // context cancelled
			}
		}
		wg.Add(1)
		go func(fileID string) {
			defer wg.Done()
			if m.uploadLimiter != nil {
				defer m.uploadLimiter.Release()
			}
			res, err := m.local.Carve(ctx, journal.CarveOptions{FileID: journal.FileID(fileID)})
			if err != nil {
				m.uploadErrWindow.Add(1)
				m.failedSyncs.Add(1)
				logger.Warn("carve dispatcher: file carve failed", "file", fileID, "error", err)
				return
			}
			if res.BytesCarved > 0 {
				// Feed the adaptive-upload goodput sample and the lifetime
				// completed-sync counter (blocks committed for this file).
				m.uploadedBytesWindow.Add(res.BytesCarved)
				m.completedSyncs.Add(int64(res.BlocksWritten))
			}
		}(id)
	}
	wg.Wait()
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
