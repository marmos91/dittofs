package offloader

import (
	"runtime"
	"testing"
)

func TestAutoScaleParallelUploads(t *testing.T) {
	n := AutoScaleParallelUploads()

	// Must be at least 16 (floor)
	if n < 16 {
		t.Errorf("AutoScaleParallelUploads() = %d, want >= 16", n)
	}
	// Must be at most 128 (cap)
	if n > 128 {
		t.Errorf("AutoScaleParallelUploads() = %d, want <= 128", n)
	}

	// Verify formula: NumCPU * 4, clamped to [16, 128]
	expected := runtime.NumCPU() * 4
	if expected < 16 {
		expected = 16
	}
	if expected > 128 {
		expected = 128
	}
	if n != expected {
		t.Errorf("AutoScaleParallelUploads() = %d, want %d (NumCPU=%d)", n, expected, runtime.NumCPU())
	}
}

func TestAutoScaleParallelDownloads(t *testing.T) {
	n := AutoScaleParallelDownloads()

	// Must be at least 4 (floor)
	if n < 4 {
		t.Errorf("AutoScaleParallelDownloads() = %d, want >= 4", n)
	}
	// Must be at most 32 (cap)
	if n > 32 {
		t.Errorf("AutoScaleParallelDownloads() = %d, want <= 32", n)
	}

	// Verify formula: NumCPU * 2, clamped to [4, 32]
	expected := runtime.NumCPU() * 2
	if expected < 4 {
		expected = 4
	}
	if expected > 32 {
		expected = 32
	}
	if n != expected {
		t.Errorf("AutoScaleParallelDownloads() = %d, want %d (NumCPU=%d)", n, expected, runtime.NumCPU())
	}
}

func TestAutoScalePrefetchBlocks(t *testing.T) {
	tests := []struct {
		name      string
		cacheSize uint64
		want      int
	}{
		{"zero cache (unlimited)", 0, 8},          // floor
		{"small cache 32MB", 32 * 1024 * 1024, 8}, // 32MB/4MB/4 = 2, floored to 8
		{"1GB cache", 1024 * 1024 * 1024, 64},     // 1GB/4MB/4 = 64
		{"2GB cache", 2 * 1024 * 1024 * 1024, 64}, // capped at 64
		{"256MB cache", 256 * 1024 * 1024, 16},    // 256MB/4MB/4 = 16
		{"128MB cache", 128 * 1024 * 1024, 8},     // 128MB/4MB/4 = 8
		{"512MB cache", 512 * 1024 * 1024, 32},    // 512MB/4MB/4 = 32
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AutoScalePrefetchBlocks(tt.cacheSize)
			if got != tt.want {
				t.Errorf("AutoScalePrefetchBlocks(%d) = %d, want %d", tt.cacheSize, got, tt.want)
			}
		})
	}
}

func TestDefaultConfig_UsesAutoScale(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ParallelUploads != 0 {
		t.Errorf("DefaultConfig().ParallelUploads = %d, want 0 (sentinel)", cfg.ParallelUploads)
	}
	if cfg.ParallelDownloads != 0 {
		t.Errorf("DefaultConfig().ParallelDownloads = %d, want 0 (sentinel)", cfg.ParallelDownloads)
	}
	if cfg.PrefetchBlocks != 0 {
		t.Errorf("DefaultConfig().PrefetchBlocks = %d, want 0 (sentinel)", cfg.PrefetchBlocks)
	}
}

func TestDefaultSentinels(t *testing.T) {
	if DefaultParallelUploads != 0 {
		t.Errorf("DefaultParallelUploads = %d, want 0 (sentinel for auto-scale)", DefaultParallelUploads)
	}
	if DefaultParallelDownloads != 0 {
		t.Errorf("DefaultParallelDownloads = %d, want 0 (sentinel for auto-scale)", DefaultParallelDownloads)
	}
	if DefaultPrefetchBlocks != 0 {
		t.Errorf("DefaultPrefetchBlocks = %d, want 0 (sentinel for auto-scale)", DefaultPrefetchBlocks)
	}
}
