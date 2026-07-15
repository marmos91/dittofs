package journal

import (
	"bytes"
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// onlySealed returns the shard's single sealed segment, failing if there isn't
// exactly one.
func onlySealed(t *testing.T, s *Store, id FileID) *segmentMeta {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if len(sh.sealed) != 1 {
		t.Fatalf("want exactly one sealed segment, got %d", len(sh.sealed))
	}
	for _, seg := range sh.sealed {
		return seg
	}
	return nil
}

func TestDeadBytesOnOverwrite(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, bytes.Repeat([]byte("A"), 10)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 3, []byte("BBBB")); err != nil { // supersedes 4 bytes
		t.Fatal(err)
	}
	sh := s.shardFor("f")
	sh.mu.Lock()
	dead := sh.active.deadBytes.Load()
	sh.mu.Unlock()
	if dead != 4 {
		t.Fatalf("deadBytes = %d, want 4", dead)
	}
}

func TestDeadBytesOnTombstone(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}
	before := s.UnsyncedBytes()
	if before != 100 {
		t.Fatalf("unsynced before delete = %d, want 100", before)
	}
	if err := s.Delete(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	sh := s.shardFor("f")
	sh.mu.Lock()
	dead := sh.active.deadBytes.Load()
	_, stillIndexed := sh.index["f"]
	sh.mu.Unlock()
	if dead != 100 {
		t.Fatalf("deadBytes after delete = %d, want 100", dead)
	}
	if stillIndexed {
		t.Fatalf("file still indexed after delete")
	}
	if u := s.UnsyncedBytes(); u != 0 {
		t.Fatalf("unsynced after delete = %d, want 0", u)
	}
	// The deleted file now reads as all holes.
	got := make([]byte, 100)
	for i := range got {
		got[i] = 0xFF
	}
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, 100)) {
		t.Fatalf("deleted file did not read as holes")
	}
}

// seedRepackable builds a store with one sealed segment holding a synced "keep"
// record and a dirty "gone" record, then deletes "gone" so the sealed segment is
// 70% dead. It returns the keep payload for byte-identity checks.
func seedRepackable(t *testing.T, s *Store) []byte {
	t.Helper()
	ctx := context.Background()
	keep := make([]byte, 300<<10)
	gone := make([]byte, 700<<10)
	rand.New(rand.NewSource(1)).Read(keep)
	rand.New(rand.NewSource(2)).Read(gone)

	if err := s.Hydrate(ctx, "keep", 0, keep); err != nil { // synced=true
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "gone", 0, gone); err != nil { // synced=false
		t.Fatal(err)
	}
	// Roll the active segment over so keep+gone land in a sealed segment.
	if err := s.WriteAt(ctx, "trigger", 0, make([]byte, 200<<10)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	return keep
}

func TestGCForcedRepackPreservesData(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	keep := seedRepackable(t, s)

	victim := onlySealed(t, s, "keep")
	occupied := victim.liveBytes.Load()
	if occupied != int64(len(keep)+700<<10) {
		t.Fatalf("occupied = %d, want %d", occupied, len(keep)+700<<10)
	}
	if dead := victim.deadBytes.Load(); dead != 700<<10 {
		t.Fatalf("deadBytes = %d, want %d", dead, 700<<10)
	}

	// Capture keep's version + synced flag: repack must preserve both.
	sh := s.shardFor("keep")
	sh.mu.Lock()
	preVer := sh.index["keep"].ivs[0].version
	preSynced := sh.index["keep"].ivs[0].synced
	sh.mu.Unlock()

	// 0.7 dead ratio >= default GCDeadRatioForce (0.5): an auto pass repacks it.
	res, err := s.GC(ctx, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.SegmentsRepacked != 1 {
		t.Fatalf("SegmentsRepacked = %d, want 1", res.SegmentsRepacked)
	}
	if res.BytesReclaimed <= 0 {
		t.Fatalf("BytesReclaimed = %d, want > 0", res.BytesReclaimed)
	}

	// keep survives byte-identically; gone stays deleted.
	got := make([]byte, len(keep))
	if _, cold, err := s.ReadAt(ctx, "keep", 0, got); err != nil || cold {
		t.Fatalf("ReadAt keep: err=%v cold=%v", err, cold)
	}
	if !bytes.Equal(got, keep) {
		t.Fatalf("keep not byte-identical after repack")
	}

	sh.mu.Lock()
	iv := sh.index["keep"].ivs[0]
	newTarget := sh.sealed[iv.loc.SegmentID]
	sh.mu.Unlock()
	if iv.version != preVer {
		t.Fatalf("version changed by repack: %d -> %d", preVer, iv.version)
	}
	if iv.synced != preSynced || !iv.synced {
		t.Fatalf("synced flag not preserved: %v", iv.synced)
	}
	if iv.loc.SegmentID == victim.id {
		t.Fatalf("interval still points at the reclaimed victim")
	}
	if newTarget == nil || newTarget.syncedRecords.Load() != 1 {
		t.Fatalf("target syncedRecords not preserved")
	}
}

func TestGCBelowThresholdNeedsForce(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()

	// Sealed segment with a small dead fraction: keep 900KiB, supersede 50KiB.
	big := make([]byte, 900<<10)
	rand.New(rand.NewSource(3)).Read(big)
	if err := s.WriteAt(ctx, "f", 0, big); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "f", 0, make([]byte, 50<<10)); err != nil { // 50KiB dead
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "roll", 0, make([]byte, 200<<10)); err != nil { // seal it
		t.Fatal(err)
	}

	res, err := s.GC(ctx, GCOptions{}) // ~5% dead < 0.5: no-op
	if err != nil {
		t.Fatal(err)
	}
	if res.SegmentsRepacked != 0 {
		t.Fatalf("auto GC repacked below threshold: %d", res.SegmentsRepacked)
	}
	res, err = s.GC(ctx, GCOptions{Force: true}) // Force repacks the worst offender
	if err != nil {
		t.Fatal(err)
	}
	if res.SegmentsRepacked != 1 {
		t.Fatalf("forced GC did not repack: %d", res.SegmentsRepacked)
	}
}

func TestGCCrashBeforeUnlinkOrphanSwept(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SegmentSize: minSegmentSize, ShardCount: 1}
	s, err := Open(dir, cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keep := seedRepackable(t, s)

	// Repack but stop right before reclaiming the victim: on disk the target is
	// durable while the victim still exists (crash-before-unlink).
	testStopBeforeUnlink = true
	if _, err := s.GC(ctx, GCOptions{Force: true}); err != nil {
		testStopBeforeUnlink = false
		t.Fatal(err)
	}
	testStopBeforeUnlink = false

	// Two .seg files exist now: victim + target. Recovery must dedup them.
	ids, _ := scanSegmentIDs(dir)
	segCount := 0
	for range ids {
		segCount++
	}
	if segCount < 2 {
		t.Fatalf("expected victim orphan alongside target, got %d segments", segCount)
	}
	_ = s.Close()

	// Restart: recovery replays both segments (identical Version -> byte-identical).
	r, err := Open(dir, cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("recovery after crash-before-unlink: %v", err)
	}
	defer func() { _ = r.Close() }()

	got := make([]byte, len(keep))
	if _, cold, err := r.ReadAt(ctx, "keep", 0, got); err != nil || cold {
		t.Fatalf("ReadAt keep after recovery: err=%v cold=%v", err, cold)
	}
	if !bytes.Equal(got, keep) {
		t.Fatalf("keep not byte-identical after crash-before-unlink recovery")
	}
	// gone stays deleted (its tombstone survived).
	goneGot := make([]byte, 700<<10)
	for i := range goneGot {
		goneGot[i] = 0xFF
	}
	if _, _, err := r.ReadAt(ctx, "gone", 0, goneGot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goneGot, make([]byte, 700<<10)) {
		t.Fatalf("deleted file resurrected after recovery")
	}

	// The redundant orphan is reclaimable: a forced pass frees it, leaving data intact.
	if _, err := r.GC(ctx, GCOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	got2 := make([]byte, len(keep))
	if _, _, err := r.ReadAt(ctx, "keep", 0, got2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, keep) {
		t.Fatalf("keep corrupted after post-recovery GC")
	}
}

// TestGCTombstoneOnlySegmentTerminates guards pickVictim against an infinite
// repack loop. A fully-dead payload segment that also carries a tombstone gets
// repacked into a tombstone-only segment (0 live records, 0 dead bytes). That
// tombstone-only segment must NOT be re-selected — repacking it would only copy
// the tombstone into another identical segment forever.
func TestGCTombstoneOnlySegmentTerminates(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()

	// "gone" data + its tombstone land in one active segment; "roll" seals it.
	gone := make([]byte, 700<<10)
	rand.New(rand.NewSource(7)).Read(gone)
	if err := s.WriteAt(ctx, "gone", 0, gone); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "gone"); err != nil { // tombstone in the same segment
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "roll", 0, make([]byte, 400<<10)); err != nil { // force seal
		t.Fatal(err)
	}

	// Non-Force GC must terminate. With the bug it repacks the fully-dead segment
	// into a tombstone-only one, then loops forever repacking that. Bound the pass.
	done := make(chan error, 1)
	go func() { _, err := s.GC(ctx, GCOptions{}); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("GC did not terminate: tombstone-only segment repack loop")
	}

	// A second pass finds nothing to reclaim in the tombstone-only segment.
	res, err := s.GC(ctx, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.SegmentsRepacked != 0 {
		t.Fatalf("second GC pass repacked a tombstone-only segment: %d", res.SegmentsRepacked)
	}

	// "gone" stays deleted; "roll" is intact.
	goneGot := make([]byte, 700<<10)
	for i := range goneGot {
		goneGot[i] = 0xFF
	}
	if _, _, err := s.ReadAt(ctx, "gone", 0, goneGot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goneGot, make([]byte, 700<<10)) {
		t.Fatalf("deleted file resurrected across GC")
	}
}

// TestDeleteFailedTombstoneLeavesIndexIntact verifies Delete's durability-first
// ordering: if the tombstone append fails, the in-memory index and counters are
// unchanged, so a failed Delete never makes data disappear.
func TestDeleteFailedTombstoneLeavesIndexIntact(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	want := bytes.Repeat([]byte("x"), 100)
	if err := s.WriteAt(ctx, "f", 0, want); err != nil {
		t.Fatal(err)
	}
	before := s.UnsyncedBytes()

	testFailTombstone = "f"
	err := s.Delete(ctx, "f")
	testFailTombstone = ""
	if err == nil {
		t.Fatal("Delete: want error from failed tombstone append, got nil")
	}

	sh := s.shardFor("f")
	sh.mu.Lock()
	_, stillIndexed := sh.index["f"]
	dead := sh.active.deadBytes.Load()
	sh.mu.Unlock()
	if !stillIndexed {
		t.Fatal("file dropped from index despite failed tombstone append")
	}
	if dead != 0 {
		t.Fatalf("deadBytes charged despite failed tombstone append: %d", dead)
	}
	if u := s.UnsyncedBytes(); u != before {
		t.Fatalf("unsynced changed on failed delete: %d -> %d", before, u)
	}
	// The file still reads back intact.
	got := make([]byte, 100)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("file data corrupted by failed delete")
	}
}

// TestGCConcurrentWithWrites stresses the read/GC/write interplay under -race.
func TestGCConcurrentWithWrites(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 4})
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers churn overlapping ranges (producing dead bytes) across files.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := FileID(string(rune('a' + w)))
			buf := make([]byte, 64<<10)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				off := int64((i % 16) * (64 << 10))
				if err := s.WriteAt(ctx, id, off, buf); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
			}
		}(w)
	}
	// Readers race the GC over the same segments.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			dst := make([]byte, 64<<10)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _, _ = s.ReadAt(ctx, FileID(string(rune('a'+r))), 0, dst)
			}
		}(r)
	}
	// GC loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.GC(ctx, GCOptions{Force: true}); err != nil {
				t.Errorf("GC: %v", err)
				return
			}
		}
	}()

	// Let them run a bounded number of scheduler ticks.
	for i := 0; i < 2000; i++ {
		if err := s.WriteAt(ctx, "driver", int64(i%8)*4096, make([]byte, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
