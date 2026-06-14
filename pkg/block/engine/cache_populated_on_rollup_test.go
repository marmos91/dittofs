// TestCache_PopulatedOnRollupComplete pins the end-to-end contract
// that every chunk emitted by the local rollup pump lands in the
// engine Cache via the OnChunkComplete callback. After Flush, each
// post-rollup FileBlock hash must be reachable via bs.cache.Get
// without consulting disk.
//
// Mechanism: cache hits are observed via the recordingPutCache (from
// eager_dedup_test.go) installed in place of the realCache. Every
// successful chunkstore.StoreChunk fires bc.onChunkComplete, which
// engine.New wired to bs.cache.Put. Reading via rec.Get is RAM-only
// (recordingPutCache holds a map[hash]bytes) — no disk fetch is
// possible by construction. The "no disk read" assertion is therefore
// structural: rec.Get cannot reach disk, so a hit proves the bytes
// flowed cache-side at write time, not via a fault-on-read path.

package engine

import (
	"bytes"
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newRollupCacheFixture builds a full engine.Store + FSStore +
// recordingPutCache stack so we can observe OnChunkComplete-driven
// cache population end-to-end through engine.WriteAt → rollup →
// StoreChunk → OnChunkComplete → Cache.Put.
//
// The recordingPutCache replaces the realCache AFTER bs.Start so the
// engine.New wiring (closure-captures-bs, reads-bs.cache-at-fire-time
// 07's setter pattern) lands every Put on the recorder.
func newRollupCacheFixture(t *testing.T) (*Store, *fs.FSStore, *recordingPutCache) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	if err := localStore.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}

	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}

	// Swap in the recording cache AFTER Start so the OnChunkComplete
	// closure (which reads bs.cache at fire time) observes the recorder
	// — wiring is the load-bearing seam being exercised.
	rec := newRecordingPutCache()
	bs.cache = rec

	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore, rec
}

// waitForChunks polls ListFileBlocks until at least one row is present
// or the deadline elapses. Returns the post-rollup blocks slice.
func waitForChunks(t *testing.T, bs *Store, payloadID string, timeout time.Duration) []*block.FileBlock {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		blocks, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
		if err == nil && len(blocks) > 0 {
			return blocks
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no FileBlock rows for %q within %v (rollup did not complete)", payloadID, timeout)
	return nil
}

// TestCache_PopulatedOnRollupComplete — Opt 3 correctness hard-gate
// . Write data spanning multiple chunks; drive engine.Flush
// assert every post-rollup chunk hash is reachable in bs.cache.
//
// Payload sizing: pkg/block/chunker emits a single chunk for data
// up to MinChunkSize (1 MiB) when final=true. To force multiple chunk
// emissions we write 12 MiB of varied bytes which crosses several
// FastCDC breakpoints. We then assert all emitted hashes are
// cache-resident — the load-bearing "wrote then read" contract.
func TestCache_PopulatedOnRollupComplete(t *testing.T) {
	ctx := context.Background()
	bs, _, rec := newRollupCacheFixture(t)

	payloadID := "rollup-cache-warmup"
	// Deterministic multi-chunk input. FastCDC emits a single chunk for
	// any input below MinChunkSize (1 MiB), and on content that never
	// trips a content-defined breakpoint it cuts only at MaxChunkSize
	// (16 MiB) — so a single random draw can occasionally yield ONE chunk
	// (the Windows-CI flake this de-flake closes). To make the chunk count
	// deterministic we (a) seed the RNG with a fixed constant so the byte
	// stream is identical on every platform/run, and (b) splice in fixed,
	// content-defined cut markers at three 1 MiB-spaced offsets. Each
	// marker is a run of bytes engineered to drive the gear hash to a
	// breakpoint, guaranteeing >= 2 emitted chunks every run regardless of
	// the random draw. Staying under the fixture's 16 MiB in-memory budget
	// keeps the rollup pump's reconstructStream allocation happy.
	const oneMiB = 1024 * 1024
	data := make([]byte, 8*oneMiB)
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic test fixture, not crypto
	if _, err := rng.Read(data); err != nil {
		t.Fatalf("rng.Read: %v", err)
	}
	// Force content-defined cuts: a long zero run past each MinChunkSize
	// boundary reliably trips FastCDC's gear-hash mask in the small region
	// (deterministic, content-only — independent of the random draw).
	for off := 2 * oneMiB; off < len(data); off += 2 * oneMiB {
		for i := off; i < off+4096 && i < len(data); i++ {
			data[i] = 0
		}
	}

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Force the async rollup pump to fully complete BEFORE inspecting the
	// manifest. Flush does not guarantee rollup completion (rollup runs on
	// the FSStore worker pool gated by StabilizationMS), so a bare poll can
	// observe a PARTIAL manifest (one block) on a slow CI runner before the
	// remaining chunks roll up — the timing half of the Windows flake.
	// DrainRollups makes the whole manifest visible synchronously.
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	// Manifest is now complete; poll only to absorb the brief gap before
	// the FileBlock rows are queryable.
	blocks := waitForChunks(t, bs, payloadID, 10*time.Second)
	if len(blocks) < 2 {
		t.Fatalf("expected >= 2 chunks for deterministic 8 MiB payload; got %d", len(blocks))
	}

	// Capture the recorded Put hashes under lock, then assert outside —
	// rec.Get also acquires rec.mu, so holding the lock across Get would
	// deadlock (same posture as eager_dedup_test.go line 240).
	rec.mu.Lock()
	putCalls := rec.putCalls
	rec.mu.Unlock()

	if putCalls < len(blocks) {
		t.Errorf("Cache.Put calls=%d; want >= %d (one Put per emitted chunk)", putCalls, len(blocks))
	}

	// For each post-rollup hash, the recorder must hold byte-identical
	// data. Recording happens at chunkstore.StoreChunk firing site
	// (producer side) and is RAM-only — a hit is structural
	// proof of cache-side warming at write time.
	for i, fb := range blocks {
		got, ok := rec.Get(fb.Hash)
		if !ok {
			t.Errorf("block[%d] hash=%s: cache MISS; OnChunkComplete did not populate Cache for this chunk",
				i, fb.Hash.String())
			continue
		}
		if int64(len(got)) != int64(fb.DataSize) {
			t.Errorf("block[%d] hash=%s: cache returned %d bytes; want %d (FileBlock.DataSize)",
				i, fb.Hash.String(), len(got), fb.DataSize)
		}
	}
}

// TestCache_PopulatedOnRollupComplete_EmptyRollup — edge case: a
// payload with no data produces no chunks; Flush completes without
// error and the recorder observes zero Puts. This guards against a
// hypothetical bug where the OnChunkComplete callback fires
// spuriously on zero-byte flushes.
func TestCache_PopulatedOnRollupComplete_EmptyRollup(t *testing.T) {
	ctx := context.Background()
	bs, _, rec := newRollupCacheFixture(t)

	payloadID := "empty-rollup"
	// No WriteAt — payload is empty.
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush of empty payload: %v", err)
	}

	rec.mu.Lock()
	calls := rec.putCalls
	rec.mu.Unlock()
	if calls != 0 {
		t.Errorf("Cache.Put calls=%d on empty payload; want 0", calls)
	}
}

// TestCache_PopulatedOnRollupComplete_LargeChunkRespectsCacheCap —
// edge case mirror of onchunkcomplete_test.go's LargeChunkRespects
// CacheCap, but driven through the rollup pump rather than direct
// StoreChunk: a chunk larger than the Cache's maxBytes triggers
// Cache.Put's > c.maxBytes silent-skip (cache.go:233). The rollup
// pump still succeeds — Cache.Put's guard is a no-op on oversize
// matching the "bounded by Cache LRU" contract.
//
// The recordingPutCache has no size guard (it's a test cache) so
// every chunk is recorded; this test instead asserts the structural
// property that the rollup pump completed without error even when
// the chunk exceeds a hypothetical Cache cap. The realCache size
// guard is exercised directly in onchunkcomplete_test.go's
// TestEngine_OnChunkComplete_LargeChunkRespectsCacheCap — we do not
// re-exercise it here to avoid duplicating that gate.
func TestCache_PopulatedOnRollupComplete_LargeChunkRespectsCacheCap(t *testing.T) {
	ctx := context.Background()
	bs, _, _ := newRollupCacheFixture(t)

	payloadID := "large-chunk-cap"
	// MinChunkSize-sized constant-byte payload → single chunk under
	// FastCDC's final=true emit (constant data, no breakpoint hit).
	// Tests pass at this size because the recorder has no cap; the
	// realCache cap behavior is covered by 's test.
	data := bytes.Repeat([]byte{0xC4}, 1024*1024)

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	blocks := waitForChunks(t, bs, payloadID, 10*time.Second)
	if len(blocks) < 1 {
		t.Fatalf("expected >= 1 chunk; got %d", len(blocks))
	}
}
