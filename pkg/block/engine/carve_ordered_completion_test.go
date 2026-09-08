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

	arrivals chan int64

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
	return &gatedSink{real: real, arrivals: make(chan int64, 64), gates: map[int64]chan struct{}{}}
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
	return g.real.CommitBlock(ctx, chunks)
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
	ctx := context.Background()
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

	done := make(chan error, 1)
	go func() {
		_, err := f.local.Carve(ctx, journal.CarveOptions{FileID: carveFixturePayload, Force: true})
		done <- err
	}()

	// Collect the blocks that reached the sink concurrently. Packing is
	// sequential, so submission is in ascending offset order; arrival here is not.
	var offs []int64
	deadline := time.After(5 * time.Second)
	for len(offs) < 2 {
		select {
		case off := <-gated.arrivals:
			offs = append(offs, off)
		case err := <-done:
			t.Fatalf("carve returned before 2 blocks were in flight (err=%v, got %d)", err, len(offs))
		case <-deadline:
			t.Fatalf("expected at least 2 blocks in flight, got %d", len(offs))
		}
	}
	// Take whatever else is already queued, so the release below covers every
	// block that is actually waiting.
drain:
	for {
		select {
		case off := <-gated.arrivals:
			offs = append(offs, off)
		case <-time.After(300 * time.Millisecond):
			break drain
		}
	}

	first := offs[0]
	for _, o := range offs {
		if o < first {
			first = o
		}
	}

	// Release every block except the lowest-offset one. They commit for real;
	// the ordered flip gate must hold all their flips behind the one still held.
	for _, o := range offs {
		if o != first {
			gated.release(o)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if u := f.local.UnsyncedBytes(); u != full {
		t.Fatalf("records flipped before the earliest block committed: unsynced=%d want %d", u, full)
	}

	// Releasing the earliest block lets the whole chain flip in order.
	gated.release(first)
	if err := <-done; err != nil {
		t.Fatalf("carve: %v", err)
	}
	if u := f.local.UnsyncedBytes(); u != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", u)
	}
}
