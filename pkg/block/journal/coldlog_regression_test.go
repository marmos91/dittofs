package journal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestEviction_ColdMarksDirtyFragment pins the recovery rule: every record of a
// sealed segment was committed remotely (the record counter is fully synced),
// so when eviction cold-marks a segment holding a fragment re-marked dirty in
// place (a partial overwrite of one of its records), the marker is what
// recovers the fragment from the remote store. Leaving the fragment unmarked
// would point it at bytes retireSegment unlinks — reads would fail instead of
// hydrating.
func TestEviction_ColdMarksDirtyFragment(t *testing.T) {
	s, _ := evictStore(t, Config{ShardCount: 1})
	ctx := context.Background()

	// Hydrate appends records already marked synced, so the segments seal
	// evictable without a carve.
	fillUntilSealed(t, s, "f", true, 1)
	sh := s.shardFor("f")
	sealed := sealedSegs(sh)
	if len(sealed) == 0 {
		t.Fatal("no sealed segment to evict")
	}
	segID := sealed[0].id
	_ = segID

	// A partial overwrite lands a new dirty record in the active segment and
	// re-marks the surviving fragment of the sealed record dirty in place —
	// while the sealed record counter still says every record is synced.
	ext := extentStates(t, s, "f")
	if len(ext) == 0 {
		t.Fatal("no extents for f")
	}
	// The evicted segment's records are the hydrated ones: capture the first
	// record's range (the overwrite target) so the post-evict pin can tell
	// "cold-marked" from "left dirty".
	fragRange := [2]int64{ext[0].Off, ext[0].Off + ext[0].Len}
	mid := ext[0].Off + ext[0].Len/2
	buf := randBytes(int(ext[0].Len/2), 9)
	if err := s.WriteAt(ctx, "f", mid, buf); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	ext = extentStates(t, s, "f")
	dirty := false
	for _, e := range ext {
		if e.State == StateDirty {
			dirty = true
		}
	}
	if !dirty {
		t.Fatalf("partial overwrite produced no dirty fragment: %+v", ext)
	}

	// Eviction must retire the segment (the record counter is fully synced and
	// the store's evictable gate passes) and cold-mark the dirty fragment —
	// leaving it unmarked would point it at unlinked bytes. The overwrite's new
	// record lives in the active segment, so its dirty state is correct.
	res, err := s.Evict(ctx, 0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatalf("eviction retired no segments: %+v", res)
	}
	ext = extentStates(t, s, "f")
	// The fragment (ext[0]) is backed by the evicted segment: cold-mark it or
	// reads of its range would fail — its local bytes were unlinked. The
	// overwrite's new record lives in the active segment, so its dirty state is
	// correct; ranges alone don't identify backing, so the pin is the fragment's
	// own state plus the retirement guard above.
	if ext[0].Off != fragRange[0] || ext[0].State != StateRemote {
		t.Fatalf("fragment cold-mark not applied: %+v", ext)
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
