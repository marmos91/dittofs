package journal

import (
	"bytes"
	"context"
	"testing"
)

// zeroFilled reports whether every byte in b is zero.
func zeroFilled(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func TestTruncateShrinksFileSizeAndExtents(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	data := randBytes(1000, 1)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "f", 400); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	if sz, ok := s.FileSize(ctx, "f"); !ok || sz != 400 {
		t.Fatalf("FileSize after truncate = (%d,%v), want (400,true)", sz, ok)
	}
	ext, err := s.DataExtents(ctx, "f", 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 1 || ext[0] != [2]uint64{0, 400} {
		t.Fatalf("DataExtents = %v, want [{0 400}]", ext)
	}

	// Read across the new EOF: kept bytes match, bytes past newSize zero-fill.
	got := make([]byte, 600)
	for i := range got {
		got[i] = 0xFF
	}
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[:400], data[:400]) {
		t.Fatalf("kept bytes mismatch after truncate")
	}
	if !zeroFilled(got[400:]) {
		t.Fatalf("bytes past newSize not zero-filled: %x", got[400:])
	}
}

func TestTruncatePartialIntervalClip(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	// A single record straddling newSize must be clipped (not dropped) and its
	// tail charged as dead bytes to its segment.
	data := randBytes(1000, 2)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "f", 400); err != nil {
		t.Fatal(err)
	}

	sh := s.shardFor("f")
	sh.mu.Lock()
	fi := sh.index["f"]
	nivs := len(fi.ivs)
	iv := fi.ivs[0]
	dead := sh.segment(iv.loc.SegmentID).deadBytes.Load()
	sh.mu.Unlock()

	if nivs != 1 || iv.fileOff != 0 || iv.length != 400 {
		t.Fatalf("clipped interval = {off:%d len:%d} (ivs=%d), want {0 400}", iv.fileOff, iv.length, nivs)
	}
	if dead != 600 {
		t.Fatalf("dead bytes after clip = %d, want 600", dead)
	}
}

func TestTruncateGrowIsNoOp(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, randBytes(100, 3)); err != nil {
		t.Fatal(err)
	}
	before := s.UnsyncedBytes()
	if err := s.Truncate(ctx, "f", 500); err != nil { // grow past the high-water mark
		t.Fatalf("Truncate grow: %v", err)
	}
	if sz, _ := s.FileSize(ctx, "f"); sz != 100 {
		t.Fatalf("grow-truncate changed FileSize to %d, want 100", sz)
	}
	if u := s.UnsyncedBytes(); u != before {
		t.Fatalf("grow-truncate changed unsynced %d -> %d", before, u)
	}
}

func TestTruncateVersionFenceWriteAfterSurvives(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, randBytes(1000, 4)); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "f", 400); err != nil {
		t.Fatal(err)
	}
	// A write past newSize AFTER the truncate carries a higher Version and must
	// re-extend the file.
	tail := randBytes(100, 5)
	if err := s.WriteAt(ctx, "f", 500, tail); err != nil {
		t.Fatal(err)
	}
	if sz, _ := s.FileSize(ctx, "f"); sz != 600 {
		t.Fatalf("FileSize after post-truncate write = %d, want 600", sz)
	}
	got := make([]byte, 100)
	if _, _, err := s.ReadAt(ctx, "f", 500, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, tail) {
		t.Fatalf("post-truncate write not readable")
	}
}

func TestTruncateFailureLeavesStateIntact(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	data := randBytes(1000, 6)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	before := s.UnsyncedBytes()

	testFailTruncate = "f"
	defer func() { testFailTruncate = "" }()
	if err := s.Truncate(ctx, "f", 400); err == nil {
		t.Fatalf("expected injected truncate failure")
	}

	// A failed truncate must not mutate the index or the counters.
	if sz, _ := s.FileSize(ctx, "f"); sz != 1000 {
		t.Fatalf("failed truncate changed FileSize to %d, want 1000", sz)
	}
	if u := s.UnsyncedBytes(); u != before {
		t.Fatalf("failed truncate changed unsynced %d -> %d", before, u)
	}
	got := make([]byte, 1000)
	if _, _, err := s.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("failed truncate corrupted data")
	}
}

func TestTruncateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data := randBytes(1000, 7)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "f", 400); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Recovery replays the still-full on-disk record, then re-applies the durable
	// truncate marker so the truncated bytes never resurrect.
	r, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = r.Close() }()

	if sz, ok := r.FileSize(ctx, "f"); !ok || sz != 400 {
		t.Fatalf("recovered FileSize = (%d,%v), want (400,true)", sz, ok)
	}
	got := make([]byte, 600)
	for i := range got {
		got[i] = 0xFF
	}
	if _, _, err := r.ReadAt(ctx, "f", 0, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:400], data[:400]) {
		t.Fatalf("recovered kept bytes mismatch")
	}
	if !zeroFilled(got[400:]) {
		t.Fatalf("recovered bytes past newSize not zero: %x", got[400:])
	}
}

// TestTruncateDeadBytesReconstructedOnReopen guards the recovery deadBytes
// rebuild: replay charges only physical records, so a truncate's clipped tail
// leaves dead payload the counter never saw. Without reconstruction, pickVictim's
// deadBytes<=0 gate skips the segment forever and GC can't reclaim the space.
func TestTruncateDeadBytesReconstructedOnReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.WriteAt(ctx, "f", 0, randBytes(1000, 9)); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "f", 400); err != nil {
		t.Fatal(err)
	}
	if d := s.Stats().DeadBytes; d != 600 {
		t.Fatalf("pre-reopen DeadBytes = %d, want 600", d)
	}

	r := reopen(t, s)
	if d := r.Stats().DeadBytes; d != 600 {
		t.Fatalf("recovered DeadBytes = %d, want 600 (deadBytes not reconstructed)", d)
	}
}

func TestVictimMarkersCollectsTruncate(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()

	// Write then truncate so the active segment carries a truncate marker.
	if err := s.WriteAt(ctx, "f", 0, randBytes(1000, 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.appendTruncateMarker(ctx, "f", 400); err != nil {
		t.Fatalf("appendTruncateMarker: %v", err)
	}
	sh := s.shardFor("f")
	sh.mu.Lock()
	seg := sh.active
	sh.mu.Unlock()

	markers := victimMarkers(seg, s.cfg.SegmentSize)
	var found bool
	for _, m := range markers {
		if m.flags&flagTruncate != 0 && m.id == "f" {
			found = true
			if m.newSize != 400 {
				t.Fatalf("carried truncate newSize = %d, want 400", m.newSize)
			}
		}
	}
	if !found {
		t.Fatalf("victimMarkers did not collect the truncate marker: %+v", markers)
	}
}
