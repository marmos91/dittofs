package fs

import (
	"testing"
	"time"
)

func TestIntervalTree_BasicInsertLenConsume(t *testing.T) {
	it := newIntervalTree()
	now := time.Unix(1000, 0)
	it.Insert(0, 100, now)
	it.Insert(100, 100, now)
	it.Insert(200, 100, now)
	if it.Len() != 3 {
		t.Fatalf("len: got %d want 3", it.Len())
	}
	it.ConsumeUpTo(150) // drops [0,100) because 0+100=100 <= 150
	if it.Len() != 2 {
		t.Fatalf("after ConsumeUpTo(150): got %d want 2", it.Len())
	}
}

func TestIntervalTree_EarliestStable_HonorsTimestamp(t *testing.T) {
	it := newIntervalTree()
	base := time.Unix(1000, 0)
	it.Insert(0, 64, base)
	if _, ok := it.EarliestStable(base, 100*time.Millisecond); ok {
		t.Fatal("expected not stable at same instant")
	}
	if iv, ok := it.EarliestStable(base.Add(200*time.Millisecond), 100*time.Millisecond); !ok || iv.Offset != 0 {
		t.Fatalf("expected stable at +200ms, got ok=%v off=%d", ok, iv.Offset)
	}
}

// TestIntervalTree_EarliestStable_BlockedByEarlierUnstable asserts that an
// unstable low-offset interval blocks a later-offset stable interval from
// being returned. If EarliestStable skipped the unstable entry and returned
// the offset-100 stable one, the rollup would advance past file bytes
// [0..100) — bytes which are dirty but not yet stable — filling that gap
// with zeros when reconstructStream materializes the contiguous buffer
// (data loss). INV-17 / D-16 requires the rollup to wait instead.
func TestIntervalTree_EarliestStable_BlockedByEarlierUnstable(t *testing.T) {
	it := newIntervalTree()
	now := time.Unix(1000, 0)
	// Interval at offset 0 is JUST touched — unstable vs a 1-minute window.
	it.Insert(0, 50, now)
	// Interval at offset 100 is 5 minutes old — stable in isolation.
	it.Insert(100, 50, now.Add(-5*time.Minute))

	iv, ok := it.EarliestStable(now, 1*time.Minute)
	if ok {
		t.Fatalf("EarliestStable must return not-found when earliest-offset interval is unstable; got off=%d len=%d", iv.Offset, iv.Length)
	}
}

func TestIntervalTree_DropAbove_ClipsStraddling(t *testing.T) {
	it := newIntervalTree()
	now := time.Unix(1000, 0)
	it.Insert(100, 50, now) // covers [100, 150)
	it.Insert(200, 50, now) // covers [200, 250)
	it.DropAbove(120)
	if it.Len() != 1 {
		t.Fatalf("after DropAbove(120): len=%d want 1", it.Len())
	}
	iv, ok := it.EarliestStable(now.Add(time.Hour), time.Millisecond)
	if !ok || iv.Offset != 100 || iv.Length != 20 {
		t.Fatalf("clipped: off=%d len=%d ok=%v; want off=100 len=20 ok=true", iv.Offset, iv.Length, ok)
	}
}
