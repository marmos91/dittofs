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
	maxSize uint64   // Maximum window size (Samba-compatible `smb2 max credits`)
	// available is the server's view of the client-advertised credit balance:
	// the number of sequence numbers the client is entitled to use. Tracked
	// separately from (high - low) because advanceLow can lag behind actual
	// consumption (the oldest unconsumed message pins `low` in place) and
	// separately from the bitmap popcount because Reclaim decrements only
	// this counter when compound middle responses have their on-the-wire
	// Credits zeroed — the corresponding bits stay set but the client was
	// never told about them. Mirrors Samba's xconn->smb2.credits.seq_range.
	available uint64
}

// NewCommandSequenceWindow creates a new sequence window initialized with
// sequences {0, 1} available. maxSize caps outstanding credits on the
// connection (the client's cur_credits counter) — callers should pass
// CreditConfig.MaxSessionCredits (Samba default: 8192). See
// NewSequenceWindowForConnection in internal/adapter/smb/conn_types.go.
//
// Per MS-SMB2 3.3.1.1, the initial window starts at sequence 0, but in
// practice clients pick either 0 (Samba smbtorture) or 1 (MS-WPTS) for the
// NEGOTIATE MessageID. Both are valid SMB2 first MessageIDs — Windows
// servers accept either. Pre-seeding bits 0 and 1 lets Consume succeed
// regardless of which convention the client follows, while the `available`
// counter still starts at 1 because the client's initial cur_credits is 1
// (the extra bit is just a free MessageID slot, not a credit).
func NewCommandSequenceWindow(maxSize uint64) *CommandSequenceWindow {
	return &CommandSequenceWindow{
		low:       0,
		high:      2,
		bitmap:    []uint64{0b11},
		maxSize:   maxSize,
		available: 1,
	}
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
	// Saturate rather than underflow. After Reclaim the bitmap can hold
	// more set bits than `available` tracks (we never told the client about
	// those bits); a client sending one of them is a protocol violation,
	// but saturating at 0 keeps the accounting invariants intact so a
	// later grant can proceed from a clean zero baseline.
	if charge > w.available {
		w.available = 0
	} else {
		w.available -= charge
	}

	// Advance low watermark past fully consumed words
	w.advanceLow()

	return true
}

// Grant extends the window by up to `count` sequence numbers and returns
// the actual amount granted, which may be less than `count` if the grant
// would push the client's outstanding credits past maxSize.
//
// Callers MUST use the returned value as the CreditResponse field in the
// outgoing SMB2 header. A naïve two-step "read Remaining(), clamp, Grant()"
// pattern races under pipelining on the same connection: two goroutines
// read the same remaining capacity, each clamps to it, then both Grant()
// — the server's own bookkeeping stays correct (Grant caps internally),
// but the advertised credits on the wire double-count, re-creating the
// client-side cur_credits overflow that issue #378 fixed.
//
// Per MS-SMB2 3.3.1.2. Matches Samba's credits_possible computation in
// source3/smbd/smb2_server.c:smb2_set_operation_credit, which also caps
// against outstanding credits atomically. The bitmap's raw span
// (high - low) can briefly overshoot maxSize when advanceLow is blocked
// by a stale bit, but bitmap memory stays O(maxSize) because advanceLow
// reclaims full 64-bit words as consumption catches up.
func (w *CommandSequenceWindow) Grant(count uint16) uint16 {
	w.mu.Lock()
	defer w.mu.Unlock()

	grant := uint64(count)
	if w.available+grant > w.maxSize {
		if w.available >= w.maxSize {
			return 0
		}
		grant = w.maxSize - w.available
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
	w.available += grant
	return uint16(grant)
}

// Size returns the current window size (the span from low to high watermark).
func (w *CommandSequenceWindow) Size() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.high - w.low
}

// Reclaim decreases the outstanding-credit counter without retracting any
// message IDs from the window. Used by the compound response path: each
// sub-response's Grant() extends the window individually, but MS-SMB2
// 3.2.4.1.4 requires middle compound responses to advertise Credits=0
// on the wire. After zeroing those headers, Reclaim rolls back the
// `available` bookkeeping so the server's view of the client's
// cur_credits matches what we actually told the client. The underlying
// message IDs stay marked valid — if the client ever sent one (it won't,
// because we didn't grant it) we'd still accept it. Reclaim is an
// over-grant correction, not a credit revocation.
//
// Safe to call with n exceeding available; the counter saturates at 0.
func (w *CommandSequenceWindow) Reclaim(n uint16) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reclaim := uint64(n)
	if reclaim > w.available {
		reclaim = w.available
	}
	w.available -= reclaim
}

// Available returns the number of unconsumed credits currently in the window.
// Equivalent to the client's cur_credits counter and mirrors Samba's
// xconn->smb2.credits.seq_range.
func (w *CommandSequenceWindow) Available() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.available
}

// MaxSize returns the window's maximum allowed span. Callers use this to
// bound credit grants so the total outstanding credits on the connection
// stay within the client's tracked window.
func (w *CommandSequenceWindow) MaxSize() uint64 {
	return w.maxSize
}

// Remaining returns how many new credits the server can extend the window
// by without overflowing the client's per-connection cur_credits counter
// (MS-SMB2 3.3.1.2). Callers (buildResponseHeaderAndBody) must clamp the
// credits field in outgoing responses to this value. Matches Samba's
// credits_possible = max - seq_range bookkeeping (#378).
func (w *CommandSequenceWindow) Remaining() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.available >= w.maxSize {
		return 0
	}
	return w.maxSize - w.available
}

// advanceLow advances the low watermark past fully consumed bitmap words
// (all-zero uint64 blocks), compacting the bitmap. The `available` counter
// is the authoritative credit tally — this only reclaims memory. Must be
// called with w.mu held.
func (w *CommandSequenceWindow) advanceLow() {
	for len(w.bitmap) > 0 && w.bitmap[0] == 0 {
		nextLow := w.low + 64
		if nextLow > w.high {
			break
		}
		w.bitmap = w.bitmap[1:]
		w.low = nextLow
	}
}
