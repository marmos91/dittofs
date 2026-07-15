package journal

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// coveringSeg returns the segment ID plan attributes to file offset off, or
// (0, false) if off is a hole.
func coveringSeg(fi *fileIndex, off int64) (uint64, bool) {
	pieces := fi.plan(off, 1)
	if len(pieces) == 0 || pieces[0].hole || pieces[0].cold {
		return 0, false
	}
	return pieces[0].loc.SegmentID, true
}

// TestInsertNewestWinsByVersion checks that the interval index resolves
// overlaps by version, not insertion order: a lower-version write indexed after
// a higher-version one must not supersede it. This is the recovery/repack
// out-of-order case (WriteAt itself assigns versions monotonically).
func TestInsertNewestWinsByVersion(t *testing.T) {
	locHi := SegmentLocation{SegmentID: 1, Offset: 100, Length: 10}
	locLo := SegmentLocation{SegmentID: 2, Offset: 200, Length: 4}

	// v5 then v3: the later-indexed older write loses the overlap.
	var a fileIndex
	a.insert(interval{fileOff: 0, length: 10, version: 5, loc: locHi})
	a.insert(interval{fileOff: 3, length: 4, version: 3, loc: locLo})
	for off := int64(0); off < 10; off++ {
		if seg, ok := coveringSeg(&a, off); !ok || seg != 1 {
			t.Fatalf("v5-then-v3: offset %d served by seg %d ok=%v, want seg 1", off, seg, ok)
		}
	}

	// v3 then v5: same winner regardless of order.
	var b fileIndex
	b.insert(interval{fileOff: 3, length: 4, version: 3, loc: locLo})
	b.insert(interval{fileOff: 0, length: 10, version: 5, loc: locHi})
	for off := int64(0); off < 10; off++ {
		if seg, ok := coveringSeg(&b, off); !ok || seg != 1 {
			t.Fatalf("v3-then-v5: offset %d served by seg %d ok=%v, want seg 1", off, seg, ok)
		}
	}
}

// TestInsertPartialOverlapSplitsLocation checks that a higher-version write
// partially overlapping an older one trims the older interval and advances its
// segment offset so the surviving fragment still points at the right bytes.
func TestInsertPartialOverlapSplitsLocation(t *testing.T) {
	var fi fileIndex
	fi.insert(interval{fileOff: 0, length: 10, version: 5, loc: SegmentLocation{SegmentID: 1, Offset: 100, Length: 10}})
	fi.insert(interval{fileOff: 8, length: 7, version: 3, loc: SegmentLocation{SegmentID: 2, Offset: 200, Length: 7}})

	want := []interval{
		{fileOff: 0, length: 10, version: 5, loc: SegmentLocation{SegmentID: 1, Offset: 100, Length: 10}},
		{fileOff: 10, length: 5, version: 3, loc: SegmentLocation{SegmentID: 2, Offset: 202, Length: 5}},
	}
	if len(fi.ivs) != len(want) {
		t.Fatalf("ivs=%+v want %+v", fi.ivs, want)
	}
	for i := range want {
		if fi.ivs[i] != want[i] {
			t.Fatalf("ivs[%d]=%+v want %+v", i, fi.ivs[i], want[i])
		}
	}
}

// TestSparseOverwriteReadback drives real bytes through the Store: layered
// overlapping writes must read back newest-wins with holes zero-filled.
func TestSparseOverwriteReadback(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, bytes.Repeat([]byte("A"), 20)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 5, bytes.Repeat([]byte("B"), 5)); err != nil { // middle overwrite
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 18, bytes.Repeat([]byte("C"), 6)); err != nil { // straddles the tail
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 40, bytes.Repeat([]byte("D"), 4)); err != nil { // island past a hole
		t.Fatal(err)
	}

	got := make([]byte, 50)
	for i := range got {
		got[i] = 0xFF
	}
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 50)
	copy(want[0:20], bytes.Repeat([]byte("A"), 20))
	copy(want[5:10], bytes.Repeat([]byte("B"), 5))
	copy(want[18:24], bytes.Repeat([]byte("C"), 6))
	copy(want[40:44], bytes.Repeat([]byte("D"), 4))
	if !bytes.Equal(got, want) {
		t.Fatalf("readback mismatch:\n got %q\nwant %q", got, want)
	}

	ext, err := s.DataExtents(ctx, "f", 50)
	if err != nil {
		t.Fatal(err)
	}
	wantExt := [][2]uint64{{0, 24}, {40, 44}}
	if len(ext) != len(wantExt) {
		t.Fatalf("extents=%v want %v", ext, wantExt)
	}
	for i := range wantExt {
		if ext[i] != wantExt[i] {
			t.Fatalf("extent %d=%v want %v", i, ext[i], wantExt[i])
		}
	}
}

// TestColdMarkerReadAndExtents checks the hole-vs-evicted distinction: a cold
// range reports cold on read (not a silent hole) and still counts as written
// data in DataExtents, while a true gap does neither.
func TestColdMarkerReadAndExtents(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, bytes.Repeat([]byte("x"), 10)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 100, bytes.Repeat([]byte("y"), 10)); err != nil {
		t.Fatal(err)
	}

	// Simulate eviction of the second range: flip its interval to cold (the
	// eviction path that mutates these lands in a later change).
	sh := s.shardFor("f")
	sh.mu.Lock()
	for i := range sh.index["f"].ivs {
		if sh.index["f"].ivs[i].fileOff == 100 {
			sh.index["f"].ivs[i].cold = true
		}
	}
	sh.mu.Unlock()

	// Reading the cold range reports cold; reading a true hole does not.
	dst := make([]byte, 10)
	_, cold, err := s.ReadAt(ctx, "f", 100, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !cold {
		t.Fatalf("expected cold read over evicted range")
	}
	_, cold, err = s.ReadAt(ctx, "f", 50, dst) // gap between the two writes
	if err != nil {
		t.Fatal(err)
	}
	if cold {
		t.Fatalf("hole must not report cold")
	}

	// Both the warm and the evicted range are present in DataExtents.
	ext, err := s.DataExtents(ctx, "f", 200)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]uint64{{0, 10}, {100, 110}}
	if len(ext) != len(want) {
		t.Fatalf("extents=%v want %v", ext, want)
	}
	for i := range want {
		if ext[i] != want[i] {
			t.Fatalf("extent %d=%v want %v", i, ext[i], want[i])
		}
	}
}

// TestConcurrentReadWriteExtents exercises the shard mutex under -race: many
// goroutines write, read, and query extents on the same file at once.
func TestConcurrentReadWriteExtents(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	const goroutines, iters = 8, 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte('a' + g)}, 64)
			dst := make([]byte, 64)
			for i := 0; i < iters; i++ {
				off := int64((i*goroutines + g) % 4096 * 64)
				if err := s.WriteAt(ctx, "shared", off, data); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
				if _, _, err := s.ReadAt(ctx, "shared", off, dst); err != nil {
					t.Errorf("ReadAt: %v", err)
					return
				}
				if _, err := s.DataExtents(ctx, "shared", 1<<20); err != nil {
					t.Errorf("DataExtents: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
