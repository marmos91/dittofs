package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// coldLogSize is the on-disk footprint of the store's cold log, 0 when it does
// not exist. It is read off the filesystem rather than off the store's own
// counter, so a test asserts the file the next recovery would read.
func coldLogSize(t *testing.T, s *Store) int64 {
	t.Helper()
	st, err := os.Stat(filepath.Join(s.dir, coldLogName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat cold log: %v", err)
	}
	return st.Size()
}

// setColdHooks installs the cold-log test seams under the lock the store reads
// them with.
func setColdHooks(s *Store, betweenAppendAndPublish, beforeCompactVerify func()) {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	s.betweenColdAppendAndPublish = betweenAppendAndPublish
	s.beforeColdCompactVerify = beforeCompactVerify
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
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})

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

	before := coldLogSize(t, s)
	s.maybeCompactColdLog()
	after := coldLogSize(t, s)
	if after >= before {
		t.Fatalf("compaction did not shrink the cold log: before=%d after=%d", before, after)
	}

	for _, off := range live {
		wantColdAt(t, s, "evicted", off, "after compaction")
	}
	wantColdAt(t, s, "seeded", 0, "after compaction")

	s2 := reopen(t, s, Config{})
	for _, off := range live {
		wantColdAt(t, s2, "evicted", off, "after reopen")
	}
	wantColdAt(t, s2, "seeded", 0, "after reopen")
}

// TestColdLogCompactionKeepsAppendedButUnpublishedEntries is the regression for
// the window between a cold-log append and the index publish that follows it.
// SeedColdBatch appends once for the whole batch and only then takes each shard
// lock to insert, exactly as eviction appends before re-locking to flip; a
// compaction that snapshots an index in that window finds nothing cold for those
// entries and would rewrite the log without them, while the caller goes on to
// publish — and, in eviction's case, to unlink the only local copy. The loss is
// invisible until the next open.
//
// The compaction is driven from the seam inside that window rather than raced
// against it, so the ordering is the same on every run.
func TestColdLogCompactionKeepsAppendedButUnpublishedEntries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	if err := s.SeedCold(ctx, "seeded", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	seedDeadColdEntries(t, s, "pad", 1200)

	before := coldLogSize(t, s)
	setColdHooks(s, func() { s.maybeCompactColdLog() }, nil)
	if err := s.SeedColdBatch(ctx, []ColdSeed{{ID: "victim", Extents: [][2]int64{{0, 4096}}}}); err != nil {
		t.Fatalf("SeedColdBatch: %v", err)
	}
	setColdHooks(s, nil, nil)

	// The log has to be longer than it was — the victim's entry appended, nothing
	// rewritten away. A shorter log means the pass rewrote from an index that did
	// not describe the entry yet.
	if got := coldLogSize(t, s); got <= before {
		t.Fatalf("compaction rewrote the log while an append was unpublished: %d -> %d bytes, so the entry the log already held was dropped", before, got)
	}
	// In-uptime the index carries the range whether or not the log kept it, which
	// is why the assertion that matters is the one after the reopen.
	wantColdAt(t, s, "victim", 0, "after the compaction that raced the publish")
	s2 := reopen(t, s, Config{})
	wantColdAt(t, s2, "victim", 0, "after reopen")
	wantColdAt(t, s2, "seeded", 0, "after reopen")
}

// TestColdLogCompactionAbortsOnConcurrentAppend covers the other half of the
// verify: an append that lands after the pass read the log's entry count, which
// SeedCold does here under its own shard lock. The pass has to abandon the
// rewrite, and the entry has to still be in the log a reopen reads.
func TestColdLogCompactionAbortsOnConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	if err := s.SeedCold(ctx, "seeded", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	seedDeadColdEntries(t, s, "pad", 1200)

	before := coldLogSize(t, s)
	setColdHooks(s, nil, func() {
		if err := s.SeedCold(ctx, "late", [][2]int64{{0, 4096}}); err != nil {
			t.Errorf("SeedCold inside the snapshot window: %v", err)
		}
	})
	s.maybeCompactColdLog()
	setColdHooks(s, nil, nil)

	if got := coldLogSize(t, s); got <= before {
		t.Fatalf("compaction did not abort: cold log went from %d to %d bytes, so the entry appended inside the snapshot window was dropped", before, got)
	}
	wantColdAt(t, s, "late", 0, "after the aborted compaction")

	s2 := reopen(t, s, Config{})
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
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	if err := s.SeedCold(ctx, "keep", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}

	const cycles = 6
	var sizes []int64
	for c := 0; c < cycles; c++ {
		seedDeadColdEntries(t, s, FileID(fmt.Sprintf("pad-%d", c)), 600)
		s.maybeCompactColdLog()
		sizes = append(sizes, coldLogSize(t, s))
	}

	// The first cycle is below the compaction floor, so sizes[0] is one cycle's
	// worth of appends and the right yardstick: the log must not outgrow it by
	// more than the batch a pass can be mid-way through.
	if bound := 2 * sizes[0]; sizes[len(sizes)-1] > sizes[2] || slices.Max(sizes) > bound {
		t.Fatalf("cold log grows with churn instead of being reclaimed: sizes=%v, bound=%d", sizes, bound)
	}
	wantColdAt(t, s, "keep", 0, "after the last compaction")
	if got := s.Stats().ColdLogEntries; got == 0 || got > 2 {
		t.Fatalf("Stats should report the surviving cold-log entries, got %d", got)
	}
}

// TestColdLogCompactionCarriesUnverifiedShardsForward pins what a shard holding
// unfsynced records costs: that shard's entries, not the pass. Dropping one of
// them would be unsafe — the record that superseded it can still be lost, and the
// range would come back a hole — but keeping it only costs a replay that resolves
// it away, so the other shards are still compacted. Without that split a store
// under sustained writes never compacts at all, which is the load the log grows
// under in the first place.
//
// DirtyExpiry is negative so no loop fsyncs on its own schedule, which is both
// the operator opt-out and the only way to hold a shard dirty across the pass.
func TestColdLogCompactionCarriesUnverifiedShardsForward(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{ShardCount: 4, SegmentSize: minSegmentSize, DirtyExpiry: -1})
	dirtyID, cleanID := twoShardIDs(t, s)

	// A cold entry the index no longer calls cold, superseded by a record no
	// fsync has covered: exactly the entry a rewrite must not drop.
	if err := s.SeedCold(ctx, dirtyID, [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	if err := s.WriteAt(ctx, dirtyID, 0, randBytes(4096, 7)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, unverified := s.liveColdSnapshot(); !unverified[s.shardIndex(dirtyID)] {
		t.Fatal("the shard holding an unfsynced record should be reported unverified")
	}
	seedDeadColdEntries(t, s, cleanID, 1200)

	before := coldLogSize(t, s)
	s.maybeCompactColdLog()
	if after := coldLogSize(t, s); after >= before {
		t.Fatalf("a single dirty shard blocked the whole pass: cold log %d -> %d bytes", before, after)
	}

	// The dirty shard's entry survives the rewrite, so a lost record still leaves
	// the range fetchable rather than a hole.
	kept, _, err := loadCold(s.dir, s.log)
	if err != nil {
		t.Fatalf("loadCold: %v", err)
	}
	found := false
	for _, e := range kept {
		if e.id == dirtyID && e.fileOff == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unverified shard's entry was dropped; log now holds %d entries", len(kept))
	}
}

// TestColdLogCompactionKeepsEvictionsAppendedButUnpublished is the same window as
// the batch-seed regression, on the path that makes it expensive: eviction
// appends its markers, re-takes the shard lock to flip the intervals cold, and
// then unlinks the segment holding the only local copy. A compaction that
// rewrote the log from an index snapshotted in that window would drop markers for
// bytes that are about to stop existing locally.
func TestColdLogCompactionKeepsEvictionsAppendedButUnpublished(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	if err := s.SeedCold(ctx, "seeded", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	seedDeadColdEntries(t, s, "pad", 1200)
	fillUntilSealed(t, s, "evicted", true, 2)

	setColdHooks(s, func() { s.maybeCompactColdLog() }, nil)
	if _, err := s.Evict(ctx, 1<<30); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	setColdHooks(s, nil, nil)

	live := coldOffsets(t, s, "evicted")
	if len(live) < 2 {
		t.Fatalf("expected the evicted file to hold several cold ranges, got %v", live)
	}
	s2 := reopen(t, s, Config{})
	for _, off := range live {
		wantColdAt(t, s2, "evicted", off, "after reopen")
	}
	wantColdAt(t, s2, "seeded", 0, "after reopen")
}

// TestColdLogCompactionSurvivesAFailedShardCommit pins that the commit sweep
// decides how much of the store a pass shrinks and nothing else. A shard whose
// fsync fails is left dirty by that very failure, so the carry already covers it;
// abandoning the pass would take the healthy shards down with it, which is the
// same store-wide inertness by another route.
func TestColdLogCompactionSurvivesAFailedShardCommit(t *testing.T) {
	ctx := context.Background()
	// DirtyExpiry is long rather than negative: the compaction's own commit sweep
	// has to run (that is the path under test) while the background loop does not
	// get to mark the shard failed before the pass does.
	s := testStore(t, Config{ShardCount: 4, SegmentSize: minSegmentSize, DirtyExpiry: 10 * time.Minute})
	failID, cleanID := twoShardIDs(t, s)
	s.shardFor(failID).segSync = func(*segmentMeta) error {
		return errors.New("simulated fsync failure")
	}

	if err := s.SeedCold(ctx, failID, [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	if err := s.WriteAt(ctx, failID, 0, randBytes(4096, 11)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	seedDeadColdEntries(t, s, cleanID, 1200)

	// The sweep has to fail inside the pass, so nothing may commit that shard
	// first: the failure is sticky, and a shard already marked failed is skipped
	// by the sweep rather than reported by it.
	before := coldLogSize(t, s)
	s.maybeCompactColdLog()
	if !s.shardFor(failID).syncFailed.Load() {
		t.Fatal("the pass's own commit sweep never reached the failing shard, so this proves nothing about a failed commit")
	}
	if after := coldLogSize(t, s); after >= before {
		t.Fatalf("a failed shard commit blocked the whole pass: cold log %d -> %d bytes", before, after)
	}
	kept, _, err := loadCold(s.dir, s.log)
	if err != nil {
		t.Fatalf("loadCold: %v", err)
	}
	found := false
	for _, e := range kept {
		if e.id == failID && e.fileOff == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the entry of the shard that could not commit was dropped; log now holds %d entries", len(kept))
	}
}

// twoShardIDs returns two file IDs that hash to different shards.
func twoShardIDs(t *testing.T, s *Store) (FileID, FileID) {
	t.Helper()
	first := FileID("f0")
	for i := 1; i < 200; i++ {
		id := FileID(fmt.Sprintf("f%d", i))
		if s.shardIndex(id) != s.shardIndex(first) {
			return first, id
		}
	}
	t.Fatal("no two of 200 file IDs landed in different shards")
	return "", ""
}
