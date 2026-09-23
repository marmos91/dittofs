package journal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// coldLogSize is the on-disk footprint of the cold log, 0 when it does not
// exist. It is read off the filesystem rather than off the store's own counter,
// so a test asserts the file the next recovery would read.
func coldLogSize(t *testing.T, dir string) int64 {
	t.Helper()
	st, err := os.Stat(filepath.Join(dir, coldLogName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat cold log: %v", err)
	}
	return st.Size()
}

// seedDeadColdEntries appends n cold entries and then buries every one of them:
// the file is seeded cold extent by extent and immediately deleted, so the log
// holds n entries the live index no longer has an interval for. That is the dead
// weight an evict/hydrate/evict cycle leaves behind, reached in two calls.
func seedDeadColdEntries(t *testing.T, s *Store, id FileID, n int) {
	t.Helper()
	ctx := context.Background()
	extents := make([][2]int64, n)
	for i := range extents {
		extents[i] = [2]int64{int64(i) * 8192, 4096}
	}
	if err := s.SeedCold(ctx, id, extents); err != nil {
		t.Fatalf("SeedCold %s: %v", id, err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete %s: %v", id, err)
	}
}

// wantColdAt asserts a read of one byte at off reports cold, so the caller
// fetches from the remote instead of serving the zero-filled placeholder as
// data. This is the consumer-side truth a lost cold entry destroys.
func wantColdAt(t *testing.T, s *Store, id FileID, off int64, when string) {
	t.Helper()
	dst := make([]byte, 1)
	if _, st, err := s.ReadAt(context.Background(), id, off, dst); err != nil {
		t.Fatalf("ReadAt %s@%d %s: %v", id, off, when, err)
	} else if !st.Cold {
		t.Fatalf("ReadAt %s@%d %s: cold=false — the range reads as a hole, so the caller returns zeros without fetching from the remote", id, off, when)
	}
}

// coldOffsets returns the start offset of every cold extent of id.
func coldOffsets(t *testing.T, s *Store, id FileID) []int64 {
	t.Helper()
	var out []int64
	for _, e := range extentStates(t, s, id) {
		if e.State == StateRemote {
			out = append(out, e.Off)
		}
	}
	return out
}

// TestColdLogCompactionKeepsLiveRangesCold drives enough dead weight into the
// cold log to trip the compaction ratio, compacts, and pins that every range
// still marked cold is still reported cold — both against the running store and
// against a reopened one, which replays the compacted log. A range the rewrite
// drops comes back as a POSIX hole and reads as zeros with no fetch.
func TestColdLogCompactionKeepsLiveRangesCold(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}
	s, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The eviction path: synced records so the whole shard is evictable, then a
	// target large enough to drain it. Every one of the file's ranges is cold
	// afterwards, and each one owns a cold-log entry.
	fillUntilSealed(t, s, "evicted", true, 2)
	if _, err := s.Evict(ctx, 1<<30); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	// The seeding path: a range the local tier never held.
	if err := s.SeedCold(ctx, "seeded", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	live := coldOffsets(t, s, "evicted")
	if len(live) < 2 {
		t.Fatalf("expected the evicted file to hold several cold ranges, got %v", live)
	}
	seedDeadColdEntries(t, s, "pad", 1200)

	before := coldLogSize(t, dir)
	s.maybeCompactColdLog()
	after := coldLogSize(t, dir)
	if after >= before {
		t.Fatalf("compaction did not shrink the cold log: before=%d after=%d", before, after)
	}

	for _, off := range live {
		wantColdAt(t, s, "evicted", off, "after compaction")
	}
	wantColdAt(t, s, "seeded", 0, "after compaction")

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	for _, off := range live {
		wantColdAt(t, s2, "evicted", off, "after reopen")
	}
	wantColdAt(t, s2, "seeded", 0, "after reopen")
}

// TestColdLogCompactionAbortsOnConcurrentAppend lands an append inside the
// window between the compactor's snapshot and its verify — deterministically,
// through the seam, not by timing one — and pins both halves of the contract:
// the pass abandons the rewrite, and the entry that landed is still in the log a
// reopen reads. Rewriting the snapshot instead would drop that entry, which is
// silent zeros for a range its appender has already stopped keeping locally.
func TestColdLogCompactionAbortsOnConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}
	s, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SeedCold(ctx, "seeded", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	seedDeadColdEntries(t, s, "pad", 1200)

	before := coldLogSize(t, dir)
	s.beforeColdCompactVerify = func() {
		if err := s.SeedCold(ctx, "late", [][2]int64{{0, 4096}}); err != nil {
			t.Errorf("SeedCold inside the snapshot window: %v", err)
		}
	}
	s.maybeCompactColdLog()
	s.beforeColdCompactVerify = nil

	// An aborted pass leaves the log as the append found it: longer by that one
	// entry, never rewritten down to the snapshot.
	if got := coldLogSize(t, dir); got <= before {
		t.Fatalf("compaction did not abort: cold log went from %d to %d bytes, so the entry appended inside the snapshot window was dropped", before, got)
	}
	wantColdAt(t, s, "late", 0, "after the aborted compaction")

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	wantColdAt(t, s2, "late", 0, "after reopen")
	wantColdAt(t, s2, "seeded", 0, "after reopen")
}

// TestColdLogGrowthBoundedAcrossCycles is the unbounded-growth regression. Each
// cycle appends a batch of cold entries and buries them, exactly as an
// evict/hydrate/evict cycle does; with compaction only at recovery the file
// climbs monotonically for the whole uptime. One range stays cold throughout so
// the bound is not the trivial one of an emptied log.
func TestColdLogGrowthBoundedAcrossCycles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.SeedCold(ctx, "keep", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}

	const cycles = 6
	var sizes []int64
	for c := 0; c < cycles; c++ {
		seedDeadColdEntries(t, s, FileID(fmt.Sprintf("pad-%d", c)), 600)
		s.maybeCompactColdLog()
		sizes = append(sizes, coldLogSize(t, dir))
	}

	// The first cycle is below the compaction floor, so sizes[0] is one cycle's
	// worth of appends and the right yardstick: the log must not outgrow it by
	// more than the batch a pass can be mid-way through.
	if bound := 2 * sizes[0]; sizes[len(sizes)-1] > sizes[2] || maxSize(sizes) > bound {
		t.Fatalf("cold log grows with churn instead of being reclaimed: sizes=%v, bound=%d", sizes, bound)
	}
	wantColdAt(t, s, "keep", 0, "after the last compaction")
}

// TestColdLogSnapshotRefusesUnsyncedShard pins the durability half of the
// snapshot rule. The entries a snapshot leaves out are the ones a rewrite drops,
// and an entry superseded by a record no fsync has covered has to be kept: losing
// that record turns the range back into a hole, and without the entry the read
// serves zeros instead of fetching. The snapshot refuses wholesale rather than
// deciding entry by entry.
func TestColdLogSnapshotRefusesUnsyncedShard(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	if _, ok := s.liveColdSnapshot(); !ok {
		t.Fatal("a store holding nothing unsynced must snapshot")
	}
	if err := s.WriteAt(ctx, "f", 0, randBytes(4096, 3)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, ok := s.liveColdSnapshot(); ok {
		t.Fatal("a shard holding a record no fsync has covered must refuse the snapshot, or a cold entry that record supersedes is dropped while the record can still be lost")
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, ok := s.liveColdSnapshot(); !ok {
		t.Fatal("a committed shard must snapshot again")
	}
}

func maxSize(xs []int64) int64 {
	var m int64
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}
