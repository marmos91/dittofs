package journal

import (
	"context"
	"testing"
)

// TestOpenSetsDefaultLocalCapWhenUnset guards the root cause of the disk-full
// wedge: an unset MaxLocalBytes used to leave the write-path capacity gate a
// permanent no-op, so dead records were never reclaimed and disk usage grew
// without bound. Open must now derive a soft cap from the volume's free space.
func TestOpenSetsDefaultLocalCapWhenUnset(t *testing.T) {
	dir := t.TempDir()
	// The default is only expected when free space is observable. On a platform
	// where the statfs probe fails Open deliberately leaves the cap unset, so
	// gate the assertion on the same probe succeeding here.
	if free, err := diskFreeBytes(dir); err != nil || free == 0 {
		t.Skipf("free-space probe unavailable (free=%d err=%v); default cap not expected", free, err)
	}
	s, err := Open(dir, Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if s.cfg.MaxLocalBytes <= 0 {
		t.Fatalf("expected Open to default MaxLocalBytes from free space, got %d", s.cfg.MaxLocalBytes)
	}
}

// TestGCReclaimsDeadOverwrites is the wedge regression test. It overwrites a
// tiny live footprint thousands of times so sealed segments fill with dead
// (superseded) records — the exact shape that grew on-disk bytes without bound
// before the fix — then asserts a GC pass reclaims that dead space, leaving
// disk usage tracking live bytes rather than total writes.
func TestGCReclaimsDeadOverwrites(t *testing.T) {
	// SegmentSize at the 1 MiB floor so overwrites seal many segments; single
	// shard is deterministic; a generous explicit cap keeps eviction/backpressure
	// out of the way so this isolates the dead-ratio repack path.
	const segSize = 1 << 20
	cfg := Config{SegmentSize: segSize, ShardCount: 1, MaxLocalBytes: 1 << 30}
	s, err := Open(t.TempDir(), cfg, newFakeRemote(), SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	const blk = 4096
	const slots = 64 // 256 KiB live footprint, rewritten in place
	data := make([]byte, blk)
	for i := 0; i < 8000; i++ { // ~32 MiB appended for 256 KiB of live data
		off := int64(i%slots) * blk
		if err := s.WriteAt(ctx, "f", off, data); err != nil {
			t.Fatalf("WriteAt #%d: %v", i, err)
		}
	}

	before := s.diskBytes.Load()
	if _, err := s.GC(ctx, GCOptions{}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	after := s.diskBytes.Load()

	if after >= before {
		t.Fatalf("GC reclaimed nothing: before=%d after=%d", before, after)
	}
	// ~32 MiB was appended for 256 KiB of live data. After repack the surviving
	// bytes must track live data (a few segments), not total writes. A handful of
	// segments is a generous bound that still fails hard if dead records are left
	// unreclaimed (the pre-fix unbounded-growth behaviour).
	if bound := int64(4 * segSize); after > bound {
		t.Fatalf("on-disk bytes not bounded after GC: after=%d, want <= %d (before=%d)", after, bound, before)
	}
}
