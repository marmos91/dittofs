package journal

import (
	"context"
	"testing"
)

func TestFileSize(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	// Unknown file: no index entry.
	if sz, ok := s.FileSize(ctx, "missing"); ok || sz != 0 {
		t.Fatalf("FileSize(missing) = (%d,%v), want (0,false)", sz, ok)
	}

	_ = s.WriteAt(ctx, "f", 0, make([]byte, 100))
	_ = s.WriteAt(ctx, "f", 500, make([]byte, 50)) // high-water mark = 550
	if sz, ok := s.FileSize(ctx, "f"); !ok || sz != 550 {
		t.Fatalf("FileSize = (%d,%v), want (550,true)", sz, ok)
	}

	// After deleting the file the entry is gone.
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sz, ok := s.FileSize(ctx, "f"); ok || sz != 0 {
		t.Fatalf("FileSize after delete = (%d,%v), want (0,false)", sz, ok)
	}
}

func TestListFiles(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if got := s.ListFiles(ctx); len(got) != 0 {
		t.Fatalf("empty store ListFiles = %v, want none", got)
	}

	for _, id := range []FileID{"a", "b", "c"} {
		if err := s.WriteAt(ctx, id, 0, []byte("x")); err != nil {
			t.Fatalf("WriteAt %q: %v", id, err)
		}
	}
	set := func() map[FileID]bool {
		m := map[FileID]bool{}
		for _, id := range s.ListFiles(ctx) {
			m[id] = true
		}
		return m
	}
	got := set()
	for _, id := range []FileID{"a", "b", "c"} {
		if !got[id] {
			t.Fatalf("ListFiles missing %q: %v", id, got)
		}
	}

	if err := s.Delete(ctx, "b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := set(); got["b"] || len(got) != 2 {
		t.Fatalf("ListFiles after delete = %v, want {a,c}", got)
	}
}

func TestSetEvictionEnabledGatesEvict(t *testing.T) {
	// A sealed, fully-synced segment is evictable; the gate must suppress it while
	// eviction is disabled and release it once re-enabled.
	s, _ := evictStore(t, Config{})
	ctx := context.Background()
	fillUntilSealed(t, s, "f", true, 1) // Hydrate => synced => evictable
	before := len(sealedSegs(s.shardFor("f")))
	if before < 1 {
		t.Fatalf("want a sealed segment to evict, got %d", before)
	}

	s.SetEvictionEnabled(false)
	res, err := s.Evict(ctx, 1)
	if err != nil {
		t.Fatalf("Evict (disabled): %v", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Fatalf("eviction disabled but evicted %d segments", res.SegmentsEvicted)
	}

	s.SetEvictionEnabled(true)
	res, err = s.Evict(ctx, 1)
	if err != nil {
		t.Fatalf("Evict (enabled): %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatalf("eviction enabled but nothing evicted")
	}
}

func TestEvictionPinOutranksEnable(t *testing.T) {
	// The pin and the health-driven gate are independent: whichever call comes
	// last, a pinned store must not evict, and clearing the pin must restore
	// whatever the health gate last asked for.
	s, _ := evictStore(t, Config{})
	ctx := context.Background()
	fillUntilSealed(t, s, "f", true, 1) // Hydrate => synced => evictable

	s.SetEvictionPinned(true)
	s.SetEvictionEnabled(true) // what Start and SetRemoteStore do on a healthy remote
	res, err := s.Evict(ctx, 1)
	if err != nil {
		t.Fatalf("Evict (pinned): %v", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Fatalf("pinned but evicted %d segments", res.SegmentsEvicted)
	}

	// Unpinned while the health gate is suspended: still no eviction.
	s.SetEvictionEnabled(false)
	s.SetEvictionPinned(false)
	res, err = s.Evict(ctx, 1)
	if err != nil {
		t.Fatalf("Evict (unpinned, suspended): %v", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Fatalf("eviction suspended but evicted %d segments", res.SegmentsEvicted)
	}

	s.SetEvictionEnabled(true)
	res, err = s.Evict(ctx, 1)
	if err != nil {
		t.Fatalf("Evict (unpinned, healthy): %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatalf("unpinned and healthy but nothing evicted")
	}
}
