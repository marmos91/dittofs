package journal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestEviction_RefusesDirtyFragment pins the sole-copy rule: a partial
// overwrite of a synced record re-marks its surviving fragment dirty in place
// (the physical record counter stays equal), and eviction of the sealed segment
// holding it must keep that fragment local — demoting it would turn the sole
// copy into zeros.
func TestEviction_RefusesDirtyFragment(t *testing.T) {
	s, _ := evictStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	const chunk = 128 << 10
	buf := randBytes(chunk, 9)

	// Hydrate appends records already marked synced, so the segment is sealable
	// without a carve.
	if err := s.Hydrate(ctx, "f", 0, buf, 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	wantStates(t, s, "f", StateResident)

	// Seal the active segment by hydrating past the roll threshold once.
	if err := s.Hydrate(ctx, "g", 0, buf, 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	// A partial overwrite lands a new dirty record in the active segment and
	// re-marks the surviving fragment of "f"'s synced record dirty in place —
	// while the sealed record counter still says every record is synced.
	if err := s.WriteAt(ctx, "f", chunk/2, buf[:chunk/2]); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	ext := extentStates(t, s, "f")
	dirty := false
	for _, e := range ext {
		if e.State == StateDirty {
			dirty = true
		}
	}
	if !dirty {
		t.Fatalf("partial overwrite produced no dirty fragment: %+v", ext)
	}

	// Eviction must keep the dirty fragment local: the segment still holds the
	// sole copy of its bytes.
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	ext = extentStates(t, s, "f")
	foundDirty := false
	for _, e := range ext {
		if e.State == StateDirty {
			foundDirty = true
		}
	}
	if !foundDirty {
		t.Fatalf("dirty fragment did not survive eviction: %+v", ext)
	}
}

// TestAppendCold_BrokenLogRefusesLaterAppends pins the broken-log rule: a
// failed append whose rollback also fails leaves a torn tail that ends replay,
// so the log must refuse every later append — a demotion the store cannot keep
// is refused, never taken with entries behind a tear.
func TestAppendCold_BrokenLogRefusesLaterAppends(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{})
	ctx := context.Background()
	const chunk = 128 << 10
	if err := s.WriteAt(ctx, "f", 0, randBytes(chunk, 11)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}

	// Break the log so the first demote's append fails, and make the rollback
	// fail too by pointing the log at a directory whose truncate errors. The
	// simplest forced rollback failure: mark the log broken directly (the flag
	// is what appendCold consults) — the tear behind it is modeled by the flag,
	// so the refused append is the observable contract.
	s.coldMu.Lock()
	s.coldBroken = true
	s.coldMu.Unlock()

	err := s.Invalidate(ctx, "f", 0, chunk)
	if err == nil {
		t.Fatal("a demotion the broken log cannot keep must fail")
	}
	if !errors.Is(err, ErrStateLost) {
		t.Fatalf("want ErrStateLost, got %v", err)
	}
	if !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("error should name the unusable log: %v", err)
	}
	// The refusal leaves the bytes resident: the local copy is the only one.
	wantStates(t, s, "f", StateResident)

	// A later append is refused the same way, never taken behind a tear.
	err = s.Invalidate(ctx, "f", 0, chunk)
	if !errors.Is(err, ErrStateLost) {
		t.Fatalf("later append behind a tear must be refused too, got %v", err)
	}
}
