package lock

import (
	"sync"
	"testing"
	"time"
)

// TestLock_DelegationIsRecalledNotDenied covers the lock-path counterpart of
// TestCheckForIO_DelegationBlocksAndRecalls: a delegation participates in
// fileLockConflictsWithUnified as the whole-file claim it stands in for, so
// Manager.Lock must recall it and wait for the DELEGRETURN before judging the
// conflict -- not deny the SMB byte-range lock outright. A denial here is what
// an SMB client sees as STATUS_LOCK_NOT_GRANTED for a file an NFSv4 client has
// delegated, even when nothing else holds a lock.
func TestLock_DelegationIsRecalledNotDenied(t *testing.T) {
	const handle = xHandle

	t.Run("a returned delegation lets the lock be granted", func(t *testing.T) {
		lm := NewManager()
		recalls := &recordingBreakCallbacks{}
		lm.RegisterBreakCallbacks(recalls)

		deleg := NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)
		if err := lm.GrantDelegation(handle, deleg); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		lm.RegisterBreakCallbacks(&delegReturningCallbacks{
			onRecall: func() { _ = lm.ReturnDelegation(handle, deleg.DelegationID) },
		})

		start := time.Now()
		if err := lm.Lock(handle, smbExclusive("smb:open-1", 0, 0)); err != nil {
			t.Fatalf("an SMB lock must be granted once the delegation is returned, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > delegationIOWaitTimeout/2 {
			t.Fatalf("Lock took %v, so it waited out the timeout instead of waking on the return", elapsed)
		}
		if got := len(recalls.getDelegationRecalls()); got != 1 {
			t.Fatalf("expected exactly one delegation recall, got %d", got)
		}
	})

	t.Run("an unanswered recall denies the lock", func(t *testing.T) {
		defer func(d time.Duration) { delegationIOWaitTimeout = d }(delegationIOWaitTimeout)
		delegationIOWaitTimeout = 50 * time.Millisecond

		lm := NewManager()
		if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		if err := lm.Lock(handle, smbExclusive("smb:open-1", 0, 0)); err == nil {
			t.Fatal("an unanswered recall must deny the lock")
		}
	})

	t.Run("a shared delegation does not block a shared lock", func(t *testing.T) {
		lm := NewManager()
		lm.RegisterBreakCallbacks(&recordingBreakCallbacks{})
		if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeRead, "nfs4:c1", "share-a", false)); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		shared := FileLock{OpenID: "smb:open-1", SessionID: 7, Offset: 0, Length: 0, Exclusive: false}
		if err := lm.Lock(handle, shared); err != nil {
			t.Fatalf("a read delegation does not conflict with a shared lock, got %v", err)
		}
	})

	// A zero-byte SMB lock never conflicts (SMB2 semantics), so it must be
	// granted without recalling anything. Deriving the recall predicate from the
	// lock's exclusivity alone instead of from the conflict predicate would make
	// this recall a write delegation and park on it for the whole timeout.
	t.Run("a zero-byte lock neither recalls nor waits", func(t *testing.T) {
		defer func(d time.Duration) { delegationIOWaitTimeout = d }(delegationIOWaitTimeout)
		delegationIOWaitTimeout = 300 * time.Millisecond

		lm := NewManager()
		recalls := &recordingBreakCallbacks{}
		lm.RegisterBreakCallbacks(recalls)
		if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}

		zb := FileLock{OpenID: "smb:open-1", SessionID: 7, Offset: 0, Length: 0, Exclusive: true, IsZeroByte: true}
		start := time.Now()
		if err := lm.Lock(handle, zb); err != nil {
			t.Fatalf("a zero-byte lock must be granted, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("a zero-byte lock must not wait on a recall, took %v", elapsed)
		}
		if got := len(recalls.getDelegationRecalls()); got != 0 {
			t.Fatalf("a zero-byte lock must not recall a delegation, got %d recalls", got)
		}
	})
}

// TestLock_DelegationArrivingDuringAcquireIsRecalledNotDenied covers the window
// between the recall decision and the conflict judgment: a delegation granted
// by a concurrent OPEN after the recall pre-filter ran but before the lock was
// inserted must be recalled, not turned into a bare denial.
//
// The interleaving is forced through the break callback rather than raced: the
// first recall returns the recalled delegation and immediately grants a second
// one, standing in for an OPEN that won the window. A single-pass acquire would
// stop at the second delegation and deny; the re-judge loop recalls that one
// too and grants. Each wait returns on the break signal, so the test does not
// depend on the timeout.
func TestLock_DelegationArrivingDuringAcquireIsRecalledNotDenied(t *testing.T) {
	const handle = xHandle

	// A negative recently-broken TTL disables the anti-storm cache, which would
	// otherwise refuse the second grant for seconds after the first recall and
	// mask the re-judge behavior under test. Zero is not enough: the mark and the
	// re-grant can land in the same clock tick, where the age is 0, not past the
	// TTL. That cache has its own tests.
	lm := NewManagerWithTTL(-1)
	recalls := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(recalls)

	// A second delegation, granted by the first recall, that the acquire must
	// also recall before it can be granted.
	arrived := NewDelegation(DelegTypeWrite, "nfs4:c2", "share-a", false)
	first := NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)
	if err := lm.GrantDelegation(handle, first); err != nil {
		t.Fatalf("GrantDelegation: %v", err)
	}
	// Returns the delegation actually being recalled (the callback hands us the
	// lock), and on the first recall only, grants the one that "arrived
	// concurrently". Returning a fixed ID would leave the second delegation
	// unreturned and hang on the timeout instead of exercising the re-judge.
	var grants sync.Once
	lm.RegisterBreakCallbacks(&delegationReturningByID{
		onRecall: func(lock *UnifiedLock) {
			_ = lm.ReturnDelegation(handle, lock.Delegation.DelegationID)
			grants.Do(func() {
				if err := lm.GrantDelegation(handle, arrived); err != nil {
					t.Errorf("GrantDelegation(arrived): %v", err)
				}
			})
		},
	})

	if err := lm.Lock(handle, smbExclusive("smb:open-1", 0, 0)); err != nil {
		t.Fatalf("a delegation arriving during acquire must be recalled, not deny the lock, got %v", err)
	}
	if got := len(recalls.getDelegationRecalls()); got != 2 {
		t.Fatalf("expected both delegations to be recalled, got %d recalls", got)
	}
}

// TestTestLock_AgreesWithLockOnDelegation pins that the preview reports the
// same verdict the acquire would produce: a delegation is not a conflict, it is
// a recall. A TestLock that reports conflict while Lock grants would make an
// SMB client skip a lock it could have taken.
func TestTestLock_AgreesWithLockOnDelegation(t *testing.T) {
	const handle = xHandle

	lm := NewManager()
	deleg := NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)
	if err := lm.GrantDelegation(handle, deleg); err != nil {
		t.Fatalf("GrantDelegation: %v", err)
	}
	// The holder answers the recall, so both calls judge the conflict set after
	// the DELEGRETURN rather than after the timeout.
	lm.RegisterBreakCallbacks(&delegReturningCallbacks{
		onRecall: func() { _ = lm.ReturnDelegation(handle, deleg.DelegationID) },
	})

	conflict, err := lm.TestLock(handle, smbExclusive("smb:open-1", 0, 0))
	if err != nil {
		t.Fatalf("TestLock: %v", err)
	}
	if conflict != nil {
		t.Fatalf("TestLock must not report a returned delegation as a conflict, got %+v", conflict)
	}

	if err := lm.Lock(handle, smbExclusive("smb:open-1", 0, 0)); err != nil {
		t.Fatalf("Lock after the same recall must agree with TestLock, got %v", err)
	}
}

// delegationReturningByID returns the delegation named by the lock it is handed,
// so a test can drive a multi-recall sequence without guessing IDs.
type delegationReturningByID struct {
	onRecall func(*UnifiedLock)
}

func (d *delegationReturningByID) OnOpLockBreak(string, *UnifiedLock, uint32)        {}
func (d *delegationReturningByID) OnByteRangeRevoke(string, *UnifiedLock, string)    {}
func (d *delegationReturningByID) OnAccessConflict(string, *UnifiedLock, AccessMode) {}
func (d *delegationReturningByID) OnDelegationRecall(_ string, lock *UnifiedLock) {
	if d.onRecall != nil {
		d.onRecall(lock)
	}
}
