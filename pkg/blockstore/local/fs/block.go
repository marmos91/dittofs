package fs

import (
	"sync"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// blockBufPool reuses BlockSize buffers across memBlock lifecycles.
// Channel-based pool to avoid sync.Pool's GC-cycle MADV_DONTNEED churn on
// large buffers. Buffers returned via putBlockBuf (from purgeMemBlocks)
// land here; getBlockBuf historically allocated from this pool but the
// allocator was deleted with the legacy block-flush write path. The pool
// is retained as a recycle sink so purgeMemBlocks can hand buffers back
// without the runtime immediately faulting them out.
var blockBufPool = make(chan []byte, 32)

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

// blockKey uniquely identifies a local block by the file it belongs to
// (payloadID, from metadata) and its position within the file
// (blockIdx = fileOffset / BlockSize).
type blockKey struct {
	payloadID string // PayloadID from metadata -- identifies the file's content
	blockIdx  uint64 // Block position within the file (0-based)
}

// memBlock holds the residual per-block mutex + data pointer used by
// purgeMemBlocks to release buffers back to blockBufPool on EvictMemory /
// Truncate. The hot-path write buffer (dataSize / dirty / lastWrite /
// writeGen) was retired alongside the legacy block-flush path; what
// remains is the bookkeeping shell so the eviction surface still has
// something to drain when the maps are repopulated by a future caller.
type memBlock struct {
	mu   sync.RWMutex
	data []byte // BlockSize buffer; nil after release
}

// fileInfo tracks per-file metadata in the local store.
// This is a lightweight struct (just file size) -- not related to metadata.File
// which carries full POSIX attributes. The local store only needs the file size
// to answer GetFileSize queries without hitting the metadata store.
type fileInfo struct {
	mu       sync.RWMutex
	fileSize uint64 // Highest byte offset written to this file
}
