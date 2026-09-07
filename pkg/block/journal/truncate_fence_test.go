package journal

import (
	"bytes"
	"context"
	"testing"
)

// TestHydrateInTheTruncateVersionWindowIsRefused covers the gap between the two
// versions a truncate carries.
//
// Truncate publishes its fence from a peek taken before the marker exists, but
// clips the index against the marker's own version, which is higher whenever an
// unrelated write lands in between. A hydrate that sampled its bound inside
// that window passes the fence — its bound is above the peek, so it does not
// look stale — and the interval it writes back is stamped higher than the
// marker, so it also survives the clip. The truncated tail comes back and stays
// back, with no error and no log line.
//
// The seam lands the unrelated write that opens the window; in production any
// concurrent writer to the store does the same thing.
func TestHydrateInTheTruncateVersionWindowIsRefused(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	const (
		full = 8192
		half = 4096
	)
	data := randBytes(full, 11)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Sampled inside the window: after Truncate peeked, before it minted its
	// marker. This is the bound a cold read that started here would carry.
	var inWindow uint64
	s.beforeTruncateMarker = func() {
		if err := s.WriteAt(ctx, "other", 0, data[:64]); err != nil {
			t.Errorf("WriteAt(other): %v", err)
		}
		inWindow = s.WriteVersion()
	}

	if err := s.Truncate(ctx, "f", half); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	s.beforeTruncateMarker = nil

	if sz, ok := s.FileSize(ctx, "f"); !ok || sz != half {
		t.Fatalf("FileSize after truncate = (%d,%v), want (%d,true)", sz, ok, half)
	}
	if fenced := s.shardFor("f").hydrateFence["f"].minBound; inWindow >= fenced {
		t.Fatalf("window never opened: sampled %d, fence at %d — the seam did not land a write", inWindow, fenced)
	}

	// A fetch resolved from pre-truncate state, written back under that bound.
	// The fence has to refuse it: the truncate emptied this range away, so
	// there is no interval left for hydratable to weigh it against and it reads
	// as an unwritten hole.
	if err := s.Hydrate(ctx, "f", half, data[half:], inWindow); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	if sz, ok := s.FileSize(ctx, "f"); !ok || sz != half {
		t.Fatalf("truncated tail resurrected: FileSize = (%d,%v), want (%d,true)", sz, ok, half)
	}
	got := make([]byte, full)
	for i := range got {
		got[i] = 0xFF
	}
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[:half], data[:half]) {
		t.Fatalf("kept bytes mismatch")
	}
	if !zeroFilled(got[half:]) {
		t.Fatalf("bytes past newSize are not zero: %x", got[half:half+8])
	}
}

// TestTruncateDoesNotFenceOffItsOwnSurvivors is the other direction: the fence
// is per-file, so raising it to the marker's version must not refuse a hydrate
// of the prefix the truncate kept. DiscardLocalContent is the live caller that
// depends on this — it truncates to zero and the copied content is hydrated
// straight back, under a bound sampled after the truncate.
func TestTruncateDoesNotFenceOffItsOwnSurvivors(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	data := randBytes(8192, 17)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Truncate(ctx, "f", 4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}

	// Sampled after the truncate: this fetch already reflects it.
	notAfter := s.WriteVersion()
	if err := s.Hydrate(ctx, "f", 0, data[:4096], notAfter); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	got := make([]byte, 4096)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, data[:4096]) {
		t.Fatalf("the surviving prefix was fenced off: the hydrate did not land")
	}
}

// TestTruncateDoesNotLowerADeleteFence covers the other half of the same stamp.
//
// fenceDelete raises the fence rather than assigning it, with the stated reason
// that a racing Truncate may have published a higher one. Truncate assigned, so
// the reverse ordering dropped the delete's fence to the truncate's peek and
// re-admitted the hydrates the delete was refusing.
func TestTruncateDoesNotLowerADeleteFence(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	data := randBytes(8192, 13)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	sh := s.shardFor("f")
	sh.mu.Lock()
	sh.fenceDelete("f", s.version.Load()+1000) // a delete fence from far ahead
	fenced := sh.hydrateFence["f"].minBound
	sh.mu.Unlock()

	if err := s.Truncate(ctx, "f", 4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	sh.mu.Lock()
	after := sh.hydrateFence["f"].minBound
	sh.mu.Unlock()
	if after < fenced {
		t.Fatalf("Truncate lowered the fence from %d to %d", fenced, after)
	}
}

// TestTruncateAfterDeleteDoesNotReAdmitPreDeleteHydrates covers the range half
// of the same non-relaxation rule.
//
// A delete fences every range; a later truncate on the re-created file raises
// the version but must not widen what the fence admits. Otherwise a hydrate
// that predates the delete is let back in below newSize and fills whatever
// holes the re-created file left there with the content the delete removed —
// the delete-resurrection class again, limited to the prefix.
func TestTruncateAfterDeleteDoesNotReAdmitPreDeleteHydrates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{})

	old := bytes.Repeat([]byte{0xAB}, 8192)
	if err := s.WriteAt(ctx, "f", 0, old); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// A cold read that resolved here, before the delete.
	stale := s.WriteVersion()

	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Re-created sparsely: [4096,8192) is written, [0,4096) is a hole the stale
	// hydrate would land in.
	if err := s.WriteAt(ctx, "f", 4096, bytes.Repeat([]byte{0xCD}, 4096)); err != nil {
		t.Fatalf("WriteAt(recreate): %v", err)
	}
	if err := s.Truncate(ctx, "f", 6144); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	if err := s.Hydrate(ctx, "f", 0, old[:4096], stale); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	got := make([]byte, 4096)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if bytes.Equal(got, old[:4096]) {
		t.Fatal("a truncate after a delete re-admitted a pre-delete hydrate: deleted content resurrected in the prefix")
	}
}
