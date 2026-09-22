package journal

import (
	"bytes"
	"context"
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
