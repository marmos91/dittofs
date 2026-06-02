package fs

// Pooled reconstruction buffers for rollupFile -> reconstructStream.
//
// reconstructStream builds a file-offset-indexed byte buffer sized to the
// furthest record end (maxEnd) on every rollup pass. Allocating it fresh
// each time was the dominant blockstore heap-allocation source under
// sustained writes. These channel-based pools recycle the backing arrays.
//
// Channel pools (not sync.Pool) for the same reason as blockBufPool in
// block.go: sync.Pool's GC-cycle reclamation MADV_DONTNEEDs idle multi-MiB
// buffers, so the next use faults every page back in. A bounded channel
// keeps a small working set resident.
//
// Two size classes cover the realistic range — protocol writes arrive
// <=1 MiB chunked, a rollup pass typically spans tens of MiB, large-file
// multi-pass rollups reach a few hundred MiB. Requests above the large
// class are allocated fresh and never pooled: holding a >512 MiB buffer
// idle defeats the residency goal.
const (
	reconstructSmallSize = 64 << 20  // 64 MiB
	reconstructLargeSize = 512 << 20 // 512 MiB
)

var (
	reconstructBufPoolSmall = make(chan []byte, 8)
	reconstructBufPoolLarge = make(chan []byte, 4)
)

// getReconstructBuf returns a zeroed buffer of exactly size bytes. The
// caller MUST return it via putReconstructBuf once the rollup pass that
// owns it completes. Buffers larger than the large size class are
// allocated fresh (and discarded by putReconstructBuf).
func getReconstructBuf(size uint64) []byte {
	pool, capHint := poolFor(size)
	if pool != nil {
		select {
		case buf := <-pool:
			buf = buf[:size]
			clear(buf)
			return buf
		default:
		}
		// Miss: allocate at the bucket capacity so the buffer is
		// pool-eligible when returned.
		return make([]byte, size, capHint)
	}
	return make([]byte, size)
}

// putReconstructBuf returns buf to its size-class pool. buf MUST NOT be
// used after this call. Buffers that don't match a pooled size class, or
// that arrive when the pool is full, are dropped for the GC.
func putReconstructBuf(buf []byte) {
	switch {
	case cap(buf) >= reconstructLargeSize:
		select {
		case reconstructBufPoolLarge <- buf:
		default:
		}
	case cap(buf) >= reconstructSmallSize:
		select {
		case reconstructBufPoolSmall <- buf:
		default:
		}
	}
}

// poolFor selects the pool + backing-array capacity for a requested size.
// Returns (nil, 0) for sizes above the large class (fresh-alloc, no pool).
func poolFor(size uint64) (chan []byte, int) {
	switch {
	case size <= reconstructSmallSize:
		return reconstructBufPoolSmall, reconstructSmallSize
	case size <= reconstructLargeSize:
		return reconstructBufPoolLarge, reconstructLargeSize
	default:
		return nil, 0
	}
}
