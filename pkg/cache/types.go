// Package cache implements a memory-buffered block cache for DittoFS.
//
// Writes are buffered in memory and flushed to disk atomically on NFS COMMIT
// or when memory budget is exceeded. This avoids per-4KB disk I/O and OS page
// cache bloat that caused OOM on servers with large caches.
//
// Key design:
//   - Memory buffer tier: 4KB NFS writes go to in-memory []byte buffers (no disk I/O)
//   - Atomic flush: complete blocks written to .blk files with FADV_DONTNEED
//   - Backpressure: memory budget limits dirty buffers, oldest flushed first
//   - Flat addressing: blockIdx = fileOffset / 8MB (no chunk layer)
package cache

import "errors"

// BlockSize is the size of a single block (8MB).
const BlockSize = 8 * 1024 * 1024

// Errors returned by BlockCache.
var (
	ErrCacheClosed    = errors.New("cache: closed")
	ErrDiskFull       = errors.New("cache: disk full after eviction")
	ErrFileNotInCache = errors.New("file not in cache")
	ErrBlockNotFound  = errors.New("block not found")
)

// PendingBlock represents a block ready for upload to the block store.
type PendingBlock struct {
	BlockIndex uint64   // Flat block index (fileOffset / BlockSize)
	Data       []byte   // Block content
	DataSize   uint32   // Actual size of valid data in the block
	Hash       [32]byte // SHA-256 content hash; zero means not yet computed
}

// FlushedBlock records info about a block that was just flushed from memory to disk.
// Used by GetDirtyBlocks to avoid a BadgerDB round-trip (write then read back).
type FlushedBlock struct {
	BlockIndex uint64
	CachePath  string
	DataSize   uint32
}

// Stats contains cache statistics for observability.
type Stats struct {
	DiskUsed      int64 // Current total size of on-disk cached data in bytes
	MaxDisk       int64 // Configured maximum disk cache size (0 = unlimited)
	MemUsed       int64 // Current in-memory dirty buffer usage in bytes
	MaxMemory     int64 // Configured memory budget for dirty buffers
	FileCount     int   // Number of files with cached data
	MemBlockCount int   // Number of in-memory dirty blocks
}
