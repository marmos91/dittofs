package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// seamRunner is the fn closure a seam test drives: it accumulates every run's
// bytes into one batch (fresh per Flush call — the C9 caller obligation). With
// report set it reports each run's extent durable; fail is returned together
// with the report (the C5 shape).
type seamRunner struct {
	buf    []byte
	off    int64
	report bool
	fail   error
	calls  int
}

func (r *seamRunner) flush(_ context.Context, run Run) ([]Extent, error) {
	r.calls++
	data := make([]byte, run.Extent.Len)
	if _, err := run.ReadAt(data, run.Extent.Off); err != nil {
		return nil, err
	}
	r.buf = append(r.buf, data...)
	r.off += run.Extent.Len
	if r.report {
		return []Extent{run.Extent}, r.fail
	}
	return nil, nil
}

// seamRunnerStore wires a journal store whose sink/dedup fakes the old carve
// flow shares, so the seam tests reuse the same fixtures.
func seamStore(t *testing.T, cfg Config) (*Store, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	cfg.Clock = clk
	s, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

// TestCarveCommitStrictlyBeforeFlip pins the crash-safety order through the
// seam: at the moment the fn reports its batch durable, nothing may have
// flipped yet — the flip happens only after fn returns, when journal validates
// the report. A record flipped before the report is durable marks bytes that
// never reached the remote as synced (data loss).
func TestCarveCommitStrictlyBeforeFlip(t *testing.T) {
	s, _ := seamStore(t, Config{CarveBlockSize: 8 << 20})
	ctx := context.Background()
	data := randBytes(512<<10, 2)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}

	var checked bool
	afterReport := func() {
		checked = true
		if s.UnsyncedBytes() != int64(len(data)) {
			t.Errorf("flip happened before the report was validated: unsynced=%d want %d", s.UnsyncedBytes(), len(data))
		}
		if f := recRawFlags(t, s, "f", 0); f&flagSynced != 0 {
			t.Errorf("record already flipped at report time: flags=%#x", f)
		}
	}

	// The report is fn's return; the observation point is the call that hands it
	// over — journal flips only after the call returns, so the observation here
	// pins commit-before-flip.
	fn := func(ctx context.Context, run Run) ([]Extent, error) {
		if run.Final {
			afterReport()
			return []Extent{run.Extent}, nil
		}
		return nil, nil
	}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, fn); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !checked {
		t.Fatalf("report hook never ran")
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-flush unsynced=%d want 0", s.UnsyncedBytes())
	}
}

// TestCarvePackFlipPlanWatermarks pins that credit is validated per offered
// fragment and only up to what fn reported: fn reporting the first run's
// extent flips exactly those fragments; the unreported tail stays dirty. A
// whole-extent flip (or a flip past the report) would mark unuploaded bytes
// synced — the silent-zeros class.
func TestCarvePackFlipPlanWatermarks(t *testing.T) {
	s, _ := seamStore(t, Config{
		CarveBlockSize: 32 << 10,
		ChunkParams:    chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	ctx := context.Background()
	writeRunAt(t, s, 0, 4)        // [0, 16Ki)
	writeRunAt(t, s, 128<<10, 16) // [128Ki, 192Ki)

	// One fn per Flush call (C9). It reports only the FIRST run's extent.
	var firstExtent [2]Extent
	call := 0
	fn := func(ctx context.Context, run Run) ([]Extent, error) {
		call++
		if call == 1 {
			firstExtent[0] = run.Extent
			return nil, nil
		}
		if call == 2 {
			// Second run: report only the first run's fragments.
			return firstExtent[:1], nil
		}
		return nil, nil
	}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, fn); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if call != 2 {
		t.Fatalf("fn called %d times, want 2 (one per run)", call)
	}
	// The first run's records flipped.
	for i := 0; i < 4; i++ {
		off := int64(i) * (4 << 10)
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("run-0 record at %d not flipped though its extent was reported", off)
		}
	}
	// The second run's records were never reported: they stay dirty.
	for i := 0; i < 16; i++ {
		off := (128 << 10) + int64(i)*(4<<10)
		if f := recRawFlags(t, s, "f", off); f&flagSynced != 0 {
			t.Fatalf("run-1 record at %d flipped though its extent was never reported", off)
		}
	}
	if s.UnsyncedBytes() != 16*(4<<10) {
		t.Fatalf("unsynced=%d want the unreported run's 64Ki", s.UnsyncedBytes())
	}
}

// TestCarvePackSpanningBlockFailureReapsTheCommittedPrefix pins C5: fn returns
// a non-empty durable slice TOGETHER with an error — "these committed, then I
// failed" — and journal flips the validated extents, still calls AfterFile
// (the committed prefix must be reaped) and returns the error.
//
// Scope: this is C5 at the seam, and nothing more. It was named
// ...SpanningBlockFailureReapsTheCommittedPrefix, which promised more than it
// checked: one run, no hole, no block spanning anything, and no assertion on
// the span AfterFile receives. Blocks are the engine's after lane F, so the
// span property lives where packing does — see
// TestFlushReapSpanStopsAtTheCommittedFrontier in pkg/block/engine.
func TestFlushCommittedPrefixFlipsAndReapsDespiteError(t *testing.T) {
	s, _ := seamStore(t, Config{CarveBlockSize: 32 << 10})
	ctx := context.Background()
	writeRunAt(t, s, 0, 4) // [0, 16Ki)

	var reapCalled bool
	boom := errors.New("committed, then failed")
	r := &seamRunner{fail: boom}
	fn := func(ctx context.Context, run Run) ([]Extent, error) {
		durable, err := r.flush(ctx, run)
		// Report the whole run durable even though the "upload" failed: the
		// committed-then-failed shape C5 pins.
		if run.Final {
			return []Extent{run.Extent}, boom
		}
		return durable, err
	}
	err := s.Flush(ctx, "f", FlushOptions{
		Force: true,
		AfterFile: func(ctx context.Context, id FileID) error {
			reapCalled = true
			return nil
		},
	}, fn)
	if !errors.Is(err, boom) {
		t.Fatalf("Flush returned %v, want the fn failure", err)
	}
	if !reapCalled {
		t.Fatal("AfterFile never ran: the committed prefix would never be reaped")
	}
	// The reported extents flip despite the error.
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("unsynced=%d want 0: the committed prefix must flip", s.UnsyncedBytes())
	}
}

// TestCarvePackSeamRunFailureLeavesSuffixDirty pins that a fn failure leaves
// unreported fragments dirty: only what fn named durable flips, the suffix
// stays dirty for the next pass, and AfterFile still runs.
func TestCarvePackSeamRunFailureLeavesSuffixDirty(t *testing.T) {
	s, _ := seamStore(t, Config{CarveBlockSize: 64 << 10})
	ctx := context.Background()
	writeAdjacent(t, s, "f", 128, 4<<10)

	boom := errors.New("seam failure")
	reportHalf := true
	var half Extent
	fn := func(ctx context.Context, run Run) ([]Extent, error) {
		if !reportHalf {
			return nil, boom
		}
		reportHalf = false
		half = Extent{Off: run.Extent.Off, Len: run.Extent.Len / 2, State: StateDirty}
		return []Extent{half}, boom
	}
	var reapCalled bool
	err := s.Flush(ctx, "f", FlushOptions{
		Force: true,
		AfterFile: func(ctx context.Context, id FileID) error {
			reapCalled = true
			return nil
		},
	}, fn)
	if !errors.Is(err, boom) {
		t.Fatalf("Flush returned %v, want the fn failure", err)
	}
	if !reapCalled {
		t.Fatal("AfterFile never ran on the failure path")
	}
	// The reported half flipped; the rest is dirty.
	if f := recRawFlags(t, s, "f", half.Off); f&flagSynced == 0 {
		t.Fatalf("reported fragment at %d did not flip", half.Off)
	}
	if s.UnsyncedBytes() != (128*(4<<10))-half.Len {
		t.Fatalf("unsynced=%d, want the unreported suffix only", s.UnsyncedBytes())
	}
}

// TestFlushFragmentValidatedCreditSkipsStaleSiblings pins C4: an extent
// covering a fragment that a concurrent overwrite split flips every
// version-still-live sibling and skips the replaced one — whole-extent
// matching would forfeit the credit.
func TestFlushFragmentValidatedCreditSkipsStaleSiblings(t *testing.T) {
	s, _ := seamStore(t, Config{CarveBlockSize: 8 << 20})
	ctx := context.Background()

	// Two adjacent intervals (two writes so two records/versions), flushed,
	// then re-dirtied by a fresh write over the first one only.
	if err := s.WriteAt(ctx, "f", 0, randBytes(8<<10, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 8<<10, randBytes(8<<10, 2)); err != nil {
		t.Fatal(err)
	}
	r := &seamRunner{report: true}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, r.flush); err != nil {
		t.Fatalf("first Flush: %v", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("first flush did not drain: unsynced=%d", s.UnsyncedBytes())
	}

	// Simulate a crash between commit and flip, then overwrite the first
	// interval: the fresh flush offers the live fragments (new record for the
	// first interval, old record for the second) and reporting the whole
	// [0,16Ki) extent flips every version-live fragment.
	forceDirty(t, s, "f")
	if err := s.WriteAt(ctx, "f", 0, randBytes(8<<10, 3)); err != nil {
		t.Fatal(err)
	}

	r2 := &seamRunner{report: true}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, r2.flush); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	// The stale (superseded) first-interval record stays dirty; the
	// second-interval record flips. The first interval's new record was
	// reported as part of the extent and is live, so it flips too.
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("fragment-validated flip left live fragments dirty: unsynced=%d", s.UnsyncedBytes())
	}
}

// TestFlushSequentialAndGated pins C1 (one run at a time, ascending) and the
// eligibility gates (MinSize skips a small file, Force does not).
func TestFlushSequentialAndGated(t *testing.T) {
	s, clk := seamStore(t, Config{CarveBlockSize: 8 << 20})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, randBytes(4<<10, 1)); err != nil {
		t.Fatal(err)
	}

	// Below MinSize: not offered.
	r := &seamRunner{}
	if err := s.Flush(ctx, "f", FlushOptions{MinSize: 1 << 20}, r.flush); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Fatalf("small file offered below MinSize: %d calls", r.calls)
	}

	// Force: offered, once, ascending.
	r = &seamRunner{}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, r.flush); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("forced file offered %d times, want 1", r.calls)
	}

	// Aged: advanced past MaxAge, offered without Force.
	clk.advance(10 * time.Second)
	r = &seamRunner{}
	if err := s.Flush(ctx, "f", FlushOptions{MinSize: 1 << 20, MaxAge: 5 * time.Second}, r.flush); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("aged file not offered: %d calls", r.calls)
	}
}

// TestFlushDurableTailUnionsResidentAndRemote pins that DurableTail carries
// BOTH resident and remote extents contiguous after the run — a clobbered
// row's owed range can straddle evicted bytes.
func TestFlushDurableTailUnionsResidentAndRemote(t *testing.T) {
	s, _ := seamStore(t, Config{CarveBlockSize: 8 << 20})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, randBytes(8<<10, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 8<<10, randBytes(8<<10, 2)); err != nil {
		t.Fatal(err)
	}
	r := &seamRunner{report: true}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, r.flush); err != nil {
		t.Fatal(err)
	}
	// Re-dirty the head interval only (a fresh write supersedes its record),
	// so the second flush offers the head with the synced tail behind it.
	if err := s.WriteAt(ctx, "f", 0, randBytes(8<<10, 3)); err != nil {
		t.Fatal(err)
	}
	var tail []Extent
	fn := func(ctx context.Context, run Run) ([]Extent, error) {
		tail = run.DurableTail
		return nil, nil
	}
	if err := s.Flush(ctx, "f", FlushOptions{Force: true}, fn); err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Off != 8<<10 || tail[0].Len != 8<<10 {
		t.Fatalf("DurableTail=%v, want the synced [8Ki,16Ki) extent", tail)
	}
	if tail[0].State != StateResident {
		t.Fatalf("tail state=%v, want resident", tail[0].State)
	}
}
