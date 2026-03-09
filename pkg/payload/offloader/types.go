package offloader

import (
	"runtime"

	"github.com/marmos91/dittofs/pkg/payload/block"
)

// BlockSize is the size of a single block (4MB).
// Re-exported from block package for convenience.
const BlockSize = block.Size

// DefaultParallelUploads is the sentinel value indicating auto-scaling.
// When ParallelUploads is 0 (or unset), AutoScaleParallelUploads() computes
// the actual value based on runtime.NumCPU().
const DefaultParallelUploads = 0

// DefaultParallelDownloads is the sentinel value indicating auto-scaling.
// When ParallelDownloads is 0 (or unset), AutoScaleParallelDownloads() computes
// the actual value based on runtime.NumCPU().
const DefaultParallelDownloads = 0

// DefaultPrefetchBlocks is the sentinel value indicating auto-scaling.
// When PrefetchBlocks is 0 (or unset), AutoScalePrefetchBlocks() computes
// the actual value based on cache size.
const DefaultPrefetchBlocks = 0

// clamp restricts n to the range [lo, hi].
func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// AutoScaleParallelUploads computes the number of parallel uploads based on
// available CPUs. Uses NumCPU*4 with a floor of 16 and cap of 128.
// This scales with VM size: 4-core -> 16, 8-core -> 32, 16-core -> 64.
func AutoScaleParallelUploads() int {
	return clamp(runtime.NumCPU()*4, 16, 128)
}

// AutoScaleParallelDownloads computes the number of parallel downloads per file
// based on available CPUs. Uses NumCPU*2 with a floor of 4 and cap of 32.
func AutoScaleParallelDownloads() int {
	return clamp(runtime.NumCPU()*2, 4, 32)
}

// AutoScalePrefetchBlocks computes the number of prefetch blocks based on
// cache size. Uses cacheSize/BlockSize/4 with a floor of 8 and cap of 64.
// If cacheSizeBytes is 0 (unlimited), returns the floor (8).
func AutoScalePrefetchBlocks(cacheSizeBytes uint64) int {
	if cacheSizeBytes == 0 {
		return 8
	}
	return clamp(int(cacheSizeBytes/BlockSize/4), 8, 64)
}

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

// Config holds configuration for the Offloader.
type Config struct {
	// ParallelUploads is the initial number of concurrent block uploads.
	// The adaptive congestion control will start from this value.
	// Default: 4
	ParallelUploads int

	// MaxParallelUploads caps the maximum concurrent uploads.
	// Use this to limit bandwidth consumption.
	// Set to 0 for unlimited (congestion control will find optimal).
	// Default: 0 (unlimited, auto-tuned)
	MaxParallelUploads int

	// ParallelDownloads is the number of concurrent block downloads per file.
	// Default: 4
	ParallelDownloads int

	// PrefetchBlocks is the number of blocks to prefetch ahead of reads.
	// Set to 0 to disable prefetching.
	// Default: 4 (16MB ahead at 4MB block size)
	PrefetchBlocks int

	// SmallFileThreshold is the file size threshold for synchronous flush.
	// Files smaller than this size are uploaded synchronously during Flush()
	// to immediately free their block buffers and prevent pendingSize buildup
	// when creating many small files.
	// Set to 0 to disable (all files use async flush).
	// Default: 0 (disabled)
	SmallFileThreshold int64
}

// DefaultConfig returns the default Offloader configuration.
// These defaults are tuned for good S3 performance out of the box:
//   - ParallelUploads = 0: auto-scaled based on CPU count (NumCPU*4, floor=16, cap=128)
//   - ParallelDownloads = 0: auto-scaled based on CPU count (NumCPU*2, floor=4, cap=32)
//   - PrefetchBlocks = 0: auto-scaled based on cache size (cacheSize/BlockSize/4, floor=8, cap=64)
//   - SmallFileThreshold = 0: all flushes are async, data safety via WAL-backed cache
func DefaultConfig() Config {
	return Config{
		ParallelUploads:    DefaultParallelUploads,
		MaxParallelUploads: 0, // Unlimited, auto-tuned
		ParallelDownloads:  DefaultParallelDownloads,
		PrefetchBlocks:     DefaultPrefetchBlocks,
		SmallFileThreshold: 0, // Disabled - all flushes async, WAL ensures durability
	}
}

// TransferQueueConfig holds configuration for the transfer queue.
type TransferQueueConfig struct {
	// QueueSize is the maximum number of pending transfer requests per channel.
	// Default: 1000
	QueueSize int

	// Workers is the number of concurrent worker goroutines.
	// Default: 4
	Workers int
}

// DefaultTransferQueueConfig returns sensible defaults.
func DefaultTransferQueueConfig() TransferQueueConfig {
	return TransferQueueConfig{
		QueueSize: 1000,
		Workers:   4,
	}
}

// FlushResult indicates the outcome of a flush operation.
type FlushResult struct {
	// BytesFlushed is the number of bytes written.
	BytesFlushed uint64

	// AlreadyFlushed indicates all data was already flushed (no-op).
	AlreadyFlushed bool

	// Finalized indicates the data is durable in block store.
	Finalized bool
}

// RecoveryStats holds statistics about the recovery scan.
// Note: Uploads happen asynchronously after scan completes.
type RecoveryStats struct {
	FilesScanned int   // Number of files in cache
	BlocksFound  int   // Number of dirty blocks found
	BytesPending int64 // Bytes of dirty data to upload

	// RecoveredFileSizes maps payloadID to the actual file size recovered from WAL.
	// This allows consumers to reconcile metadata with actual cached data.
	// File size is calculated as max(blockBase + dataSize) across all recovered blocks.
	//
	// Key insight: WAL logs individual block writes. On crash recovery, the metadata
	// may have a larger file size from CommitWrite if crash occurred after metadata
	// update but before WAL persistence. Use this map to truncate metadata to match
	// actual recovered data.
	RecoveredFileSizes map[string]uint64
}
