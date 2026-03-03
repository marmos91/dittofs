package lock

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Cross-Protocol Break: Lease + Delegation broken in parallel
// ============================================================================

func TestCrossProtocolBreak_WriteBothLeaseAndDelegation(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// NFS client holds a Read delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	// SMB client holds a Read-only lease (coexists with Read delegation)
	smbLease := &UnifiedLock{
		ID:    "smb-lease-1",
		Owner: LockOwner{OwnerID: "smb:session1", ClientID: "smb-conn1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3},
			LeaseState: LeaseStateRead,
		},
	}
	err = lm.AddUnifiedLock("shared-file", smbLease)
	require.NoError(t, err)

	// A write operation breaks both the lease and the delegation
	writer := &LockOwner{OwnerID: "writer:client99"}
	err = lm.CheckAndBreakCachingForWrite("shared-file", writer)
	require.NoError(t, err)

	// Verify both OnOpLockBreak and OnDelegationRecall were called
	opBreaks := cb.getOpLockBreaks()
	delegRecalls := cb.getDelegationRecalls()

	assert.Len(t, opBreaks, 1, "expected 1 oplock break")
	assert.Len(t, delegRecalls, 1, "expected 1 delegation recall")

	assert.Equal(t, "smb:session1", opBreaks[0].ownerID)
	assert.Equal(t, LeaseStateNone, opBreaks[0].breakToState)
	assert.Equal(t, deleg.DelegationID, delegRecalls[0].delegationID)
}

// ============================================================================
// Coexistence: Read delegation + Read lease NOT broken by read
// ============================================================================

func TestCrossProtocolBreak_ReadCoexistence(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// NFS client holds a Read delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	// SMB client holds a Read-only lease
	smbLease := &UnifiedLock{
		ID:    "smb-lease-1",
		Owner: LockOwner{OwnerID: "smb:session1", ClientID: "smb-conn1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3},
			LeaseState: LeaseStateRead,
		},
	}
	err = lm.AddUnifiedLock("shared-file", smbLease)
	require.NoError(t, err)

	// A read operation should NOT break either
	reader := &LockOwner{OwnerID: "reader:client99"}
	err = lm.CheckAndBreakCachingForRead("shared-file", reader)
	require.NoError(t, err)

	opBreaks := cb.getOpLockBreaks()
	delegRecalls := cb.getDelegationRecalls()

	assert.Empty(t, opBreaks, "read should not break read lease")
	assert.Empty(t, delegRecalls, "read should not break read delegation")
}

// ============================================================================
// Delete breaks all: both lease and delegation
// ============================================================================

func TestCrossProtocolBreak_DeleteBreaksAll(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// NFS delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	// SMB lease
	smbLease := &UnifiedLock{
		ID:    "smb-lease-1",
		Owner: LockOwner{OwnerID: "smb:session1", ClientID: "smb-conn1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3},
			LeaseState: LeaseStateRead | LeaseStateHandle,
		},
	}
	err = lm.AddUnifiedLock("shared-file", smbLease)
	require.NoError(t, err)

	// Delete operation breaks everything
	deleter := &LockOwner{OwnerID: "deleter:client99"}
	err = lm.CheckAndBreakCachingForDelete("shared-file", deleter)
	require.NoError(t, err)

	opBreaks := cb.getOpLockBreaks()
	delegRecalls := cb.getDelegationRecalls()

	assert.Len(t, opBreaks, 1, "delete should break lease")
	assert.Len(t, delegRecalls, 1, "delete should break delegation")
}

// ============================================================================
// Anti-storm: after breaking delegation, re-grant blocked by recentlyBroken
// ============================================================================

func TestCrossProtocolBreak_AntiStormCache(t *testing.T) {
	t.Parallel()

	// Use NewManagerWithTTL to set a short TTL for testing
	lm := NewManagerWithTTL(200 * time.Millisecond)

	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Grant a delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	// Break it via write
	writer := &LockOwner{OwnerID: "writer:client99"}
	err = lm.CheckAndBreakCachingForWrite("shared-file", writer)
	require.NoError(t, err)

	// Simulate the client returning the delegation
	err = lm.ReturnDelegation("shared-file", deleg.DelegationID)
	require.NoError(t, err)

	// Attempt to re-grant immediately -- should be blocked by anti-storm cache
	deleg2 := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err = lm.GrantDelegation("shared-file", deleg2)
	assert.Error(t, err, "re-grant should be blocked by anti-storm cache")

	// Wait for TTL to expire
	time.Sleep(250 * time.Millisecond)

	// Now re-grant should succeed
	deleg3 := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err = lm.GrantDelegation("shared-file", deleg3)
	assert.NoError(t, err, "re-grant should succeed after anti-storm TTL expires")
}

// ============================================================================
// WaitForBreakCompletion: blocks until break resolved
// ============================================================================

func TestCrossProtocolBreak_WaitForBreakCompletion(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Grant a delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	// Break it
	writer := &LockOwner{OwnerID: "writer:client99"}
	err = lm.CheckAndBreakCachingForWrite("shared-file", writer)
	require.NoError(t, err)

	// WaitForBreakCompletion should block
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- lm.WaitForBreakCompletion(ctx, "shared-file")
	}()

	// Simulate client returning delegation after a brief delay
	time.Sleep(50 * time.Millisecond)
	err = lm.ReturnDelegation("shared-file", deleg.DelegationID)
	require.NoError(t, err)

	// Wait should complete
	select {
	case err := <-done:
		assert.NoError(t, err, "wait should complete after delegation returned")
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForBreakCompletion did not return in time")
	}
}

// ============================================================================
// WaitForBreakCompletion: context cancellation
// ============================================================================

func TestCrossProtocolBreak_WaitForBreakCompletion_ContextCancelled(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Grant and break a delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	writer := &LockOwner{OwnerID: "writer:client99"}
	err = lm.CheckAndBreakCachingForWrite("shared-file", writer)
	require.NoError(t, err)

	// Cancel context before delegation is returned
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = lm.WaitForBreakCompletion(ctx, "shared-file")
	assert.Error(t, err, "wait should fail when context is cancelled")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// ============================================================================
// Write breaks NFS delegation but not from same owner
// ============================================================================

func TestCrossProtocolBreak_ExcludeOwner(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// NFS client holds a Read delegation
	deleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", deleg)
	require.NoError(t, err)

	// Same NFS client writes -- should NOT break its own delegation.
	// Use the public DelegationOwnerID helper to construct the expected owner.
	sameOwner := &LockOwner{OwnerID: DelegationOwnerID(deleg.ClientID, deleg.DelegationID)}
	err = lm.CheckAndBreakCachingForWrite("shared-file", sameOwner)
	require.NoError(t, err)

	delegRecalls := cb.getDelegationRecalls()
	assert.Empty(t, delegRecalls, "should not break own delegation")
}

// ============================================================================
// BreakResult and formatting helpers
// ============================================================================

func TestBreakResult_IsCrossProtocol(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		result   BreakResult
		expected bool
	}{
		{"both broken", BreakResult{LeasesBroken: 1, DelegationsBroken: 1}, true},
		{"only leases", BreakResult{LeasesBroken: 2, DelegationsBroken: 0}, false},
		{"only delegations", BreakResult{LeasesBroken: 0, DelegationsBroken: 3}, false},
		{"none broken", BreakResult{LeasesBroken: 0, DelegationsBroken: 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.result.IsCrossProtocol())
		})
	}
}

func TestBreakResult_Total(t *testing.T) {
	t.Parallel()

	result := BreakResult{LeasesBroken: 2, DelegationsBroken: 3}
	assert.Equal(t, 5, result.Total())
}

func TestFormatCrossProtocolBreak(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		result   BreakResult
		contains string
	}{
		{
			"cross-protocol",
			BreakResult{LeasesBroken: 1, DelegationsBroken: 2, HandleKey: "file1", Duration: 45 * time.Millisecond},
			"cross-protocol break",
		},
		{
			"lease only",
			BreakResult{LeasesBroken: 1, HandleKey: "file2", Duration: 12 * time.Millisecond},
			"lease break",
		},
		{
			"delegation only",
			BreakResult{DelegationsBroken: 1, HandleKey: "file3", Duration: 8 * time.Millisecond},
			"delegation recall",
		},
		{
			"no breaks",
			BreakResult{HandleKey: "file4"},
			"no breaks needed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := FormatCrossProtocolBreak(tt.result)
			assert.Contains(t, msg, tt.contains)
			assert.Contains(t, msg, tt.result.HandleKey)
		})
	}
}

func TestClassifyBreakScenario(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opType   string
		result   BreakResult
		contains string
	}{
		{"SMB write breaks delegation", "SMB write", BreakResult{DelegationsBroken: 1}, "breaks NFS delegation"},
		{"NFS open breaks lease", "NFS open", BreakResult{LeasesBroken: 1}, "breaks SMB lease"},
		{"delete breaks both", "delete", BreakResult{LeasesBroken: 1, DelegationsBroken: 1}, "NFS delegation + SMB lease"},
		{"no breaks", "read", BreakResult{}, "no breaks needed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := ClassifyBreakScenario(tt.opType, tt.result)
			assert.Contains(t, msg, tt.contains)
		})
	}
}

func TestIsCrossProtocolConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		locks    []*UnifiedLock
		expected bool
	}{
		{
			"both lease and delegation",
			[]*UnifiedLock{
				{Lease: &OpLock{LeaseState: LeaseStateRead}},
				{Delegation: &Delegation{DelegType: DelegTypeRead}},
			},
			true,
		},
		{
			"only lease",
			[]*UnifiedLock{
				{Lease: &OpLock{LeaseState: LeaseStateRead}},
			},
			false,
		},
		{
			"only delegation",
			[]*UnifiedLock{
				{Delegation: &Delegation{DelegType: DelegTypeRead}},
			},
			false,
		},
		{
			"empty locks",
			[]*UnifiedLock{},
			false,
		},
		{
			"revoked delegation does not count",
			[]*UnifiedLock{
				{Lease: &OpLock{LeaseState: LeaseStateRead}},
				{Delegation: &Delegation{DelegType: DelegTypeRead, Revoked: true}},
			},
			false,
		},
		{
			"none lease state does not count",
			[]*UnifiedLock{
				{Lease: &OpLock{LeaseState: LeaseStateNone}},
				{Delegation: &Delegation{DelegType: DelegTypeRead}},
			},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsCrossProtocolConflict(tt.locks))
		})
	}
}

func TestCountBreakableState(t *testing.T) {
	t.Parallel()

	locks := []*UnifiedLock{
		{Lease: &OpLock{LeaseState: LeaseStateRead}},
		{Lease: &OpLock{LeaseState: LeaseStateRead | LeaseStateWrite}},
		{Lease: &OpLock{LeaseState: LeaseStateNone}},                        // not countable (None)
		{Lease: &OpLock{LeaseState: LeaseStateRead, Breaking: true}},        // not countable (breaking)
		{Delegation: &Delegation{DelegType: DelegTypeRead}},                 // countable
		{Delegation: &Delegation{DelegType: DelegTypeWrite}},                // countable
		{Delegation: &Delegation{DelegType: DelegTypeRead, Revoked: true}},  // not countable (revoked)
		{Delegation: &Delegation{DelegType: DelegTypeRead, Breaking: true}}, // not countable (breaking)
	}

	leases, delegations := CountBreakableState(locks)
	assert.Equal(t, 2, leases, "should count 2 active leases")
	assert.Equal(t, 2, delegations, "should count 2 active delegations")
}

// ============================================================================
// Write only breaks write delegation, not read delegation
// ============================================================================

func TestCrossProtocolBreak_ReadOnlyBreaksWriteDeleg(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Read delegation
	readDeleg := NewDelegation(DelegTypeRead, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", readDeleg)
	require.NoError(t, err)

	// Read operation should NOT break read delegation
	reader := &LockOwner{OwnerID: "reader:client99"}
	err = lm.CheckAndBreakCachingForRead("shared-file", reader)
	require.NoError(t, err)

	delegRecalls := cb.getDelegationRecalls()
	assert.Empty(t, delegRecalls, "read should not break read delegation")
}

func TestCrossProtocolBreak_ReadBreaksWriteDeleg(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Write delegation
	writeDeleg := NewDelegation(DelegTypeWrite, "nfs-client1", "/export", false)
	err := lm.GrantDelegation("shared-file", writeDeleg)
	require.NoError(t, err)

	// Read operation SHOULD break write delegation
	reader := &LockOwner{OwnerID: "reader:client99"}
	err = lm.CheckAndBreakCachingForRead("shared-file", reader)
	require.NoError(t, err)

	delegRecalls := cb.getDelegationRecalls()
	assert.Len(t, delegRecalls, 1, "read should break write delegation")
	assert.Equal(t, writeDeleg.DelegationID, delegRecalls[0].delegationID)
}

// ============================================================================
// recordingBreakCallbacks - test helper
// ============================================================================

type recordingBreakCallbacks struct {
	mu             sync.Mutex
	opLockBreaks   []xpOpLockBreakEvent
	delegRecalls   []delegRecallEvent
	byteRevokes    []byteRevokeEvent
	accessConflict []xpAccessConflictEvent
}

type xpOpLockBreakEvent struct {
	handleKey    string
	ownerID      string
	breakToState uint32
}

type delegRecallEvent struct {
	handleKey    string
	delegationID string
	clientID     string
}

type byteRevokeEvent struct {
	handleKey string
	reason    string
}

type xpAccessConflictEvent struct {
	handleKey string
	mode      AccessMode
}

func (r *recordingBreakCallbacks) OnOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opLockBreaks = append(r.opLockBreaks, xpOpLockBreakEvent{
		handleKey:    handleKey,
		ownerID:      lock.Owner.OwnerID,
		breakToState: breakToState,
	})
}

func (r *recordingBreakCallbacks) OnDelegationRecall(handleKey string, lock *UnifiedLock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var delegID, clientID string
	if lock != nil && lock.Delegation != nil {
		delegID = lock.Delegation.DelegationID
		clientID = lock.Delegation.ClientID
	}
	r.delegRecalls = append(r.delegRecalls, delegRecallEvent{
		handleKey:    handleKey,
		delegationID: delegID,
		clientID:     clientID,
	})
}

func (r *recordingBreakCallbacks) OnByteRangeRevoke(handleKey string, _ *UnifiedLock, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byteRevokes = append(r.byteRevokes, byteRevokeEvent{
		handleKey: handleKey,
		reason:    reason,
	})
}

func (r *recordingBreakCallbacks) OnAccessConflict(handleKey string, _ *UnifiedLock, mode AccessMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accessConflict = append(r.accessConflict, xpAccessConflictEvent{
		handleKey: handleKey,
		mode:      mode,
	})
}

func (r *recordingBreakCallbacks) getOpLockBreaks() []xpOpLockBreakEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]xpOpLockBreakEvent, len(r.opLockBreaks))
	copy(result, r.opLockBreaks)
	return result
}

func (r *recordingBreakCallbacks) getDelegationRecalls() []delegRecallEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]delegRecallEvent, len(r.delegRecalls))
	copy(result, r.delegRecalls)
	return result
}

// Compile-time verification.
var _ BreakCallbacks = (*recordingBreakCallbacks)(nil)
