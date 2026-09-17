package lock

import (
	"testing"
)

// TestCheckForIO_DelegationBlocksAndRecalls covers the cross-protocol rule this
// fix adds: an NFSv4 delegation is the whole-file claim of a client that
// satisfies its byte-range locks locally (RFC 8881 §10.4.4), so a foreign SMB
// read/write it conflicts with must be denied and the delegation recalled --
// not silently allowed through, which is what the old IsDelegation() skip did.
func TestCheckForIO_DelegationBlocksAndRecalls(t *testing.T) {
	const handle = "/export:file1"

	newManagerWithDelegation := func(t *testing.T, delegType DelegationType) *Manager {
		t.Helper()
		lm := NewManager()
		deleg := NewDelegation(delegType, "nfs:1", "/export", false)
		if err := lm.GrantDelegation(handle, deleg); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		return lm
	}

	t.Run("write delegation blocks a foreign read and write", func(t *testing.T) {
		lm := newManagerWithDelegation(t, DelegTypeWrite)
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c == nil {
			t.Fatal("a foreign read under a write delegation must be denied")
		}
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, true); c == nil {
			t.Fatal("a foreign write under a write delegation must be denied")
		}
	})

	t.Run("read delegation blocks a foreign write but not a read", func(t *testing.T) {
		lm := newManagerWithDelegation(t, DelegTypeRead)
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c != nil {
			t.Fatalf("a foreign read under a read delegation must be allowed, got %+v", c)
		}
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, true); c == nil {
			t.Fatal("a foreign write under a read delegation must be denied")
		}
	})

	t.Run("a denied IO dispatches a recall", func(t *testing.T) {
		lm := NewManager()
		cb := &recordingBreakCallbacks{}
		lm.RegisterBreakCallbacks(cb)
		if err := lm.GrantDelegation(handle, NewDelegation(DelegTypeWrite, "nfs:1", "/export", false)); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c == nil {
			t.Fatal("expected a conflict")
		}
		if got := len(cb.getDelegationRecalls()); got != 1 {
			t.Fatalf("expected exactly one recall dispatch, got %d", got)
		}
	})

	t.Run("a returned delegation stops blocking", func(t *testing.T) {
		lm := NewManager()
		deleg := NewDelegation(DelegTypeWrite, "nfs:1", "/export", false)
		if err := lm.GrantDelegation(handle, deleg); err != nil {
			t.Fatalf("GrantDelegation: %v", err)
		}
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c == nil {
			t.Fatal("expected a conflict before the return")
		}
		if err := lm.ReturnDelegation(handle, deleg.DelegationID); err != nil {
			t.Fatalf("ReturnDelegation: %v", err)
		}
		if c := lm.CheckForIO(handle, "smb:1", 1, 0, 4096, false); c != nil {
			t.Fatalf("a returned delegation must not block, got %+v", c)
		}
	})
}
