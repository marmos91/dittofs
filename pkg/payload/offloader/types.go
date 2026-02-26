package offloader

import "github.com/marmos91/dittofs/pkg/payload/block"

// ============================================================================
// Constants
// ============================================================================

// BlockSize is the size of a single block (4MB).
// Re-exported from block package for convenience.
const BlockSize = block.Size

// DefaultParallelUploads is the default number of concurrent uploads.
// At ~4 MB/s per S3 connection, 16 connections yields ~64 MB/s upload bandwidth.
const DefaultParallelUploads = 16

// DefaultParallelDownloads is the default number of concurrent downloads per file.
const DefaultParallelDownloads = 4

// DefaultPrefetchBlocks is the default number of blocks to prefetch.
const DefaultPrefetchBlocks = 4

// ============================================================================
// Transfer Type
// ============================================================================

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

// ============================================================================
// Configuration
// ============================================================================

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
func DefaultConfig() Config {
	return Config{
		ParallelUploads:    DefaultParallelUploads,
		MaxParallelUploads: 0, // Unlimited, auto-tuned
		ParallelDownloads:  DefaultParallelDownloads,
		PrefetchBlocks:     DefaultPrefetchBlocks,
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

// ============================================================================
// Result Types
// ============================================================================

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
