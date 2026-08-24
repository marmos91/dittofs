package journal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// TestCarveScatteredRunsConvergeConcurrently pins that a scattered dirty set still
// drains to zero when its runs execute concurrently: every record flips synced, the
// carved bytes match, and -race sees no shared state across runs.
func TestCarveScatteredRunsConvergeConcurrently(t *testing.T) {
	const (
		runs      = 200
		runSize   = 4 << 10
		gap       = 8 << 10 // stride > runSize keeps every run non-contiguous
		totalSize = runs * runSize
	)
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 4 << 20, CarveUploadConcurrency: 8})
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
	if s.UnsyncedBytes() != int64(totalSize) {
		t.Fatalf("pre-carve unsynced=%d want %d", s.UnsyncedBytes(), totalSize)
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BytesCarved != int64(totalSize) {
		t.Fatalf("BytesCarved=%d want %d", res.BytesCarved, totalSize)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
	for off, b := range want {
		if got := sink.chunkAt(off); got == nil {
			t.Fatalf("no committed chunk at %d", off)
		} else if string(got) != string(b) {
			t.Fatalf("committed bytes at %d differ", off)
		}
	}
	for off := range want {
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("record at %d not flipped synced on disk: flags=%#x", off, f)
		}
	}
}

// runsSharingARecord reports whether the file's dirty snapshot splits into two
// or more runs that hold fragments of one physical record — the shape whose
// safety rests on flipUpTo doing mark-check-flip under the shard lock.
func runsSharingARecord(s *Store, id FileID) bool {
	type recKey struct {
		seg uint64
		off int64
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	var snap []interval
	if fi != nil {
		for _, iv := range fi.ivs {
			if !iv.synced && !iv.cold {
				snap = append(snap, iv)
			}
		}
	}
	sh.mu.Unlock()
	owner := map[recKey]int{}
	for ri, run := range splitRuns(snap) {
		for _, iv := range run {
			k := recKey{iv.loc.SegmentID, iv.recOff}
			if prev, ok := owner[k]; ok && prev != ri {
				return true
			}
			owner[k] = ri
		}
	}
	return false
}

// TestCarveSharedRecordAcrossRuns builds the one shape where concurrent runs are
// not trivially independent: two runs holding fragments of a single physical
// record, separated by a synced gap. It pins that such a file still drains —
// both runs carve their fragment, the file ends with nothing unsynced, and each
// run commits exactly its own bytes — with the shard-lock-held mark-check-flip
// in flipUpTo racing between the two runs under -race.
//
// The shape is built by splitting a record with a small overwrite, carving
// everything, then clipping each surviving fragment with a later write: the
// clipped fragments go dirty again while the small record between them stays
// synced, and that synced gap is what splits the dirty set into two runs.
//
// Note what this cannot check: a record shared across two runs was necessarily
// split by a write that had already been carved, so its on-disk synced bit is
// already set before this carve begins and flipRecordSynced only ORs the bit in.
// The flag assertions below are therefore live only at offsets 0 and rightCut,
// whose records are new; UnsyncedBytes reaching zero is what actually pins the
// in-memory flip for the shared record.
func TestCarveSharedRecordAcrossRuns(t *testing.T) {
	const (
		size     = 96 << 10
		midOff   = 40 << 10
		midEnd   = 48 << 10
		leftCut  = 32 << 10
		rightCut = 56 << 10
	)
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 4 << 20, CarveUploadConcurrency: 4})
	ctx := context.Background()

	base := randBytes(size, 1)
	if err := s.WriteAt(ctx, "f", 0, base); err != nil {
		t.Fatalf("base write: %v", err)
	}
	mid := randBytes(midEnd-midOff, 2)
	if err := s.WriteAt(ctx, "f", midOff, mid); err != nil {
		t.Fatalf("mid write: %v", err)
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("seed carve: %v", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("seed carve left %d unsynced", s.UnsyncedBytes())
	}

	// Clip both surviving fragments of the base record; each write re-dirties the
	// fragment it splits, while the record covering [midOff,midEnd) stays synced.
	left := randBytes(leftCut, 3)
	if err := s.WriteAt(ctx, "f", 0, left); err != nil {
		t.Fatalf("left write: %v", err)
	}
	right := randBytes(size-rightCut, 4)
	if err := s.WriteAt(ctx, "f", rightCut, right); err != nil {
		t.Fatalf("right write: %v", err)
	}

	if !runsSharingARecord(s, "f") {
		t.Fatalf("premise not constructed: no record has fragments in two runs")
	}
	if got, want := s.UnsyncedBytes(), int64(midOff+(size-midEnd)); got != want {
		t.Fatalf("unsynced=%d want %d", got, want)
	}

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}

	wantLeft := append(append([]byte{}, left...), base[leftCut:midOff]...)
	wantRight := append(append([]byte{}, base[midEnd:rightCut]...), right...)
	if got := sink.chunkAt(0); string(got) != string(wantLeft) {
		t.Fatalf("left run committed %d bytes, want %d", len(got), len(wantLeft))
	}
	if got := sink.chunkAt(midEnd); string(got) != string(wantRight) {
		t.Fatalf("right run committed %d bytes, want %d", len(got), len(wantRight))
	}
	for _, off := range []int64{0, leftCut, midOff, midEnd, rightCut} {
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("record at %d not flipped synced on disk: flags=%#x", off, f)
		}
	}
}

// extendingSink is a fakeSink that also answers the manifest-row lookup and
// records every run-end reap range. The lookup for gateOff returns a row end
// that reaches past the next run's start, exercising the refusal that keeps a
// run's extension from reaching into a range another run owns.
type extendingSink struct {
	*fakeSink
	mu      sync.Mutex
	reaps   [][2]int64
	gateOff int64
	rowEnd  int64
	gated   int
}

func (e *extendingSink) ManifestRowEndAfter(_ context.Context, _ FileID, off int64) (int64, error) {
	if off != e.gateOff {
		return 0, nil
	}
	e.mu.Lock()
	e.gated++
	e.mu.Unlock()
	return e.rowEnd, nil
}

func (e *extendingSink) ReapSupersededManifest(_ context.Context, _ FileID, runStart, runEnd int64, _ map[int64]struct{}) error {
	e.mu.Lock()
	e.reaps = append(e.reaps, [2]int64{runStart, runEnd})
	e.mu.Unlock()
	return nil
}

// TestCarveRunDoesNotExtendPastNextRun pins the end-to-end property: no run's
// reap range ever reaches into a range another run owns. The run-end reap
// deletes every manifest row starting inside its range that it did not itself
// write, so a reap range crossing into the next run would drop rows that run
// still owns, leaving an uncovered range that cold-reads as zeros with no error
// anywhere.
//
// It does not, on its own, pin the extension refusal in extendRunToRowEnd —
// warmTail's own all-or-nothing bail on a non-warm interval produces the same
// reap ranges whether or not that refusal exists, since nothing flips a record
// synced while extents are being resolved. TestWarmTailStopsAtNonWarmInterval
// pins that warmTail invariant, which is what makes the refusal redundant
// today; the refusal itself is deliberately left unpinned by any test, since
// it is unreachable under the current barrier and only matters again if a
// future change reintroduces a flip during extent resolution.
func TestCarveRunDoesNotExtendPastNextRun(t *testing.T) {
	const rec = 8 << 10
	s, dd, base, _ := carveStore(t, Config{CarveBlockSize: 4 << 20, CarveUploadConcurrency: 4})
	sink := &extendingSink{
		fakeSink: base,
		gateOff:  rec,     // the first run ends here
		rowEnd:   4 * rec, // a row reaching past the second run
	}
	s.SetCarveTargets(dd, sink)
	ctx := context.Background()

	// Six records, each written separately so a later full-width overwrite
	// replaces one outright instead of splitting a larger record (which would
	// re-dirty its neighbours and merge everything into one run).
	for i := 0; i < 6; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*rec, randBytes(rec, int64(i))); err != nil {
			t.Fatalf("seed write %d: %v", i, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("seed carve: %v", err)
	}

	// Re-dirty records 0 and 2, leaving record 1 warm between the two runs.
	dirty := []int64{0, 2 * rec}
	for _, off := range dirty {
		if err := s.WriteAt(ctx, "f", off, randBytes(rec, off+99)); err != nil {
			t.Fatalf("dirty write %d: %v", off, err)
		}
	}

	sink.mu.Lock()
	sink.reaps = nil
	sink.mu.Unlock()

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}

	sink.mu.Lock()
	reaps := append([][2]int64{}, sink.reaps...)
	gated := sink.gated
	sink.mu.Unlock()
	// extendRunToRowEnd short-circuits on warmAt before the sink is consulted, and
	// the gate is keyed on an exact offset, so drift in either could leave the
	// refusal branch unreached while every assertion below still passes.
	if gated == 0 {
		t.Fatalf("the gated lookup was never reached, so the refusal branch was never exercised and this test proves nothing")
	}
	if len(reaps) != len(dirty) {
		t.Fatalf("got %d reap ranges, want %d: %v", len(reaps), len(dirty), reaps)
	}
	// No run's reap range may reach into a range another run is carving.
	for _, r := range reaps {
		for _, start := range dirty {
			if r[0] < start && start < r[1] {
				t.Fatalf("reap range [%d,%d) covers the run starting at %d", r[0], r[1], start)
			}
		}
	}
}

// TestCarveBlocksCommitConcurrently pins the block-level concurrency itself: a
// file's successive packed blocks commit at the same time, and the file-scoped
// semaphore caps how many overlap. Asserting the drained result alone does not
// cover this — a fully serial carve produces byte-identical output — so this is
// what fails if the commits are ever serialised again.
//
// The geometry gives the file many blocks (small chunks, small CarveBlockSize)
// spread over several dirty runs, so the packer crosses run boundaries mid-block
// while the window still bounds what is in flight.
//
// The commit hook blocks until more than window commits are in flight rather
// than sleeping a fixed time, so a carve that breaches the cap releases
// immediately and is caught; a correct one never breaches it and pays the
// timeout once, which the gaveUp flag keeps to a single wait for the pass. That
// wait is also what holds the in-flight set open long enough to observe the
// overlap.
func TestCarveBlocksCommitConcurrently(t *testing.T) {
	const (
		runs    = 8
		runSize = 32 << 10
		gap     = 64 << 10 // a hole between runs keeps them separate
		window  = 4
	)
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         8 << 10,
		CarveUploadConcurrency: window,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	ctx := context.Background()

	var (
		mu       sync.Mutex
		cur, max int
		commits  int
		gaveUp   bool
		exceeded = make(chan struct{})
		once     sync.Once
	)
	sink.onCommit = func(_ []CarveChunk) {
		mu.Lock()
		cur++
		commits++
		if cur > max {
			max = cur
		}
		if cur > window {
			once.Do(func() { close(exceeded) })
		}
		skip := gaveUp
		mu.Unlock()

		if !skip {
			select {
			case <-exceeded:
			case <-time.After(time.Second):
				// The cap held. Stop waiting so the rest of the pass does not pay
				// the timeout too.
				mu.Lock()
				gaveUp = true
				mu.Unlock()
			}
		}
		mu.Lock()
		cur--
		mu.Unlock()
	}

	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}

	mu.Lock()
	defer mu.Unlock()
	// More blocks than the window is what makes the cap assertion able to fail.
	if commits <= window {
		t.Fatalf("%d commits for a window of %d: too few blocks for the cap assertion to fail", commits, window)
	}
	if max < 2 {
		t.Fatalf("max blocks committing at once = %d, want > 1: the commits did not overlap", max)
	}
	if max > window {
		t.Fatalf("max concurrent commits = %d, want <= CarveUploadConcurrency (%d): the file-scoped bound on in-flight blocks was breached", max, window)
	}
}

// reapCtxSink fails the block covering failFrom, but only once the earlier block
// has committed, so the surviving run is always past its commits — and therefore
// has its records flipped synced — by the time the failure lands.
type reapCtxSink struct {
	*fakeSink
	failFrom  int64
	committed chan struct{}
	failed    chan struct{}
	onceC     sync.Once
	onceF     sync.Once

	mu      sync.Mutex
	reaps   int
	stalled bool
}

func (r *reapCtxSink) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	if len(chunks) > 0 && chunks[0].FileOffset >= r.failFrom {
		select {
		case <-r.committed:
		case <-time.After(5 * time.Second):
			r.mu.Lock()
			r.stalled = true
			r.mu.Unlock()
			return errors.New("the surviving run never committed")
		}
		r.onceF.Do(func() { close(r.failed) })
		return errors.New("boom")
	}
	err := r.fakeSink.CommitBlock(ctx, chunks)
	if err == nil {
		r.onceC.Do(func() { close(r.committed) })
	}
	return err
}

func (r *reapCtxSink) ReapSupersededManifest(_ context.Context, _ FileID, _, _ int64, _ map[int64]struct{}) error {
	// Only count a reap that runs after the failure has landed: that is the one
	// a completed run would lose if the failure suppressed it.
	select {
	case <-r.failed:
	case <-time.After(5 * time.Second):
		r.mu.Lock()
		r.stalled = true
		r.mu.Unlock()
		return nil
	}
	r.mu.Lock()
	r.reaps++
	r.mu.Unlock()
	return nil
}

// TestCarveReapSurvivesSiblingFailure pins that a run whose records all committed
// and flipped still reaps the manifest rows it superseded when a later block of
// the same carve fails.
//
// The reap runs after every one of the run's records is committed and flipped
// synced. Nothing retries it and a later pass will not revisit those records, so
// a reap skipped here leaves superseded rows alive forever — and overlap
// resolution is greatest-start, so a stale row starting later than a fresh one
// wins and serves old bytes on a cold read.
//
// CarveBlockSize is set to exactly one run so each run flushes as its own block:
// the packer cuts blocks at that size, not at run boundaries, so a larger one
// would coalesce both runs into a single block, the seeded failure would never
// fire, and the test would prove nothing.
func TestCarveReapSurvivesSiblingFailure(t *testing.T) {
	const (
		runSize  = 4 << 10
		failFrom = 64 << 10
	)
	s, dd, base, _ := carveStore(t, Config{CarveBlockSize: runSize, CarveUploadConcurrency: 4})
	sink := &reapCtxSink{
		fakeSink:  base,
		failFrom:  failFrom,
		committed: make(chan struct{}),
		failed:    make(chan struct{}),
	}
	s.SetCarveTargets(dd, sink)
	ctx := context.Background()

	for _, off := range []int64{0, failFrom} {
		if err := s.WriteAt(ctx, "f", off, randBytes(runSize, off)); err != nil {
			t.Fatalf("write %d: %v", off, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve succeeded, want the seeded commit failure")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.stalled {
		t.Fatal("the runs did not interleave as set up, so this test proves nothing")
	}
	if sink.reaps != 1 {
		t.Fatalf("reaps=%d, want 1 from the surviving run", sink.reaps)
	}
}
