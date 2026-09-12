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

// carveDispatcher is the background carve loop. Every UploadInterval it asks the
// journal-backed local store to pack its eligible dirty ranges into remote
// blocks (journal.Carve applies its own age/size batching gate). The journal
// serializes carve per shard internally, so the dispatcher adds no lock of its
// own. It never triggers journal.GC — dead-byte GC and FileChunk-refcount reap
// are the engine's own concern (gc_block.go), kept off this loop.
//
// Runs only when a remote and the carve substrate are wired (carveActive) and
// not in ManualSync mode (where Flush/SyncNow are the sole carve drivers).
func (m *RemoteSync) carveDispatcher(ctx context.Context) {
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
// There is no per-pass limit here: concurrent PutBlock calls across all passes
// are bounded by the engine's upload window, acquired in the block sink itself
// around each PutBlock — that semaphore is the invariant, and it is what the
// goodput controller samples through TakePeak, so the count it samples is the
// count of PUTs in the air and a single large file (one pass, many PUTs) reads
// as window-limited and is ramped. Files in one shard still serialize on the
// journal's internal carve lock, so the concurrency here overlaps distinct
// shards' upload latency.
func (m *RemoteSync) carvePass(ctx context.Context) {
	ids := m.local.ListFiles(ctx)
	files := make([]string, 0, len(ids))
	for _, id := range ids {
		files = append(files, string(id))
	}
	if len(files) == 0 {
		return
	}
	// stopCh is not observed once blocked inside a file's Carve, so derive a
	// pass context that a stop cancels — otherwise a shutdown while a carve is
	// stuck on a slow PutBlock would hang the dispatcher until the slot frees.
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
		wg.Add(1)
		go func(fileID string) {
			defer wg.Done()
			// Success needs no bookkeeping here: the sink feeds the goodput sample
			// and the completed-sync counter as each block lands, which keeps both
			// advancing during a pass rather than only at its end. Concurrent PUTs
			// across all passes are bounded by the engine's upload window in the
			// block sink itself, which is the invariant — no per-pass limit here.
			if _, err := m.local.Carve(ctx, journal.CarveOptions{FileID: journal.FileID(fileID)}); err != nil {
				m.uploadErrWindow.Add(1)
				m.failedSyncs.Add(1)
				logger.Warn("carve dispatcher: file carve failed", "file", fileID, "error", err)
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
