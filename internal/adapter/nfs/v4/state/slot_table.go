package state

import (
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// Slot Table Constants
// ============================================================================

const (
	// DefaultMaxSlots is the server-imposed maximum slot count for DittoFS.
	// Each slot consumes memory for cached replies, so this bounds resource usage.
	DefaultMaxSlots uint32 = 64

	// MinSlots is the minimum number of slots per session, per RFC 8881.
	MinSlots uint32 = 1
)

// ============================================================================
// SequenceValidation (v4.1 slot-based)
// ============================================================================

// SequenceValidation is the result of validating a slot sequence ID
// per RFC 8881 Section 2.10.6.1.
//
// NOTE: This is separate from the v4.0 SeqIDValidation in openowner.go.
// v4.1 slot-based seqid validation has different semantics:
//   - Initial seqid is 0 (not 1)
//   - Wraps through 0 (0xFFFFFFFF + 1 = 0 is valid, not skipped)
//   - Per-slot, not per-owner
type SequenceValidation int

const (
	// SeqNew indicates a new request (seqid == cached + 1).
	SeqNew SequenceValidation = iota

	// SeqRetry indicates a retransmission (seqid == cached, slot not in-use,
	// cached reply exists).
	SeqRetry

	// SeqMisordered indicates an out-of-range seqid (gap or behind).
	SeqMisordered
)

// ============================================================================
// Slot
// ============================================================================

// Slot represents a single slot in an NFSv4.1 slot table.
//
// Each slot tracks the last completed sequence ID and optionally caches
// the full XDR-encoded COMPOUND4res for replay detection (exactly-once
// semantics).
type Slot struct {
	// SeqID is the last completed sequence ID for this slot.
	// Starts at 0; first valid request uses seqID=1.
	SeqID uint32

	// InUse indicates a request is currently being processed on this slot.
	InUse bool

	// CachedReply holds the full XDR-encoded COMPOUND4res for replay.
	// nil if no reply has been cached (e.g., sa_cachethis was false).
	CachedReply []byte
}

// ============================================================================
// SlotTable
// ============================================================================

// SlotTable implements the NFSv4.1 slot table per RFC 8881 Section 2.10.6.
//
// Each session has its own SlotTable with a per-table mutex, avoiding
// contention on the global StateManager.mu for the hot SEQUENCE path.
//
// The slot table provides exactly-once semantics by:
//  1. Validating sequence IDs to detect new requests vs retries
//  2. Caching COMPOUND responses for replay on retransmission
//  3. Tracking in-use slots to detect duplicate in-flight requests
type SlotTable struct {
	mu sync.Mutex

	// slots is the array of slot entries. Length is fixed at creation.
	slots []Slot

	// highestSlotID is the highest allocated slot ID (max valid slot index).
	// Returned as sr_highest_slotid in SEQUENCE responses.
	highestSlotID uint32

	// targetHighestSlotID is the server's desired maximum slot ID.
	// Returned as sr_target_highest_slotid to signal the client to
	// adjust its slot usage (dynamic flow control).
	targetHighestSlotID uint32

	// maxSlots is the allocated slot count (len(slots)).
	// Immutable after creation.
	maxSlots uint32
}

// NewSlotTable creates a new SlotTable with the given number of slots.
//
// numSlots is clamped to [MinSlots, DefaultMaxSlots].
// All slots are initialized to SeqID=0, InUse=false, CachedReply=nil.
func NewSlotTable(numSlots uint32) *SlotTable {
	if numSlots < MinSlots {
		numSlots = MinSlots
	}
	if numSlots > DefaultMaxSlots {
		numSlots = DefaultMaxSlots
	}

	st := &SlotTable{
		slots:               make([]Slot, numSlots),
		highestSlotID:       numSlots - 1,
		targetHighestSlotID: numSlots - 1,
		maxSlots:            numSlots,
	}

	// All slots start at SeqID=0, InUse=false, CachedReply=nil (zero values)
	return st
}

// ValidateSequence implements the RFC 8881 Section 2.10.6.1 sequence
// validation algorithm.
//
// Returns the validation result, a pointer to the slot (for SeqNew/SeqRetry),
// and an error (for BADSLOT, SEQ_MISORDERED, DELAY, RETRY_UNCACHED_REP).
//
// For a SeqNew result, this method also marks the slot as in-use while
// holding st.mu, making validation and reservation atomic. The caller must
// call CompleteSlotRequest when the request is finished.
//
// Thread-safe: acquires st.mu for the duration of validation.
func (st *SlotTable) ValidateSequence(slotID, seqID uint32) (SequenceValidation, *Slot, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Step 1: Check slot ID range
	if slotID >= st.maxSlots {
		return SeqMisordered, nil, &NFS4StateError{
			Status:  types.NFS4ERR_BADSLOT,
			Message: "slot ID out of range",
		}
	}

	// Step 2: Get slot reference
	slot := &st.slots[slotID]

	// Step 3: Compute expected next sequence ID.
	// uint32 natural overflow handles wrap: 0xFFFFFFFF + 1 = 0, which
	// is valid in v4.1 (unlike v4.0 where seqid=0 is reserved).
	expectedSeqID := slot.SeqID + 1

	// Step 4: New request (seqid == expected next)
	if seqID == expectedSeqID {
		if slot.InUse {
			// Slot is already processing a request for this seqid.
			// This is a retransmission of the in-flight request; tell
			// the client to wait (RFC 8881 Section 2.10.6.1).
			return SeqMisordered, nil, &NFS4StateError{
				Status:  types.NFS4ERR_DELAY,
				Message: "slot in use; request in flight",
			}
		}
		// Atomically mark slot in-use and update highestSlotID.
		slot.InUse = true
		if slotID > st.highestSlotID {
			st.highestSlotID = slotID
		}
		return SeqNew, slot, nil
	}

	// Step 5: Retry (seqid == cached seqid)
	if seqID == slot.SeqID {
		if slot.InUse {
			// The original request is still in flight -- tell client to wait.
			return SeqMisordered, nil, &NFS4StateError{
				Status:  types.NFS4ERR_DELAY,
				Message: "retry while original request in flight",
			}
		}
		if slot.CachedReply == nil {
			// No cached reply available (sa_cachethis was false).
			return SeqMisordered, nil, &NFS4StateError{
				Status:  types.NFS4ERR_RETRY_UNCACHED_REP,
				Message: "retry of uncached reply",
			}
		}
		return SeqRetry, slot, nil
	}

	// Step 6: Misordered (gap or behind)
	return SeqMisordered, nil, &NFS4StateError{
		Status:  types.NFS4ERR_SEQ_MISORDERED,
		Message: "sequence ID misordered",
	}
}

// CompleteSlotRequest marks a slot as completed, stores the sequence ID,
// and optionally caches the reply bytes.
//
// Called after the full COMPOUND response has been encoded and is ready
// to send to the client.
//
// If cacheThis is true, reply bytes are copied into the slot's CachedReply.
// If false, CachedReply is set to nil.
//
// Thread-safe: acquires st.mu.
func (st *SlotTable) CompleteSlotRequest(slotID, seqID uint32, cacheThis bool, reply []byte) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if slotID >= st.maxSlots {
		return
	}

	slot := &st.slots[slotID]
	slot.SeqID = seqID
	slot.InUse = false

	if cacheThis && reply != nil {
		// Copy reply bytes to avoid holding references to caller's buffer.
		slot.CachedReply = make([]byte, len(reply))
		copy(slot.CachedReply, reply)
	} else {
		slot.CachedReply = nil
	}

	if slotID > st.highestSlotID {
		st.highestSlotID = slotID
	}
}

// SetTargetHighestSlotID sets the desired maximum slot ID for dynamic
// flow control. The target is clamped to maxSlots-1.
//
// The client reads sr_target_highest_slotid from SEQUENCE responses
// and adjusts its slot usage accordingly (reducing parallelism under
// server pressure, or increasing it when resources are available).
//
// Thread-safe: acquires st.mu.
func (st *SlotTable) SetTargetHighestSlotID(target uint32) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if target >= st.maxSlots {
		target = st.maxSlots - 1
	}
	st.targetHighestSlotID = target
}

// GetHighestSlotID returns the highest slot ID that has been used.
//
// Thread-safe: acquires st.mu.
func (st *SlotTable) GetHighestSlotID() uint32 {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.highestSlotID
}

// GetTargetHighestSlotID returns the server's desired maximum slot ID.
//
// Thread-safe: acquires st.mu.
func (st *SlotTable) GetTargetHighestSlotID() uint32 {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.targetHighestSlotID
}

// MaxSlots returns the total number of allocated slots.
// This value is immutable after creation, so no lock is needed.
func (st *SlotTable) MaxSlots() uint32 {
	return st.maxSlots
}

// SlotsInUse returns the count of slots currently in use (InUse == true).
// Thread-safe: acquires st.mu.
func (st *SlotTable) SlotsInUse() int {
	st.mu.Lock()
	defer st.mu.Unlock()

	count := 0
	for i := uint32(0); i < st.maxSlots; i++ {
		if st.slots[i].InUse {
			count++
		}
	}
	return count
}

// HasInFlightRequests returns true if any slot in the table is currently
// processing a request (InUse == true).
// Thread-safe: acquires st.mu.
func (st *SlotTable) HasInFlightRequests() bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	for i := uint32(0); i < st.maxSlots; i++ {
		if st.slots[i].InUse {
			return true
		}
	}
	return false
}
