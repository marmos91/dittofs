package sqlstat

import "testing"

// TestBuild verifies the shared assembler computes used / available from the
// scanned aggregate and reports the unlimited ceilings.
func TestBuild(t *testing.T) {
	stats := Build(4096, 3)

	if stats.UsedBytes != 4096 {
		t.Errorf("UsedBytes = %d, want 4096", stats.UsedBytes)
	}
	if stats.UsedFiles != 3 {
		t.Errorf("UsedFiles = %d, want 3", stats.UsedFiles)
	}
	if stats.TotalBytes != TotalBytes {
		t.Errorf("TotalBytes = %d, want %d", stats.TotalBytes, TotalBytes)
	}
	if stats.TotalFiles != TotalFiles {
		t.Errorf("TotalFiles = %d, want %d", stats.TotalFiles, TotalFiles)
	}
	if stats.AvailableBytes != TotalBytes-4096 {
		t.Errorf("AvailableBytes = %d, want %d", stats.AvailableBytes, TotalBytes-4096)
	}
	if stats.AvailableFiles != TotalFiles-3 {
		t.Errorf("AvailableFiles = %d, want %d", stats.AvailableFiles, TotalFiles-3)
	}
}

// TestBuild_ZeroUsage covers the empty-share case: zero usage yields the full
// ceiling as available.
func TestBuild_ZeroUsage(t *testing.T) {
	stats := Build(0, 0)
	if stats.AvailableBytes != TotalBytes {
		t.Errorf("AvailableBytes = %d, want full ceiling %d", stats.AvailableBytes, TotalBytes)
	}
	if stats.AvailableFiles != TotalFiles {
		t.Errorf("AvailableFiles = %d, want full ceiling %d", stats.AvailableFiles, TotalFiles)
	}
}
