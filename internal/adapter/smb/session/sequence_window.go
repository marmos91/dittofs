package session

import "sync"

// CommandSequenceWindow tracks granted MessageIds per MS-SMB2 3.3.1.1.
// It uses a bitmap-based sliding window to efficiently validate and consume
// sequence numbers. Each bit represents whether a sequence number is
// available (1) or already consumed (0).
//
// Per MS-SMB2 3.3.5.2.3: The server MUST validate MessageId by checking
// if it falls within the CommandSequenceWindow. If not, the request is
// rejected with STATUS_INVALID_PARAMETER.
//
// The bitmap maps sequence number `seq` to:
//   - word index: (seq - low) / 64
//   - bit position: (seq - low) % 64
//
// A set bit (1) means the sequence number is available and can be consumed.
type CommandSequenceWindow struct {
	mu      sync.Mutex
	low     uint64   // Lowest tracked sequence number (bitmap base)
	high    uint64   // Next sequence number to be granted (exclusive upper bound)
	bitmap  []uint64 // Bit i set = sequence (low + bit_position) is available
	maxSize uint64   // Maximum window size (2x MaxSessionCredits per MS-SMB2)
}

// NewCommandSequenceWindow creates a new sequence window initialized with
// sequence {0} available. The maxSize parameter limits the maximum window
// size to prevent unbounded bitmap growth; it should be set to
// 2 * CreditConfig.MaxSessionCredits (default: 2 * 65535 = 131070).
//
// Per MS-SMB2 3.3.1.1: Upon creation, the server MUST initialize the
// CommandSequenceWindow to {0}.
func NewCommandSequenceWindow(maxSize uint64) *CommandSequenceWindow {
	w := &CommandSequenceWindow{
		low:     0,
		high:    1, // Sequence 0 is available, so next to grant is 1
		bitmap:  make([]uint64, 1),
		maxSize: maxSize,
	}
	// Set bit 0 = sequence 0 is available
	w.bitmap[0] = 1
	return w
}

// Consume validates and consumes MessageId sequence numbers [messageId, messageId+charge).
// It returns true if ALL sequence numbers in the range are within the window and available,
// and atomically clears them. Returns false if any sequence number is out of range, already
// consumed, or unavailable.
//
// Per MS-SMB2 3.3.5.2.3: CreditCharge=0 is treated as CreditCharge=1
// (SMB 2.0.2 compatibility).
func (w *CommandSequenceWindow) Consume(messageId uint64, creditCharge uint16) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Per MS-SMB2 3.3.5.2.3: CreditCharge of 0 is treated as 1
	charge := uint64(creditCharge)
	if charge == 0 {
		charge = 1
	}

	// Range check: all sequences must be within [low, high)
	if messageId < w.low || messageId+charge > w.high {
		return false
	}

	// Check all bits are available before consuming (atomic check)
	for i := uint64(0); i < charge; i++ {
		seq := messageId + i
		offset := seq - w.low
		wordIdx := offset / 64
		bitIdx := offset % 64

		if wordIdx >= uint64(len(w.bitmap)) {
			return false
		}
		if w.bitmap[wordIdx]&(1<<bitIdx) == 0 {
			return false // Already consumed or not available
		}
	}

	// All available -- consume them
	for i := uint64(0); i < charge; i++ {
		seq := messageId + i
		offset := seq - w.low
		wordIdx := offset / 64
		bitIdx := offset % 64
		w.bitmap[wordIdx] &^= 1 << bitIdx
	}

	// Advance low watermark past fully consumed words
	w.advanceLow()

	return true
}

// Grant adds count new available sequence numbers at the high end of the window.
// If the window would exceed maxSize, the grant is capped to keep the window
// within bounds.
//
// Per MS-SMB2 3.3.1.2: The server grants credits by expanding the
// CommandSequenceWindow with new available sequence numbers.
func (w *CommandSequenceWindow) Grant(count uint16) {
	w.mu.Lock()
	defer w.mu.Unlock()

	grant := uint64(count)

	// Cap the window size at maxSize
	currentSize := w.high - w.low
	if currentSize+grant > w.maxSize {
		if currentSize >= w.maxSize {
			return // Already at maximum
		}
		grant = w.maxSize - currentSize
	}

	newHigh := w.high + grant

	// Ensure bitmap has enough words for the new range
	totalSpan := newHigh - w.low
	neededWords := (totalSpan + 63) / 64
	for uint64(len(w.bitmap)) < neededWords {
		w.bitmap = append(w.bitmap, 0)
	}

	// Set bits for new sequences [high, newHigh)
	for seq := w.high; seq < newHigh; seq++ {
		offset := seq - w.low
		wordIdx := offset / 64
		bitIdx := offset % 64
		w.bitmap[wordIdx] |= 1 << bitIdx
	}

	w.high = newHigh
}

// Size returns the current window size (the span from low to high watermark).
func (w *CommandSequenceWindow) Size() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.high - w.low
}

// advanceLow advances the low watermark past fully consumed bitmap words
// (all-zero uint64 blocks), compacting the bitmap. Must be called with
// w.mu held.
func (w *CommandSequenceWindow) advanceLow() {
	// Advance past fully consumed words (all bits zero)
	for len(w.bitmap) > 0 && w.bitmap[0] == 0 {
		// Only advance if this word is fully within the tracked range
		nextLow := w.low + 64
		if nextLow > w.high {
			// Don't advance past high watermark
			break
		}
		w.bitmap = w.bitmap[1:]
		w.low = nextLow
	}
}
