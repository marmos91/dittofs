package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestByteRangeLock_WaitForBreakBeforeGrant_ACK reproduces the XPRO-02
// regression (issue #1501): an NFS exclusive byte-range LOCK on a file whose
// conflicting SMB write-lease must first be broken.
//
// BreakLeasesForByteRangeLock is fire-and-forget — it marks the write lease
// Breaking and dispatches the break asynchronously, but the lease keeps its
// LeaseState (RWH) until the holder ACKs. opLockConflictsWithByteLock gates on
// lease.HasWrite(), not on the Breaking flag, so AddUnifiedLock observes the
// still-present write lease as a conflict if it runs before the break drains.
// The adapter fix waits (WaitForByteRangeLockBreak, the helper the NFSv4 and NLM
// LOCK paths call) between the break and the insert; this test drives that exact
// helper: the byte lock is denied while the break is in flight and granted once
// the holder ACKs to None.
func TestByteRangeLock_WaitForBreakBeforeGrant_ACK(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	const fileID = "xpro2-ack.dat"
	smbLease := [16]byte{0xD1}

	// SMB write lease (RWH) held by another client.
	require.NoError(t, lm.AddUnifiedLock(fileID, &UnifiedLock{
		ID:    "ul-smb-write",
		Owner: LockOwner{OwnerID: "smb:writer", ClientID: "client-SMB"},
		Lease: &OpLock{LeaseKey: smbLease, LeaseState: LeaseStateRead | LeaseStateWrite | LeaseStateHandle},
	}))

	// NFS asks for an exclusive byte-range lock → break the write lease.
	require.NoError(t, lm.BreakLeasesForByteRangeLock(fileID, &LockOwner{}))

	nfsLock := NewUnifiedLock(
		LockOwner{OwnerID: "nfs:owner", ClientID: "client-NFS"},
		FileHandle(fileID), 0, 100, LockTypeExclusive,
	)

	// Pre-fix behaviour: inserting now (lease still Breaking, LeaseState=RWH)
	// is a conflict — this is exactly the path that returned NFS4ERR_DENIED →
	// client EIO.
	require.Error(t, lm.AddUnifiedLock(fileID, nfsLock),
		"byte lock must conflict while the write lease is still present (Breaking, not yet ACKed)")

	// Holder ACKs the break to None (concurrently, as the real SMB client does).
	go func() {
		// Small stagger so the wait below parks on the break-wait channel first;
		// correctness does not depend on it (the wait re-checks state on entry),
		// but it exercises the channel-signal path.
		time.Sleep(10 * time.Millisecond)
		_ = lm.AcknowledgeLeaseBreak(context.Background(), smbLease, LeaseStateNone, 0)
	}()

	// The adapter fix: wait for the break to drain before retrying the insert.
	// Drive the exact helper the NFSv4/NLM LOCK paths use.
	require.NoError(t, WaitForByteRangeLockBreak(context.Background(), lm, fileID),
		"wait must observe the ACK and return without a timeout")

	// Post-break: the write bit is gone, so the byte lock is granted.
	require.NoError(t, lm.AddUnifiedLock(fileID, nfsLock),
		"byte lock must be granted once the conflicting write lease has drained to None")
}

// TestByteRangeLock_WaitForBreakBeforeGrant_Timeout covers the non-ACKing
// holder: WaitForBreakCompletionExceptKey honours the context deadline and, on
// expiry, force-downgrades the breaking lease to None (Samba lease_timeout
// parity). The byte lock is then grantable. This guarantees the adapter cannot
// hang forever and never returns EIO when the SMB client misbehaves.
func TestByteRangeLock_WaitForBreakBeforeGrant_Timeout(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	const fileID = "xpro2-timeout.dat"
	smbLease := [16]byte{0xE1}

	require.NoError(t, lm.AddUnifiedLock(fileID, &UnifiedLock{
		ID:    "ul-smb-write",
		Owner: LockOwner{OwnerID: "smb:writer", ClientID: "client-SMB"},
		Lease: &OpLock{LeaseKey: smbLease, LeaseState: LeaseStateRead | LeaseStateWrite | LeaseStateHandle},
	}))

	require.NoError(t, lm.BreakLeasesForByteRangeLock(fileID, &LockOwner{}))

	// No ACK ever arrives → the bounded wait expires and force-completes.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := lm.WaitForBreakCompletionExceptKey(ctx, fileID, [16]byte{})
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"wait must time out when the holder never ACKs")

	// Force-complete downgraded the lease to None, so the byte lock is grantable.
	nfsLock := NewUnifiedLock(
		LockOwner{OwnerID: "nfs:owner", ClientID: "client-NFS"},
		FileHandle(fileID), 0, 100, LockTypeExclusive,
	)
	assert.NoError(t, lm.AddUnifiedLock(fileID, nfsLock),
		"after force-complete the write lease no longer conflicts with the byte lock")
}

// TestWaitForByteRangeLeaseBreak_IgnoresBreakingDelegation pins the deadlock
// fix: the byte-range-lock wait must NOT block on an in-flight NFSv4 delegation
// break. acquireLock runs this wait while holding the StateManager mutex, and
// DELEGRETURN (which clears a delegation break) needs that same mutex — waiting
// on a delegation here would be a circular wait. The byte-lock break never
// marks a delegation Breaking and byte locks never conflict with delegations,
// so a Breaking delegation on the file must be invisible to this wait: it
// returns immediately even with no context deadline.
func TestWaitForByteRangeLeaseBreak_IgnoresBreakingDelegation(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	const fileID = "xpro2-deleg.dat"

	// A write delegation mid-recall (Breaking) on the same file, e.g. recalled
	// by a concurrent open. No SMB lease is breaking.
	require.NoError(t, lm.AddUnifiedLock(fileID, &UnifiedLock{
		ID:    "ul-deleg",
		Owner: LockOwner{OwnerID: "nfs:delegholder", ClientID: "client-NFS-D"},
		Delegation: &Delegation{
			DelegType: DelegTypeWrite,
			Breaking:  true,
		},
	}))

	// Must return immediately (no timeout, no block) despite the Breaking
	// delegation — a background context has no deadline, so a hang would wedge
	// the test rather than time out.
	done := make(chan error, 1)
	go func() { done <- lm.WaitForByteRangeLeaseBreak(context.Background(), fileID) }()
	select {
	case err := <-done:
		require.NoError(t, err, "wait must ignore delegation breaks and return nil")
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForByteRangeLeaseBreak blocked on a Breaking delegation (deadlock risk)")
	}

	// The delegation is left untouched — only DELEGRETURN may clear it.
	lm.mu.Lock()
	defer lm.mu.Unlock()
	require.Len(t, lm.unifiedLocks[fileID], 1)
	assert.True(t, lm.unifiedLocks[fileID][0].Delegation.Breaking,
		"the wait must not force-complete or remove the delegation")
}

// TestWaitForByteRangeLeaseBreak_CancelDoesNotForceBreak asserts that a client
// cancellation of the LOCK request does NOT force-downgrade the conflicting SMB
// lease. Only a genuine wait-deadline expiry (the holder failed to ACK) may
// revoke the lease — a cancelled request inserts no lock, so it must leave the
// holder's lease intact.
func TestWaitForByteRangeLeaseBreak_CancelDoesNotForceBreak(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	const fileID = "xpro2-cancel.dat"
	smbLease := [16]byte{0xF1}

	require.NoError(t, lm.AddUnifiedLock(fileID, &UnifiedLock{
		ID:    "ul-smb-write",
		Owner: LockOwner{OwnerID: "smb:writer", ClientID: "client-SMB"},
		Lease: &OpLock{LeaseKey: smbLease, LeaseState: LeaseStateRead | LeaseStateWrite | LeaseStateHandle},
	}))
	require.NoError(t, lm.BreakLeasesForByteRangeLock(fileID, &LockOwner{}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client gave up before the holder ACKed
	require.ErrorIs(t, lm.WaitForByteRangeLeaseBreak(ctx, fileID), context.Canceled)

	// The lease must NOT have been force-downgraded: it is still Breaking with
	// its Write bit intact, awaiting the holder's own ACK.
	lm.mu.Lock()
	defer lm.mu.Unlock()
	_, ul, _ := lm.findLeaseByKey(smbLease)
	require.NotNil(t, ul)
	assert.True(t, ul.Lease.Breaking, "cancellation must not resolve the break")
	assert.True(t, ul.Lease.HasWrite(), "cancellation must not strip the holder's Write lease")
}
