package state

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// These tests cover the per-share lock-manager resolver that wires NFSv4
// byte-range locks to the same unified lock manager instance used by SMB and
// NLM. Before this wiring, the production NFSv4 StateManager had no lock manager
// configured, so every LOCK returned "no lock manager configured"
// (NFS4ERR_SERVERFAULT, surfaced to clients as EIO), and cross-protocol lock
// conflicts could never be detected.

// TestLockManagerResolver_FixesMissingManager verifies that a StateManager with
// neither a static manager nor a resolver fails LOCK (the old, broken state),
// while one with a resolver succeeds.
func TestLockManagerResolver_FixesMissingManager(t *testing.T) {
	// No manager, no resolver: LOCK must fail (reproduces the EIO bug).
	smBroken := NewStateManager(90 * time.Second)
	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, smBroken)
	if _, err := smBroken.LockNew(context.Background(),
		clientID, []byte("owner-a"), 1,
		openStateid, openSeqid,
		fileHandle, types.WRITE_LT, 0, 100, false,
	); err == nil {
		t.Fatal("expected LOCK to fail when no lock manager is configured")
	}

	// Resolver wired: LOCK must succeed.
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManagerResolver(func(_ []byte) lock.LockManager { return lm })
	clientID, fileHandle, openStateid, openSeqid = setupClientAndOpenState(t, sm)
	if _, err := sm.LockNew(context.Background(),
		clientID, []byte("owner-a"), 1,
		openStateid, openSeqid,
		fileHandle, types.WRITE_LT, 0, 100, false,
	); err != nil {
		t.Fatalf("LOCK with resolved lock manager failed: %v", err)
	}
	if locks := lm.ListUnifiedLocks(string(fileHandle)); len(locks) != 1 {
		t.Fatalf("expected 1 lock in resolved manager, got %d", len(locks))
	}
}

// TestLockManagerResolver_DetectsCrossProtocolConflict seeds an SMB-style lock
// directly in the unified manager, then verifies an NFSv4 LOCKT/LOCK on an
// overlapping range observes the conflict — proving NFSv4 and SMB share the
// same manager instance.
func TestLockManagerResolver_DetectsCrossProtocolConflict(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManagerResolver(func(_ []byte) lock.LockManager { return lm })

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// SMB client takes an exclusive lock on bytes [0,100).
	smbLock := &lock.UnifiedLock{
		ID:         "smb-lock",
		Owner:      lock.LockOwner{OwnerID: "smb:client-x", ClientID: "smb-x"},
		FileHandle: lock.FileHandle(fileHandle),
		Offset:     0,
		Length:     100,
		Type:       lock.LockTypeExclusive,
		AcquiredAt: time.Now(),
	}
	if err := lm.AddUnifiedLock(string(fileHandle), smbLock); err != nil {
		t.Fatalf("seeding SMB lock failed: %v", err)
	}

	// NFSv4 LOCKT for an overlapping range must report the conflict.
	denied, err := sm.TestLock(clientID, []byte("nfs-owner"), fileHandle, types.WRITE_LT, 50, 100)
	if err != nil {
		t.Fatalf("TestLock returned error: %v", err)
	}
	if denied == nil {
		t.Fatal("expected NFSv4 LOCKT to detect the overlapping SMB lock, got no conflict")
	}

	// NFSv4 LOCK for the same overlapping range must also be denied (not granted).
	res, err := sm.LockNew(context.Background(),
		clientID, []byte("nfs-owner"), 1,
		openStateid, openSeqid,
		fileHandle, types.WRITE_LT, 50, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew returned error: %v", err)
	}
	if res == nil || res.Denied == nil {
		t.Fatal("expected NFSv4 LOCK to be denied by the conflicting SMB lock")
	}
}

// TestLockManagerResolver_RoutesByHandle verifies the resolver is consulted with
// the operation's file handle, so locks on one share's handle are isolated from
// another's.
func TestLockManagerResolver_RoutesByHandle(t *testing.T) {
	lmA := lock.NewManager()
	lmB := lock.NewManager()
	sm := NewStateManager(90 * time.Second)

	clientID, fileHandle, _, _ := setupClientAndOpenState(t, sm)
	other := []byte("/export-b:test-file-002")

	sm.SetLockManagerResolver(func(handle []byte) lock.LockManager {
		if string(handle) == string(fileHandle) {
			return lmA
		}
		return lmB
	})

	// Conflicting lock lives only in manager A (fileHandle's share).
	if err := lmA.AddUnifiedLock(string(fileHandle), &lock.UnifiedLock{
		ID:         "a-lock",
		Owner:      lock.LockOwner{OwnerID: "smb:a"},
		FileHandle: lock.FileHandle(fileHandle),
		Offset:     0,
		Length:     100,
		Type:       lock.LockTypeExclusive,
		AcquiredAt: time.Now(),
	}); err != nil {
		t.Fatalf("seeding lock in manager A failed: %v", err)
	}

	// LOCKT on fileHandle → routed to A → conflict.
	if denied, err := sm.TestLock(clientID, []byte("o"), fileHandle, types.WRITE_LT, 0, 100); err != nil || denied == nil {
		t.Fatalf("expected conflict on fileHandle (manager A); denied=%v err=%v", denied, err)
	}

	// LOCKT on a different handle → routed to B (empty) → no conflict.
	if denied, err := sm.TestLock(clientID, []byte("o"), other, types.WRITE_LT, 0, 100); err != nil || denied != nil {
		t.Fatalf("expected no conflict on other handle (manager B); denied=%v err=%v", denied, err)
	}
}
