package state

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ============================================================================
// TestNewSession_Basic
// ============================================================================

func TestNewSession_Basic(t *testing.T) {
	foreAttrs := types.ChannelAttrs{MaxRequests: 16}
	backAttrs := types.ChannelAttrs{MaxRequests: 8}
	flags := uint32(types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN)
	cbProgram := uint32(0x40000000)

	sess, err := NewSession(42, foreAttrs, backAttrs, flags, cbProgram)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	// SessionID should not be all zeros (crypto/rand generated)
	allZero := true
	for _, b := range sess.SessionID {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("SessionID is all zeros, expected crypto/rand generated")
	}

	// ClientID
	if sess.ClientID != 42 {
		t.Errorf("ClientID = %d, want 42", sess.ClientID)
	}

	// ForeChannelSlots
	if sess.ForeChannelSlots == nil {
		t.Fatal("ForeChannelSlots is nil")
	}
	if sess.ForeChannelSlots.MaxSlots() != 16 {
		t.Errorf("ForeChannelSlots.MaxSlots() = %d, want 16", sess.ForeChannelSlots.MaxSlots())
	}

	// BackChannelSlots (should be non-nil because CONN_BACK_CHAN is set)
	if sess.BackChannelSlots == nil {
		t.Fatal("BackChannelSlots is nil, expected non-nil with CONN_BACK_CHAN flag")
	}
	if sess.BackChannelSlots.MaxSlots() != 8 {
		t.Errorf("BackChannelSlots.MaxSlots() = %d, want 8", sess.BackChannelSlots.MaxSlots())
	}

	// Flags
	if sess.Flags != flags {
		t.Errorf("Flags = %d, want %d", sess.Flags, flags)
	}

	// CbProgram
	if sess.CbProgram != cbProgram {
		t.Errorf("CbProgram = 0x%x, want 0x%x", sess.CbProgram, cbProgram)
	}

	// CreatedAt should be within the last second
	if time.Since(sess.CreatedAt) > time.Second {
		t.Errorf("CreatedAt is too old: %v (now: %v)", sess.CreatedAt, time.Now())
	}

	// Channel attributes stored
	if sess.ForeChannelAttrs.MaxRequests != 16 {
		t.Errorf("ForeChannelAttrs.MaxRequests = %d, want 16", sess.ForeChannelAttrs.MaxRequests)
	}
	if sess.BackChannelAttrs.MaxRequests != 8 {
		t.Errorf("BackChannelAttrs.MaxRequests = %d, want 8", sess.BackChannelAttrs.MaxRequests)
	}
}

// ============================================================================
// TestNewSession_NoBackChannel
// ============================================================================

func TestNewSession_NoBackChannel(t *testing.T) {
	foreAttrs := types.ChannelAttrs{MaxRequests: 16}
	backAttrs := types.ChannelAttrs{MaxRequests: 8}
	flags := uint32(0) // No CONN_BACK_CHAN

	sess, err := NewSession(1, foreAttrs, backAttrs, flags, 0)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	// ForeChannelSlots should always be created
	if sess.ForeChannelSlots == nil {
		t.Fatal("ForeChannelSlots is nil")
	}

	// BackChannelSlots should be nil when CONN_BACK_CHAN is not set
	if sess.BackChannelSlots != nil {
		t.Error("BackChannelSlots should be nil when CONN_BACK_CHAN flag is not set")
	}
}

// ============================================================================
// TestNewSession_UniqueSessionIDs
// ============================================================================

func TestNewSession_UniqueSessionIDs(t *testing.T) {
	foreAttrs := types.ChannelAttrs{MaxRequests: 4}
	backAttrs := types.ChannelAttrs{}
	seen := make(map[types.SessionId4]bool)

	for i := 0; i < 100; i++ {
		sess, err := NewSession(uint64(i), foreAttrs, backAttrs, 0, 0)
		if err != nil {
			t.Fatalf("NewSession() error = %v at iteration %d", err, i)
		}
		if seen[sess.SessionID] {
			t.Fatalf("duplicate session ID at iteration %d: %s", i, sess.SessionID.String())
		}
		seen[sess.SessionID] = true
	}

	if len(seen) != 100 {
		t.Errorf("expected 100 unique session IDs, got %d", len(seen))
	}
}

// ============================================================================
// TestNewSession_ForeChannelSlotTableWorks
// ============================================================================

func TestNewSession_ForeChannelSlotTableWorks(t *testing.T) {
	foreAttrs := types.ChannelAttrs{MaxRequests: 4}
	backAttrs := types.ChannelAttrs{}

	sess, err := NewSession(1, foreAttrs, backAttrs, 0, 0)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	st := sess.ForeChannelSlots

	// First request: slot 0, seqID 1
	result, _, err := st.ValidateSequence(0, 1)
	if err != nil {
		t.Fatalf("ValidateSequence(0, 1) error = %v", err)
	}
	if result != SeqNew {
		t.Fatalf("result = %d, want SeqNew", result)
	}

	// ValidateSequence atomically marks slot in-use; complete with cached reply
	st.CompleteSlotRequest(0, 1, true, []byte("test-reply"))

	// Retry: same seqID should return SeqRetry with cached reply
	result, slot, err := st.ValidateSequence(0, 1)
	if err != nil {
		t.Fatalf("ValidateSequence(0, 1) retry error = %v", err)
	}
	if result != SeqRetry {
		t.Fatalf("result = %d, want SeqRetry", result)
	}
	if string(slot.CachedReply) != "test-reply" {
		t.Errorf("CachedReply = %q, want %q", slot.CachedReply, "test-reply")
	}

	// Next request: seqID 2
	result, _, err = st.ValidateSequence(0, 2)
	if err != nil {
		t.Fatalf("ValidateSequence(0, 2) error = %v", err)
	}
	if result != SeqNew {
		t.Fatalf("result = %d, want SeqNew", result)
	}
}

// ============================================================================
// TestNewSession_SlotCountClamping
// ============================================================================

func TestNewSession_SlotCountClamping(t *testing.T) {
	backAttrs := types.ChannelAttrs{}

	t.Run("zero clamped to MinSlots", func(t *testing.T) {
		foreAttrs := types.ChannelAttrs{MaxRequests: 0}
		sess, err := NewSession(1, foreAttrs, backAttrs, 0, 0)
		if err != nil {
			t.Fatalf("NewSession() error = %v", err)
		}
		if sess.ForeChannelSlots.MaxSlots() != MinSlots {
			t.Errorf("MaxSlots() = %d, want %d (MinSlots)", sess.ForeChannelSlots.MaxSlots(), MinSlots)
		}
	})

	t.Run("large value clamped to DefaultMaxSlots", func(t *testing.T) {
		foreAttrs := types.ChannelAttrs{MaxRequests: 1000}
		sess, err := NewSession(1, foreAttrs, backAttrs, 0, 0)
		if err != nil {
			t.Fatalf("NewSession() error = %v", err)
		}
		if sess.ForeChannelSlots.MaxSlots() != DefaultMaxSlots {
			t.Errorf("MaxSlots() = %d, want %d (DefaultMaxSlots)", sess.ForeChannelSlots.MaxSlots(), DefaultMaxSlots)
		}
	})
}
