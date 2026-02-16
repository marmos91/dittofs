package gss

import "sync"

// SeqWindow implements a sliding window sequence number tracker for RPCSEC_GSS.
//
// Per RFC 2203 Section 5.3.3.1, the server must track recently used sequence
// numbers to detect replays and out-of-order messages. The window tracks the
// highest seen sequence number and uses a bitmap to record which numbers within
// the window have been seen.
//
// Silent discard semantics: When a sequence number is rejected (duplicate,
// below window, or above MAXSEQ), the server silently discards the request
// without sending an error reply. This is per RFC 2203 Section 5.3.3.1.
//
// Thread Safety: All methods are safe for concurrent use.
type SeqWindow struct {
	size    uint32
	highest uint32
	bitmap  []uint64
	mu      sync.Mutex
}

// NewSeqWindow creates a new sequence window with the given size.
//
// The size determines how many recent sequence numbers are tracked.
// Typical values are 128 or 256. The bitmap is allocated with enough
// uint64 elements to cover the window size.
//
// Parameters:
//   - size: Number of sequence numbers to track (must be > 0)
//
// Returns:
//   - *SeqWindow: New sequence window
func NewSeqWindow(size uint32) *SeqWindow {
	if size == 0 {
		size = 1
	}
	bitmapSize := (size + 63) / 64
	return &SeqWindow{
		size:   size,
		bitmap: make([]uint64, bitmapSize),
	}
}

// Accept checks whether a sequence number is valid and marks it as seen.
//
// A sequence number is accepted if:
//   - It is not greater than MAXSEQ (0x80000000)
//   - It is within the current window (>= highest - size)
//   - It has not been seen before (no duplicate)
//
// When a new highest sequence number is received, the window slides forward,
// clearing bits for sequence numbers that have fallen out of the window.
//
// Per RFC 2203 Section 5.3.3.1, invalid sequence numbers should result
// in silent discard of the RPC request (no error reply).
//
// Parameters:
//   - seqNum: The sequence number to check
//
// Returns:
//   - bool: true if the sequence number is accepted (valid and new),
//     false if it should be silently discarded
func (w *SeqWindow) Accept(seqNum uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Reject sequence numbers above MAXSEQ
	if seqNum > MAXSEQ {
		return false
	}

	// Reject zero (not a valid sequence number in RPCSEC_GSS)
	if seqNum == 0 {
		return false
	}

	// If this is the first sequence number, initialize
	if w.highest == 0 {
		w.highest = seqNum
		w.setBit(seqNum)
		return true
	}

	// Reject if below the window
	if w.highest >= w.size && seqNum < w.highest-w.size+1 {
		return false
	}

	// If within or equal to current window, check for duplicate
	if seqNum <= w.highest {
		if w.isBitSet(seqNum) {
			return false // duplicate
		}
		w.setBit(seqNum)
		return true
	}

	// seqNum > w.highest: slide window forward
	shift := seqNum - w.highest
	w.slideWindow(shift)
	w.highest = seqNum
	w.setBit(seqNum)
	return true
}

// Reset clears the sequence window state, allowing reuse.
// Primarily useful for testing.
func (w *SeqWindow) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.highest = 0
	for i := range w.bitmap {
		w.bitmap[i] = 0
	}
}

// setBit marks a sequence number as seen in the bitmap.
// Must be called with w.mu held.
func (w *SeqWindow) setBit(seqNum uint32) {
	offset := seqNum % w.size
	wordIdx := offset / 64
	bitIdx := offset % 64
	if int(wordIdx) < len(w.bitmap) {
		w.bitmap[wordIdx] |= 1 << bitIdx
	}
}

// isBitSet checks if a sequence number is marked as seen.
// Must be called with w.mu held.
func (w *SeqWindow) isBitSet(seqNum uint32) bool {
	offset := seqNum % w.size
	wordIdx := offset / 64
	bitIdx := offset % 64
	if int(wordIdx) < len(w.bitmap) {
		return w.bitmap[wordIdx]&(1<<bitIdx) != 0
	}
	return false
}

// slideWindow shifts the bitmap forward by the given amount,
// clearing bits that have fallen out of the window.
// Must be called with w.mu held.
func (w *SeqWindow) slideWindow(shift uint32) {
	if shift >= w.size {
		// Entire window has shifted past; clear everything
		for i := range w.bitmap {
			w.bitmap[i] = 0
		}
		return
	}

	// Clear bits for sequence numbers that are being overwritten.
	// We need to clear the positions that will be reused by the new
	// sequence numbers entering the window.
	for i := uint32(0); i < shift; i++ {
		pos := (w.highest + 1 + i) % w.size
		wordIdx := pos / 64
		bitIdx := pos % 64
		if int(wordIdx) < len(w.bitmap) {
			w.bitmap[wordIdx] &^= 1 << bitIdx
		}
	}
}
