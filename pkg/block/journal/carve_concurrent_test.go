package journal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

// syncedAt reports whether the live interval covering off is warm in memory.
func syncedAt(s *Store, id FileID, off int64) bool {
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return false
	}
	for k := range fi.ivs {
		if fi.ivs[k].fileOff <= off && off < fi.ivs[k].end() {
			return fi.ivs[k].synced && !fi.ivs[k].cold
		}
	}
	return false
}

// extendingSink is a fakeSink that also answers the manifest-row lookup and
// records every run-end reap range. The lookup for gateOff blocks until the
// interval at waitFor has been flipped synced, which pins the ordering the
// clamp exists for: the first run asks how far its row reaches only after the
// second run has already marked its own intervals warm in memory.
type extendingSink struct {
	*fakeSink
	mu      sync.Mutex
	reaps   [][2]int64
	store   *Store
	gateOff int64
	waitFor int64
	rowEnd  int64
	timeout error
}

func (e *extendingSink) ManifestRowEndAfter(_ context.Context, id FileID, off int64) (int64, error) {
	if off != e.gateOff {
		return 0, nil
	}
	for i := 0; i < 5000; i++ {
		if syncedAt(e.store, id, e.waitFor) {
			return e.rowEnd, nil
		}
		time.Sleep(time.Millisecond)
	}
	e.mu.Lock()
	e.timeout = errors.New("timed out waiting for the sibling run to flip")
	e.mu.Unlock()
	return 0, nil
}

func (e *extendingSink) ReapSupersededManifest(_ context.Context, _ FileID, runStart, runEnd int64, _ map[int64]struct{}) error {
	e.mu.Lock()
	e.reaps = append(e.reaps, [2]int64{runStart, runEnd})
	e.mu.Unlock()
	return nil
}

// TestCarveRunDoesNotExtendPastNextRun pins that a run's manifest-row extension
// stops at the next run's start.
//
// warmTail cannot tell a sibling run's already-flipped intervals from
// pre-existing warm data — flipUpTo sets interval.synced in memory as a run's
// watermark advances — so once runs carve concurrently a run can be offered an
// extension that reaches straight through a range a sibling is carving. That
// matters because the run-end reap deletes every manifest row starting inside
// its range that it did not itself write, so an over-extended run drops the
// rows the sibling just committed, leaving an uncovered range that cold-reads
// as zeros with no error anywhere.
func TestCarveRunDoesNotExtendPastNextRun(t *testing.T) {
	const rec = 8 << 10
	s, dd, base, _ := carveStore(t, Config{CarveBlockSize: 4 << 20, CarveUploadConcurrency: 4})
	sink := &extendingSink{
		fakeSink: base,
		store:    s,
		gateOff:  rec,     // the first run ends here
		waitFor:  2 * rec, // the second run starts here
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
	timedOut := sink.timeout
	sink.mu.Unlock()
	if timedOut != nil {
		t.Fatalf("ordering not reached, test proves nothing: %v", timedOut)
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
