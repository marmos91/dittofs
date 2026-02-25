package state

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// Helper: register a v4.1 client and create a session, return sessionID
// ============================================================================

func setupClientAndSession(t *testing.T, sm *StateManager) (uint64, types.SessionId4) {
	t.Helper()
	clientID, seqID := registerV41Client(t, sm)

	result, _, err := sm.CreateSession(
		clientID,
		seqID,
		0,
		defaultForeAttrs(),
		defaultBackAttrs(),
		0x40000000,
		nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	return clientID, result.SessionID
}

// ============================================================================
// TestNegotiateDirection
// ============================================================================

func TestNegotiateDirection(t *testing.T) {
	tests := []struct {
		name      string
		clientDir uint32
		wantDir   ConnectionDirection
		wantSrv   uint32
	}{
		{"CDFC4_FORE", types.CDFC4_FORE, ConnDirFore, types.CDFS4_FORE},
		{"CDFC4_BACK", types.CDFC4_BACK, ConnDirBack, types.CDFS4_BACK},
		{"CDFC4_FORE_OR_BOTH", types.CDFC4_FORE_OR_BOTH, ConnDirBoth, types.CDFS4_BOTH},
		{"CDFC4_BACK_OR_BOTH", types.CDFC4_BACK_OR_BOTH, ConnDirBoth, types.CDFS4_BOTH},
		{"unknown_defaults_to_fore", 99, ConnDirFore, types.CDFS4_FORE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, srv := negotiateDirection(tt.clientDir)
			if dir != tt.wantDir {
				t.Errorf("negotiateDirection(%d) dir = %v, want %v", tt.clientDir, dir, tt.wantDir)
			}
			if srv != tt.wantSrv {
				t.Errorf("negotiateDirection(%d) serverDir = %d, want %d", tt.clientDir, srv, tt.wantSrv)
			}
		})
	}
}

// ============================================================================
// TestBindConnToSession_Basic
// ============================================================================

func TestBindConnToSession_Basic(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	result, err := sm.BindConnToSession(100, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("BindConnToSession error: %v", err)
	}
	if result.ServerDir != types.CDFS4_FORE {
		t.Errorf("ServerDir = %d, want %d", result.ServerDir, types.CDFS4_FORE)
	}

	// Verify connection is stored
	binding := sm.GetConnectionBinding(100)
	if binding == nil {
		t.Fatal("expected binding to exist")
	}
	if binding.Direction != ConnDirFore {
		t.Errorf("Direction = %v, want %v", binding.Direction, ConnDirFore)
	}
	if binding.SessionID != sessionID {
		t.Error("SessionID mismatch")
	}
}

// ============================================================================
// TestBindConnToSession_RebindDirection
// ============================================================================

func TestBindConnToSession_RebindDirection(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	// First bind as fore
	_, err := sm.BindConnToSession(200, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("first bind error: %v", err)
	}

	// Rebind as both
	result, err := sm.BindConnToSession(200, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("rebind error: %v", err)
	}
	if result.ServerDir != types.CDFS4_BOTH {
		t.Errorf("ServerDir after rebind = %d, want %d", result.ServerDir, types.CDFS4_BOTH)
	}

	// Verify direction updated
	binding := sm.GetConnectionBinding(200)
	if binding == nil {
		t.Fatal("expected binding to exist after rebind")
	}
	if binding.Direction != ConnDirBoth {
		t.Errorf("Direction = %v, want %v", binding.Direction, ConnDirBoth)
	}

	// Verify only one binding exists for this session
	bindings := sm.GetConnectionBindings(sessionID)
	if len(bindings) != 1 {
		t.Errorf("expected 1 binding, got %d", len(bindings))
	}
}

// ============================================================================
// TestBindConnToSession_SilentUnbindFromOtherSession
// ============================================================================

func TestBindConnToSession_SilentUnbindFromOtherSession(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionA := setupClientAndSession(t, sm)
	_, sessionB := setupClientAndSession(t, sm)

	// Bind conn 300 to session A
	_, err := sm.BindConnToSession(300, sessionA, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("bind to A error: %v", err)
	}

	// Bind conn 300 to session B (should silently unbind from A)
	_, err = sm.BindConnToSession(300, sessionB, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("bind to B error: %v", err)
	}

	// Verify removed from session A
	bindingsA := sm.GetConnectionBindings(sessionA)
	if len(bindingsA) != 0 {
		t.Errorf("session A should have 0 bindings, got %d", len(bindingsA))
	}

	// Verify present in session B
	bindingsB := sm.GetConnectionBindings(sessionB)
	if len(bindingsB) != 1 {
		t.Errorf("session B should have 1 binding, got %d", len(bindingsB))
	}
}

// ============================================================================
// TestBindConnToSession_ConnectionLimit
// ============================================================================

func TestBindConnToSession_ConnectionLimit(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxConnectionsPerSession(3) // small limit for testing
	_, sessionID := setupClientAndSession(t, sm)

	// Bind 3 connections (at limit)
	for i := uint64(1); i <= 3; i++ {
		_, err := sm.BindConnToSession(i, sessionID, types.CDFC4_FORE)
		if err != nil {
			t.Fatalf("bind conn %d error: %v", i, err)
		}
	}

	// 4th connection should fail with NFS4ERR_RESOURCE
	_, err := sm.BindConnToSession(4, sessionID, types.CDFC4_FORE)
	if err == nil {
		t.Fatal("expected error for 4th connection, got nil")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_RESOURCE {
		t.Errorf("status = %d, want %d (NFS4ERR_RESOURCE)", stateErr.Status, types.NFS4ERR_RESOURCE)
	}
}

// ============================================================================
// TestBindConnToSession_RebindAtLimit
// ============================================================================

func TestBindConnToSession_RebindAtLimit(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxConnectionsPerSession(3)
	_, sessionID := setupClientAndSession(t, sm)

	// Bind 3 connections
	for i := uint64(1); i <= 3; i++ {
		_, err := sm.BindConnToSession(i, sessionID, types.CDFC4_FORE)
		if err != nil {
			t.Fatalf("bind conn %d error: %v", i, err)
		}
	}

	// Rebinding existing connection (conn 2) should succeed even at limit
	result, err := sm.BindConnToSession(2, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("rebind at limit error: %v", err)
	}
	if result.ServerDir != types.CDFS4_BOTH {
		t.Errorf("ServerDir = %d, want %d", result.ServerDir, types.CDFS4_BOTH)
	}
}

// ============================================================================
// TestBindConnToSession_ForeChannelEnforcement
// ============================================================================

func TestBindConnToSession_ForeChannelEnforcement(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	// Bind a single fore-channel connection
	_, err := sm.BindConnToSession(500, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("initial bind error: %v", err)
	}

	// Try to rebind that same connection as back-only: should fail (leaves zero fore)
	_, err = sm.BindConnToSession(500, sessionID, types.CDFC4_BACK)
	if err == nil {
		t.Fatal("expected error when rebinding sole fore connection as back-only")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_INVAL {
		t.Errorf("status = %d, want %d (NFS4ERR_INVAL)", stateErr.Status, types.NFS4ERR_INVAL)
	}

	// Add a second fore connection, then try back-only on 500 again (should now succeed)
	_, err = sm.BindConnToSession(501, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("bind second fore error: %v", err)
	}

	_, err = sm.BindConnToSession(500, sessionID, types.CDFC4_BACK)
	if err != nil {
		t.Fatalf("rebind as back with second fore present error: %v", err)
	}
}

// ============================================================================
// TestBindConnToSession_BadSession
// ============================================================================

func TestBindConnToSession_BadSession(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Try to bind to a non-existent session
	var fakeSessionID types.SessionId4
	copy(fakeSessionID[:], "nonexistent01234")

	_, err := sm.BindConnToSession(600, fakeSessionID, types.CDFC4_FORE)
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("status = %d, want %d (NFS4ERR_BADSESSION)", stateErr.Status, types.NFS4ERR_BADSESSION)
	}
}

// ============================================================================
// TestUnbindConnection
// ============================================================================

func TestUnbindConnection(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	_, err := sm.BindConnToSession(700, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("bind error: %v", err)
	}

	// Verify it's there
	if sm.GetConnectionBinding(700) == nil {
		t.Fatal("expected binding to exist before unbind")
	}

	// Unbind
	sm.UnbindConnection(700)

	// Verify removed from both maps
	if sm.GetConnectionBinding(700) != nil {
		t.Error("expected binding to be nil after unbind")
	}
	bindings := sm.GetConnectionBindings(sessionID)
	if len(bindings) != 0 {
		t.Errorf("expected 0 bindings for session, got %d", len(bindings))
	}
}

// ============================================================================
// TestUnbindAllForSession
// ============================================================================

func TestUnbindAllForSession(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	// Bind 3 connections
	for i := uint64(800); i < 803; i++ {
		_, err := sm.BindConnToSession(i, sessionID, types.CDFC4_FORE)
		if err != nil {
			t.Fatalf("bind conn %d error: %v", i, err)
		}
	}

	// Verify all present
	bindings := sm.GetConnectionBindings(sessionID)
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings, got %d", len(bindings))
	}

	// Unbind all
	sm.UnbindAllForSession(sessionID)

	// Verify all removed
	for i := uint64(800); i < 803; i++ {
		if sm.GetConnectionBinding(i) != nil {
			t.Errorf("conn %d still exists after UnbindAllForSession", i)
		}
	}
	bindings = sm.GetConnectionBindings(sessionID)
	if len(bindings) != 0 {
		t.Errorf("expected 0 bindings, got %d", len(bindings))
	}
}

// ============================================================================
// TestGetConnectionBindings
// ============================================================================

func TestGetConnectionBindings(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	// Bind 3 connections with different directions
	_, _ = sm.BindConnToSession(900, sessionID, types.CDFC4_FORE)
	_, _ = sm.BindConnToSession(901, sessionID, types.CDFC4_FORE_OR_BOTH)
	_, _ = sm.BindConnToSession(902, sessionID, types.CDFC4_FORE)

	bindings := sm.GetConnectionBindings(sessionID)
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings, got %d", len(bindings))
	}

	// Verify they are copies (modifying returned struct doesn't affect original)
	bindings[0].Draining = true
	original := sm.GetConnectionBinding(900)
	if original.Draining {
		t.Error("modifying returned binding should not affect internal state")
	}
}

// ============================================================================
// TestDestroySession_UnbindsConnections
// ============================================================================

func TestDestroySession_UnbindsConnections(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	// Bind connections
	for i := uint64(1000); i < 1003; i++ {
		_, err := sm.BindConnToSession(i, sessionID, types.CDFC4_FORE)
		if err != nil {
			t.Fatalf("bind conn %d error: %v", i, err)
		}
	}

	// Destroy the session
	err := sm.DestroySession(sessionID)
	if err != nil {
		t.Fatalf("DestroySession error: %v", err)
	}

	// Verify all connections are unbound
	for i := uint64(1000); i < 1003; i++ {
		if sm.GetConnectionBinding(i) != nil {
			t.Errorf("conn %d still bound after session destroy", i)
		}
	}
	bindings := sm.GetConnectionBindings(sessionID)
	if len(bindings) != 0 {
		t.Errorf("expected 0 bindings after destroy, got %d", len(bindings))
	}
}

// ============================================================================
// TestSetConnectionDraining
// ============================================================================

func TestSetConnectionDraining(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	_, err := sm.BindConnToSession(1100, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("bind error: %v", err)
	}

	// Initially not draining
	if sm.IsConnectionDraining(1100) {
		t.Error("expected not draining initially")
	}

	// Set draining
	if err := sm.SetConnectionDraining(1100, true); err != nil {
		t.Fatalf("SetConnectionDraining error: %v", err)
	}
	if !sm.IsConnectionDraining(1100) {
		t.Error("expected draining after SetConnectionDraining(true)")
	}

	// Unset draining
	if err := sm.SetConnectionDraining(1100, false); err != nil {
		t.Fatalf("SetConnectionDraining(false) error: %v", err)
	}
	if sm.IsConnectionDraining(1100) {
		t.Error("expected not draining after SetConnectionDraining(false)")
	}

	// Error for non-existent connection
	if err := sm.SetConnectionDraining(9999, true); err == nil {
		t.Error("expected error for non-existent connection")
	}
}

// ============================================================================
// TestUpdateConnectionActivity
// ============================================================================

func TestUpdateConnectionActivity(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	_, sessionID := setupClientAndSession(t, sm)

	_, err := sm.BindConnToSession(1200, sessionID, types.CDFC4_FORE)
	if err != nil {
		t.Fatalf("bind error: %v", err)
	}

	// Record initial activity
	binding := sm.GetConnectionBinding(1200)
	initialActivity := binding.LastActivity

	// Wait briefly then update
	time.Sleep(2 * time.Millisecond)
	sm.UpdateConnectionActivity(1200)

	// Verify timestamp changed
	binding = sm.GetConnectionBinding(1200)
	if !binding.LastActivity.After(initialActivity) {
		t.Error("expected LastActivity to advance after update")
	}
}

// ============================================================================
// TestIsConnectionDraining_NotFound
// ============================================================================

func TestIsConnectionDraining_NotFound(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Non-existent connection should return false (not draining)
	if sm.IsConnectionDraining(9999) {
		t.Error("expected false for non-existent connection")
	}
}

// ============================================================================
// TestConnectionDirection_String
// ============================================================================

func TestConnectionDirection_String(t *testing.T) {
	if ConnDirFore.String() != "fore" {
		t.Errorf("ConnDirFore.String() = %s, want fore", ConnDirFore.String())
	}
	if ConnDirBack.String() != "back" {
		t.Errorf("ConnDirBack.String() = %s, want back", ConnDirBack.String())
	}
	if ConnDirBoth.String() != "both" {
		t.Errorf("ConnDirBoth.String() = %s, want both", ConnDirBoth.String())
	}
}

// ============================================================================
// TestConnectionType_String
// ============================================================================

func TestConnectionType_String(t *testing.T) {
	if ConnTypeTCP.String() != "TCP" {
		t.Errorf("ConnTypeTCP.String() = %s, want TCP", ConnTypeTCP.String())
	}
	if ConnTypeRDMA.String() != "RDMA" {
		t.Errorf("ConnTypeRDMA.String() = %s, want RDMA", ConnTypeRDMA.String())
	}
}

// ============================================================================
// TestBindConnToSession_UnlimitedConnections
// ============================================================================

func TestBindConnToSession_UnlimitedConnections(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxConnectionsPerSession(0) // 0 = unlimited
	_, sessionID := setupClientAndSession(t, sm)

	// Should be able to bind many connections without hitting a limit
	for i := uint64(1); i <= 50; i++ {
		_, err := sm.BindConnToSession(i, sessionID, types.CDFC4_FORE)
		if err != nil {
			t.Fatalf("bind conn %d error with unlimited setting: %v", i, err)
		}
	}

	bindings := sm.GetConnectionBindings(sessionID)
	if len(bindings) != 50 {
		t.Errorf("expected 50 bindings, got %d", len(bindings))
	}
}
