package cache

import (
	"sync"
	"time"
)

// blockKey uniquely identifies a cached block by the file it belongs to
// (payloadID, from metadata) and its position within the file
// (blockIdx = fileOffset / BlockSize).
type blockKey struct {
	payloadID string // PayloadID from metadata — identifies the file's content
	blockIdx  uint64 // Block position within the file (0-based)
}

// memBlock is an in-memory write buffer for a single 8MB block.
//
// NFS WRITE operations (typically 4KB each) accumulate into this buffer.
// When the block is full (dataSize == BlockSize) or on NFS COMMIT, the
// buffer is flushed atomically to a .blk file on disk (see flushBlock).
// After flushing, data is set to nil to release the 8MB allocation.
//
// The 8MB buffer is pre-allocated when the memBlock is created (see
// getOrCreateMemBlock) to avoid allocation jitter on the write hot path.
type memBlock struct {
	mu        sync.RWMutex
	data      []byte    // Pre-allocated BlockSize buffer; nil after flush to disk
	dataSize  uint32    // Highest byte offset written (valid data extent)
	dirty     bool      // true if buffer has data not yet flushed to disk
	lastWrite time.Time // Timestamp of last write; used for LRU flush ordering
}

// fileInfo tracks per-file metadata in the cache.
// This is a lightweight struct (just file size) — not related to metadata.File
// which carries full POSIX attributes. The cache only needs the file size to
// answer GetFileSize queries without hitting the metadata store.
type fileInfo struct {
	mu       sync.RWMutex
	fileSize uint64 // Highest byte offset written to this file
}
