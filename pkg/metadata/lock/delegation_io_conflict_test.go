package lock

import (
	"testing"
	"time"
)

// TestCheckForIO_DelegationBlocksAndRecalls covers the cross-protocol rule this
// fix adds: an NFSv4 delegation is the whole-file claim of a client that
// satisfies its byte-range locks locally (RFC 8881 §10.4.4), so a foreign SMB
// read/write it conflicts with must be denied and the delegation recalled --
// not silently allowed through, which is what the old IsDelegation() skip did.
func TestCheckForIO_DelegationBlocksAndRecalls(t *testing.T) {
	const handle = "/export:file1"

	// The predicate is checked directly for the blocking assertions: CheckForIO
	// now parks on a recalled delegation, so going through it would make every
	// one of these cases wait out the timeout.
	newManagerWithDelegation := func(t *testing.T, delegType DelegationType) *Manager {
		t.Helper()
		lm := NewManager()
		deleg := NewDelegation(delegType, "nfs:1", "/export", false)
		if err := lm.GrantDelegation(handle, deleg); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		return lm
	}
	conflict := func(lm *Manager, isWrite bool) bool {
		c, _ := lm.checkForIOLocked(handle, "smb:1", 1, 0, 4096, isWrite)
		return c != nil
	}

	t.Run("write delegation blocks a foreign read and write", func(t *testing.T) {
		lm := newManagerWithDelegation(t, DelegTypeWrite)
		if !conflict(lm, false) {
			t.Fatal("a foreign read under a write delegation must be denied")
		}
		if !conflict(lm, true) {
			t.Fatal("a foreign write under a write delegation must be denied")
		}
	})

	t.Run("read delegation blocks a foreign write but not a read", func(t *testing.T) {
		lm := newManagerWithDelegation(t, DelegTypeRead)
		if conflict(lm, false) {
			t.Fatal("a foreign read under a read delegation must be allowed")
		}
		if !conflict(lm, true) {
			t.Fatal("a foreign write under a read delegation must be denied")
		}
	})

	// CheckForIO's contract is recall-and-wait, not deny: the holder answers the
	// recall from inside the blocked call, so the wait is released by the break
	// signal and the re-judge sees no conflict. The elapsed-time assertion is
	// what distinguishes waking on the signal from timing out.
	t.Run("a recall is dispatched and the IO proceeds once it is returned", func(t *testing.T) {
		lm := NewManager()
		cb := &recordingBreakCallbacks{}
		lm.RegisterBreakCallbacks(cb)
		deleg := NewDelegation(DelegTypeWrite, "nfs:1", "/export", false)
		if err := lm.GrantDelegation(handle, deleg); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		lm.RegisterBreakCallbacks(&delegReturningCallbacks{
			onRecall: func() { _ = lm.ReturnDelegation(handle, deleg.DelegationID) },
		})

		start := time.Now()
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c != nil {
			t.Fatalf("a returned delegation must not block, got %+v", c)
		}
		if elapsed := time.Since(start); elapsed > delegationIOWaitTimeout/2 {
			t.Fatalf("CheckForIO took %v, so the wait timed out instead of waking on the return", elapsed)
		}
		if got := len(cb.getDelegationRecalls()); got != 1 {
			t.Fatalf("expected exactly one recall dispatch, got %d", got)
		}
	})

	t.Run("an unanswered recall denies the IO", func(t *testing.T) {
		// The client never answers, so the wait expires and the I/O is refused
		// rather than let through under a delegation the server cannot account
		// for. The timeout is shortened here because the behavior under test is
		// the expiry, not its duration.
		defer func(d time.Duration) { delegationIOWaitTimeout = d }(delegationIOWaitTimeout)
		delegationIOWaitTimeout = 50 * time.Millisecond

		lm := NewManager()
		if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeWrite, "nfs:1", "/export", false)); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c == nil {
			t.Fatal("an unanswered recall must deny the IO")
		}
	})
}

// delegReturningCallbacks returns the recalled delegation from inside the
// recall callback, which is what makes CheckForIO's wait wake on the break
// signal rather than on its timeout.
type delegReturningCallbacks struct {
	onRecall func()
}

func (d *delegReturningCallbacks) OnOpLockBreak(string, *UnifiedLock, uint32)        {}
func (d *delegReturningCallbacks) OnByteRangeRevoke(string, *UnifiedLock, string)    {}
func (d *delegReturningCallbacks) OnAccessConflict(string, *UnifiedLock, AccessMode) {}
func (d *delegReturningCallbacks) OnDelegationRecall(string, *UnifiedLock) {
	if d.onRecall != nil {
		d.onRecall()
	}
}
