package engine

import (
	"errors"
	"time"
)

// ErrClosed is returned when an operation is attempted on a closed Syncer.
var ErrClosed = errors.New("syncer is closed")

// ErrStoreClosed is returned by a Store data op (WriteAt, ReadAt, Flush,
// Truncate, Delete, …) that arrives after the Store has been Closed —
// typically because an admin removed or hot-reloaded the share while a
// client was mid-transfer (area-7 H-A). Adapters map it to a stale-handle
// status (NFS NFS3ERR_STALE / NFS4ERR_STALE, SMB STATUS_FILE_CLOSED) so the
// client observes the share going away rather than a torn op or a panic.
var ErrStoreClosed = errors.New("engine: block store is closed")

// DefaultParallelDownloads is the default number of concurrent downloads per file.
// With 200-connection S3 pool and 8MB blocks, 32 workers can saturate the pool.
const DefaultParallelDownloads = 32

// DefaultPrefetchBlocks is the default number of blocks to prefetch.
// 64 blocks = 512MB lookahead at 8MB block size.
const DefaultPrefetchBlocks = 64

// TransferType indicates the type of transfer operation.
type TransferType int

const (
	// TransferDownload is the highest priority - user is waiting for data.
	TransferDownload TransferType = iota
	// TransferUpload is medium priority - ensures data durability.
	TransferUpload
	// TransferPrefetch is lowest priority - speculative optimization.
	TransferPrefetch
)

// String returns a string representation of the transfer type.
func (t TransferType) String() string {
	switch t {
	case TransferDownload:
		return "download"
	case TransferUpload:
		return "upload"
	case TransferPrefetch:
		return "prefetch"
	default:
		return "unknown"
	}
}

// TransferRequest holds data for a pending transfer operation (download, upload, or prefetch).
type TransferRequest struct {
	Type       TransferType // Transfer type and priority
	PayloadID  string       // Payload ID
	BlockIndex uint64       // Flat block index (fileOffset / BlockSize)
	Done       chan error   // Completion channel; nil for async (fire-and-forget)
}

// Adaptive upload-concurrency bounds (#1407). When ParallelUploads is unset
// (<= 0), the syncer auto-tunes the number of concurrent CAS-chunk uploads to
// saturate the uplink: it starts at AdaptiveUploadFloor and ramps toward
// AdaptiveUploadCeiling, settling at the goodput knee. A pinned
// ParallelUploads > 0 overrides this with a fixed window.
const (
	AdaptiveUploadFloor   = 16 // starting window in adaptive mode (greedy start)
	AdaptiveUploadCeiling = 64 // max window adaptive mode ramps to
)

// Config holds configuration for the Syncer.
type SyncerConfig struct {
	// ParallelUploads is the concurrent CAS-chunk upload count. > 0 pins a fixed
	// window; <= 0 (the default) enables adaptive auto-tuning between
	// AdaptiveUploadFloor and AdaptiveUploadCeiling (#1407).
	ParallelUploads    int
	ParallelDownloads  int           // Concurrent block downloads per file (default: 32)
	PrefetchBlocks     int           // Blocks to prefetch ahead of reads; 0 = disabled (default: 64)
	SmallFileThreshold int64         // Files below this are flushed synchronously; 0 = disabled
	UploadInterval     time.Duration // Periodic uploader scan interval (default: 2s)
	UploadDelay        time.Duration // Min block age before periodic upload; Flush ignores this (default: 10s)

	// Health check configuration for remote store monitoring.
	HealthCheckInterval         time.Duration // Probe interval when healthy (default: 30s)
	HealthCheckFailureThreshold int           // Consecutive failures to mark unhealthy (default: 3)
	UnhealthyCheckInterval      time.Duration // Probe interval when unhealthy (default: 5s)

	// — CAS upload-path knobs. The
	// authoritative defaults live in pkg/config.SyncerConfig; these fields
	// mirror them on the engine-local config struct so the syncer can be
	// constructed without depending on pkg/config (avoids an import cycle
	// from local/fs and other low-level callers).
	ClaimTimeout time.Duration // Max age of a Syncing row before the janitor requeues it (default: 10m)
}

// DefaultConfig returns the default Syncer configuration tuned for S3 performance.
func DefaultConfig() SyncerConfig {
	return SyncerConfig{
		// 0 = adaptive: the syncer auto-tunes upload concurrency to saturate the
		// uplink (#1407). A pinned --parallel-uploads overrides this.
		ParallelUploads:             0,
		ParallelDownloads:           DefaultParallelDownloads,
		PrefetchBlocks:              DefaultPrefetchBlocks,
		SmallFileThreshold:          0,
		UploadInterval:              2 * time.Second,
		UploadDelay:                 10 * time.Second,
		HealthCheckInterval:         30 * time.Second,
		HealthCheckFailureThreshold: 3,
		UnhealthyCheckInterval:      5 * time.Second,
		// — match pkg/config defaults.
		ClaimTimeout: 10 * time.Minute,
	}
}

// SyncQueueConfig holds configuration for the transfer queue.
type SyncQueueConfig struct {
	QueueSize       int // Max pending requests per channel (default: 1000)
	Workers         int // Upload worker goroutines (default: 4)
	DownloadWorkers int // Download+prefetch worker goroutines (default: ParallelDownloads)
}

// DefaultSyncQueueConfig returns sensible defaults.
func DefaultSyncQueueConfig() SyncQueueConfig {
	return SyncQueueConfig{
		QueueSize:       1000,
		Workers:         4,
		DownloadWorkers: DefaultParallelDownloads,
	}
}
