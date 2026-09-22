package journal

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestEvictCarriesMarkersOutOfAMixedSegment is the regression for a delete that
// came back.
//
// A tombstone lands in whatever segment is active, so in any shard that is not
// idle it shares that segment with data records. seg.records counts only
// payload-bearing records — markers never raise it, on the append path, the
// recovery replay or the repack carry-forward — so a segment holding one synced
// data record plus a tombstone reports 1 == 1, reads as fully durable, and is
// evicted with the delete's only trace inside it.
//
// The victim's own data segment is typically still dirty and therefore not
// evictable, so the data outlives the marker that buried it and the next
// recovery replays it. That shape is what this builds: the data segment stays
// unsynced, only the mixed segment is marked synced.
//
// Unlike TestEvictKeepsMarkerOnlySegment, nothing here is contrived to make the
// marker rotate into a segment of its own. This is the ordinary layout.
func TestEvictCarriesMarkersOutOfAMixedSegment(t *testing.T) {
	const victim, filler = FileID("aa"), FileID("bb")
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	// A payload that fills a fresh segment exactly, so the next write rotates.
	full := func(id FileID) []byte {
		p := bytes.Repeat([]byte("resurrect"), 1+int(s.cfg.SegmentSize)/9)
		return p[:s.cfg.SegmentSize-segHeaderSize-recordLen(len(id), 0)]
	}

	// Segment 0: the victim's data. Left unsynced, so it is never evictable and
	// the bytes outlive any marker that buries them.
	if err := s.WriteAt(ctx, victim, 0, full(victim)); err != nil {
		t.Fatalf("WriteAt %q: %v", victim, err)
	}
	if err := s.Commit(ctx, victim); err != nil {
		t.Fatalf("Commit %q: %v", victim, err)
	}

	// Segment 1: a filler data record, then the victim's tombstone beside it.
	// This is the mixed segment — a record AND a marker.
	if err := s.WriteAt(ctx, filler, 0, []byte("keepme")); err != nil {
		t.Fatalf("WriteAt %q: %v", filler, err)
	}
	if err := s.Commit(ctx, filler); err != nil {
		t.Fatalf("Commit %q: %v", filler, err)
	}
	if err := s.Delete(ctx, victim); err != nil {
		t.Fatalf("Delete %q: %v", victim, err)
	}

	// Rotate so the mixed segment seals: eviction scans only the sealed set.
	if err := s.WriteAt(ctx, FileID("cc"), 0, full(FileID("cc"))); err != nil {
		t.Fatalf("WriteAt cc: %v", err)
	}

	// Mark ONLY the mixed segment synced. Marking every segment would make the
	// victim's data evictable too, and its eviction would hide the bug by
	// removing the records the lost marker was burying.
	sh := s.shardFor(victim)
	sh.mu.Lock()
	mixed := 0
	for _, seg := range sh.sealed {
		markers, err := victimMarkers(seg, s.cfg.SegmentSize)
		if err != nil {
			sh.mu.Unlock()
			t.Fatalf("victimMarkers(seg %d): %v", seg.id, err)
		}
		if len(markers) > 0 && seg.records.Load() > 0 {
			seg.syncedRecords.Store(seg.records.Load())
			mixed++
		}
	}
	sh.mu.Unlock()

	// Setup guard: without a sealed segment holding BOTH a record and a marker
	// the eviction below cannot exercise the hazard, and a green result would
	// mean nothing.
	if mixed != 1 {
		t.Fatalf("setup no longer builds one mixed data+marker sealed segment: got %d", mixed)
	}

	if _, err := s.Evict(ctx, s.cfg.SegmentSize); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	r := reopen(t, s, Config{})
	if size, ok := r.FileSize(ctx, victim); ok {
		t.Fatalf("deleted file %q came back after reopen with size %d: the tombstone was evicted with the segment that carried it", victim, size)
	}
}

// TestReclaimEmptiedCarriesMarkersOfAnotherFile covers evictable's second
// caller. reclaimEmptied runs at the end of every Delete and retires segments a
// tombstone just emptied — it reaches retireSegment by a different route than
// eviction, so it needs its own carry-forward and its own test.
//
// The shape is a segment whose own payload is entirely dead but which carries a
// DIFFERENT file's tombstone. Reclaiming it for the dead payload drops that
// tombstone, and the other file comes back.
func TestReclaimEmptiedCarriesMarkersOfAnotherFile(t *testing.T) {
	const victim, dead, filler = FileID("aa"), FileID("bb"), FileID("cc")
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	full := func(id FileID) []byte {
		p := bytes.Repeat([]byte("resurrect"), 1+int(s.cfg.SegmentSize)/9)
		return p[:s.cfg.SegmentSize-segHeaderSize-recordLen(len(id), 0)]
	}

	// Segment 0: the victim's data, left unsynced so nothing ever retires it.
	if err := s.WriteAt(ctx, victim, 0, full(victim)); err != nil {
		t.Fatalf("WriteAt %q: %v", victim, err)
	}
	if err := s.Commit(ctx, victim); err != nil {
		t.Fatalf("Commit %q: %v", victim, err)
	}

	// Segment 1: dead's data, then the victim's tombstone beside it.
	if err := s.WriteAt(ctx, dead, 0, []byte("short-lived")); err != nil {
		t.Fatalf("WriteAt %q: %v", dead, err)
	}
	if err := s.Commit(ctx, dead); err != nil {
		t.Fatalf("Commit %q: %v", dead, err)
	}
	if err := s.Delete(ctx, victim); err != nil {
		t.Fatalf("Delete %q: %v", victim, err)
	}

	// Rotate so segment 1 seals, then mark only it synced.
	if err := s.WriteAt(ctx, filler, 0, full(filler)); err != nil {
		t.Fatalf("WriteAt %q: %v", filler, err)
	}
	sh := s.shardFor(victim)
	sh.mu.Lock()
	mixed := 0
	for _, seg := range sh.sealed {
		markers, err := victimMarkers(seg, s.cfg.SegmentSize)
		if err != nil {
			sh.mu.Unlock()
			t.Fatalf("victimMarkers(seg %d): %v", seg.id, err)
		}
		if len(markers) > 0 && seg.records.Load() > 0 {
			seg.syncedRecords.Store(seg.records.Load())
			mixed++
		}
	}
	sh.mu.Unlock()
	if mixed != 1 {
		t.Fatalf("setup no longer builds one mixed data+marker sealed segment: got %d", mixed)
	}

	// Deleting dead empties segment 1 of live payload, so the reclaim at the end
	// of this Delete retires it — with the victim's tombstone inside.
	if err := s.Delete(ctx, dead); err != nil {
		t.Fatalf("Delete %q: %v", dead, err)
	}

	r := reopen(t, s, Config{})
	if size, ok := r.FileSize(ctx, victim); ok {
		t.Fatalf("deleted file %q came back after reopen with size %d: reclaimEmptied dropped the segment carrying its tombstone", victim, size)
	}
}

// syncOnlyMixedSegment marks every sealed segment holding BOTH a data record and
// a marker as fully synced, and fails unless exactly one segment has that shape.
// Marking every segment instead would make the data segment the markers bury
// evictable too, and its eviction would hide the bug by removing the very
// records a lost marker was burying. Returns the mixed segment.
func syncOnlyMixedSegment(t *testing.T, s *Store, id FileID) *segmentMeta {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	var mixed *segmentMeta
	n := 0
	for _, seg := range sh.sealed {
		// Read the markers off disk rather than from seg.markers: the counter is
		// itself under test here, and a setup keyed on it would fail before the
		// assertion that shows what a wrong count costs.
		markers, err := victimMarkers(seg, s.cfg.SegmentSize)
		if err != nil {
			t.Fatalf("victimMarkers(seg %d): %v", seg.id, err)
		}
		if len(markers) > 0 && seg.records.Load() > 0 {
			seg.syncedRecords.Store(seg.records.Load())
			mixed, n = seg, n+1
		}
	}
	if n != 1 {
		t.Fatalf("setup no longer builds one mixed data+marker sealed segment: got %d", n)
	}
	return mixed
}

// fillSegment returns a payload that fills a fresh segment exactly, so the next
// write rotates.
func fillSegment(s *Store, id FileID) []byte {
	p := bytes.Repeat([]byte("carryfwd"), 1+int(s.cfg.SegmentSize)/8)
	return p[:s.cfg.SegmentSize-segHeaderSize-recordLen(len(id), 0)]
}

// TestEvictCarriesTruncateMarkerAsATruncate pins that a carried marker keeps its
// KIND. Both markers are zero-payload records distinguished only by a header
// flag, so writing every carried marker as a tombstone costs nothing at the
// write and passes the delete-side regressions: a carried size-down would
// silently become a full file delete.
//
// The clipped tail has to outlive the marker for the assertion to mean anything,
// so the victim's data segment is left unsynced — never evictable — and only the
// segment holding the marker is evicted.
func TestEvictCarriesTruncateMarkerAsATruncate(t *testing.T) {
	const victim, filler = FileID("aa"), FileID("bb")
	const keptSize = 4096
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	// Segment 0: the victim's data, left unsynced.
	if err := s.WriteAt(ctx, victim, 0, fillSegment(s, victim)); err != nil {
		t.Fatalf("WriteAt %q: %v", victim, err)
	}
	if err := s.Commit(ctx, victim); err != nil {
		t.Fatalf("Commit %q: %v", victim, err)
	}

	// Segment 1: a filler data record, then the victim's truncate marker beside it.
	if err := s.WriteAt(ctx, filler, 0, []byte("keepme")); err != nil {
		t.Fatalf("WriteAt %q: %v", filler, err)
	}
	if err := s.Commit(ctx, filler); err != nil {
		t.Fatalf("Commit %q: %v", filler, err)
	}
	if err := s.Truncate(ctx, victim, keptSize); err != nil {
		t.Fatalf("Truncate %q: %v", victim, err)
	}

	// Rotate so the mixed segment seals: eviction scans only the sealed set.
	if err := s.WriteAt(ctx, FileID("cc"), 0, fillSegment(s, FileID("cc"))); err != nil {
		t.Fatalf("WriteAt cc: %v", err)
	}
	syncOnlyMixedSegment(t, s, victim)

	if _, err := s.Evict(ctx, s.cfg.SegmentSize); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	r := reopen(t, s, Config{})
	size, ok := r.FileSize(ctx, victim)
	if !ok {
		t.Fatalf("truncated file %q is gone after reopen: the carried marker buried the whole file instead of clipping it", victim)
	}
	if size != keptSize {
		t.Fatalf("file %q is %d bytes after reopen, want %d: the truncate marker was lost with the segment that carried it", victim, size, keptSize)
	}
}

// TestCarriedTombstoneDoesNotBuryARewrite pins that a carried marker keeps its
// ORIGINAL Version. Recovery folds markers by MAX Version, so minting a fresh
// one at carry time moves the delete ahead of everything written since — the
// re-created file is buried by its own predecessor's tombstone. A carry test
// whose deleted file is never rewritten cannot see this: with nothing above the
// tombstone, any version buries the same set.
func TestCarriedTombstoneDoesNotBuryARewrite(t *testing.T) {
	const target, pad, spacer = FileID("aa"), FileID("bb"), FileID("cc")
	rewritten := []byte("written again after the delete")
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	// Segment 0: padding, so the target's first life starts a segment of its own.
	if err := s.WriteAt(ctx, pad, 0, fillSegment(s, pad)); err != nil {
		t.Fatalf("WriteAt %q: %v", pad, err)
	}
	if err := s.Commit(ctx, pad); err != nil {
		t.Fatalf("Commit %q: %v", pad, err)
	}

	// Segment 1: the target's first life, then the tombstone that ends it. This
	// is the mixed segment the eviction below retires.
	if err := s.WriteAt(ctx, target, 0, []byte("first life")); err != nil {
		t.Fatalf("WriteAt %q: %v", target, err)
	}
	if err := s.Commit(ctx, target); err != nil {
		t.Fatalf("Commit %q: %v", target, err)
	}
	if err := s.Delete(ctx, target); err != nil {
		t.Fatalf("Delete %q: %v", target, err)
	}

	// Segment 2: a spacer, so the rewrite lands in a segment the eviction leaves
	// alone — otherwise the rewrite's own bytes go cold with the marker and the
	// assertion could not tell the two losses apart.
	if err := s.WriteAt(ctx, spacer, 0, fillSegment(s, spacer)); err != nil {
		t.Fatalf("WriteAt %q: %v", spacer, err)
	}

	// Segment 3 (active): the rewrite, at a Version above the tombstone's.
	if err := s.WriteAt(ctx, target, 0, rewritten); err != nil {
		t.Fatalf("rewrite %q: %v", target, err)
	}
	if err := s.Commit(ctx, target); err != nil {
		t.Fatalf("Commit rewritten %q: %v", target, err)
	}

	syncOnlyMixedSegment(t, s, target)
	if _, err := s.Evict(ctx, s.cfg.SegmentSize); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	r := reopen(t, s, Config{})
	size, ok := r.FileSize(ctx, target)
	if !ok {
		t.Fatalf("rewritten file %q is gone after reopen: the carried tombstone was given a fresh Version and buried a write that came after the delete", target)
	}
	if size != int64(len(rewritten)) {
		t.Fatalf("rewritten file %q is %d bytes after reopen, want %d", target, size, len(rewritten))
	}
	if got := readAll(t, r, target, len(rewritten)); !bytes.Equal(got, rewritten) {
		t.Fatalf("rewritten file %q reads %q after reopen, want %q", target, got, rewritten)
	}
}

// TestSegmentMarkerCountSurvivesReopen pins the counter the retire paths consult
// before deciding a segment is worth scanning. The counter is in-memory only, so
// a recovery that did not rebuild it would leave every pre-restart segment
// claiming it holds no marker — and the first eviction after a restart would
// skip the scan and unlink the marker with the segment.
func TestSegmentMarkerCountSurvivesReopen(t *testing.T) {
	const victim, filler = FileID("aa"), FileID("bb")
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	if err := s.WriteAt(ctx, victim, 0, fillSegment(s, victim)); err != nil {
		t.Fatalf("WriteAt %q: %v", victim, err)
	}
	if err := s.Commit(ctx, victim); err != nil {
		t.Fatalf("Commit %q: %v", victim, err)
	}
	if err := s.WriteAt(ctx, filler, 0, []byte("keepme")); err != nil {
		t.Fatalf("WriteAt %q: %v", filler, err)
	}
	if err := s.Commit(ctx, filler); err != nil {
		t.Fatalf("Commit %q: %v", filler, err)
	}
	if err := s.Delete(ctx, victim); err != nil {
		t.Fatalf("Delete %q: %v", victim, err)
	}
	if err := s.WriteAt(ctx, FileID("cc"), 0, fillSegment(s, FileID("cc"))); err != nil {
		t.Fatalf("WriteAt cc: %v", err)
	}

	want := map[uint64]int64{}
	sh := s.shardFor(victim)
	sh.mu.Lock()
	for id, seg := range sh.sealed {
		want[id] = seg.markers.Load()
	}
	sh.mu.Unlock()
	if want[1] == 0 {
		t.Fatalf("setup no longer puts a marker in a sealed segment: %v", want)
	}

	r := reopen(t, s, Config{})
	rsh := r.shardFor(victim)
	rsh.mu.Lock()
	got := map[uint64]int64{}
	for id, seg := range rsh.sealed {
		got[id] = seg.markers.Load()
	}
	rsh.mu.Unlock()
	for id, n := range want {
		if got[id] != n {
			t.Fatalf("segment %d reports %d markers after reopen, want %d: recovery did not rebuild the counter, so the next retire skips the marker scan", id, got[id], n)
		}
	}

	// And the consumer: an eviction driven by the rebuilt counter still carries
	// the marker out before unlinking.
	syncOnlyMixedSegment(t, r, victim)
	if _, err := r.Evict(ctx, r.cfg.SegmentSize); err != nil {
		t.Fatalf("Evict after reopen: %v", err)
	}
	r2 := reopen(t, r, Config{})
	if size, ok := r2.FileSize(ctx, victim); ok {
		t.Fatalf("deleted file %q came back with size %d after a post-restart eviction", victim, size)
	}
}

// TestTornSegmentDoesNotWedgeEviction pins that one damaged segment costs its own
// reclamation and nothing more. The marker scan a retire needs reports
// errTornRecord when the record stream stops short; propagating that aborted the
// whole pass, and because the defer cleared busy while evictable still accepted
// the segment, the next claim picked the same coldest segment again — the local
// cap could never be relieved and ensureSpace handed the error to every writer.
func TestTornSegmentDoesNotWedgeEviction(t *testing.T) {
	const victim, filler = FileID("aa"), FileID("bb")
	s := testStore(t, Config{ShardCount: 1, SegmentSize: minSegmentSize})
	ctx := context.Background()

	if err := s.WriteAt(ctx, victim, 0, fillSegment(s, victim)); err != nil {
		t.Fatalf("WriteAt %q: %v", victim, err)
	}
	if err := s.Commit(ctx, victim); err != nil {
		t.Fatalf("Commit %q: %v", victim, err)
	}
	if err := s.WriteAt(ctx, filler, 0, []byte("keepme")); err != nil {
		t.Fatalf("WriteAt %q: %v", filler, err)
	}
	if err := s.Commit(ctx, filler); err != nil {
		t.Fatalf("Commit %q: %v", filler, err)
	}
	if err := s.Delete(ctx, victim); err != nil {
		t.Fatalf("Delete %q: %v", victim, err)
	}
	if err := s.WriteAt(ctx, FileID("cc"), 0, fillSegment(s, FileID("cc"))); err != nil {
		t.Fatalf("WriteAt cc: %v", err)
	}
	mixed := syncOnlyMixedSegment(t, s, victim)

	// Damage the segment's first record so the scan stops at the segment header,
	// short of the tail — the shape a half-written record leaves behind.
	if _, err := mixed.fd.WriteAt(bytes.Repeat([]byte{0xFF}, 64), segHeaderSize); err != nil {
		t.Fatalf("corrupt segment %d: %v", mixed.id, err)
	}

	res, err := s.Evict(ctx, s.cfg.SegmentSize)
	if err != nil {
		t.Fatalf("Evict aborted on a torn segment: %v — one damaged segment must not fail every writer's capacity gate", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Fatalf("Evict reclaimed %d segments, want 0: the torn segment must keep its bytes", res.SegmentsEvicted)
	}
	if !mixed.corrupt.Load() {
		t.Fatalf("segment %d was not quarantined, so every later pass re-picks it", mixed.id)
	}
	if _, err := os.Stat(s.segPath(mixed.id)); err != nil {
		t.Fatalf("torn segment %d was unlinked anyway: %v", mixed.id, err)
	}

	// A second pass must not re-claim it. Without the quarantine this call never
	// returns: the segment is still the coldest evictable one every round.
	if _, err := s.Evict(ctx, s.cfg.SegmentSize); err != nil {
		t.Fatalf("second Evict: %v", err)
	}
}
