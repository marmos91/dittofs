package lock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBreakLeasesForByteRangeLock_BreaksOtherKeyReadLeasesToNone pins down the
// smbtorture smb2.lease.lock1 wire shape: when a byte-range lock is acquired
// on an open holding LEASE1, every OTHER lease (different lease key) that
// holds Read caching must be broken to None — including same-client_guid
// leases (LEASE2) and different-client leases (LEASE3) — while the locker's
// own lease (LEASE1) is preserved.
//
// Per MS-SMB2 3.3.5.14 and Samba
// `source3/smbd/smb2_oplock.c::contend_level2_oplocks_begin_default` +
// `do_break_lease_to_none`. Pre-fix the SMB Lock handler called
// CheckAndBreakLeasesForSMBOpen which only stripped the Write bit, so RH
// leases were never broken — failing lock1's
// `CHECK_VAL(lease_break_info.count, 2)` assertion.
func TestBreakLeasesForByteRangeLock_BreaksOtherKeyReadLeasesToNone(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	const handleKey = "locktest.dat"
	lease1 := [16]byte{0x01}
	lease2 := [16]byte{0x02}
	lease3 := [16]byte{0x03}

	// Three RH leases on the same file: LEASE1 on tree1a (client_guid C1),
	// LEASE2 on tree1b (same client_guid C1, different lease key), LEASE3 on
	// tree2 (different client_guid C2).
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-lease1",
		Owner: LockOwner{OwnerID: "smb:s1:f1", ClientID: "client-C1"},
		Lease: &OpLock{LeaseKey: lease1, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-lease2",
		Owner: LockOwner{OwnerID: "smb:s2:f2", ClientID: "client-C1"},
		Lease: &OpLock{LeaseKey: lease2, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-lease3",
		Owner: LockOwner{OwnerID: "smb:s3:f3", ClientID: "client-C2"},
		Lease: &OpLock{LeaseKey: lease3, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))

	// Acquire BRL on the LEASE1 handle.
	require.NoError(t, lm.BreakLeasesForByteRangeLock(handleKey, &LockOwner{
		ExcludeLeaseKey: lease1,
	}))

	breaks := cb.getOpLockBreaks()
	require.Len(t, breaks, 2,
		"BRL must break exactly the two other-key Read leases (LEASE2, LEASE3) to None")

	for _, ev := range breaks {
		assert.Equal(t, LeaseStateNone, ev.breakToState,
			"BRL break target must be None (full revocation), not strip-W")
		assert.NotEqual(t, "smb:s1:f1", ev.ownerID,
			"locker's own lease (LEASE1) must not be broken (nobreakself)")
	}
}

// TestBreakLeasesForByteRangeLock_SkipsLeasesWithoutRead asserts that leases
// without any Read bit (None — e.g., already drained) are not re-notified.
// This matches Samba's `(current_state & SMB2_LEASE_READ) == 0` early return.
func TestBreakLeasesForByteRangeLock_SkipsLeasesWithoutRead(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	const handleKey = "locktest2.dat"
	leaseLocker := [16]byte{0xA1}
	leaseDrained := [16]byte{0xA2}

	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-locker",
		Owner: LockOwner{OwnerID: "smb:locker"},
		Lease: &OpLock{LeaseKey: leaseLocker, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-drained",
		Owner: LockOwner{OwnerID: "smb:drained"},
		Lease: &OpLock{LeaseKey: leaseDrained, LeaseState: LeaseStateNone},
	}))

	require.NoError(t, lm.BreakLeasesForByteRangeLock(handleKey, &LockOwner{
		ExcludeLeaseKey: leaseLocker,
	}))

	assert.Empty(t, cb.getOpLockBreaks(),
		"None-state leases (drained) must not be re-broken — no Read cache to invalidate")
}

// TestBreakLeasesForByteRangeLock_TightensInFlightBreakToNone asserts that
// when a BRL is acquired on a file whose other-key lease is already in a
// Breaking state (e.g., RWH→RH dispatched but not yet ACKed), the cumulative
// final target (BreakingToRequired) is AND-merged down to None. No second
// notification is dispatched — the next progressive stage will fire on ACK.
// This matches the breakOpLocks contract used by every other break path.
func TestBreakLeasesForByteRangeLock_TightensInFlightBreakToNone(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	const handleKey = "locktest4.dat"
	leaseLocker := [16]byte{0xC1}
	leaseOther := [16]byte{0xC2}

	// Locker's own lease (preserved).
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-locker",
		Owner: LockOwner{OwnerID: "smb:locker"},
		Lease: &OpLock{LeaseKey: leaseLocker, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))
	// Other-key lease already in Breaking state with RH as the in-flight
	// target. Pre-fix, the BRL break could have left BreakingToRequired at RH.
	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-other",
		Owner: LockOwner{OwnerID: "smb:other"},
		Lease: &OpLock{
			LeaseKey:           leaseOther,
			LeaseState:         LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
			BreakToState:       LeaseStateRead | LeaseStateHandle,
			BreakingToRequired: LeaseStateRead | LeaseStateHandle,
			Breaking:           true,
		},
	}))

	require.NoError(t, lm.BreakLeasesForByteRangeLock(handleKey, &LockOwner{
		ExcludeLeaseKey: leaseLocker,
	}))

	// AND-merge: no new notification (Breaking suppressed dispatch).
	assert.Empty(t, cb.getOpLockBreaks(),
		"AND-merge of in-flight break must not dispatch a fresh notification")

	// Verify BreakingToRequired tightened to None.
	lm.mu.Lock()
	defer lm.mu.Unlock()
	_, ul, _ := lm.findLeaseByKey(leaseOther)
	require.NotNil(t, ul)
	assert.Equal(t, uint32(0), ul.Lease.BreakingToRequired,
		"BRL must AND-merge BreakingToRequired down to None")
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, ul.Lease.BreakToState,
		"in-flight BreakToState (intermediate stage) must be unchanged")
}

// TestBreakLeasesForByteRangeLock_ExcludesLockerByLeaseKey covers the
// "nobreakself" invariant: a different open with the same lease key
// (e.g., a same-key reopen) is excluded. Mirrors Samba `smb2_lease_equal`.
func TestBreakLeasesForByteRangeLock_ExcludesLockerByLeaseKey(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	cb := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	const handleKey = "locktest3.dat"
	sharedKey := [16]byte{0xB1}

	require.NoError(t, lm.AddUnifiedLock(handleKey, &UnifiedLock{
		ID:    "ul-h1",
		Owner: LockOwner{OwnerID: "smb:h1"},
		Lease: &OpLock{LeaseKey: sharedKey, LeaseState: LeaseStateRead | LeaseStateHandle},
	}))

	require.NoError(t, lm.BreakLeasesForByteRangeLock(handleKey, &LockOwner{
		ExcludeLeaseKey: sharedKey,
	}))

	assert.Empty(t, cb.getOpLockBreaks(),
		"locker's own lease key must be excluded (nobreakself)")
}
