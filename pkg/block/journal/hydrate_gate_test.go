package journal

import (
	"bytes"
	"context"
	"fmt"
	"sync"
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

// TestHydrateAfterTruncateServesSurvivingPrefix bounds how far the truncate
// fence may go. The file survives a truncate, so once the clip has run a cold
// read of the range the truncate did NOT remove is legitimate and must be
// filled. If nothing appends after the marker the reader's bound equals the
// marker's own Version, so a fence that refuses at-or-below its stamp refuses
// this reader — permanently, until some unrelated write happens to raise the
// store's LSN. That is the shape that took out the engine's cold-read tests.
func TestHydrateAfterTruncateServesSurvivingPrefix(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	orig := bytes.Repeat([]byte{0xAB}, 8192)
	if err := s.WriteAt(ctx, "f", 0, orig); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Truncate(ctx, "f", 4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	// Evict what survived: the prefix is now cold, so a read of it fetches and
	// hydrates rather than serving local bytes.
	sh := s.shardFor("f")
	sh.mu.Lock()
	for i := range sh.index["f"].ivs {
		sh.index["f"].ivs[i].cold = true
	}
	sh.mu.Unlock()

	fetched := bytes.Repeat([]byte{0xCD}, 4096)
	// Nothing has appended since the marker, so this IS the marker's Version.
	if err := s.Hydrate(ctx, "f", 0, fetched, s.WriteVersion()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := make([]byte, 4096)
	if _, st, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	} else if st.Cold {
		t.Fatal("the surviving prefix is still cold: the truncate fence refused a legitimate cold read")
	}
	if !bytes.Equal(got, fetched) {
		t.Fatal("the surviving prefix did not take the fetched bytes")
	}
}

// TestHydrateDuringTruncateIsDropped is the other half of the two-state
// requirement, and the half that fails if the fence is simply relaxed. sh.mu is
// released between the marker's mint and the clip, and the marker's fsync runs
// in that gap, so a hydrate can take the lock while the index still holds the
// pre-truncate intervals. Bounded at the marker's own Version it would offer the
// whole range, and the record it appends carries a Version the clip's own
// iv.version > truncVer test then preserves — the removed tail comes back.
//
// Together with TestHydrateAfterTruncateServesSurvivingPrefix this pins both
// states: the SAME bound must be refused during the truncate and served after
// it, which is why no single comparison against a fixed stamp can be right.
func TestHydrateDuringTruncateIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{})
	orig := bytes.Repeat([]byte{0xAB}, 8192)
	if err := s.WriteAt(ctx, "f", 0, orig); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	sh := s.shardFor("f")
	sh.mu.Lock()
	for i := range sh.index["f"].ivs {
		sh.index["f"].ivs[i].cold = true // evicted: hydratable will offer these
	}
	sh.mu.Unlock()

	realSync := sh.segSync
	var once sync.Once
	var hydErr error
	sh.segSync = func(seg *segmentMeta) error {
		err := realSync(seg)
		once.Do(func() {
			// Mid-marker-fsync: the Version is minted, the clip has not run.
			hydErr = s.Hydrate(ctx, "f", 0, orig, s.WriteVersion())
		})
		return err
	}
	if err := s.Truncate(ctx, "f", 4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if hydErr != nil {
		t.Fatalf("Hydrate: %v", hydErr)
	}

	got := make([]byte, 8192)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[4096:], make([]byte, 4096)) {
		t.Fatal("a hydrate landing inside the truncate window resurrected the removed tail")
	}
}

// TestHydrateOverEvictedFenceIsDropped pins the direction the FIFO fails in.
// The cap is reachable — ShardCount may be 1, so every delete lands in one
// shard, and a remote serving 503s stretches the fetch window widest exactly
// when cold reads are most likely to be outstanding — so evicting a fence whose
// hydrate is still in flight must not let that hydrate land. evictedFenceFloor
// stands in for the dropped entry, which makes overflow cost a re-fetch instead
// of a resurrection, and makes maxDeleteFences a memory knob rather than a
// correctness bound.
func TestHydrateOverEvictedFenceIsDropped(t *testing.T) {
	ctx := context.Background()
	s, _ := evictStore(t, Config{}) // single shard: every fence shares one FIFO
	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := s.WriteAt(ctx, "victim", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	mark := s.WriteVersion() // a cold read resolves the pre-delete manifest here
	if err := s.Delete(ctx, "victim"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Push the victim's fence out of the FIFO behind enough later deletes. Real
	// deletes would each cost a tombstone fsync; the fences they stamp go through
	// this same call.
	sh := s.shardFor("victim")
	sh.mu.Lock()
	base := s.WriteVersion()
	for i := 0; i <= maxDeleteFences; i++ {
		sh.fenceDelete(FileID(fmt.Sprintf("later%d", i)), base+1+uint64(i))
	}
	_, stillFenced := sh.hydrateFence["victim"]
	sh.mu.Unlock()
	if stillFenced {
		t.Fatal("setup: the victim's fence was not evicted, so this proves nothing")
	}

	if err := s.Hydrate(ctx, "victim", 0, data, mark); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if sz, ok := s.FileSize(ctx, "victim"); ok {
		t.Fatalf("a hydrate over an evicted fence resurrected the file: FileSize = %d", sz)
	}
}
