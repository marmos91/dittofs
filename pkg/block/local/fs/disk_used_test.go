package fs

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
)

// segmentBytesOnDisk sums the physical size of every journal segment file under
// dir — the footprint an operator sees with du, and what DiskUsed must report.
func segmentBytesOnDisk(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".seg" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", dir, err)
	}
	if total == 0 {
		t.Fatalf("fixture wrote no segment bytes under %q", dir)
	}
	return total
}

// A store opened over blocks a previous process wrote must report the footprint
// that is actually on disk. An incrementally-maintained counter that only ever
// saw this session's writes would report a fraction of it, leaving the evictor
// convinced there is headroom while the volume fills.
func TestStats_DiskUsedReportsPreExistingFootprint(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// First process: write a payload, then overwrite part of it so the store
	// holds superseded records too — payload-length accounting charges those
	// twice (once live, once dead) and so cannot stand in for the footprint.
	first, err := New(dir, 0, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	data := make([]byte, 1<<20)
	for i := range 4 {
		if err := first.WriteAt(ctx, "payload-a", int64(i)<<20, data); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}
	for range 3 {
		if err := first.WriteAt(ctx, "payload-a", 0, data); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
	}
	if err := first.Commit(ctx, "payload-a"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := segmentBytesOnDisk(t, dir)

	// Second process: opens the same directory and writes nothing.
	second, err := New(dir, 0, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()

	if got := second.Stats().DiskUsed; got != want {
		t.Fatalf("DiskUsed after fresh open = %d, want %d (bytes physically on disk)", got, want)
	}
}
