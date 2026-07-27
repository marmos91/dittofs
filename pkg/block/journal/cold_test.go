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
