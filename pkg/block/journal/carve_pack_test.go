package journal

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	badgerstore "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestCarvePackReachesBlockSizeOnScatteredRuns pins the defect this plan fixes:
// a scattered dirty set must coalesce into one remote block, not one per run.
// Each run is far below CarveBlockSize, so a packer that flushes at the end of
// every run emits `runs` blocks; one that flushes only at CarveBlockSize emits 1.
func TestCarvePackReachesBlockSizeOnScatteredRuns(t *testing.T) {
	const (
		runs    = 300
		runSize = 4 << 10
		gap     = 64 << 10 // a hole between runs keeps them separate
	)
	s, _, _, _ := carveStore(t, Config{
		CarveBlockSize:         4 << 20,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
	ctx := context.Background()

	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BytesCarved != int64(runs*runSize) {
		t.Fatalf("BytesCarved=%d want %d", res.BytesCarved, runs*runSize)
	}
	// 300 x 4 KiB = 1.2 MiB, comfortably inside one 4 MiB block.
	if res.BlocksWritten != 1 {
		t.Fatalf("BlocksWritten=%d want 1: blocks did not span runs", res.BlocksWritten)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
}

// TestCarvePackSpansRunsFlipsEveryContributingRun pins that when one block covers
// many runs, every record in every contributing run has its durable synced bit
// set — not merely that the in-memory unsynced counter reached zero. The on-disk
// flag is what recovery reads, so it is the only assertion that rules out the
// silent-zeros class.
func TestCarvePackSpansRunsFlipsEveryContributingRun(t *testing.T) {
	const (
		runs    = 64
		runSize = 4 << 10
		gap     = 32 << 10
	)
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         4 << 20,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
	ctx := context.Background()

	want := map[int64][]byte{}
	for i := 0; i < runs; i++ {
		off := int64(i) * gap
		b := randBytes(runSize, int64(i))
		if err := s.WriteAt(ctx, "f", off, b); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
		want[off] = b
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	for off, b := range want {
		if got := sink.chunkAt(off); got == nil {
			t.Fatalf("no committed chunk at %d", off)
		} else if string(got) != string(b) {
			t.Fatalf("committed bytes at %d differ", off)
		}
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("record at %d not flipped synced on disk: flags=%#x", off, f)
		}
	}
}

// writeRunAt lays down a run of n adjacent 4 KiB writes starting at off. A run
// needs many intervals because flipUpTo advances at interval granularity: one
// large write would be a single interval that no mid-run watermark can flip.
func writeRunAt(t *testing.T, s *Store, off int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.WriteAt(context.Background(), "f", off+int64(i)*(4<<10), randBytes(4<<10, off+int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", off+int64(i)*(4<<10), err)
		}
	}
}

// TestCarvePackFlipPlanWatermarks pins the per-run watermark derivation through
// the real carve path: a block that covers all of run 0 and only a prefix of
// run 1 must flip run 0 to its own end and run 1 only as far as it actually
// packed. The block after it fails, so nothing else can flip and the two
// watermarks are readable straight off the on-disk synced bits.
//
// This is what fails if commitAndFlip stops distinguishing plan.last: flipping
// every run to its own end would mark run 1's uncommitted tail synced, which is
// the silent-zeros class — a record recovery replays as durable whose bytes
// never reached the remote.
func TestCarvePackFlipPlanWatermarks(t *testing.T) {
	const (
		run0Recs = 4         // [0, 16Ki)
		run1Off  = 128 << 10 // a hole keeps it a separate run
		run1Recs = 16        // [128Ki, 192Ki)
		run1End  = run1Off + run1Recs*(4<<10)
	)
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         32 << 10,
		CarveUploadConcurrency: 1,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	writeRunAt(t, s, 0, run0Recs)
	writeRunAt(t, s, run1Off, run1Recs)

	// Let the first block commit and fail every one after it, so the first block
	// is the only one that ever flips.
	sink.okCommits = 1
	sink.failErr = errors.New("second block fails")

	if _, err := s.Carve(context.Background(), CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve: want the seeded failure to surface, got nil")
	}

	// Run 0 is not the plan's last run, so it flips to its own end: every record.
	for i := 0; i < run0Recs; i++ {
		off := int64(i) * (4 << 10)
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("run 0 record at %d not flipped: the block did not cover the run to its end (flags=%#x)", off, f)
		}
	}
	// The block reached into run 1, so its head flipped. Without this the test
	// would pass on a packer that still cut blocks at run boundaries.
	if f := recRawFlags(t, s, "f", run1Off); f&flagSynced == 0 {
		t.Fatalf("run 1 head at %d not flipped: the block never spanned the run boundary (flags=%#x)", run1Off, f)
	}
	// Run 1 is the plan's last run, so it flips only to lastOff — its tail, which
	// only the failed block carried, must still be dirty.
	if f := recRawFlags(t, s, "f", run1End-(4<<10)); f&flagSynced != 0 {
		t.Fatalf("run 1 tail flipped synced though its block never committed: flags=%#x", f)
	}
	if s.UnsyncedBytes() == 0 {
		t.Fatal("post-carve unsynced=0: run 1's uncommitted tail must stay dirty")
	}
}

// TestCarvePackSpanningBlockFailureLeavesEarlierRunUnreaped pins the failure
// shape that only exists now that blocks span runs: a block carrying the tail of
// run 0 and the head of run 1 fails, so run 0 never completes and must NOT be
// reaped — even though an earlier block committed part of it. Reaping it would
// delete the rows its committed prefix superseded while the run's own fresh
// tiling is still missing the range the failed block held.
func TestCarvePackSpanningBlockFailureLeavesEarlierRunUnreaped(t *testing.T) {
	const (
		run0Recs = 12        // [0, 48Ki): more than one 32 KiB block
		run1Off  = 128 << 10 // a hole keeps it a separate run
		run1Recs = 4         // [128Ki, 144Ki)
		gap      = run1Off
	)
	s, dd, base, _ := carveStore(t, Config{
		CarveBlockSize:         32 << 10,
		CarveUploadConcurrency: 1,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	sink := &extendingSink{fakeSink: base}
	s.SetCarveTargets(dd, sink)

	var spanned bool
	base.onCommit = func(chunks []CarveChunk) {
		for _, c := range chunks {
			if c.FileOffset/gap != chunks[0].FileOffset/gap {
				spanned = true
			}
		}
	}
	writeRunAt(t, s, 0, run0Recs)
	writeRunAt(t, s, run1Off, run1Recs)

	// The first block (run 0's prefix) commits; the one that spans the boundary
	// fails.
	base.okCommits = 1
	base.failErr = errors.New("spanning block fails")

	if _, err := s.Carve(context.Background(), CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve: want the seeded failure to surface, got nil")
	}
	if !spanned {
		t.Fatal("no block carried chunks from both runs: the geometry does not build the shape under test")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reaps) != 0 {
		t.Fatalf("reaps=%v, want none: neither run completed, so neither may be reaped", sink.reaps)
	}
}

// TestCarvePackReapFailureDoesNotSuppressLaterRuns pins that one run's reap
// failing still leaves every later complete run reaped. Those runs have already
// flipped synced, so no later pass revisits them: a reap skipped here never
// happens at all. The first error still surfaces.
func TestCarvePackReapFailureDoesNotSuppressLaterRuns(t *testing.T) {
	const (
		runSize = 4 << 10
		gap     = 64 << 10
		runs    = 3
	)
	s, dd, base, _ := carveStore(t, Config{CarveBlockSize: 4 << 20, CarveUploadConcurrency: 4})
	boom := errors.New("reap failed")
	sink := &extendingSink{fakeSink: base, failFirstReap: boom}
	s.SetCarveTargets(dd, sink)

	ctx := context.Background()
	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); !errors.Is(err, boom) {
		t.Fatalf("Carve returned %v, want the reap failure", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0: every run committed", s.UnsyncedBytes())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reaps) != runs {
		t.Fatalf("reaps=%d want %d: a failed reap suppressed the runs after it", len(sink.reaps), runs)
	}
}

// TestCarvePackSeamRunFailureLeavesSuffixDirty pins the abort semantics at a
// block seam: when a run is split across blocks k and k+1 and k+1's commit
// fails, the run's prefix is durable and flipped while its suffix stays dirty
// for the next pass. This is the same mid-run semantics carve has always had;
// spanning runs must not widen it into a half-flipped run reported as complete.
//
// The run is laid down as many adjacent writes rather than one large one: a
// record is the granularity flipUpTo advances at, so a single 512 KiB write
// would be one interval that no mid-run watermark can flip.
func TestCarvePackSeamRunFailureLeavesSuffixDirty(t *testing.T) {
	const runSize = 512 << 10
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         64 << 10,
		CarveUploadConcurrency: 1,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	writeAdjacent(t, s, "f", 128, 4<<10)
	ctx := context.Background()
	// Let the first block commit, then fail every one after it. fakeSink already
	// has both hooks — do not add a new failure field.
	sink.okCommits = 1
	sink.failErr = errors.New("seam commit failed")

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve: want the seam failure to surface, got nil")
	}
	if s.UnsyncedBytes() == 0 {
		t.Fatal("post-carve unsynced=0: the failed suffix must stay dirty")
	}
	if s.UnsyncedBytes() == int64(runSize) {
		t.Fatal("post-carve unsynced=runSize: the committed prefix must have flipped")
	}
}

// badgerDeduper answers IsChunkDurable from a real BadgerMetadataStore's
// IsSynced, the same lookup production wiring uses (pkg/block/engine's
// engineDeduper). Unlike fakeDeduper's mutex-guarded map, each call opens an
// actual Badger read transaction — the per-call cost BenchmarkCarveScatteredPass
// exists to measure. Never marked synced by this benchmark, so every lookup is
// a miss, which matches carve encountering novel scattered writes.
type badgerDeduper struct {
	store *badgerstore.BadgerMetadataStore
}

func (d badgerDeduper) IsChunkDurable(ctx context.Context, h ChunkHash) (bool, error) {
	return d.store.IsSynced(ctx, block.ContentHash(h))
}

// BenchmarkCarveScatteredPass measures a full scattered carve pass against a
// real dedup oracle, so the cost of serialising IsChunkDurable through one
// packer goroutine is visible rather than elided by a map-backed fake.
func BenchmarkCarveScatteredPass(b *testing.B) {
	const (
		runs    = 5000
		runSize = 4 << 10
		gap     = 16 << 10
	)
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s, _, sink, _ := carveStore(b, Config{
			CarveBlockSize:         4 << 20,
			CarveUploadConcurrency: 8,
			ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
		})
		md, err := badgerstore.NewBadgerMetadataStoreWithDefaults(ctx, b.TempDir())
		if err != nil {
			b.Fatalf("open badger metadata store: %v", err)
		}
		b.Cleanup(func() { _ = md.Close() })
		s.SetCarveTargets(badgerDeduper{md}, sink)
		for r := 0; r < runs; r++ {
			if err := s.WriteAt(ctx, "f", int64(r)*gap, randBytes(runSize, int64(r))); err != nil {
				b.Fatalf("WriteAt %d: %v", r, err)
			}
		}
		b.StartTimer()
		if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
			b.Fatalf("Carve: %v", err)
		}
	}
}
