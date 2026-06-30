package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// blockingRemote wraps a real in-memory remote and makes every Put park until
// the test releases it, recording the high-water mark of concurrent in-flight
// Puts. It lets a deterministic test observe how many uploads the continuous
// dispatcher keeps in flight at once — without any real network or timing.
type blockingRemote struct {
	remote.RemoteStore
	release     chan struct{} // closed to let all parked Puts return
	releaseOnce sync.Once
	inflight    atomic.Int64
	peak        atomic.Int64
	started     atomic.Int64 // total Puts that have parked
}

func newBlockingRemote() *blockingRemote {
	return &blockingRemote{
		RemoteStore: remotememory.New(),
		release:     make(chan struct{}),
	}
}

// releaseAll unblocks every parked Put. Idempotent.
func (b *blockingRemote) releaseAll() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingRemote) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	cur := b.inflight.Add(1)
	b.started.Add(1)
	for {
		p := b.peak.Load()
		if cur <= p || b.peak.CompareAndSwap(p, cur) {
			break
		}
	}
	defer b.inflight.Add(-1)

	// Park until the test releases the batch (or the context is cancelled), so
	// many Puts pile up concurrently and the peak reflects the real window.
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.RemoteStore.Put(ctx, hash, data)
}

// newBlockingUploadEngine builds a started engine.Store whose remote parks
// every Put, with the upload window pinned to `window` (no adaptive controller).
// Returns the store, its local store (to feed chunks) and the blocking remote.
func newBlockingUploadEngine(t *testing.T, window int) (*Store, *fs.FSStore, *blockingRemote) {
	t.Helper()
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 1024*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     256 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}

	rs := newBlockingRemote()
	cfg := DefaultConfig()
	cfg.ParallelUploads = window
	syncer := NewSyncer(localStore, rs, ms, cfg)
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          rs,
		Syncer:          syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() {
		rs.releaseAll() // unblock anything still parked so Close can drain
		_ = bs.Close()
	})
	return bs, localStore, rs
}

// TestUploadDispatcher_SustainsConcurrencyOnShallowFeed is the #1432 behavioural
// guard. Chunks are fed into the pending set ONE AT A TIME (a trickle / shallow
// queue) while every remote Put parks. The pre-#1432 periodic uploader drained
// in snapshot-then-g.Wait() passes, so a trickle feed kept only a single Put in
// flight (one TCP stream → ~25 Mbit/s). The continuous dispatcher must instead
// fill the upload window: with parked Puts the in-flight count must climb to the
// pinned window, proving inflight tracks the window rather than collapsing to 1.
func TestUploadDispatcher_SustainsConcurrencyOnShallowFeed(t *testing.T) {
	ctx := context.Background()
	// Pin the upload window so the test isolates the dispatcher's behaviour from
	// the adaptive controller (exercised separately in upload_adaptive_test):
	// with a fixed window of `want`, the dispatcher must drive in-flight Puts up
	// to exactly that window on a shallow feed.
	const want = 24
	bs, localStore, rs := newBlockingUploadEngine(t, want)

	// Feed comfortably more chunks than the window so the dispatcher always has
	// work to fill it.
	total := want * 3

	// Trickle the chunks in one at a time, each in its own goroutine-free step,
	// to emulate a shallow queue rather than a deep backlog burst.
	for i := 0; i < total; i++ {
		data := []byte(fmt.Sprintf("upload-concurrency-chunk-%d-padding-for-distinct-bytes", i))
		h := block.ContentHash(blake3.Sum256(data))
		if err := localStore.StoreChunk(ctx, h, data); err != nil {
			t.Fatalf("StoreChunk %d: %v", i, err)
		}
		// A brief yield between feeds keeps the queue shallow: without the
		// dispatcher, only ~1 Put would ever be in flight at a time.
		time.Sleep(time.Millisecond)
	}

	// The dispatcher keeps Puts in flight without a barrier, so concurrency must
	// climb to the window. Poll the parked-Put count.
	deadline := time.Now().Add(5 * time.Second)
	for rs.inflight.Load() < int64(want) {
		if time.Now().After(deadline) {
			t.Fatalf("dispatcher did not sustain concurrency: peak in-flight Puts = %d, want >= %d (collapsed toward single-stream)",
				rs.peak.Load(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}

	if peak := rs.peak.Load(); peak < int64(want) {
		t.Fatalf("peak concurrent Puts = %d, want >= %d", peak, want)
	}
	t.Logf("sustained %d concurrent in-flight Puts on a shallow trickle feed (window=%d)",
		rs.peak.Load(), want)

	// Release the batch and let the pipeline drain cleanly.
	rs.releaseAll()

	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := bs.DrainAllUploads(drainCtx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
}

// TestFlush_NonManualSync_SoftFailsDuringUploadsThenFinalizes pins the
// production (non-ManualSync) Flush durability contract under the continuous
// dispatcher (#1432, #670). While dispatcher uploads are in flight, the
// non-blocking Flush must soft-fail (Finalized=false) rather than block — the
// upload gate is busy. Once the pipeline drains, Flush must finalize. This is
// the only test that exercises Flush with the dispatcher running (the mirror-
// loop scenarios all use ManualSync).
func TestFlush_NonManualSync_SoftFailsDuringUploadsThenFinalizes(t *testing.T) {
	ctx := context.Background()
	bs, localStore, rs := newBlockingUploadEngine(t, 8)

	const payloadID = "flush-contract"
	for i := 0; i < 6; i++ {
		data := []byte(fmt.Sprintf("flush-contract-chunk-%d-distinct-padding", i))
		h := block.ContentHash(blake3.Sum256(data))
		if err := localStore.StoreChunk(ctx, h, data); err != nil {
			t.Fatalf("StoreChunk %d: %v", i, err)
		}
	}

	// Wait until the dispatcher has at least one Put parked (gate.active > 0).
	deadline := time.Now().Add(5 * time.Second)
	for rs.inflight.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no upload became in-flight")
		}
		time.Sleep(time.Millisecond)
	}

	// Flush must soft-fail (not block) while uploads are in flight.
	res, err := bs.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush during uploads: unexpected error %v", err)
	}
	if res == nil || res.Finalized {
		t.Fatalf("Flush during in-flight uploads = %+v; want Finalized=false (soft-fail per #670)", res)
	}

	// Drain the pipeline, then Flush must finalize.
	rs.releaseAll()
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := bs.DrainAllUploads(drainCtx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}

	// Poll: once the dispatcher is idle (gate.active==0, nothing pending), the
	// non-blocking Flush acquires the gate and finalizes.
	deadline = time.Now().Add(5 * time.Second)
	for {
		res, err := bs.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush after drain: %v", err)
		}
		if res != nil && res.Finalized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Flush never finalized after the pipeline drained")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUploadDispatcher_SingleStreamWouldFailGuard documents, via a sanity check,
// that the fake remote actually serializes when only one Put is allowed at a
// time — guarding against a false-positive where the peak counter is broken.
func TestUploadDispatcher_blockingRemoteCountsConcurrency(t *testing.T) {
	rs := newBlockingRemote()
	ctx := context.Background()
	var wg sync.WaitGroup
	const n = 4
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte(fmt.Sprintf("probe-%d", i))
			_ = rs.Put(ctx, block.ContentHash(blake3.Sum256(data)), data)
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for rs.started.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d Puts parked", rs.started.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
	if peak := rs.peak.Load(); peak < n {
		t.Fatalf("blockingRemote peak=%d, want %d (counter broken)", peak, n)
	}
	rs.releaseAll()
	wg.Wait()
}
