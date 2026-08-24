package journal

import (
	"context"
	"testing"
)

// TestColdExtents_CountsEvictedRangesOnly drives the real evict path and
// asserts the tally moves with it: a store holding only resident data reports
// nothing remote-only, and evicting a segment turns exactly those bytes into
// the figure an operator reads as "this would not serve offline".
func TestColdExtents_CountsEvictedRangesOnly(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	fillUntilSealed(t, s, "f", true, 2)

	if bytes, extents := s.ColdExtents(); bytes != 0 || extents != 0 {
		t.Fatalf("before eviction: ColdExtents() = (%d, %d), want (0, 0) — nothing has been evicted", bytes, extents)
	}

	res, err := s.Evict(ctx, 0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 1 {
		t.Fatalf("Evict evicted %d segments, want 1", res.SegmentsEvicted)
	}

	bytes, extents := s.ColdExtents()
	if bytes <= 0 || extents <= 0 {
		t.Fatalf("after eviction: ColdExtents() = (%d, %d), want both positive", bytes, extents)
	}
	// The tally counts payload ranges, so it must not exceed the segment's
	// on-disk footprint, which also carries record framing.
	if bytes > res.BytesFreed {
		t.Errorf("cold bytes %d exceed the %d bytes the eviction freed on disk", bytes, res.BytesFreed)
	}
}

// TestColdExtents_HydrateClearsIt asserts pulling an evicted range back local
// removes it from the tally, so an operator running a warm can watch the
// number fall to zero rather than watching a figure that only ever grows.
func TestColdExtents_HydrateClearsIt(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	fillUntilSealed(t, s, "f", true, 2)
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	coldBefore, extentsBefore := s.ColdExtents()
	if coldBefore == 0 {
		t.Fatal("eviction produced no cold bytes; the rest of this test proves nothing")
	}

	// Re-hydrate every cold range, which is what a warm does.
	for _, iv := range coldIntervals(s) {
		buf := make([]byte, iv.length)
		if err := s.Hydrate(ctx, "f", iv.fileOff, buf, 0); err != nil {
			t.Fatalf("Hydrate(%d, %d): %v", iv.fileOff, iv.length, err)
		}
	}

	if bytes, extents := s.ColdExtents(); bytes != 0 || extents != 0 {
		t.Fatalf("after re-hydrating everything: ColdExtents() = (%d, %d) (was %d, %d), want (0, 0)",
			bytes, extents, coldBefore, extentsBefore)
	}
}

// coldIntervals snapshots the store's cold ranges so a test can act on them.
func coldIntervals(s *Store) []interval {
	var out []interval
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, fi := range sh.index {
			for _, iv := range fi.ivs {
				if iv.cold {
					out = append(out, iv)
				}
			}
		}
		sh.mu.Unlock()
	}
	return out
}
