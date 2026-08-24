package journal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// allZero reports whether b is entirely zero bytes — the shape a silent
// zero-fill takes when a cold range is mistaken for a POSIX hole.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestColdIntervalSurvivesReopen is the regression for a share serving all-zeros
// after a restart. A cold interval owns no record, so if it is only held in
// memory the reopened store sees a hole, ReadAt zero-fills and reports cold=false
// — the engine never fetches, and the read silently returns zeros for data that
// is safe on the remote.
func TestColdIntervalSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Hydrate (not WriteAt) so every record is born synced and the whole shard is
	// evictable; two sealed segments plus the active one.
	fillUntilSealed(t, s, "f", true, 2)

	// Drain the shard: a large target evicts every qualifying segment, including
	// the force-sealed active, so offset 0 is unambiguously cold afterwards.
	if _, err := s.Evict(ctx, 1<<30); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	dst := make([]byte, chunk256)
	_, st, err := s.ReadAt(ctx, "f", 0, dst)
	if err != nil {
		t.Fatalf("ReadAt before reopen: %v", err)
	}
	if !st.Cold {
		t.Fatal("ReadAt before reopen: cold=false, want true after evicting the range")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	dst2 := make([]byte, chunk256)
	n, st, err := s2.ReadAt(ctx, "f", 0, dst2)
	if err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	}
	if !st.Cold {
		t.Errorf("ReadAt after reopen: cold=false — the evicted range reads as a hole, so the caller returns %d zero bytes without fetching from the remote", n)
	}
	if !allZero(dst2) {
		t.Errorf("ReadAt after reopen: cold range should still zero-fill its placeholder bytes")
	}
}

// TestSeedColdSurvivesReopen covers the seeded flavor: an upgrade that archives a
// pre-journal layout aside (or a snapshot restore) seeds cold intervals from the
// surviving manifest against an otherwise empty journal. Losing them on restart
// is what left a migrated share serving zeros.
func TestSeedColdSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	dst := make([]byte, 4096)
	if _, st, err := s2.ReadAt(ctx, "f", 0, dst); err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	} else if !st.Cold {
		t.Error("ReadAt after reopen: cold=false — a seeded cold range reads as a hole, so reads return zeros with no remote fetch")
	}
}

// TestColdLogSupersededByLaterWrite checks the version ordering: a cold entry
// replayed at recovery must not shadow a warm write that landed after it.
func TestColdLogSupersededByLaterWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	want := bytes.Repeat([]byte{0x7E}, 4096)
	if err := s.WriteAt(ctx, "f", 0, want); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got := make([]byte, 4096)
	_, st, err := s2.ReadAt(ctx, "f", 0, got)
	if err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	}
	if st.Cold {
		t.Error("ReadAt after reopen: cold=true — the stale cold entry shadowed the newer local write")
	}
	if !bytes.Equal(got, want) {
		t.Error("ReadAt after reopen: did not return the bytes written after the seed")
	}
}

// TestColdLogTornTailKeepsIntactEntries checks that a crash mid-append costs only
// the torn entry: everything written before it still replays.
func TestColdLogTornTailKeepsIntactEntries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	if err := s.SeedCold(ctx, "g", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Tear the final entry: drop its last byte so its CRC check fails.
	path := filepath.Join(dir, coldLogName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cold log: %v", err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o644); err != nil {
		t.Fatalf("truncate cold log: %v", err)
	}

	s2, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	dst := make([]byte, 4096)
	if _, st, err := s2.ReadAt(ctx, "f", 0, dst); err != nil {
		t.Fatalf("ReadAt: %v", err)
	} else if !st.Cold {
		t.Error("the entry before the torn tail was dropped; only the torn one should be lost")
	}
}

// TestSeedColdLeavesLocalBytesAlone is what makes a re-seed safe to run against a
// store that is only partly missing its markers. A cold interval carries a fresh
// version, so seeding a range the journal already holds would shadow those bytes
// — and a chunk not yet on the remote cannot be fetched back, so the read would
// fail or return zeros for data that exists only locally.
func TestSeedColdLeavesLocalBytesAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// A locally-written range in the middle of what the manifest describes.
	want := bytes.Repeat([]byte{0x3C}, 4096)
	if err := s.WriteAt(ctx, "f", 4096, want); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 12288}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}

	got := make([]byte, 4096)
	_, st, err := s.ReadAt(ctx, "f", 4096, got)
	if err != nil {
		t.Fatalf("ReadAt over the written range: %v", err)
	}
	if st.Cold {
		t.Error("the seed shadowed a range the journal holds locally; the read now needs a remote copy that may not exist")
	}
	if !bytes.Equal(got, want) {
		t.Error("the seed replaced locally-written bytes")
	}
	// The ranges on either side had nothing local, so they must be cold.
	for _, off := range []int64{0, 8192} {
		if _, st, err := s.ReadAt(ctx, "f", off, make([]byte, 4096)); err != nil {
			t.Fatalf("ReadAt at %d: %v", off, err)
		} else if !st.Cold {
			t.Errorf("range at %d was not seeded, so it reads as a hole and returns zeros with no remote fetch", off)
		}
	}
}

// TestSeedColdIsIdempotent covers a seed that is interrupted and repeated: the
// second pass must add nothing, so an open that dies mid-seed can simply run the
// whole thing again.
func TestSeedColdIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	extents := [][2]int64{{0, 4096}, {4096, 4096}}
	if err := s.SeedCold(ctx, "f", extents); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	first, err := os.Stat(filepath.Join(dir, coldLogName))
	if err != nil {
		t.Fatalf("stat cold log: %v", err)
	}
	if err := s.SeedCold(ctx, "f", extents); err != nil {
		t.Fatalf("SeedCold again: %v", err)
	}
	second, err := os.Stat(filepath.Join(dir, coldLogName))
	if err != nil {
		t.Fatalf("stat cold log: %v", err)
	}
	if second.Size() != first.Size() {
		t.Errorf("re-seeding appended %d bytes for ranges already covered; the log grows on every open",
			second.Size()-first.Size())
	}
}

// TestColdSeededMarkerSurvivesReopen: the marker is what keeps a repaired store
// from paying for the manifest scan on every start, so it has to be on disk
// rather than in memory.
func TestColdSeededMarkerSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.ColdSeeded() {
		t.Fatal("a fresh journal reports it has already been seeded, so the repair scan never runs")
	}
	if err := s.MarkColdSeeded(); err != nil {
		t.Fatalf("MarkColdSeeded: %v", err)
	}
	if !s.ColdSeeded() {
		t.Fatal("the marker was written but is not reported")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.ColdSeeded() {
		t.Error("the marker did not survive a reopen, so every start rescans the manifest")
	}
}

// TestEvictAppendsColdEntriesBeforeReturning pins the half of appendCold's
// contract that cannot be relaxed. Eviction unlinks the only local copy of the
// bytes it just described, so its entries have to be on disk by the time Evict
// returns; a restart that finds them missing reads those ranges as holes and
// serves zeros. The seed path is allowed to batch its appends because it takes
// nothing away, and this test is what fails if that batching is ever extended to
// eviction as a symmetry cleanup.
//
// It reads the log back from the filesystem rather than from the store's index,
// so a deferred or buffered append fails it. It cannot observe the fsync itself:
// a write without Sync still reads back out of page cache, so no in-process test
// can tell the two apart — losing the Sync alone is caught by the device-loss
// rig, not here.
func TestEvictAppendsColdEntriesBeforeReturning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	s, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Hydrated records are born synced, so the whole shard qualifies for eviction.
	fillUntilSealed(t, s, "f", true, 2)
	res, err := s.Evict(ctx, 1<<30)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatal("Evict freed nothing, so the durability this test is about was never exercised")
	}

	// Deliberately no Close and no Sync from the test: whatever is on disk here,
	// the eviction put there before it returned.
	entries, err := loadCold(dir)
	if err != nil {
		t.Fatalf("loadCold: %v", err)
	}
	var covered int64
	for _, e := range entries {
		if e.id == "f" {
			covered += e.length
		}
	}
	if covered == 0 {
		t.Fatalf("Evict unlinked %d bytes of the only local copy but left no cold entry on disk (%d entries): "+
			"a restart takes those ranges for holes and serves zeros instead of fetching them", res.BytesFreed, len(entries))
	}
}

// TestSeedColdBatchIsDurableAndMatchesPerFileSeeding is the seed side of the same
// contract. Batching is allowed to make the append rarer; it is not allowed to
// make it optional, and it must not change what lands. The batch is compared
// against the same files seeded one at a time: a fresh store hands out versions
// in the same order either way, so the two logs are byte-for-byte identical.
func TestSeedColdBatchIsDurableAndMatchesPerFileSeeding(t *testing.T) {
	ctx := context.Background()
	seeds := []ColdSeed{
		{ID: "a", Extents: [][2]int64{{0, 4096}, {4096, 4096}}},
		{ID: "b", Extents: [][2]int64{{0, 8192}}},
		{ID: "c", Extents: [][2]int64{{4096, 4096}}},
	}
	cfg := Config{ShardCount: 4, SegmentSize: minSegmentSize}

	batchDir := t.TempDir()
	batched, err := Open(batchDir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = batched.Close() }()
	if err := batched.SeedColdBatch(ctx, seeds); err != nil {
		t.Fatalf("SeedColdBatch: %v", err)
	}

	// Again no Close: a batch that only becomes durable on shutdown is the bug.
	got, err := loadCold(batchDir)
	if err != nil {
		t.Fatalf("loadCold after batch: %v", err)
	}
	for _, sd := range seeds {
		found := false
		for _, e := range got {
			if e.id == sd.ID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SeedColdBatch returned with no on-disk entry for %q; a restart reads its ranges as holes", sd.ID)
		}
	}

	oneDir := t.TempDir()
	oneAtATime, err := Open(oneDir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = oneAtATime.Close() }()
	for _, sd := range seeds {
		if err := oneAtATime.SeedCold(ctx, sd.ID, sd.Extents); err != nil {
			t.Fatalf("SeedCold %q: %v", sd.ID, err)
		}
	}
	want, err := loadCold(oneDir)
	if err != nil {
		t.Fatalf("loadCold after per-file seeding: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("batched seed recorded %v, seeding the same files one at a time recorded %v", got, want)
	}
}
