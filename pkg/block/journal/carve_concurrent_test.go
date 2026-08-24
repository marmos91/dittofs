package journal

import (
	"context"
	"testing"
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
// record, separated by a synced gap. Only the run that sees no dirty fragment
// left may flip that record, and flipUpTo holds the shard lock across the whole
// mark-check-flip, so whichever run finishes last is the one that flips.
//
// The shape is built by splitting a record with a small overwrite, carving
// everything, then clipping each surviving fragment with a later write: the
// clipped fragments go dirty again while the small record between them stays
// synced, and that synced gap is what splits the dirty set into two runs.
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
