package bench

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ParseSize
// ---------------------------------------------------------------------------

func TestParseSize_ValidInputs(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		// Binary suffixes (case-insensitive).
		{"1KiB", 1 << 10},
		{"1kib", 1 << 10},
		{"2KiB", 2 << 10},
		{"1MiB", 1 << 20},
		{"1mib", 1 << 20},
		{"1GiB", 1 << 30},
		{"1gib", 1 << 30},
		{"1TiB", 1 << 40},
		{"1tib", 1 << 40},

		// Decimal suffixes (mapped to binary in this implementation).
		{"1KB", 1 << 10},
		{"1kb", 1 << 10},
		{"1MB", 1 << 20},
		{"1mb", 1 << 20},
		{"1GB", 1 << 30},
		{"1gb", 1 << 30},
		{"1TB", 1 << 40},
		{"1tb", 1 << 40},

		// Explicit B suffix.
		{"512B", 512},
		{"512b", 512},

		// Bare number (bytes).
		{"4096", 4096},
		{"0", 0},

		// Fractional values.
		{"0.5GiB", 1 << 29},
		{"1.5MiB", int64(1.5 * float64(1<<20))},
		{"2.5KiB", int64(2.5 * float64(1<<10))},

		// Whitespace trimming.
		{"  64KiB  ", 64 << 10},
		{"  1024  ", 1024},

		// Space between number and suffix.
		{"64 KiB", 64 << 10},
		{"1 GiB", 1 << 30},
	}

	for _, tc := range tests {
		got, err := ParseSize(tc.input)
		if err != nil {
			t.Errorf("ParseSize(%q) returned unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestParseSize_InvalidInputs(t *testing.T) {
	bad := []string{
		"",
		"abc",
		"GiB",    // no number
		"--1KB",  // bad number format
		"hello5", // number not at start
	}

	for _, s := range bad {
		_, err := ParseSize(s)
		if err == nil {
			t.Errorf("ParseSize(%q) expected error, got nil", s)
		}
	}
}

// ---------------------------------------------------------------------------
// FormatSize
// ---------------------------------------------------------------------------

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1 << 10, "1.0 KiB"},
		{1 << 20, "1.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{int64(1) << 40, "1.0 TiB"},
		{int64(1536), "1.5 KiB"},
		{3 * (1 << 30), "3.0 GiB"},
	}

	for _, tc := range tests {
		got := FormatSize(tc.bytes)
		if got != tc.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// computePercentiles
// ---------------------------------------------------------------------------

func TestComputePercentiles_Empty(t *testing.T) {
	stats := computePercentiles(nil)
	if stats.P50 != 0 || stats.P95 != 0 || stats.P99 != 0 || stats.Avg != 0 {
		t.Errorf("computePercentiles(nil) = %+v, want all zeros", stats)
	}
}

func TestComputePercentiles_Single(t *testing.T) {
	samples := []time.Duration{10 * time.Microsecond}
	stats := computePercentiles(samples)

	if stats.P50 != 10 {
		t.Errorf("P50 = %v, want 10", stats.P50)
	}
	if stats.P95 != 10 {
		t.Errorf("P95 = %v, want 10", stats.P95)
	}
	if stats.P99 != 10 {
		t.Errorf("P99 = %v, want 10", stats.P99)
	}
	if stats.Avg != 10 {
		t.Errorf("Avg = %v, want 10", stats.Avg)
	}
}

func TestComputePercentiles_Known(t *testing.T) {
	// 100 samples: 1us, 2us, ..., 100us.
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Microsecond
	}

	stats := computePercentiles(samples)

	// P50 = ceil(0.50*100)-1 = 49 => samples[49] = 50us.
	if stats.P50 != 50 {
		t.Errorf("P50 = %v, want 50", stats.P50)
	}
	// P95 = ceil(0.95*100)-1 = 94 => samples[94] = 95us.
	if stats.P95 != 95 {
		t.Errorf("P95 = %v, want 95", stats.P95)
	}
	// P99 = ceil(0.99*100)-1 = 98 => samples[98] = 99us.
	if stats.P99 != 99 {
		t.Errorf("P99 = %v, want 99", stats.P99)
	}
	// Avg = (1+2+...+100)/100 = 5050/100 = 50.5.
	if stats.Avg != 50.5 {
		t.Errorf("Avg = %v, want 50.5", stats.Avg)
	}
}

func TestComputePercentiles_DoesNotMutateInput(t *testing.T) {
	samples := []time.Duration{
		50 * time.Microsecond,
		10 * time.Microsecond,
		30 * time.Microsecond,
	}
	orig := make([]time.Duration, len(samples))
	copy(orig, samples)

	computePercentiles(samples)

	for i := range samples {
		if samples[i] != orig[i] {
			t.Fatalf("computePercentiles mutated input at index %d: got %v, want %v", i, samples[i], orig[i])
		}
	}
}

// ---------------------------------------------------------------------------
// AllWorkloads
// ---------------------------------------------------------------------------

func TestAllWorkloads(t *testing.T) {
	all := AllWorkloads()

	expected := []WorkloadType{SeqWrite, SeqRead, RandWrite, RandRead, Metadata, SmallFiles}
	if len(all) != len(expected) {
		t.Fatalf("AllWorkloads() returned %d items, want %d", len(all), len(expected))
	}
	for i, w := range expected {
		if all[i] != w {
			t.Errorf("AllWorkloads()[%d] = %q, want %q", i, all[i], w)
		}
	}
}

// ---------------------------------------------------------------------------
// Runner.Validate
// ---------------------------------------------------------------------------

func TestValidate_ValidDir(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner(Config{Path: dir}, nil)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() on valid tempdir returned error: %v", err)
	}
}

func TestValidate_NonExistentDir(t *testing.T) {
	r := NewRunner(Config{Path: "/nonexistent_path_xyzzy_12345"}, nil)
	if err := r.Validate(); err == nil {
		t.Fatal("Validate() on non-existent path expected error, got nil")
	}
}

func TestValidate_FileNotDir(t *testing.T) {
	// Create a regular file and try to validate it as a path.
	f, err := os.CreateTemp(t.TempDir(), "notadir")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	r := NewRunner(Config{Path: f.Name()}, nil)
	if err := r.Validate(); err == nil {
		t.Fatal("Validate() on a regular file expected error, got nil")
	}
}

func TestValidate_ReadOnlyDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping read-only test when running as root")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce directory permission bits")
	}

	dir := t.TempDir()
	roDir := filepath.Join(dir, "readonly")
	if err := os.Mkdir(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	// Ensure cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	r := NewRunner(Config{Path: roDir}, nil)
	if err := r.Validate(); err == nil {
		t.Fatal("Validate() on read-only dir expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// DefaultConfig
// ---------------------------------------------------------------------------

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Threads != 4 {
		t.Errorf("DefaultConfig().Threads = %d, want 4", cfg.Threads)
	}
	if cfg.FileSize != 1<<30 {
		t.Errorf("DefaultConfig().FileSize = %d, want %d", cfg.FileSize, int64(1<<30))
	}
	if cfg.BlockSize != 4<<10 {
		t.Errorf("DefaultConfig().BlockSize = %d, want %d", cfg.BlockSize, int64(4<<10))
	}
	if cfg.Duration != 60*time.Second {
		t.Errorf("DefaultConfig().Duration = %v, want 60s", cfg.Duration)
	}
	if cfg.MetaFiles != 1000 {
		t.Errorf("DefaultConfig().MetaFiles = %d, want 1000", cfg.MetaFiles)
	}
}

// ---------------------------------------------------------------------------
// Runner.Run — full integration (tiny config, all workloads)
// ---------------------------------------------------------------------------

func TestRun_AllWorkloads(t *testing.T) {
	dir := t.TempDir()

	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10, // 64 KiB
		BlockSize: 4 << 10,  // 4 KiB
		Duration:  1 * time.Second,
		MetaFiles: 10,
		System:    "test-system",
	}

	r := NewRunner(cfg, nil)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	if result == nil {
		t.Fatal("Run() returned nil result")
	}

	// Check all five workloads present.
	expectedWorkloads := AllWorkloads()
	if len(result.Workloads) != len(expectedWorkloads) {
		t.Fatalf("Run() returned %d workloads, want %d", len(result.Workloads), len(expectedWorkloads))
	}

	for _, w := range expectedWorkloads {
		wr, ok := result.Workloads[w]
		if !ok {
			t.Errorf("missing workload result for %q", w)
			continue
		}
		if wr.Workload != w {
			t.Errorf("workload %q: Workload field = %q", w, wr.Workload)
		}
		if wr.TotalOps <= 0 {
			t.Errorf("workload %q: TotalOps = %d, want > 0", w, wr.TotalOps)
		}
		if wr.Duration < 0 {
			t.Errorf("workload %q: Duration = %v, want >= 0", w, wr.Duration)
		}
	}

	// Sequential workloads should report throughput.
	for _, w := range []WorkloadType{SeqWrite, SeqRead} {
		wr := result.Workloads[w]
		if wr.ThroughputMBps <= 0 {
			t.Errorf("workload %q: ThroughputMBps = %f, want > 0", w, wr.ThroughputMBps)
		}
		if wr.TotalBytes <= 0 {
			t.Errorf("workload %q: TotalBytes = %d, want > 0", w, wr.TotalBytes)
		}
	}

	// Random workloads should report IOPS.
	for _, w := range []WorkloadType{RandWrite, RandRead} {
		wr := result.Workloads[w]
		if wr.IOPS <= 0 {
			t.Errorf("workload %q: IOPS = %f, want > 0", w, wr.IOPS)
		}
	}

	// Metadata workload should report OpsPerSec.
	metaWR := result.Workloads[Metadata]
	if metaWR.OpsPerSec <= 0 {
		t.Errorf("metadata workload: OpsPerSec = %f, want > 0", metaWR.OpsPerSec)
	}
	// Metadata creates + stats + deletes = 3 * MetaFiles.
	if metaWR.TotalOps != int64(cfg.MetaFiles*3) {
		t.Errorf("metadata workload: TotalOps = %d, want %d", metaWR.TotalOps, cfg.MetaFiles*3)
	}

	// Top-level result fields.
	if result.TotalDuration <= 0 {
		t.Errorf("TotalDuration = %v, want > 0", result.TotalDuration)
	}
	if result.System != "test-system" {
		t.Errorf("System = %q, want %q", result.System, "test-system")
	}
	if result.Path != dir {
		t.Errorf("Path = %q, want %q", result.Path, dir)
	}
	if result.Config.Threads != cfg.Threads {
		t.Errorf("Config.Threads = %d, want %d", result.Config.Threads, cfg.Threads)
	}
	if result.Config.FileSize != cfg.FileSize {
		t.Errorf("Config.FileSize = %d, want %d", result.Config.FileSize, cfg.FileSize)
	}

	// Benchmark dir should be cleaned up.
	benchPath := filepath.Join(dir, benchDir)
	if _, err := os.Stat(benchPath); !os.IsNotExist(err) {
		t.Errorf("bench dir %q still exists after Run()", benchPath)
	}
}

// ---------------------------------------------------------------------------
// Runner.Run — individual workload selection
// ---------------------------------------------------------------------------

func TestRun_SingleWorkload_SeqWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 10,
		Workloads: []WorkloadType{SeqWrite},
	}

	r := NewRunner(cfg, nil)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run(SeqWrite) returned error: %v", err)
	}

	if len(result.Workloads) != 1 {
		t.Fatalf("expected 1 workload result, got %d", len(result.Workloads))
	}
	if _, ok := result.Workloads[SeqWrite]; !ok {
		t.Error("missing SeqWrite workload result")
	}
}

func TestRun_SingleWorkload_Metadata(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 5,
		Workloads: []WorkloadType{Metadata},
	}

	r := NewRunner(cfg, nil)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run(Metadata) returned error: %v", err)
	}

	if len(result.Workloads) != 1 {
		t.Fatalf("expected 1 workload result, got %d", len(result.Workloads))
	}
	wr, ok := result.Workloads[Metadata]
	if !ok {
		t.Fatal("missing Metadata workload result")
	}
	if wr.TotalOps != int64(5*3) {
		t.Errorf("Metadata TotalOps = %d, want %d", wr.TotalOps, 5*3)
	}
}

func TestRun_SubsetWorkloads(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 5,
		Workloads: []WorkloadType{SeqWrite, Metadata},
	}

	r := NewRunner(cfg, nil)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run(SeqWrite, Metadata) returned error: %v", err)
	}

	if len(result.Workloads) != 2 {
		t.Fatalf("expected 2 workload results, got %d", len(result.Workloads))
	}
	if _, ok := result.Workloads[SeqWrite]; !ok {
		t.Error("missing SeqWrite workload result")
	}
	if _, ok := result.Workloads[Metadata]; !ok {
		t.Error("missing Metadata workload result")
	}
}

// ---------------------------------------------------------------------------
// Runner.Run — context cancellation
// ---------------------------------------------------------------------------

func TestRun_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  60 * time.Second, // Long duration that should be interrupted.
		MetaFiles: 10,
		Workloads: []WorkloadType{RandWrite},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	r := NewRunner(cfg, nil)
	_, err := r.Run(ctx)
	if err == nil {
		t.Fatal("Run() with cancelled context expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Runner.Run — progress callback
// ---------------------------------------------------------------------------

func TestRun_ProgressCallback(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 10,
		Workloads: []WorkloadType{SeqWrite, Metadata},
	}

	callCount := 0
	progress := func(workload WorkloadType, pct float64) {
		callCount++
		if pct < 0 {
			t.Errorf("progress pct = %f, want >= 0", pct)
		}
	}

	r := NewRunner(cfg, progress)
	_, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	if callCount == 0 {
		t.Error("progress callback was never called")
	}
}

// ---------------------------------------------------------------------------
// Runner.Run — unknown workload
// ---------------------------------------------------------------------------

func TestRun_UnknownWorkload(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 10,
		Workloads: []WorkloadType{"nonexistent-workload"},
	}

	r := NewRunner(cfg, nil)
	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run() with unknown workload expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Sequential Read+Write together (read depends on write files)
// ---------------------------------------------------------------------------

func TestRun_SeqReadWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 10,
		Workloads: []WorkloadType{SeqWrite, SeqRead},
	}

	r := NewRunner(cfg, nil)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run(SeqWrite, SeqRead) returned error: %v", err)
	}

	if len(result.Workloads) != 2 {
		t.Fatalf("expected 2 workload results, got %d", len(result.Workloads))
	}

	readWR := result.Workloads[SeqRead]
	if readWR.TotalBytes <= 0 {
		t.Errorf("SeqRead TotalBytes = %d, want > 0", readWR.TotalBytes)
	}
}

// ---------------------------------------------------------------------------
// Random Read+Write together (read depends on write files)
// ---------------------------------------------------------------------------

func TestRun_RandReadWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Path:      dir,
		Threads:   1,
		FileSize:  64 << 10,
		BlockSize: 4 << 10,
		Duration:  1 * time.Second,
		MetaFiles: 10,
		Workloads: []WorkloadType{RandWrite, RandRead},
	}

	r := NewRunner(cfg, nil)
	result, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run(RandWrite, RandRead) returned error: %v", err)
	}

	if len(result.Workloads) != 2 {
		t.Fatalf("expected 2 workload results, got %d", len(result.Workloads))
	}
}

// ---------------------------------------------------------------------------
// WorkloadType string values
// ---------------------------------------------------------------------------

func TestWorkloadType_StringValues(t *testing.T) {
	tests := []struct {
		wt   WorkloadType
		want string
	}{
		{SeqWrite, "seq-write"},
		{SeqRead, "seq-read"},
		{RandWrite, "rand-write"},
		{RandRead, "rand-read"},
		{Metadata, "metadata"},
	}
	for _, tc := range tests {
		if string(tc.wt) != tc.want {
			t.Errorf("WorkloadType = %q, want %q", tc.wt, tc.want)
		}
	}
}
