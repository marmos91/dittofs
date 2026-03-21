package engine

import (
	"context"
	"testing"
)

// TestStats_EmptyStore verifies Stats() returns UsedSize==0 for an empty store.
func TestStats_EmptyStore(t *testing.T) {
	bs := newTestEngine(t, 0, 0)

	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	if stats.UsedSize != 0 {
		t.Fatalf("expected UsedSize==0 for empty store, got %d", stats.UsedSize)
	}
	if stats.ContentCount != 0 {
		t.Fatalf("expected ContentCount==0 for empty store, got %d", stats.ContentCount)
	}
}

// TestStats_UsedSizeMatchesDiskUsed verifies Stats().UsedSize == local.Stats().DiskUsed.
func TestStats_UsedSizeMatchesDiskUsed(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	// Write data to the local store.
	if err := bs.WriteAt(ctx, "stats-test", []byte("some data for stats"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	localStats := bs.local.Stats()
	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	// Verify UsedSize is wired to local DiskUsed.
	if stats.UsedSize != uint64(localStats.DiskUsed) {
		t.Fatalf("UsedSize=%d does not match localStats.DiskUsed=%d", stats.UsedSize, localStats.DiskUsed)
	}

	// Verify ContentCount reflects the file count.
	if stats.ContentCount == 0 {
		t.Fatal("expected ContentCount > 0 after writing data")
	}
}

// TestStats_AvailableSize verifies AvailableSize == TotalSize - UsedSize.
func TestStats_AvailableSize(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	// Write data.
	if err := bs.WriteAt(ctx, "avail-test", []byte("data"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	// When TotalSize > UsedSize, AvailableSize should be the difference.
	if stats.TotalSize > stats.UsedSize {
		expected := stats.TotalSize - stats.UsedSize
		if stats.AvailableSize != expected {
			t.Fatalf("AvailableSize=%d, expected TotalSize(%d) - UsedSize(%d) = %d",
				stats.AvailableSize, stats.TotalSize, stats.UsedSize, expected)
		}
	}

	// When TotalSize <= UsedSize, AvailableSize should be 0.
	// (Memory store has TotalSize=0 and UsedSize=0, so AvailableSize=0 is correct)
	if stats.TotalSize <= stats.UsedSize && stats.AvailableSize != 0 {
		t.Fatalf("expected AvailableSize==0 when TotalSize(%d) <= UsedSize(%d), got %d",
			stats.TotalSize, stats.UsedSize, stats.AvailableSize)
	}
}

// TestStats_AverageSize verifies AverageSize is computed correctly.
func TestStats_AverageSize(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	// Write data to two files.
	if err := bs.WriteAt(ctx, "avg-1", []byte("data1"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if err := bs.WriteAt(ctx, "avg-2", []byte("data2data2"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats, err := bs.Stats()
	if err != nil {
		t.Fatalf("Stats() failed: %v", err)
	}

	if stats.ContentCount > 0 && stats.UsedSize > 0 {
		expected := stats.UsedSize / stats.ContentCount
		if stats.AverageSize != expected {
			t.Fatalf("AverageSize=%d, expected UsedSize(%d) / ContentCount(%d) = %d",
				stats.AverageSize, stats.UsedSize, stats.ContentCount, expected)
		}
	}

	// When ContentCount == 0, AverageSize should be 0 (tested by empty store test).
}
