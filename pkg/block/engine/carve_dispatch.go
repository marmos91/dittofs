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
	"github.com/marmos91/dittofs/pkg/block/syncer"
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

// carveFanOut bounds how many files this loop carves at once. It throttles
// goroutine spawn and the per-pass read buffers, nothing else: the blocks those
// passes produce queue on the syncer's shared upload window, which is the only
// bound on PutBlock concurrency. Files in one shard serialize on the journal's
// internal carve lock regardless, so the fan-out only overlaps distinct shards.
//
// ponytail: a fixed cap, not a second controller. It is deliberately the
// adaptive floor, so file fan-out never exceeds what an adaptive pass already
// allowed at its least greedy. Make it adaptive only if profiling shows passes
// starved of carve concurrency while the upload window sits unfilled — the
// bottleneck this path has actually measured is the uplink, not the carver.
const carveFanOut = AdaptiveUploadFloor

// carvePass packs every file with local data into remote blocks, carving files
// concurrently so multiple blocks are uploaded at once. A single sequential
// pass (one file, one block, one PutBlock at a time) leaves the uplink almost
// idle — the block-upload latency, not the link or CPU, caps throughput.
//
// Concurrency here is carveFanOut files, a fixed cap held separately from the
// upload window. The window itself is consumed one slot per in-flight PutBlock
// inside the passes, so it bounds exactly what it is named for and what the
// controller samples through TakePeak is the count of PUTs. Acquiring the
// window here instead would both nest the two bounds into a product and
// deadlock: a pass cannot upload while the loop holds the slot it needs.
func (m *RemoteSync) carvePass(ctx context.Context) {
	ids := m.local.ListFiles(ctx)
	files := make([]string, 0, len(ids))
	for _, id := range ids {
		files = append(files, string(id))
	}
	if len(files) == 0 {
		return
	}
	// stopCh is not observed once blocked inside the fan-out Acquire or a
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
	// Bounds goroutine spawn and live read buffers. Separate from the upload
	// window on purpose; see carveFanOut.
	fanOut := syncer.NewDynamicSemaphore(carveFanOut)
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
		// Blocks here when the fan-out is full, throttling goroutine spawn;
		// released by the worker.
		if err := fanOut.Acquire(ctx); err != nil {
			break // context cancelled
		}
		wg.Add(1)
		go func(fileID string) {
			defer wg.Done()
			defer fanOut.Release()
			// Success needs no bookkeeping here: the sink feeds the goodput sample
			// and the completed-sync counter as each block lands, which keeps both
			// advancing during a pass rather than only at its end.
			fn, reap := m.flushFn()
			if err := m.local.Flush(ctx, journal.FileID(fileID), journal.FlushOptions{AfterFile: reap}, fn); err != nil {
				m.uploadErrWindow.Add(1)
				m.failedSyncs.Add(1)
				logger.Warn("flush dispatcher: file flush failed", "file", fileID, "error", err)
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
