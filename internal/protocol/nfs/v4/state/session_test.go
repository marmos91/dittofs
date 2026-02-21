package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ============================================================================
// Helper: register a v4.1 client and return its clientID and initial seqID
// ============================================================================

func registerV41Client(t *testing.T, sm *StateManager) (uint64, uint32) {
	t.Helper()
	ownerID := []byte("session-test-" + t.Name())
	var verifier [8]byte
	copy(verifier[:], "verify01")

	result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID error: %v", err)
	}
	return result.ClientID, result.SequenceID
}

func defaultForeAttrs() types.ChannelAttrs {
	return types.ChannelAttrs{
		HeaderPadSize:         0,
		MaxRequestSize:        1048576,
		MaxResponseSize:       1048576,
		MaxResponseSizeCached: 65536,
		MaxOperations:         0,
		MaxRequests:           32,
	}
}

func defaultBackAttrs() types.ChannelAttrs {
	return types.ChannelAttrs{
		HeaderPadSize:         0,
		MaxRequestSize:        65536,
		MaxResponseSize:       65536,
		MaxResponseSizeCached: 65536,
		MaxOperations:         0,
		MaxRequests:           4,
	}
}

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

// ============================================================================
// CreateSession Tests (Phase 19)
// ============================================================================

func TestCreateSession_Success(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	result, cached, err := sm.CreateSession(
		clientID, seqID+1, types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
		defaultForeAttrs(), defaultBackAttrs(), 0x40000000, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if cached != nil {
		t.Fatal("Expected nil cached bytes for new session")
	}
	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	// Verify session ID is non-zero
	var zeroSID types.SessionId4
	if result.SessionID == zeroSID {
		t.Error("SessionID should not be zero")
	}

	// Verify sequence ID was incremented
	if result.SequenceID != seqID+1 {
		t.Errorf("SequenceID = %d, want %d", result.SequenceID, seqID+1)
	}

	// Verify PERSIST flag was cleared
	if result.Flags&types.CREATE_SESSION4_FLAG_PERSIST != 0 {
		t.Error("PERSIST flag should be cleared")
	}

	// Verify CONN_BACK_CHAN is set (we requested it)
	if result.Flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN == 0 {
		t.Error("CONN_BACK_CHAN flag should be set when requested")
	}

	// Verify session is retrievable
	session := sm.GetSession(result.SessionID)
	if session == nil {
		t.Fatal("GetSession returned nil for created session")
	}
	if session.ClientID != clientID {
		t.Errorf("Session.ClientID = %d, want %d", session.ClientID, clientID)
	}
}

func TestCreateSession_ConfirmsClient(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// Before CREATE_SESSION, client should not be confirmed
	sm.mu.RLock()
	record := sm.v41ClientsByID[clientID]
	confirmed := record.Confirmed
	sm.mu.RUnlock()

	if confirmed {
		t.Fatal("Client should not be confirmed before CREATE_SESSION")
	}

	_, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// After CREATE_SESSION, client should be confirmed with a lease
	sm.mu.RLock()
	record = sm.v41ClientsByID[clientID]
	confirmed = record.Confirmed
	hasLease := record.Lease != nil
	sm.mu.RUnlock()

	if !confirmed {
		t.Error("Client should be confirmed after first CREATE_SESSION")
	}
	if !hasLease {
		t.Error("Client should have a lease after first CREATE_SESSION")
	}
}

func TestCreateSession_UnknownClient(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	_, _, err := sm.CreateSession(
		99999, 2, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err == nil {
		t.Fatal("Expected error for unknown client")
	}
	if !errors.Is(err, ErrStaleClientID) {
		t.Errorf("Expected ErrStaleClientID, got: %v", err)
	}
}

func TestCreateSession_Replay(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// First request
	_, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession #1 error: %v", err)
	}

	// Cache the response (handler normally does this)
	sm.CacheCreateSessionResponse(clientID, []byte("cached-response-bytes"))

	// Replay with same seqid (which is now seqID+1 after increment)
	replayResult, cached, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession replay error: %v", err)
	}
	if replayResult != nil {
		t.Error("Replay should return nil result")
	}
	if cached == nil {
		t.Fatal("Replay should return cached bytes")
	}
	if string(cached) != "cached-response-bytes" {
		t.Errorf("Cached bytes = %q, want %q", string(cached), "cached-response-bytes")
	}

	// Second session should NOT have been created
	sessions := sm.ListSessionsForClient(clientID)
	if len(sessions) != 1 {
		t.Errorf("Expected 1 session after replay, got %d", len(sessions))
	}
}

func TestCreateSession_ReplayNoCachedResponse(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// First request - but don't cache the response
	_, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Replay with same seqid (record's SequenceID is now seqID+1)
	_, _, err = sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err == nil {
		t.Fatal("Expected error for replay without cached response")
	}
	if !errors.Is(err, ErrSeqMisordered) {
		t.Errorf("Expected ErrSeqMisordered, got: %v", err)
	}
}

func TestCreateSession_MisorderedSeqID(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// Send with wrong seqid (too high)
	_, _, err := sm.CreateSession(
		clientID, seqID+5, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err == nil {
		t.Fatal("Expected error for misordered seqid")
	}
	if !errors.Is(err, ErrSeqMisordered) {
		t.Errorf("Expected ErrSeqMisordered, got: %v", err)
	}

	// Send with seqid lower than current (seqID=1 initially, so try 0)
	_, _, err = sm.CreateSession(
		clientID, 0, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err == nil {
		t.Fatal("Expected error for old seqid")
	}
	if !errors.Is(err, ErrSeqMisordered) {
		t.Errorf("Expected ErrSeqMisordered, got: %v", err)
	}
}

func TestCreateSession_PerClientLimit(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	sm.mu.Lock()
	sm.maxSessionsPerClient = 3
	sm.mu.Unlock()

	clientID, seqID := registerV41Client(t, sm)

	// Create 3 sessions (the max)
	for i := 0; i < 3; i++ {
		_, _, err := sm.CreateSession(
			clientID, seqID+uint32(i)+1, 0,
			defaultForeAttrs(), defaultBackAttrs(), 0, nil,
		)
		if err != nil {
			t.Fatalf("CreateSession #%d error: %v", i+1, err)
		}
		// Cache response so replay detection works
		sm.CacheCreateSessionResponse(clientID, []byte("cached"))
	}

	// 4th should fail
	_, _, err := sm.CreateSession(
		clientID, seqID+4, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err == nil {
		t.Fatal("Expected error for session limit exceeded")
	}
	if !errors.Is(err, ErrTooManySessions) {
		t.Errorf("Expected ErrTooManySessions, got: %v", err)
	}
}

func TestCreateSession_PersistFlagCleared(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	result, _, err := sm.CreateSession(
		clientID, seqID+1,
		types.CREATE_SESSION4_FLAG_PERSIST|types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	if result.Flags&types.CREATE_SESSION4_FLAG_PERSIST != 0 {
		t.Error("PERSIST flag should be cleared in response")
	}
	if result.Flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN == 0 {
		t.Error("CONN_BACK_CHAN flag should be preserved")
	}
}

func TestCreateSession_ConnBackChanFlag(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// Without CONN_BACK_CHAN
	result, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	if result.Flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN != 0 {
		t.Error("CONN_BACK_CHAN should not be set when not requested")
	}

	// Verify back channel slot table was NOT created
	session := sm.GetSession(result.SessionID)
	if session.BackChannelSlots != nil {
		t.Error("BackChannelSlots should be nil when CONN_BACK_CHAN not requested")
	}
}

func TestCreateSession_ChannelNegotiation(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// Request with values exceeding server limits
	oversizedFore := types.ChannelAttrs{
		MaxRequestSize:        5000000,  // Exceeds 1MB limit
		MaxResponseSize:       5000000,  // Exceeds 1MB limit
		MaxResponseSizeCached: 1000000,  // Exceeds 64KB limit
		MaxRequests:           200,      // Exceeds 64 slot limit
		HeaderPadSize:         100,      // Should be forced to 0
		RdmaIrd:               []uint32{4},
	}

	result, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		oversizedFore, defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Verify clamped values
	if result.ForeChannelAttrs.MaxRequestSize != 1048576 {
		t.Errorf("MaxRequestSize = %d, want 1048576", result.ForeChannelAttrs.MaxRequestSize)
	}
	if result.ForeChannelAttrs.MaxResponseSize != 1048576 {
		t.Errorf("MaxResponseSize = %d, want 1048576", result.ForeChannelAttrs.MaxResponseSize)
	}
	if result.ForeChannelAttrs.MaxResponseSizeCached != 65536 {
		t.Errorf("MaxResponseSizeCached = %d, want 65536", result.ForeChannelAttrs.MaxResponseSizeCached)
	}
	if result.ForeChannelAttrs.MaxRequests != 64 {
		t.Errorf("MaxRequests = %d, want 64", result.ForeChannelAttrs.MaxRequests)
	}
	if result.ForeChannelAttrs.HeaderPadSize != 0 {
		t.Errorf("HeaderPadSize = %d, want 0", result.ForeChannelAttrs.HeaderPadSize)
	}
	if result.ForeChannelAttrs.RdmaIrd != nil {
		t.Errorf("RdmaIrd = %v, want nil", result.ForeChannelAttrs.RdmaIrd)
	}
}

// ============================================================================
// DestroySession Tests (Phase 19)
// ============================================================================

func TestDestroySession_Success(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	result, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	err = sm.DestroySession(result.SessionID)
	if err != nil {
		t.Fatalf("DestroySession error: %v", err)
	}

	// Verify session is gone
	if sm.GetSession(result.SessionID) != nil {
		t.Error("Session should be nil after destroy")
	}

	// Verify session list is empty
	sessions := sm.ListSessionsForClient(clientID)
	if len(sessions) != 0 {
		t.Errorf("Expected 0 sessions after destroy, got %d", len(sessions))
	}
}

func TestDestroySession_NotFound(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	var fakeSID types.SessionId4
	copy(fakeSID[:], "fake-session-id!")

	err := sm.DestroySession(fakeSID)
	if err == nil {
		t.Fatal("Expected error for non-existent session")
	}
	if !errors.Is(err, ErrBadSession) {
		t.Errorf("Expected ErrBadSession, got: %v", err)
	}
}

func TestDestroySession_InFlightRequest(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	result, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Simulate an in-flight request by marking a slot in-use
	session := sm.GetSession(result.SessionID)
	session.ForeChannelSlots.mu.Lock()
	session.ForeChannelSlots.slots[0].InUse = true
	session.ForeChannelSlots.mu.Unlock()

	// DestroySession should return ErrDelay
	err = sm.DestroySession(result.SessionID)
	if err == nil {
		t.Fatal("Expected error for in-flight request")
	}
	if !errors.Is(err, ErrDelay) {
		t.Errorf("Expected ErrDelay, got: %v", err)
	}

	// Session should still exist
	if sm.GetSession(result.SessionID) == nil {
		t.Error("Session should still exist after ErrDelay")
	}
}

func TestForceDestroySession_BypassesInFlight(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	result, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Simulate an in-flight request
	session := sm.GetSession(result.SessionID)
	session.ForeChannelSlots.mu.Lock()
	session.ForeChannelSlots.slots[0].InUse = true
	session.ForeChannelSlots.mu.Unlock()

	// ForceDestroySession should bypass in-flight check
	err = sm.ForceDestroySession(result.SessionID)
	if err != nil {
		t.Fatalf("ForceDestroySession error: %v", err)
	}

	// Session should be gone
	if sm.GetSession(result.SessionID) != nil {
		t.Error("Session should be nil after ForceDestroySession")
	}
}

// ============================================================================
// Channel Negotiation Tests (Phase 19)
// ============================================================================

func TestNegotiateChannelAttrs_ClampToLimits(t *testing.T) {
	limits := ChannelLimits{
		MaxSlots:              64,
		MaxRequestSize:        1048576,
		MaxResponseSize:       1048576,
		MaxResponseSizeCached: 65536,
		MinRequestSize:        8192,
		MinResponseSize:       8192,
	}

	// Request exceeding limits
	requested := types.ChannelAttrs{
		HeaderPadSize:         100,
		MaxRequestSize:        2000000,
		MaxResponseSize:       2000000,
		MaxResponseSizeCached: 100000,
		MaxOperations:         10,
		MaxRequests:           200,
		RdmaIrd:               []uint32{4},
	}

	result := negotiateChannelAttrs(requested, limits)

	if result.HeaderPadSize != 0 {
		t.Errorf("HeaderPadSize = %d, want 0", result.HeaderPadSize)
	}
	if result.MaxRequestSize != 1048576 {
		t.Errorf("MaxRequestSize = %d, want 1048576", result.MaxRequestSize)
	}
	if result.MaxResponseSize != 1048576 {
		t.Errorf("MaxResponseSize = %d, want 1048576", result.MaxResponseSize)
	}
	if result.MaxResponseSizeCached != 65536 {
		t.Errorf("MaxResponseSizeCached = %d, want 65536", result.MaxResponseSizeCached)
	}
	if result.MaxOperations != 0 {
		t.Errorf("MaxOperations = %d, want 0", result.MaxOperations)
	}
	if result.MaxRequests != 64 {
		t.Errorf("MaxRequests = %d, want 64", result.MaxRequests)
	}
	if result.RdmaIrd != nil {
		t.Errorf("RdmaIrd = %v, want nil", result.RdmaIrd)
	}
}

func TestNegotiateChannelAttrs_BelowFloor(t *testing.T) {
	limits := DefaultForeLimits()

	// Request below floor
	requested := types.ChannelAttrs{
		MaxRequestSize:  100,
		MaxResponseSize: 100,
		MaxRequests:     0,
	}

	result := negotiateChannelAttrs(requested, limits)

	if result.MaxRequestSize != 8192 {
		t.Errorf("MaxRequestSize = %d, want 8192 (floor)", result.MaxRequestSize)
	}
	if result.MaxResponseSize != 8192 {
		t.Errorf("MaxResponseSize = %d, want 8192 (floor)", result.MaxResponseSize)
	}
	if result.MaxRequests != 1 {
		t.Errorf("MaxRequests = %d, want 1 (minimum)", result.MaxRequests)
	}
}

func TestNegotiateChannelAttrs_BackChannelSmaller(t *testing.T) {
	backLimits := DefaultBackLimits()

	requested := types.ChannelAttrs{
		MaxRequestSize:  1048576,
		MaxResponseSize: 1048576,
		MaxRequests:     64,
	}

	result := negotiateChannelAttrs(requested, backLimits)

	if result.MaxRequests != 8 {
		t.Errorf("Back channel MaxRequests = %d, want 8", result.MaxRequests)
	}
	if result.MaxRequestSize != 65536 {
		t.Errorf("Back channel MaxRequestSize = %d, want 65536", result.MaxRequestSize)
	}
	if result.MaxResponseSize != 65536 {
		t.Errorf("Back channel MaxResponseSize = %d, want 65536", result.MaxResponseSize)
	}
}

func TestNegotiateChannelAttrs_WithinLimits(t *testing.T) {
	limits := DefaultForeLimits()

	requested := types.ChannelAttrs{
		MaxRequestSize:        524288,
		MaxResponseSize:       524288,
		MaxResponseSizeCached: 32768,
		MaxRequests:           16,
	}

	result := negotiateChannelAttrs(requested, limits)

	if result.MaxRequestSize != 524288 {
		t.Errorf("MaxRequestSize = %d, want 524288 (unchanged)", result.MaxRequestSize)
	}
	if result.MaxResponseSize != 524288 {
		t.Errorf("MaxResponseSize = %d, want 524288 (unchanged)", result.MaxResponseSize)
	}
	if result.MaxResponseSizeCached != 32768 {
		t.Errorf("MaxResponseSizeCached = %d, want 32768 (unchanged)", result.MaxResponseSizeCached)
	}
	if result.MaxRequests != 16 {
		t.Errorf("MaxRequests = %d, want 16 (unchanged)", result.MaxRequests)
	}
}

// ============================================================================
// HasInFlightRequests Tests (Phase 19)
// ============================================================================

func TestSlotTable_HasInFlightRequests(t *testing.T) {
	st := NewSlotTable(8)

	if st.HasInFlightRequests() {
		t.Error("New slot table should not have in-flight requests")
	}

	// Mark a slot in-use
	st.mu.Lock()
	st.slots[3].InUse = true
	st.mu.Unlock()

	if !st.HasInFlightRequests() {
		t.Error("Should have in-flight requests after marking slot in-use")
	}

	// Clear it
	st.mu.Lock()
	st.slots[3].InUse = false
	st.mu.Unlock()

	if st.HasInFlightRequests() {
		t.Error("Should not have in-flight requests after clearing slot")
	}
}

func TestSession_HasInFlightRequests(t *testing.T) {
	fore := types.ChannelAttrs{MaxRequests: 4}
	back := types.ChannelAttrs{MaxRequests: 2}

	session, err := NewSession(1, fore, back, types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN, 0)
	if err != nil {
		t.Fatalf("NewSession error: %v", err)
	}

	if session.HasInFlightRequests() {
		t.Error("New session should not have in-flight requests")
	}

	// Mark a fore channel slot in-use
	session.ForeChannelSlots.mu.Lock()
	session.ForeChannelSlots.slots[0].InUse = true
	session.ForeChannelSlots.mu.Unlock()

	if !session.HasInFlightRequests() {
		t.Error("Session should have in-flight requests")
	}
}

func TestSession_HasInFlightRequests_NilForeChannel(t *testing.T) {
	session := &Session{ForeChannelSlots: nil}
	if session.HasInFlightRequests() {
		t.Error("Session with nil fore channel should not have in-flight requests")
	}
}

// ============================================================================
// Callback Security Tests (Phase 19)
// ============================================================================

func TestHasAcceptableCallbackSecurity(t *testing.T) {
	tests := []struct {
		name     string
		parms    []types.CallbackSecParms4
		expected bool
	}{
		{"empty slice", nil, true},
		{"AUTH_NONE only", []types.CallbackSecParms4{{CbSecFlavor: 0}}, true},
		{"AUTH_SYS only", []types.CallbackSecParms4{{CbSecFlavor: 1}}, true},
		{"RPCSEC_GSS only", []types.CallbackSecParms4{{CbSecFlavor: 6}}, false},
		{"mixed AUTH_NONE and RPCSEC_GSS", []types.CallbackSecParms4{
			{CbSecFlavor: 6},
			{CbSecFlavor: 0},
		}, true},
		{"mixed AUTH_SYS and RPCSEC_GSS", []types.CallbackSecParms4{
			{CbSecFlavor: 6},
			{CbSecFlavor: 1},
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HasAcceptableCallbackSecurity(tt.parms)
			if result != tt.expected {
				t.Errorf("HasAcceptableCallbackSecurity = %v, want %v", result, tt.expected)
			}
		})
	}
}

// ============================================================================
// Session Reaper Tests (Phase 19)
// ============================================================================

func TestReaper_ExpiredLease(t *testing.T) {
	// Use very short lease for testing
	sm := NewStateManager(50 * time.Millisecond)
	clientID, seqID := registerV41Client(t, sm)

	// Create a session (this confirms the client and creates its lease)
	_, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Verify session exists
	if len(sm.ListSessionsForClient(clientID)) != 1 {
		t.Fatal("Expected 1 session")
	}

	// Wait for lease to expire
	time.Sleep(100 * time.Millisecond)

	// Manually trigger reap (instead of waiting for ticker)
	sm.reapExpiredSessions()

	// Session and client should be cleaned up
	if len(sm.ListSessionsForClient(clientID)) != 0 {
		t.Error("Expected 0 sessions after lease expiry reap")
	}

	sm.mu.RLock()
	_, exists := sm.v41ClientsByID[clientID]
	sm.mu.RUnlock()

	if exists {
		t.Error("Client should be purged after lease expiry")
	}
}

func TestReaper_UnconfirmedTimeout(t *testing.T) {
	// Use very short lease for testing
	sm := NewStateManager(25 * time.Millisecond)
	clientID, _ := registerV41Client(t, sm)

	// Client is unconfirmed (no CREATE_SESSION yet)
	sm.mu.RLock()
	record := sm.v41ClientsByID[clientID]
	confirmed := record.Confirmed
	sm.mu.RUnlock()

	if confirmed {
		t.Fatal("Client should be unconfirmed")
	}

	// Wait for 2x lease duration
	time.Sleep(80 * time.Millisecond)

	// Trigger reap
	sm.reapExpiredSessions()

	// Client should be purged
	sm.mu.RLock()
	_, exists := sm.v41ClientsByID[clientID]
	sm.mu.RUnlock()

	if exists {
		t.Error("Unconfirmed client should be purged after 2x lease duration")
	}
}

func TestReaper_ActiveLeaseNotCleaned(t *testing.T) {
	sm := NewStateManager(5 * time.Second)
	clientID, seqID := registerV41Client(t, sm)

	// Create a session (confirms client, starts lease)
	_, _, err := sm.CreateSession(
		clientID, seqID+1, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Trigger reap immediately (lease not expired)
	sm.reapExpiredSessions()

	// Session and client should still exist
	if len(sm.ListSessionsForClient(clientID)) != 1 {
		t.Error("Session should not be cleaned up with active lease")
	}

	sm.mu.RLock()
	_, exists := sm.v41ClientsByID[clientID]
	sm.mu.RUnlock()

	if !exists {
		t.Error("Client should not be purged with active lease")
	}
}

func TestReaper_ContextCancellation(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	ctx, cancel := context.WithCancel(context.Background())

	sm.StartSessionReaper(ctx)

	// Cancel immediately
	cancel()

	// Wait briefly to ensure goroutine exits
	time.Sleep(50 * time.Millisecond)
}

// ============================================================================
// PurgeV41Client Session Cleanup Tests (Phase 19)
// ============================================================================

func TestPurgeV41Client_DestroysAllSessions(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// Create 3 sessions
	sessionIDs := make([]types.SessionId4, 3)
	for i := 0; i < 3; i++ {
		result, _, err := sm.CreateSession(
			clientID, seqID+uint32(i)+1, 0,
			defaultForeAttrs(), defaultBackAttrs(), 0, nil,
		)
		if err != nil {
			t.Fatalf("CreateSession #%d error: %v", i+1, err)
		}
		sm.CacheCreateSessionResponse(clientID, []byte("cached"))
		sessionIDs[i] = result.SessionID
	}

	// Verify sessions exist
	if len(sm.ListSessionsForClient(clientID)) != 3 {
		t.Fatal("Expected 3 sessions")
	}

	// Evict the client (which calls purgeV41Client)
	err := sm.EvictV41Client(clientID)
	if err != nil {
		t.Fatalf("EvictV41Client error: %v", err)
	}

	// All sessions should be gone
	for _, sid := range sessionIDs {
		if sm.GetSession(sid) != nil {
			t.Errorf("Session %s should be nil after eviction", sid.String())
		}
	}

	if len(sm.ListSessionsForClient(clientID)) != 0 {
		t.Error("Expected 0 sessions after eviction")
	}
}

// ============================================================================
// ListSessionsForClient Tests (Phase 19)
// ============================================================================

func TestListSessionsForClient_Empty(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	sessions := sm.ListSessionsForClient(99999)
	if sessions != nil {
		t.Errorf("Expected nil for unknown client, got %d sessions", len(sessions))
	}
}

func TestListSessionsForClient_MultipleSessions(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, seqID := registerV41Client(t, sm)

	// Create 2 sessions
	for i := 0; i < 2; i++ {
		_, _, err := sm.CreateSession(
			clientID, seqID+uint32(i)+1, 0,
			defaultForeAttrs(), defaultBackAttrs(), 0, nil,
		)
		if err != nil {
			t.Fatalf("CreateSession #%d error: %v", i+1, err)
		}
		sm.CacheCreateSessionResponse(clientID, []byte("cached"))
	}

	sessions := sm.ListSessionsForClient(clientID)
	if len(sessions) != 2 {
		t.Errorf("Expected 2 sessions, got %d", len(sessions))
	}
}

// ============================================================================
// CacheCreateSessionResponse Tests (Phase 19)
// ============================================================================

func TestCacheCreateSessionResponse(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	clientID, _ := registerV41Client(t, sm)

	responseBytes := []byte("test-response-bytes-12345")
	sm.CacheCreateSessionResponse(clientID, responseBytes)

	sm.mu.RLock()
	record := sm.v41ClientsByID[clientID]
	cached := record.CachedCreateSessionRes
	sm.mu.RUnlock()

	if string(cached) != string(responseBytes) {
		t.Errorf("Cached = %q, want %q", string(cached), string(responseBytes))
	}

	// Verify it's a copy (modifying original shouldn't affect cached)
	responseBytes[0] = 'X'
	sm.mu.RLock()
	record = sm.v41ClientsByID[clientID]
	cached = record.CachedCreateSessionRes
	sm.mu.RUnlock()

	if cached[0] == 'X' {
		t.Error("Cached bytes should be a copy, not a reference")
	}
}

func TestCacheCreateSessionResponse_UnknownClient(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	// Should not panic
	sm.CacheCreateSessionResponse(99999, []byte("test"))
}

// ============================================================================
// Session Metrics Tests (Phase 19)
// ============================================================================

func TestSessionMetrics_NilSafe(t *testing.T) {
	// Ensure nil metrics don't panic
	var m *SessionMetrics
	m.recordCreated()
	m.recordDestroyed("test", 1.0)
}

func TestSessionMetrics_NonNil(t *testing.T) {
	m := NewSessionMetrics(nil)
	m.recordCreated()
	m.recordDestroyed("client_request", 10.5)
	m.recordDestroyed("admin_evict", 5.0)
	m.recordDestroyed("lease_expired", 100.0)
}
