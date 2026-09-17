package lock

import "testing"

// Probe: does the single shared Manager enforce mandatory cross-protocol
// byte-range locking? Both protocol adapters resolve the SAME per-share
// Manager, so this is the seam the e2e XPRO tests exercise.
//
// Positive control: an NLM/NFSv4 lock in lm.unifiedLocks must block an
// overlapping SMB lock in lm.locks, and must gate SMB I/O.
func TestProbe_NLMLockBlocksSMBLockAndIO(t *testing.T) {
	lm := NewManager()
	if err := lm.AddUnifiedLock(xHandle, nlmLock("nlm:host-a", 0, 0, true)); err != nil {
		t.Fatalf("NLM exclusive whole-file lock refused: %v", err)
	}

	if err := lm.Lock(xHandle, smbExclusive("smb:open-1", 0, 0)); err == nil {
		t.Error("SMB exclusive lock overlapping an NLM exclusive lock was GRANTED (expected denied)")
	} else {
		t.Logf("SMB lock denied as expected: %v", err)
	}

	if c := lm.CheckForIO(xHandle, "smb:open-1", 7, 0, 10, false); c == nil {
		t.Error("SMB read against an NLM exclusive lock was ALLOWED (expected blocked)")
	} else {
		t.Logf("SMB read blocked as expected: %+v", c)
	}
}

// The gap: a whole-file NFSv4 delegation is a UnifiedLock with Delegation set.
// fileLockConflictsWithUnified skips delegations, so an SMB byte-range lock is
// granted against it, and unifiedLockBlocksIO skips it too. This is the state
// an NFSv4.1 client is in once it holds a delegation.
func TestProbe_DelegationDoesNotGateSMBLockOrIO(t *testing.T) {
	lm := NewManager()
	deleg := &UnifiedLock{
		Owner:      LockOwner{OwnerID: "nfs4:c1", ClientID: "nfs4:c1"},
		Type:       LockTypeExclusive,
		Delegation: &Delegation{DelegationID: "d1", DelegType: DelegTypeWrite},
	}
	if err := lm.AddUnifiedLock(xHandle, deleg); err != nil {
		t.Fatalf("AddUnifiedLock (delegation) failed: %v", err)
	}

	err := lm.Lock(xHandle, smbExclusive("smb:open-1", 0, 0))
	t.Logf("SMB exclusive lock against an NFSv4 write delegation: err=%v", err)
	if err == nil {
		t.Log("GRANTED — delegation is not a cross-protocol byte-range conflict")
	}

	c := lm.CheckForIO(xHandle, "smb:open-1", 7, 0, 10, false)
	t.Logf("SMB read against an NFSv4 write delegation: conflict=%v", c)
	if c == nil {
		t.Log("ALLOWED — delegation does not gate SMB I/O")
	}
}
