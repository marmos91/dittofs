package engine

import (
	"context"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// gatedSink wraps the production engineBlockSink and holds every CommitBlock at
// a per-block gate, so a test decides the order in which blocks become durable.
// The gate sits in front of the real sink rather than replacing it: once
// released, the block runs the real seal, frame, PutBlock and per-file locked
// commit, so what the ordering assertions run against is the production path.
//
// All three optional capabilities are forwarded. Journal discovers them by type
// assertion, so a wrapper that dropped them would leave the reap, the row-widen
// and the clobber guard silently unexercised and the test would be describing a
// carve that production never runs.
type gatedSink struct {
	real engineBlockSink

	// arrivals reports each block as it reaches the gate; committed reports each
	// one after the real sink has finished with it. Both are keyed by the
	// block's first file offset.
	arrivals  chan int64
	committed chan int64

	mu    sync.Mutex
	gates map[int64]chan struct{}
}

var (
	_ journal.BlockSink        = (*gatedSink)(nil)
	_ journal.SupersededReaper = (*gatedSink)(nil)
	_ journal.ManifestRowEnder = (*gatedSink)(nil)
	_ journal.ClobberGuard     = (*gatedSink)(nil)
)

func newGatedSink(real engineBlockSink) *gatedSink {
	return &gatedSink{
		real:      real,
		arrivals:  make(chan int64, 64),
		committed: make(chan int64, 64),
		gates:     map[int64]chan struct{}{},
	}
}

// gate returns the release channel for the block whose first chunk sits at off,
// creating it on first use — the arriving block and the releasing test race to
// name the same offset.
func (g *gatedSink) gate(off int64) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.gates[off]
	if !ok {
		ch = make(chan struct{})
		g.gates[off] = ch
	}
	return ch
}

// release lets the block whose first chunk sits at off proceed into the real sink.
func (g *gatedSink) release(off int64) { close(g.gate(off)) }

func (g *gatedSink) CommitBlock(ctx context.Context, chunks []journal.CarveChunk) error {
	if len(chunks) == 0 {
		return g.real.CommitBlock(ctx, chunks)
	}
	off := chunks[0].FileOffset
	ch := g.gate(off)
	g.arrivals <- off
	select {
	case <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := g.real.CommitBlock(ctx, chunks)
	g.committed <- off
	return err
}

func (g *gatedSink) ReapSupersededManifest(ctx context.Context, id journal.FileID, spans [][2]int64, newOffsets map[int64]struct{}) error {
	return g.real.ReapSupersededManifest(ctx, id, spans, newOffsets)
}

func (g *gatedSink) ManifestRowEndAfter(ctx context.Context, id journal.FileID, off int64) (int64, error) {
	return g.real.ManifestRowEndAfter(ctx, id, off)
}

func (g *gatedSink) PreserveClobberedRow(ctx context.Context, id journal.FileID, runStart, runEnd int64, owed [][2]int64) error {
	return g.real.PreserveClobberedRow(ctx, id, runStart, runEnd, owed)
}

// newChunkedCarveFixture is newCarveFixture with the chunker sized explicitly.
// Ordering tests need many blocks per file, and a block boundary is only taken
// once a whole chunk has crossed CarveBlockSize — so with the default chunk
// parameters a small file is one chunk, hence one block, and there is nothing
// to order. newCarveFixture does not expose ChunkParams, so the wiring is
// repeated here rather than widened for one caller.
func newChunkedCarveFixture(t *testing.T, rbs *remotememory.Store, carveBytes int64, params chunker.Params) *carveFixture {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(t.TempDir(), 0, ms, fs.FSStoreOptions{
		CarveBlockSize: carveBytes,
		ChunkParams:    params,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.ManualSync = true // explicit carve only; no background dispatcher racing assertions

	m := NewRemoteSync(local, rbs, ms, cfg)
	m.SetSyncedHashStore(ms)
	m.SetRemoteBlockStore(rbs)
	if !m.carveActive.Load() {
		t.Fatal("carve substrate should be active after wiring")
	}
	return &carveFixture{local: local, ms: ms, remote: rbs, syncer: m}
}

// TestCarveFlipsInWatermarkOrderThroughProductionSink is the engine-level port of
// journal's TestCarveFlipsInWatermarkOrder. Journal proves the ordered flip
// against its own in-package fake; this proves it against engineBlockSink with a
// real carveCommitLocks, a real metadata commit and a real remote PutBlock —
// the path a share actually runs, and the one the fake's nil commitLocks skips.
//
// The property: blocks commit concurrently and in any order, but a record flips
// synced only once every lower-offset block has committed. A later block that
// lands first must not credit durability the earliest block has not yet earned.
func TestCarveFlipsInWatermarkOrderThroughProductionSink(t *testing.T) {
	// A bounded context so a block stuck at a gate fails the test with a clear
	// message instead of hanging to the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const blockSize = 32 << 10
	const blocks = 4

	// Chunks well under the block size, so four blocks' worth of data really
	// packs into four blocks.
	f := newChunkedCarveFixture(t, remotememory.New(), blockSize,
		chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10})

	// Built from the syncer's own wired dependencies rather than from the
	// fixture's raw parts, so the sink under test is the one wireCarveTargets
	// would have installed.
	gated := newGatedSink(engineBlockSink{
		sealer:      f.syncer.chunkSealer,
		rbs:         f.remote,
		committer:   f.syncer.blockCommitter,
		commitLocks: &carveCommitLocks{},
	})
	f.local.SetCarveTargets(engineDeduper{synced: f.syncer.syncedHashStore}, gated)

	// Distinct random payloads: identical bytes would dedup into one block and
	// there would be nothing to order.
	for range blocks {
		payload := make([]byte, blockSize)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		f.storeChunk(t, ctx, payload)
	}
	full := f.local.UnsyncedBytes()
	if full == 0 {
		t.Fatal("no dirty bytes to carve")
	}

	// The chain head is the block carrying the file's lowest dirty offset.
	// packRuns walks runs in ascending order and the fixture writes from 0, so
	// that is the block whose first chunk sits at offset 0.
	const headOffset int64 = 0

	done := make(chan error, 1)
	go func() {
		_, err := f.local.Carve(ctx, journal.CarveOptions{FileID: carveFixturePayload, Force: true})
		done <- err
	}()

	// Release every block except the head, as it arrives. A background releaser
	// rather than a timed drain: a block that reaches the sink after a fixed
	// window would otherwise never be released and would stall the carve.
	var mu sync.Mutex
	var seen []int64
	go func() {
		for {
			select {
			case off := <-gated.arrivals:
				mu.Lock()
				seen = append(seen, off)
				mu.Unlock()
				if off != headOffset {
					gated.release(off)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	seenCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(seen)
	}

	// Wait for real concurrency: the head plus at least one later block.
	deadline := time.After(30 * time.Second)
	for seenCount() < 2 {
		select {
		case err := <-done:
			t.Fatalf("carve returned before 2 blocks were in flight (err=%v, seen=%d)", err, seenCount())
		case <-deadline:
			t.Fatalf("expected at least 2 blocks in flight, got %d", seenCount())
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Wait until every released block has actually come back out of the real
	// sink. Asserting after a sleep would let the test pass while the later
	// blocks were merely still uploading, which proves nothing about ordering.
	for range seenCount() - 1 {
		select {
		case <-gated.committed:
		case err := <-done:
			t.Fatalf("carve returned while the head block was still held (err=%v)", err)
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for the released blocks to commit")
		}
	}

	// Later blocks are durable and the head is not, so nothing may have flipped.
	// Blocks that arrive after this point are released by the goroutine above and
	// commit too, but none can flip while the head of the chain is still held.
	if u := f.local.UnsyncedBytes(); u != full {
		t.Fatalf("records flipped before the earliest block committed: unsynced=%d want %d", u, full)
	}

	// Releasing the head lets the whole chain flip in order.
	gated.release(headOffset)
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if u := f.local.UnsyncedBytes(); u != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", u)
	}
}
