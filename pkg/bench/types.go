// Package bench provides a filesystem benchmarking engine.
//
// It measures sequential and random I/O throughput, IOPS, latency percentiles,
// and metadata operation rates. Results are returned as structured data suitable
// for JSON serialization and cross-system comparison.
package bench

import "time"

// WorkloadType identifies a benchmark workload.
type WorkloadType string

const (
	SeqWrite   WorkloadType = "seq-write"
	SeqRead    WorkloadType = "seq-read"
	RandWrite  WorkloadType = "rand-write"
	RandRead   WorkloadType = "rand-read"
	Metadata   WorkloadType = "metadata"
	SmallFiles WorkloadType = "small-files"
)

// AllWorkloads returns every supported workload type in execution order.
func AllWorkloads() []WorkloadType {
	return []WorkloadType{SeqWrite, SeqRead, RandWrite, RandRead, Metadata, SmallFiles}
}

// Config controls benchmark execution.
type Config struct {
	// Path is the directory to benchmark (must exist and be writable).
	Path string `json:"path"`

	// Threads is the number of concurrent I/O workers (default 4).
	Threads int `json:"threads"`

	// FileSize is the size of each test file in bytes (default 1 GiB).
	FileSize int64 `json:"file_size"`

	// BlockSize is the I/O block size for random workloads in bytes (default 4 KiB).
	BlockSize int64 `json:"block_size"`

	// Duration is the time limit for duration-based workloads (default 60s).
	Duration time.Duration `json:"duration"`

	// MetaFiles is the number of small files for metadata workloads (default 1000).
	MetaFiles int `json:"meta_files"`

	// SmallFileCount is the number of files for small-files workload (default 10000).
	SmallFileCount int `json:"small_file_count,omitempty"`

	// Workloads is the list of workloads to run. Nil means all.
	Workloads []WorkloadType `json:"workloads,omitempty"`

	// System is an optional label identifying the system under test (e.g., "dittofs-badger-fs").
	System string `json:"system,omitempty"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Threads:        4,
		FileSize:       1 << 30, // 1 GiB
		BlockSize:      4 << 10, // 4 KiB
		Duration:       60 * time.Second,
		MetaFiles:      1000,
		SmallFileCount: 10000,
	}
}

// Result is the top-level benchmark output.
type Result struct {
	// Timestamp is when the benchmark started (UTC).
	Timestamp time.Time `json:"timestamp"`

	// System is the label identifying the system under test.
	System string `json:"system,omitempty"`

	// Path is the benchmarked directory.
	Path string `json:"path"`

	// Config summarizes the benchmark parameters.
	Config ConfigSummary `json:"config"`

	// Workloads contains per-workload results, keyed by WorkloadType.
	Workloads map[WorkloadType]*WorkloadResult `json:"workloads"`

	// TotalDuration is the wall-clock time for the entire benchmark.
	TotalDuration time.Duration `json:"total_duration"`
}

// ConfigSummary is a serialization-friendly snapshot of the benchmark config.
type ConfigSummary struct {
	Threads   int           `json:"threads"`
	FileSize  int64         `json:"file_size"`
	BlockSize int64         `json:"block_size"`
	Duration  time.Duration `json:"duration"`
	MetaFiles int           `json:"meta_files"`
}

// WorkloadResult holds metrics for a single workload.
type WorkloadResult struct {
	// Workload identifies which workload produced this result.
	Workload WorkloadType `json:"workload"`

	// ThroughputMBps is the sustained throughput in MB/s (sequential workloads).
	ThroughputMBps float64 `json:"throughput_mbps,omitempty"`

	// IOPS is the I/O operations per second (random workloads).
	IOPS float64 `json:"iops,omitempty"`

	// OpsPerSec is the operations per second (metadata workloads).
	OpsPerSec float64 `json:"ops_per_sec,omitempty"`

	// Latency percentiles in microseconds.
	LatencyP50Us float64 `json:"latency_p50_us"`
	LatencyP95Us float64 `json:"latency_p95_us"`
	LatencyP99Us float64 `json:"latency_p99_us"`
	LatencyAvgUs float64 `json:"latency_avg_us"`

	// TotalOps is the total number of I/O operations completed.
	TotalOps int64 `json:"total_ops"`

	// TotalBytes is the total bytes transferred.
	TotalBytes int64 `json:"total_bytes"`

	// Errors is the number of I/O errors encountered.
	Errors int64 `json:"errors"`

	// Duration is the wall-clock time for this workload.
	Duration time.Duration `json:"duration"`
}

// ProgressFunc is called to report benchmark progress.
// workload is the current workload name, pct is 0.0–1.0.
type ProgressFunc func(workload WorkloadType, pct float64)
