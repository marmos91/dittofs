package fs

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// blockBufPool reuses 8MB buffers across memBlock lifecycles.
// Uses a channel-based pool instead of sync.Pool to avoid GC scavenging
// overhead. sync.Pool entries are cleared every GC cycle, causing the runtime
// to madvise(MADV_DONTNEED) the 8MB pages back to the OS, then re-fault them
// on the next allocation. This was measured at 55% of CPU time in pprof.
//
// The channel pool holds up to 64 buffers (512MB max). Buffers survive GC
// because they're referenced by the channel. When the pool is empty, a new
// buffer is allocated. When the pool is full, returned buffers are dropped
// (GC will collect them naturally without madvise churn).
//
// Buffers may contain stale data but that's safe -- dataSize tracks the valid
// extent and all writes overwrite the relevant range before reading it.
var blockBufPool = make(chan []byte, 64)

func getBlockBuf() []byte {
	select {
	case buf := <-blockBufPool:
		return buf
	default:
		return make([]byte, blockstore.BlockSize)
	}
}

func putBlockBuf(buf []byte) {
	if cap(buf) < blockstore.BlockSize {
		return
	}
	select {
	case blockBufPool <- buf[:blockstore.BlockSize]:
	default:
		// Pool full, let GC collect
	}
}

// blockKey uniquely identifies a cached block by the file it belongs to
// (payloadID, from metadata) and its position within the file
// (blockIdx = fileOffset / BlockSize).
type blockKey struct {
	payloadID string // PayloadID from metadata -- identifies the file's content
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
// This is a lightweight struct (just file size) -- not related to metadata.File
// which carries full POSIX attributes. The cache only needs the file size to
// answer GetFileSize queries without hitting the metadata store.
type fileInfo struct {
	mu       sync.RWMutex
	fileSize uint64 // Highest byte offset written to this file
}
