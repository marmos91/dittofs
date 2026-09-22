package engine

import (
	"context"
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

// carveFanOut is the FLOOR on how many files one pass carves at once, not the
// cap: carvePass sizes its fan-out to max(carveFanOut, current upload window).
// The fan-out must never be the narrower of the two, because carve serializes
// per shard (journal flushMu is shard-scoped) and workers scatter over shards
// balls-in-bins — a fan-out equal to the shard count leaves roughly a third of
// the shards idle. Pinning it at a constant therefore capped PUT concurrency
// near 13 of an allowed 64, relocating "the window does not bound what it
// claims to" from one large file onto many small ones.
//
// ponytail: a floor plus the live window read once per pass, not a second
// controller. Sizing from the window is not the same as acquiring it — taking
// a slot per file is what made upload concurrency the product of two windows,
// and would deadlock a pass against its own uploads.
//
// The ceiling this buys is read-buffer memory, and it is bounded by shards
// rather than by the fan-out: a worker allocates chunker.MaxChunkSize (16 MiB)
// inside the flush, after the shard lock, so parked workers hold nothing. Peak
// is min(fanOut, ShardCount) x 16 MiB — 256 MiB at the default 16 shards, and
// flat as the window ramps. Revisit if ShardCount ever grows far past the
// window, where that product stops being bounded by the shard count.
const carveFanOut = AdaptiveUploadFloor

// carvePass packs every file with local data into remote blocks, carving files
// concurrently so multiple blocks are uploaded at once. A single sequential
// pass (one file, one block, one PutBlock at a time) leaves the uplink almost
// idle — the block-upload latency, not the link or CPU, caps throughput.
//
// Concurrency here is max(carveFanOut, upload window) files, sized from the
// window but holding none of its slots. The window itself is consumed one slot
// per in-flight PutBlock inside the passes, so it bounds exactly what it is
// named for, and what the controller samples through TakePeak is the count of
// PUTs. Acquiring the window here instead would both nest the two bounds into a
// product and deadlock: a pass cannot upload while the loop holds the slot it
// needs.
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
	// Bounds goroutine spawn and live read buffers. Sized from the window but
	// holding none of its slots; read once per pass, so a mid-pass resize lands
	// on the next one. See carveFanOut.
	//
	// The nil check is not defensive noise: NewRemoteSync always sets the
	// limiter, but RemoteSync is also built as a bare struct literal in tests,
	// and this is a plain read rather than an acquire, so a missing window must
	// degrade to the floor rather than panic. It cannot silently un-bound
	// anything — the fan-out is its own semaphore, and the upload bound lives
	// on the limiter that flushFn passes to the chain.
	window := carveFanOut
	if m.uploadLimiter != nil {
		window = max(carveFanOut, m.uploadLimiter.Limit())
	}
	fanOut := syncer.NewDynamicSemaphore(window)
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
