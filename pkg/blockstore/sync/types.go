package sync

import (
	"errors"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ErrClosed is returned when an operation is attempted on a closed Syncer.
var ErrClosed = errors.New("syncer is closed")

// BlockSize is the size of a single block (8MB).
// Re-exported from blockstore package for convenience.
const BlockSize = blockstore.BlockSize

// DefaultParallelUploads is the default number of concurrent uploads.
// At ~8 MB/s per S3 connection, 16 connections yields ~128 MB/s upload bandwidth.
const DefaultParallelUploads = 16

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

// Config holds configuration for the Syncer.
type Config struct {
	ParallelUploads    int           // Concurrent block uploads (default: 16)
	ParallelDownloads  int           // Concurrent block downloads per file (default: 32)
	PrefetchBlocks     int           // Blocks to prefetch ahead of reads; 0 = disabled (default: 64)
	SmallFileThreshold int64         // Files below this are flushed synchronously; 0 = disabled
	UploadInterval     time.Duration // Periodic uploader scan interval (default: 2s)
	UploadDelay        time.Duration // Min block age before periodic upload; Flush ignores this (default: 10s)
}

// DefaultConfig returns the default Syncer configuration tuned for S3 performance.
func DefaultConfig() Config {
	return Config{
		ParallelUploads:    DefaultParallelUploads,
		ParallelDownloads:  DefaultParallelDownloads,
		PrefetchBlocks:     DefaultPrefetchBlocks,
		SmallFileThreshold: 0,
		UploadInterval:     2 * time.Second,
		UploadDelay:        10 * time.Second,
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
