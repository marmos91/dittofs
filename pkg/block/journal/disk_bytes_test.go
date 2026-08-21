package journal

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// segFileBytes sums the physical size of every .seg file under dir.
//
// The size is read from an open handle rather than from the directory entry:
// the store appends to its active segment without flushing, and a directory
// entry's cached size may lag those appends until the file is closed, so
// walking the entries would under-count the segment still being written.
func segFileBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".seg" {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer func() { _ = f.Close() }()
		info, ierr := f.Stat()
		if ierr != nil {
			return ierr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", dir, err)
	}
	return total
}

// Stats().DiskBytes must equal the segment bytes on disk after a reopen, and a
// store whose seeded footprint already exceeds its cap must evict down to it on
// the next write rather than refuse the write or spin without reclaiming.
func TestDiskBytesSeededOverCapConvergesDown(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cfg := Config{ShardCount: 1, SegmentSize: minSegmentSize}

	first, err := Open(dir, cfg, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Hydrate writes land synced, so every sealed segment is evictable after the
	// reopen without a carve pass.
	buf := bytes.Repeat([]byte{0xCD}, chunk256)
	for i := range 24 {
		if err := first.Hydrate(ctx, "f", int64(i)*chunk256, buf, 0); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	onDisk := segFileBytes(t, dir)
	// Cap well below the footprint the previous run left behind.
	capBytes := onDisk / 4
	reopened := Config{ShardCount: 1, SegmentSize: minSegmentSize, MaxLocalBytes: capBytes}

	s, err := Open(dir, reopened, newFakeRemote(), newFakeClock())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := s.Stats().DiskBytes; got != onDisk {
		t.Fatalf("DiskBytes after reopen = %d, want %d (bytes physically on disk)", got, onDisk)
	}
	if onDisk <= capBytes {
		t.Fatalf("fixture is not over cap: on disk %d, cap %d", onDisk, capBytes)
	}

	// The write-path gate now sees the real footprint and reclaims down to the
	// cap instead of concluding there is headroom.
	if err := s.Hydrate(ctx, "f", 64<<20, buf, 0); err != nil {
		t.Fatalf("write over seeded cap: %v", err)
	}
	// Eviction is whole-segment and admission does not reserve, so the cap is a
	// pressure threshold: allow one segment of overshoot.
	if got, limit := s.Stats().DiskBytes, capBytes+minSegmentSize; got > limit {
		t.Fatalf("DiskBytes = %d, want <= %d (cap %d plus one segment of overshoot)", got, limit, capBytes)
	}
	if got := s.Stats().DiskBytes; got != segFileBytes(t, dir) {
		t.Fatalf("DiskBytes = %d, drifted from on-disk %d after eviction", got, segFileBytes(t, dir))
	}
}
