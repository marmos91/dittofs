package journal

import (
	"context"
	"errors"
	"os"
	"testing"
)

// flipSegByte inverts one byte of a segment file in place, simulating on-disk
// bit rot of an otherwise valid record. It writes through a fresh fd; the
// store's own fd sees the change via the shared page cache.
func flipSegByte(t *testing.T, s *Store, segID uint64, off int64) {
	t.Helper()
	f, err := os.OpenFile(s.segPath(segID), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open segment %d: %v", segID, err)
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatalf("read segment byte: %v", err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatalf("write corrupt byte: %v", err)
	}
}

// firstInterval returns a copy of id's first live interval.
func firstInterval(t *testing.T, s *Store, id FileID) interval {
	t.Helper()
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil || len(fi.ivs) == 0 {
		t.Fatalf("no interval for %q", id)
	}
	return fi.ivs[0]
}

// segFileCount counts the .seg files currently on disk.
func segFileCount(t *testing.T, s *Store) int {
	t.Helper()
	ids, err := scanSegmentIDs(s.dir)
	if err != nil {
		t.Fatalf("scanSegmentIDs: %v", err)
	}
	return len(ids)
}

// TestCarveRefusesCorruptedRecord asserts that carve fails closed on a dirty
// record whose payload rotted on disk instead of hashing those bytes and
// committing them to the remote store as genuine content — which would make the
// corrupt copy the authoritative one, under a BLAKE3 hash that matches it.
func TestCarveRefusesCorruptedRecord(t *testing.T) {
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: 1 << 20})
	ctx := context.Background()

	data := randBytes(2<<20, 7)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	corruptFirstPayloadByte(t, s, "f")

	_, err := s.Carve(ctx, CarveOptions{Force: true})
	var cre *CorruptRangeError
	if !errors.As(err, &cre) {
		t.Fatalf("Carve must refuse a corrupt record, got err=%v", err)
	}
	if cre.FileID != "f" {
		t.Fatalf("CorruptRangeError names wrong file: %+v", cre)
	}
	if got := sink.carved(); len(got) != 0 {
		t.Fatalf("carve uploaded %d bytes from a corrupt record", len(got))
	}
	// The run aborted with the bytes still dirty and still local: nothing was
	// marked durable, so the failure is recoverable rather than silently absorbed.
	if u := s.UnsyncedBytes(); u != int64(len(data)) {
		t.Fatalf("unsynced after refused carve = %d, want %d", u, len(data))
	}
	if f := recRawFlags(t, s, "f", 0); f&flagSynced != 0 {
		t.Fatalf("refused carve still flipped the record synced: flags=%#x", f)
	}
}

// TestRepackRefusesTruncatedRecordStream asserts that a repack whose victim
// carries a record that fails its CRC is abandoned. Copying the survivors
// forward would stamp a fresh CRC over rotted bytes, and the marker scan stops
// at the bad record, so every tombstone and truncate marker behind it would be
// dropped when the victim is reclaimed.
func TestRepackRefusesTruncatedRecordStream(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	seedRepackable(t, s, true)

	victim := onlySealed(t, s, "keep")
	corruptFirstPayloadByte(t, s, "keep")
	segsBefore := segFileCount(t, s)

	res, err := s.GC(ctx, GCOptions{Force: true})
	if !errors.Is(err, errTornRecord) {
		t.Fatalf("GC must refuse to repack a victim with a torn record, got err=%v res=%+v", err, res)
	}
	if res.SegmentsRepacked != 0 {
		t.Fatalf("GC reported %d repacks despite refusing", res.SegmentsRepacked)
	}
	// The victim keeps its bytes and its place: nothing is dropped or rewritten,
	// and the half-built target is cleaned up rather than left behind.
	sh := s.shardFor("keep")
	sh.mu.Lock()
	_, stillSealed := sh.sealed[victim.id]
	sh.mu.Unlock()
	if !stillSealed {
		t.Fatalf("refused repack dropped the victim segment")
	}
	if n := segFileCount(t, s); n != segsBefore {
		t.Fatalf("segment count %d, want %d (target not cleaned up)", n, segsBefore)
	}
	if !victim.corrupt.Load() {
		t.Fatalf("victim not quarantined after a failed integrity check")
	}
	// Quarantined: a later pass skips it, so one damaged segment cannot stall
	// reclamation for the rest of the shard.
	if res, err := s.GC(ctx, GCOptions{Force: true}); err != nil || res.SegmentsRepacked != 0 {
		t.Fatalf("second GC pass: err=%v res=%+v, want a clean no-op", err, res)
	}
}

// TestRepackRefusesRecordFramingAnotherFile asserts the repack checks which file
// a record frames, not just its CRCs. Neither CRC covers the FileID bytes, so a
// flipped FileID leaves the record stream fully scannable and would otherwise
// copy one file's payload forward under another file's identity.
func TestRepackRefusesRecordFramingAnotherFile(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	seedRepackable(t, s, true)

	victim := onlySealed(t, s, "keep")
	iv := firstInterval(t, s, "keep")
	flipSegByte(t, s, iv.loc.SegmentID, iv.recOff+recordHeaderSize) // first FileID byte
	segsBefore := segFileCount(t, s)

	res, err := s.GC(ctx, GCOptions{Force: true})
	if !errors.Is(err, errTornRecord) {
		t.Fatalf("GC must refuse a record framing another file, got err=%v res=%+v", err, res)
	}
	if n := segFileCount(t, s); n != segsBefore {
		t.Fatalf("segment count %d, want %d (target not cleaned up)", n, segsBefore)
	}
	if !victim.corrupt.Load() {
		t.Fatalf("victim not quarantined after a failed integrity check")
	}
}
