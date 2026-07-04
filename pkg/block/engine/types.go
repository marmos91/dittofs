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

// DefaultBlockCarveBytes is the default target size of a packed block object
// (#1414): ~16 MiB. Large enough to amortize one PUT over many small chunks,
// small enough to cap the carver's per-block RAM and keep ranged reads cheap.
const DefaultBlockCarveBytes int64 = 16 << 20

// DefaultPrefetchBlocks is the default number of blocks to prefetch.
// 64 blocks = 512MB lookahead at 8MB block size.
const DefaultPrefetchBlocks = 64

// TransferType indicates the type of transfer operation.
type TransferType int

const (
	// TransferDownload is the highest priority - user is waiting for data.
	TransferDownload TransferType = iota
	// TransferPrefetch is lowest priority - speculative optimization.
	TransferPrefetch
)

// String returns a string representation of the transfer type.
func (t TransferType) String() string {
	switch t {
	case TransferDownload:
		return "download"
	case TransferPrefetch:
		return "prefetch"
	default:
		return "unknown"
	}
}

// TransferRequest holds data for a pending transfer operation (download or prefetch).
type TransferRequest struct {
	Type       TransferType // Transfer type and priority
	PayloadID  string       // Payload ID
	BlockIndex uint64       // Flat block index (fileOffset / BlockSize)
	Done       chan error   // Completion channel; nil for async (fire-and-forget)
}

// Config holds configuration for the Syncer.
type SyncerConfig struct {
	ParallelDownloads  int           // Concurrent block downloads per file (default: 32)
	PrefetchBlocks     int           // Blocks to prefetch ahead of reads; 0 = disabled (default: 64)
	SmallFileThreshold int64         // Files below this are flushed synchronously; 0 = disabled
	UploadInterval     time.Duration // Periodic uploader scan interval (default: 2s)
	UploadDelay        time.Duration // Min block age before periodic upload; Flush ignores this (default: 10s)

	// BlockCarveBytes is the target size of a packed block object (#1414). The
	// block carver accumulates synced-pending log-blob chunks and seals one
	// block once the accumulated raw bytes reach this threshold; a partial
	// block is flushed on idle (after UploadDelay with no new chunk) or on an
	// explicit Flush/SyncNow. <= 0 falls back to DefaultBlockCarveBytes. The
	// carver buffers at most one block (this many bytes) in RAM at a time.
	BlockCarveBytes int64

	// ManualSync, when true, suppresses the background carve dispatcher.
	// Durability is then driven solely by explicit Flush, making Flush the
	// single deterministic durability driver. Off by default; used where a
	// concurrent carver would race observable sync semantics (snapshot
	// bounds, crash-replay).
	ManualSync bool

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
		ParallelDownloads:           DefaultParallelDownloads,
		PrefetchBlocks:              DefaultPrefetchBlocks,
		SmallFileThreshold:          0,
		UploadInterval:              2 * time.Second,
		UploadDelay:                 10 * time.Second,
		BlockCarveBytes:             DefaultBlockCarveBytes,
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
	DownloadWorkers int // Download+prefetch worker goroutines (default: ParallelDownloads)
}

// DefaultSyncQueueConfig returns sensible defaults.
func DefaultSyncQueueConfig() SyncQueueConfig {
	return SyncQueueConfig{
		QueueSize:       1000,
		DownloadWorkers: DefaultParallelDownloads,
	}
}
