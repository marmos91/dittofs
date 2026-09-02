package journal

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestHydrateFillsWithoutOverwriting covers what a hydrate is allowed to write
// back: live local bytes are never replaced whatever the mark, a cold range is
// filled only when it predates the mark, and a range the file does not hold is
// always filled.
func TestHydrateFillsWithoutOverwriting(t *testing.T) {
	ctx := context.Background()
	stale := bytes.Repeat([]byte{0xAB}, 4096)
	fresh := bytes.Repeat([]byte{0x00}, 4096)

	read := func(t *testing.T, s *Store, n int) []byte {
		t.Helper()
		got := make([]byte, n)
		if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		return got
	}

	t.Run("live bytes survive an unmarked hydrate", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "f", 0, stale, 0); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(fresh)), fresh) {
			t.Fatal("hydrate overwrote live local bytes")
		}
	})

	t.Run("live bytes survive a hydrate marked after them", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "f", 0, stale, s.WriteVersion()); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(fresh)), fresh) {
			t.Fatal("hydrate overwrote live local bytes")
		}
	})

	t.Run("a hole is filled", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.Hydrate(ctx, "f", 0, stale, s.WriteVersion()); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(stale)), stale) {
			t.Fatal("hydrate of a hole was dropped")
		}
	})

	t.Run("a cold range is filled only when it predates the mark", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		// An unrelated record first, so the mark taken before "f" is written is
		// still non-zero — a zero mark means "no bound", not "the beginning".
		if err := s.WriteAt(ctx, "g", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		markBefore := s.WriteVersion()
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		markAfter := s.WriteVersion()
		sh := s.shardFor("f")
		sh.mu.Lock()
		for i := range sh.index["f"].ivs {
			sh.index["f"].ivs[i].cold = true
		}
		sh.mu.Unlock()

		if err := s.Hydrate(ctx, "f", 0, stale, markBefore); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if _, st, err := s.ReadAt(ctx, "f", 0, make([]byte, len(stale))); err != nil {
			t.Fatalf("ReadAt: %v", err)
		} else if !st.Cold {
			t.Fatal("hydrate marked before the cold range was recorded was not dropped")
		}

		if err := s.Hydrate(ctx, "f", 0, stale, markAfter); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if !bytes.Equal(read(t, s, len(stale)), stale) {
			t.Fatal("hydrate of a cold range predating the mark was dropped")
		}
	})

	t.Run("only the cold part of a straddling range is filled", func(t *testing.T) {
		s, _ := evictStore(t, Config{})
		if err := s.WriteAt(ctx, "f", 0, fresh); err != nil { // [0, 4096) stays live
			t.Fatalf("WriteAt: %v", err)
		}
		both := append(append([]byte{}, stale...), stale...)
		if err := s.Hydrate(ctx, "f", 0, both, s.WriteVersion()); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		got := read(t, s, len(both))
		if !bytes.Equal(got[:len(fresh)], fresh) {
			t.Fatal("hydrate overwrote the live head")
		}
		if !bytes.Equal(got[len(fresh):], stale) {
			t.Fatal("hydrate did not fill the hole past the live head")
		}
	})
}

// TestHydrateAfterTruncateIsDropped pins the one mutation that leaves no
// interval behind: a truncate empties the range instead of recording over it,
// so a hydrate whose bound predates the truncate is refused outright.
func TestHydrateAfterTruncateIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	mark := s.WriteVersion() // a fetch resolves the pre-truncate manifest here
	if err := s.Truncate(ctx, "f", 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := s.Hydrate(ctx, "f", 0, data, mark); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := make([]byte, len(data))
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, len(data))) {
		t.Fatal("hydrate resurrected bytes a truncate removed")
	}
}

// TestHydrateAfterDeleteIsDropped is the delete twin of
// TestHydrateAfterTruncateIsDropped. A delete removes the file's index entry
// outright, so hydratable's nil-receiver path offers the whole requested range
// and a hydrate still in flight from before the delete would re-create the file
// out of pre-delete remote bytes.
func TestHydrateAfterDeleteIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	mark := s.WriteVersion() // a fetch resolves the pre-delete manifest here
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Hydrate(ctx, "f", 0, data, mark); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if sz, ok := s.FileSize(ctx, "f"); ok {
		t.Fatalf("hydrate resurrected a deleted file: FileSize = %d", sz)
	}
	got := make([]byte, len(data))
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, len(data))) {
		t.Fatal("hydrate resurrected bytes a delete removed")
	}
}

// TestHydrateBoundAtTombstoneVersionIsDropped pins WHICH Version the fence
// carries. Delete cannot stamp it from a version peeked before calling
// appendTombstone: the tombstone's own Version is minted later under a
// different acquisition of the lock and is strictly higher, so a cold read
// holding a bound in between clears the fence, finds the index entry already
// scrubbed, and re-creates the whole file through hydratable's nil receiver.
// Stamping in the same critical section that mints the Version is what leaves
// no gap, and this is the test that says so — the plain
// TestHydrateAfterDeleteIsDropped passes with the fence stamped either way.
func TestHydrateBoundAtTombstoneVersionIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Nothing appends after the tombstone, so this is exactly the tombstone's
	// Version — above any version Delete could peek before minting it, and the
	// record a fill would append lands above it too, where the scrub keeps it.
	if err := s.Hydrate(ctx, "f", 0, data, s.WriteVersion()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if sz, ok := s.FileSize(ctx, "f"); ok {
		t.Fatalf("a hydrate bounded at the tombstone's own Version resurrected the file: FileSize = %d", sz)
	}
}

// TestHydrateAfterDeleteAndRecreate pins the other side of the fence: it bounds
// hydrates that predate the delete, not the file name forever. A file written
// again after the delete accepts a hydrate whose bound was sampled after it.
func TestHydrateAfterDeleteAndRecreate(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	head := bytes.Repeat([]byte{0x11}, 4096)
	tail := bytes.Repeat([]byte{0x22}, 4096)

	if err := s.WriteAt(ctx, "f", 0, head); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.WriteAt(ctx, "f", 0, head); err != nil { // re-created after the unlink
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Hydrate(ctx, "f", int64(len(head)), tail, s.WriteVersion()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := make([]byte, len(head)+len(tail))
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[len(head):], tail) {
		t.Fatal("the delete fence outlived the file and blocked a hydrate of its replacement")
	}
}

// TestDeleteFenceCountIsBounded pins the cap on a fence that has to outlive the
// file's index entry: nothing else takes a delete fence back out, so without the
// FIFO a shard would retain one entry per FileID the store has ever deleted.
func TestDeleteFenceCountIsBounded(t *testing.T) {
	sh := newShard(nil)
	for i := 0; i < maxDeleteFences*3; i++ {
		sh.fenceDelete(FileID(fmt.Sprintf("f%d", i)), uint64(i+1))
	}
	if n := len(sh.hydrateFence); n > maxDeleteFences {
		t.Fatalf("delete fences grew unbounded: %d entries retained, cap is %d", n, maxDeleteFences)
	}
	if _, ok := sh.hydrateFence["f0"]; ok {
		t.Fatal("the oldest delete fence was never evicted")
	}
	newest := FileID(fmt.Sprintf("f%d", maxDeleteFences*3-1))
	if _, ok := sh.hydrateFence[newest]; !ok {
		t.Fatal("the newest delete fence was evicted")
	}
}

// TestDeleteFenceEvictionSparesARestampedFence covers the version guard. A file
// deleted, written again and then truncated holds a truncate fence under the
// same key; evicting the stale delete entry must leave that fence in place,
// since the file is live and still needs it.
func TestDeleteFenceEvictionSparesARestampedFence(t *testing.T) {
	sh := newShard(nil)
	sh.fenceDelete("x", 5)
	sh.hydrateFence["x"] = 99 // a later Truncate re-stamps the same key
	for i := 0; i < maxDeleteFences+1; i++ {
		sh.fenceDelete(FileID(fmt.Sprintf("f%d", i)), uint64(i+100))
	}
	if got := sh.hydrateFence["x"]; got != 99 {
		t.Fatalf("eviction dropped a re-stamped fence: hydrateFence[x] = %d, want 99", got)
	}
}

// TestHydrateBoundAtTruncateMarkerVersionIsDropped is the truncate twin of
// TestHydrateBoundAtTombstoneVersionIsDropped, and fails for the same reason if
// the fence is stamped from a version peeked before appendTruncateMarker rather
// than from the Version that call mints. TestHydrateAfterTruncateIsDropped
// passes either way, so it cannot stand in for this one.
func TestHydrateBoundAtTruncateMarkerVersionIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	data := bytes.Repeat([]byte{0xAB}, 8192)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Truncate(ctx, "f", 4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	// Nothing appends after the marker, so this is exactly the marker's Version:
	// above any version Truncate could peek before minting it. The clip has left
	// [4096, 8192) a hole, which hydratable offers in full.
	if err := s.Hydrate(ctx, "f", 0, data, s.WriteVersion()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := make([]byte, len(data))
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[4096:], make([]byte, 4096)) {
		t.Fatal("a hydrate bounded at the marker's own Version refilled the clipped tail")
	}
}
