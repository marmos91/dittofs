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

// TestLock_DelegationAheadOfTerminalConflictDoesNotWait pins that a delegation
// sitting in front of a definite byte-range conflict does not cost the caller a
// recall-and-wait. The scan must find the terminal conflict and deny at once: a
// read delegation (which only an exclusive request conflicts with) coexists with
// a shared NLM lock, so an exclusive SMB request meets both, and only the NLM
// lock decides the outcome.
func TestLock_DelegationAheadOfTerminalConflictDoesNotWait(t *testing.T) {
	const handle = xHandle

	defer func(d time.Duration) { delegationIOWaitTimeout = d }(delegationIOWaitTimeout)
	delegationIOWaitTimeout = 2 * time.Second

	lm := NewManager()
	recalls := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(recalls)

	// A read delegation: an exclusive request conflicts with it.
	if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeRead, "nfs4:c1", "share-a", false)); err != nil {
		t.Fatalf("GrantDelegation: %v", err)
	}
	// A shared NLM byte-range lock on the same range: terminal for an exclusive
	// request, and one a read delegation can coexist with.
	shared := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "nlm:holder"},
		Offset: 0,
		Length: 0,
		Type:   LockTypeShared,
	}
	if err := lm.AddUnifiedLock(handle, shared); err != nil {
		t.Fatalf("AddUnifiedLock: %v", err)
	}

	start := time.Now()
	err := lm.Lock(handle, smbExclusive("smb:open-1", 0, 0))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the shared NLM lock must deny an exclusive SMB lock")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("a terminal conflict must fail fast, not wait out the delegation, took %v", elapsed)
	}
	if got := len(recalls.getDelegationRecalls()); got != 0 {
		t.Fatalf("a terminal conflict must not recall the delegation, got %d recalls", got)
	}
}

// TestCheckForIO_DelegationArrivingDuringRecallIsRecalled pins that the I/O
// path re-judges too, not only the acquire path. Its loop is a separate one, and
// without this the existing CheckForIO tests would stay green while a delegation
// granted during the recall was returned as a conflict instead of being recalled:
// SMB I/O against the file would be denied again.
//
// The interleaving is forced through the break callback, as in the Lock case.
func TestCheckForIO_DelegationArrivingDuringRecallIsRecalled(t *testing.T) {
	const handle = xHandle

	lm := NewManagerWithTTL(-1)
	recalls := &recordingBreakCallbacks{}
	lm.RegisterBreakCallbacks(recalls)

	arrived := NewDelegation(DelegTypeWrite, "nfs4:c2", "share-a", false)
	first := NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)
	if err := lm.GrantDelegation(handle, first); err != nil {
		t.Fatalf("GrantDelegation: %v", err)
	}
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

	// A write delegation blocks a foreign read, so the I/O may proceed only once
	// both delegations are recalled.
	if conflict := lm.CheckForIO(handle, "smb:open-1", 7, 0, 4096, false); conflict != nil {
		t.Fatalf("a delegation arriving during the recall must be recalled, not reported, got %v", conflict)
	}
	if got := len(recalls.getDelegationRecalls()); got != 2 {
		t.Fatalf("expected both delegations to be recalled, got %d recalls", got)
	}
}

// TestLock_WaitIsScopedToTheSelectedDelegations pins that the recall wait covers
// only the delegations this request selected. Multiple read delegations may
// coexist on a file, so an unrelated delegation can already be breaking while
// this request waits on a different one; if the wait is not scoped, a client that
// never answers that unrelated recall holds this request until the deadline even
// though its own delegation was returned promptly.
func TestLock_WaitIsScopedToTheSelectedDelegations(t *testing.T) {
	const handle = xHandle

	defer func(d time.Duration) { delegationIOWaitTimeout = d }(delegationIOWaitTimeout)
	delegationIOWaitTimeout = 3 * time.Second

	lm := NewManagerWithTTL(-1)
	lm.RegisterBreakCallbacks(&recordingBreakCallbacks{})

	// A write delegation: an exclusive claim, so a shared lock conflicts with it
	// and selects it for recall.
	selected := NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)
	if err := lm.GrantDelegation(handle, selected); err != nil {
		t.Fatalf("GrantDelegation(selected): %v", err)
	}
	// A read delegation: a shared claim, so a shared lock does not conflict with
	// it and it is not selected. GrantDelegation does not weigh delegations
	// against each other, so the two coexist.
	unrelated := NewDelegation(DelegTypeRead, "nfs4:c2", "share-a", false)
	if err := lm.GrantDelegation(handle, unrelated); err != nil {
		t.Fatalf("GrantDelegation(unrelated): %v", err)
	}
	// The unrelated delegation is already being recalled for another reason and
	// its client never answers.
	lm.mu.Lock()
	for _, ul := range lm.unifiedLocks[handle] {
		if ul.IsDelegation() && ul.Delegation.DelegationID == unrelated.DelegationID {
			// BreakStarted is what anchors the wait budget, so a recall "in
			// flight" must set it, as breakDelegations does.
			ul.Delegation.Breaking = true
			ul.Delegation.BreakStarted = time.Now()
		}
	}
	lm.mu.Unlock()

	lm.RegisterBreakCallbacks(&delegationReturningByID{
		onRecall: func(lock *UnifiedLock) {
			_ = lm.ReturnDelegation(handle, lock.Delegation.DelegationID)
		},
	})

	shared := FileLock{OpenID: "smb:open-1", SessionID: 7, Offset: 0, Length: 0, Exclusive: false}
	start := time.Now()
	if err := lm.Lock(handle, shared); err != nil {
		t.Fatalf("a shared lock must be granted once the selected delegation is returned, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > delegationIOWaitTimeout/2 {
		t.Fatalf("waited %v on an unrelated delegation's recall instead of returning when the selected one did", elapsed)
	}
}

// TestLock_RetriesShareOneRecallBudget pins that repeated acquisition attempts
// against the same unanswered recall do not each start a fresh timeout. The SMB
// handler retries a blocking lock every BlockingLockRetryInterval and the
// fail-immediately attempt comes through the same call, so a per-call budget
// would let one SMB request accumulate waits far past its own deadline.
func TestLock_RetriesShareOneRecallBudget(t *testing.T) {
	const handle = xHandle

	defer func(d time.Duration) { delegationIOWaitTimeout = d }(delegationIOWaitTimeout)
	delegationIOWaitTimeout = 400 * time.Millisecond

	lm := NewManagerWithTTL(-1)
	lm.RegisterBreakCallbacks(&recordingBreakCallbacks{})
	if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeWrite, "nfs4:c1", "share-a", false)); err != nil {
		t.Fatalf("GrantDelegation: %v", err)
	}

	// The client never answers, so every attempt times out. Ten attempts must
	// cost about one budget in total, not ten.
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := lm.Lock(handle, smbExclusive("smb:open-1", 0, 0)); err == nil {
			t.Fatal("an unanswered recall must deny the lock")
		}
	}
	elapsed := time.Since(start)
	if elapsed > 3*delegationIOWaitTimeout {
		t.Fatalf("ten attempts waited %v, so each started a fresh %v budget", elapsed, delegationIOWaitTimeout)
	}
	t.Logf("ten attempts bounded to %v (one budget %v)", elapsed, delegationIOWaitTimeout)
}
