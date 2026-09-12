package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
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
// The reap stops at the frontier those rows actually reach. The range the failed
// block held is still dirty, still covered by its stale rows, and re-carved by
// the next pass — reaping into it would delete that cover with no fresh tiling
// to replace it.
//
// The straddled subtest pins that a row reaching past the frontier does not
// suppress the reap: it still runs over exactly the committed prefix, and
// sparing that one row is the metadata reap's own job.
func TestCarvePackReapCarriesEveryCommittedRun(t *testing.T) {
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
		t.Fatalf("reaped spans=%d want %d: a committed run was left out of the reap", len(sink.reaps), runs)
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
// real key-value oracle without importing one. The delay is what matters: every
// lookup is serialised through the single packer goroutine, so a map-backed fake
// elides the very cost the benchmark exists to show.
//
// It spins rather than sleeping because the delay is tens of microseconds, well
// under the scheduler's sleep granularity, and the figure being measured is the
// packer's blocked time — sleep noise would land directly on it.
//
// ponytail: one flat delay, no distribution and no cache-hit skew. A real store
// is bimodal (page cache vs. disk); model that only if a pass ever needs to
// predict absolute latency rather than compare two packer designs. The
// against-real-Badger measurement belongs in DittoFS's own suite, not here —
// this package must not depend on a metadata store to benchmark itself.
type slowDeduper struct {
	delay time.Duration
}

// deduperLookupDelay approximates a warm point lookup in an embedded LSM store.
// A calibration knob, not a constant of nature: re-measure it against the real
// oracle on the hardware a run is quoting before reading absolute numbers off
// this benchmark.
const deduperLookupDelay = 50 * time.Microsecond

func (d slowDeduper) IsChunkDurable(_ context.Context, _ ChunkHash) (bool, error) {
	for start := time.Now(); time.Since(start) < d.delay; {
	}
	return false, nil
}

// BenchmarkCarveScatteredPass measures a full scattered carve pass against a
// latency-injecting dedup oracle, so the cost of serialising IsChunkDurable
// through one packer goroutine is visible rather than elided by a map-backed
// fake.
func BenchmarkCarveScatteredPass(b *testing.B) {
	b.ReportAllocs()
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
		s.SetCarveTargets(slowDeduper{delay: deduperLookupDelay}, sink)
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
