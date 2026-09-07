package journal

import (
	"context"
	"math/rand"
	"testing"
)

// checkSortedDisjoint asserts the invariant FileSize reads off the tail: the
// intervals are sorted by fileOff, do not overlap, and so the last one carries
// the maximum end offset.
func checkSortedDisjoint(t *testing.T, s *Store, id FileID, when string) {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return
	}
	var maxEnd int64
	for i, iv := range fi.ivs {
		if i > 0 {
			prev := fi.ivs[i-1]
			if iv.fileOff < prev.fileOff {
				t.Fatalf("%s: ivs[%d].fileOff=%d < ivs[%d].fileOff=%d (unsorted)",
					when, i, iv.fileOff, i-1, prev.fileOff)
			}
			if iv.fileOff < prev.end() {
				t.Fatalf("%s: ivs[%d] [%d,%d) overlaps ivs[%d] [%d,%d)",
					when, i, iv.fileOff, iv.end(), i-1, prev.fileOff, prev.end())
			}
		}
		if e := iv.end(); e > maxEnd {
			maxEnd = e
		}
	}
	if n := len(fi.ivs); n > 0 && fi.ivs[n-1].end() != maxEnd {
		t.Fatalf("%s: last end=%d but max end=%d — the tail is not the high-water mark",
			when, fi.ivs[n-1].end(), maxEnd)
	}
}

// TestIntervalIndexStaysSortedAndDisjoint pins the invariant FileSize now reads
// off the tail instead of rescanning for.
//
// insert is the only path that can reorder intervals or introduce an overlap,
// so that is what this hammers, through every caller that reaches it: WriteAt,
// Truncate, Hydrate and SeedCold. The other rewrites of fi.ivs cannot break it
// by construction — carve and reclaim only flip the synced and cold flags on
// intervals already in place, and the filtering rewrites either drop an
// interval or clamp its end downwards, neither of which can unsort a sorted set
// or overlap a disjoint one. The reopen leg covers the one filter that
// transforms rather than drops: recovery re-applying a truncate marker over a
// replayed record, clipping the interval that straddles newSize.
func TestIntervalIndexStaysSortedAndDisjoint(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rng := rand.New(rand.NewSource(7))
	buf := make([]byte, 8<<10)
	// Offsets are drawn from a range narrow enough that writes collide often:
	// a probe that only ever appends would never reach the punch-and-merge path
	// that is the one able to produce an overlap.
	randOff := func() int64 { return int64(rng.Intn(512)) * (4 << 10) }

	const id = FileID("f")
	for i := 0; i < 4000; i++ {
		switch rng.Intn(7) {
		case 0, 1, 2:
			if err := s.WriteAt(ctx, id, randOff(), buf[:1+rng.Intn(len(buf))]); err != nil {
				t.Fatalf("WriteAt: %v", err)
			}
		case 3:
			if _, ok := s.FileSize(ctx, id); !ok {
				continue
			}
		case 4:
			if err := s.Truncate(ctx, id, randOff()); err != nil {
				t.Fatalf("Truncate: %v", err)
			}
		case 5:
			if err := s.Hydrate(ctx, id, randOff(), buf[:1+rng.Intn(len(buf))], 0); err != nil {
				t.Fatalf("Hydrate: %v", err)
			}
		case 6:
			off := randOff()
			if err := s.SeedCold(ctx, id, [][2]int64{{off, int64(1 + rng.Intn(8<<10))}}); err != nil {
				t.Fatalf("SeedCold: %v", err)
			}
		}
		checkSortedDisjoint(t, s, id, "after mutation")
	}

	// A file that is deleted outright exercises the tombstone filter on replay.
	if err := s.WriteAt(ctx, "doomed", 0, buf[:4096]); err != nil {
		t.Fatalf("WriteAt doomed: %v", err)
	}
	if err := s.Delete(ctx, "doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want, ok := s.FileSize(ctx, id)
	if !ok {
		t.Fatalf("FileSize before reopen: no index entry")
	}
	_ = s.Close()

	r, err := Open(dir, Config{ShardCount: 1}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = r.Close() }()

	checkSortedDisjoint(t, r, id, "after recovery")
	if got, ok := r.FileSize(ctx, id); !ok || got != want {
		t.Fatalf("recovered FileSize = (%d,%v), want (%d,true)", got, ok, want)
	}
}
