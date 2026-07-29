package journal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	_, cold, err := s.ReadAt(ctx, "f", 0, dst)
	if err != nil {
		t.Fatalf("ReadAt before reopen: %v", err)
	}
	if !cold {
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
	n, cold, err := s2.ReadAt(ctx, "f", 0, dst2)
	if err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	}
	if !cold {
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
	if _, cold, err := s2.ReadAt(ctx, "f", 0, dst); err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	} else if !cold {
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
	_, cold, err := s2.ReadAt(ctx, "f", 0, got)
	if err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	}
	if cold {
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
	if _, cold, err := s2.ReadAt(ctx, "f", 0, dst); err != nil {
		t.Fatalf("ReadAt: %v", err)
	} else if !cold {
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
	_, cold, err := s.ReadAt(ctx, "f", 4096, got)
	if err != nil {
		t.Fatalf("ReadAt over the written range: %v", err)
	}
	if cold {
		t.Error("the seed shadowed a range the journal holds locally; the read now needs a remote copy that may not exist")
	}
	if !bytes.Equal(got, want) {
		t.Error("the seed replaced locally-written bytes")
	}
	// The ranges on either side had nothing local, so they must be cold.
	for _, off := range []int64{0, 8192} {
		if _, cold, err := s.ReadAt(ctx, "f", off, make([]byte, 4096)); err != nil {
			t.Fatalf("ReadAt at %d: %v", off, err)
		} else if !cold {
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
