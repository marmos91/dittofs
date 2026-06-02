package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestLockOwnerOf verifies the OwnerID derivation matches the lock package's
// internal helper for both per-open (SMB) and session-only (NFS/NLM) locks.
func TestLockOwnerOf(t *testing.T) {
	tests := []struct {
		name string
		lock metadata.FileLock
		want string
	}{
		{
			name: "per-open SMB lock uses OpenID",
			lock: metadata.FileLock{OpenID: "abc123", SessionID: 42},
			want: "abc123",
		},
		{
			name: "session-only NFS lock falls back to session:N",
			lock: metadata.FileLock{SessionID: 42},
			want: "session:42",
		},
		{
			name: "empty OpenID falls back even with session 0",
			lock: metadata.FileLock{},
			want: "session:0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lockOwnerOf(&tc.lock); got != tc.want {
				t.Errorf("lockOwnerOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLockWaitGraph_DeadlockDetection_Cycle verifies that the WFG correctly
// flags a 2-owner cycle: A waits on B, B waits on A.
func TestLockWaitGraph_DeadlockDetection_Cycle(t *testing.T) {
	wfg := lock.NewWaitForGraph()
	// A waits for B.
	wfg.AddWaiter("A", []string{"B"})
	// B trying to wait for A would close a cycle.
	if !wfg.WouldCauseCycle("B", []string{"A"}) {
		t.Error("expected cycle (A->B->A), got no cycle")
	}
}

// TestLockWaitGraph_DeadlockDetection_NoCycle verifies acyclic chains pass.
func TestLockWaitGraph_DeadlockDetection_NoCycle(t *testing.T) {
	wfg := lock.NewWaitForGraph()
	// A -> B is fine.
	wfg.AddWaiter("A", []string{"B"})
	// C -> A: no cycle.
	if wfg.WouldCauseCycle("C", []string{"A"}) {
		t.Error("expected no cycle for C->A->B, got cycle")
	}
}

// TestLockWaitGraph_RemoveWaiter prunes the waiter so future requests succeed.
func TestLockWaitGraph_RemoveWaiter(t *testing.T) {
	wfg := lock.NewWaitForGraph()
	wfg.AddWaiter("A", []string{"B"})
	wfg.RemoveWaiter("A")
	// B can now safely wait for A.
	if wfg.WouldCauseCycle("B", []string{"A"}) {
		t.Error("expected no cycle after RemoveWaiter, still got cycle")
	}
}
