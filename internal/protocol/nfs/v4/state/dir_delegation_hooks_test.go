package state

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ============================================================================
// NotifyHook Tests -- verify mutation hooks trigger correct notification types
// ============================================================================

func TestNotifyHook_Create(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour) // Long window to inspect pending

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-create-dir")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Simulate CREATE handler calling NotifyDirChange
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "newfile.txt",
	})

	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	if len(deleg.PendingNotifs) != 1 {
		t.Fatalf("PendingNotifs = %d, want 1", len(deleg.PendingNotifs))
	}
	if deleg.PendingNotifs[0].Type != types.NOTIFY4_ADD_ENTRY {
		t.Errorf("type = %d, want NOTIFY4_ADD_ENTRY (%d)", deleg.PendingNotifs[0].Type, types.NOTIFY4_ADD_ENTRY)
	}
	if deleg.PendingNotifs[0].EntryName != "newfile.txt" {
		t.Errorf("entry = %q, want %q", deleg.PendingNotifs[0].EntryName, "newfile.txt")
	}
}

func TestNotifyHook_Remove(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-remove-dir")
	mask := uint32(1 << types.NOTIFY4_REMOVE_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_REMOVE_ENTRY,
		EntryName: "deleted.txt",
	})

	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	if len(deleg.PendingNotifs) != 1 {
		t.Fatalf("PendingNotifs = %d, want 1", len(deleg.PendingNotifs))
	}
	if deleg.PendingNotifs[0].Type != types.NOTIFY4_REMOVE_ENTRY {
		t.Errorf("type = %d, want NOTIFY4_REMOVE_ENTRY", deleg.PendingNotifs[0].Type)
	}
	if deleg.PendingNotifs[0].EntryName != "deleted.txt" {
		t.Errorf("entry = %q, want %q", deleg.PendingNotifs[0].EntryName, "deleted.txt")
	}
}

func TestNotifyHook_Rename_SameDir(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-rename-same")
	mask := uint32(1 << types.NOTIFY4_RENAME_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Same-directory rename: single RENAME_ENTRY notification
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_RENAME_ENTRY,
		EntryName: "oldname.txt",
		NewName:   "newname.txt",
	})

	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	if len(deleg.PendingNotifs) != 1 {
		t.Fatalf("PendingNotifs = %d, want 1", len(deleg.PendingNotifs))
	}
	n := deleg.PendingNotifs[0]
	if n.Type != types.NOTIFY4_RENAME_ENTRY {
		t.Errorf("type = %d, want NOTIFY4_RENAME_ENTRY", n.Type)
	}
	if n.EntryName != "oldname.txt" {
		t.Errorf("EntryName = %q, want %q", n.EntryName, "oldname.txt")
	}
	if n.NewName != "newname.txt" {
		t.Errorf("NewName = %q, want %q", n.NewName, "newname.txt")
	}
}

func TestNotifyHook_Rename_CrossDir(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	srcDirFH := []byte("hook-rename-src")
	dstDirFH := []byte("hook-rename-dst")

	// Subscribe to RENAME for source, ADD for destination
	srcMask := uint32(1 << types.NOTIFY4_RENAME_ENTRY)
	dstMask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	srcDeleg, err := sm.GrantDirDelegation(clientID, srcDirFH, srcMask)
	if err != nil {
		t.Fatalf("GrantDirDelegation(src) failed: %v", err)
	}
	dstDeleg, err := sm.GrantDirDelegation(clientID, dstDirFH, dstMask)
	if err != nil {
		t.Fatalf("GrantDirDelegation(dst) failed: %v", err)
	}

	// Simulate cross-directory rename: source gets RENAME, destination gets ADD
	sm.NotifyDirChange(srcDirFH, DirNotification{
		Type:      types.NOTIFY4_RENAME_ENTRY,
		EntryName: "moveme.txt",
		NewName:   "moved.txt",
	})
	sm.NotifyDirChange(dstDirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "moved.txt",
	})

	srcDeleg.NotifMu.Lock()
	srcCount := len(srcDeleg.PendingNotifs)
	srcDeleg.NotifMu.Unlock()

	dstDeleg.NotifMu.Lock()
	dstCount := len(dstDeleg.PendingNotifs)
	dstDeleg.NotifMu.Unlock()

	if srcCount != 1 {
		t.Errorf("source PendingNotifs = %d, want 1", srcCount)
	}
	if dstCount != 1 {
		t.Errorf("dest PendingNotifs = %d, want 1", dstCount)
	}
}

func TestNotifyHook_Link(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-link-dir")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "hardlink.txt",
	})

	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	if len(deleg.PendingNotifs) != 1 {
		t.Fatalf("PendingNotifs = %d, want 1", len(deleg.PendingNotifs))
	}
	if deleg.PendingNotifs[0].EntryName != "hardlink.txt" {
		t.Errorf("entry = %q, want %q", deleg.PendingNotifs[0].EntryName, "hardlink.txt")
	}
}

func TestNotifyHook_AttrChange(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-attr-dir")
	mask := uint32(1 << types.NOTIFY4_CHANGE_DIR_ATTRS)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	sm.NotifyDirChange(dirFH, DirNotification{
		Type: types.NOTIFY4_CHANGE_DIR_ATTRS,
	})

	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	if len(deleg.PendingNotifs) != 1 {
		t.Fatalf("PendingNotifs = %d, want 1", len(deleg.PendingNotifs))
	}
	if deleg.PendingNotifs[0].Type != types.NOTIFY4_CHANGE_DIR_ATTRS {
		t.Errorf("type = %d, want NOTIFY4_CHANGE_DIR_ATTRS", deleg.PendingNotifs[0].Type)
	}
}

func TestNotifyHook_MaskFiltering(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-mask-filter")

	// Subscribe ONLY to ADD_ENTRY
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)
	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Send REMOVE_ENTRY (not subscribed)
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_REMOVE_ENTRY,
		EntryName: "filtered.txt",
	})

	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 0 {
		t.Errorf("PendingNotifs = %d, want 0 (REMOVE filtered by mask)", count)
	}

	// Send ADD_ENTRY (subscribed) -- should be accepted
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "accepted.txt",
	})

	deleg.NotifMu.Lock()
	count = len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 1 {
		t.Errorf("PendingNotifs = %d, want 1 (ADD should pass mask)", count)
	}

	// Send CHANGE_DIR_ATTRS (not subscribed)
	sm.NotifyDirChange(dirFH, DirNotification{
		Type: types.NOTIFY4_CHANGE_DIR_ATTRS,
	})

	deleg.NotifMu.Lock()
	count = len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 1 {
		t.Errorf("PendingNotifs = %d, want 1 (CHANGE_DIR_ATTRS filtered by mask)", count)
	}
}

func TestNotifyHook_MultipleClients(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	// Register two different clients
	client1 := registerTestClientWithName(t, sm, "hook-multi-client-1")
	client2 := registerTestClientWithName(t, sm, "hook-multi-client-2")

	dirFH := []byte("hook-multi-dir")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg1, err := sm.GrantDirDelegation(client1, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation(client1) failed: %v", err)
	}
	deleg2, err := sm.GrantDirDelegation(client2, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation(client2) failed: %v", err)
	}

	// Notification from the delegation holder itself (no conflict)
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "shared-file.txt",
	})

	deleg1.NotifMu.Lock()
	count1 := len(deleg1.PendingNotifs)
	deleg1.NotifMu.Unlock()

	deleg2.NotifMu.Lock()
	count2 := len(deleg2.PendingNotifs)
	deleg2.NotifMu.Unlock()

	if count1 != 1 {
		t.Errorf("client1 PendingNotifs = %d, want 1", count1)
	}
	if count2 != 1 {
		t.Errorf("client2 PendingNotifs = %d, want 1", count2)
	}
}

func TestNotifyHook_ConflictRecall(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	// Client A holds directory delegation
	clientA := registerTestClientWithName(t, sm, "hook-conflict-A")
	clientB := registerTestClientWithName(t, sm, "hook-conflict-B")

	dirFH := []byte("hook-conflict-dir")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	delegA, err := sm.GrantDirDelegation(clientA, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation(clientA) failed: %v", err)
	}

	// Client B mutates the directory -- should trigger recall of client A's delegation
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:           types.NOTIFY4_ADD_ENTRY,
		EntryName:      "conflict-file.txt",
		OriginClientID: clientB,
	})

	// Poll for recall completion (RecallDirDelegation runs in a goroutine).
	// Use sm.mu to safely read RecallSent (set under sm.mu in RecallDirDelegation).
	deadline := time.Now().Add(2 * time.Second)
	var recalled bool
	for time.Now().Before(deadline) {
		sm.mu.RLock()
		recalled = delegA.RecallSent
		sm.mu.RUnlock()
		if recalled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Client A's delegation should be marked as recalled
	if !recalled {
		t.Error("delegA.RecallSent should be true after conflict from different client")
	}
	// RecallReason is set before the goroutine launch (in NotifyDirChange path),
	// so it is safe to read after RecallSent is observed.
	if delegA.RecallReason != "conflict" {
		t.Errorf("delegA.RecallReason = %q, want %q", delegA.RecallReason, "conflict")
	}
}

func TestNotifyHook_BatchFlush_Integration(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(50 * time.Millisecond)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-batch-flush")
	mask := uint32((1 << types.NOTIFY4_ADD_ENTRY) | (1 << types.NOTIFY4_REMOVE_ENTRY))

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Send multiple notifications
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "file1.txt",
		Cookie:    1,
	})
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_REMOVE_ENTRY,
		EntryName: "file2.txt",
		Cookie:    2,
	})
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "file3.txt",
		Cookie:    3,
	})

	// Wait for batch timer to fire and flush
	time.Sleep(200 * time.Millisecond)

	// After flush, pending should be empty (all sent via CB_NOTIFY)
	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 0 {
		t.Errorf("PendingNotifs after batch flush = %d, want 0", count)
	}
}

func TestNotifyHook_DirectoryDeleted(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-dir-deleted")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Simulate directory deletion: recall with "directory_deleted" reason
	sm.RecallDirDelegation(deleg, "directory_deleted")

	// RevokeDelegation removes from delegByFile but keeps in delegByOther
	// (for stale stateid detection). The delegation is marked Revoked=true.
	sm.mu.RLock()
	delegs := sm.delegByFile[string(dirFH)]
	revokedDeleg, inOther := sm.delegByOther[deleg.Stateid.Other]
	sm.mu.RUnlock()

	if len(delegs) != 0 {
		t.Errorf("delegByFile count = %d, want 0 (revoked)", len(delegs))
	}
	if !inOther {
		t.Error("delegation should remain in delegByOther for stale stateid detection")
	}
	if !revokedDeleg.Revoked {
		t.Error("delegation.Revoked should be true after directory_deleted recall")
	}
}

// ============================================================================
// Concurrent Notification Hook Tests
// ============================================================================

func TestNotifyHook_ConcurrentNotifications(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(10000)
	sm.SetDirDelegBatchWindow(10 * time.Millisecond)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("hook-concurrent")
	mask := uint32((1 << types.NOTIFY4_ADD_ENTRY) | (1 << types.NOTIFY4_REMOVE_ENTRY) |
		(1 << types.NOTIFY4_RENAME_ENTRY) | (1 << types.NOTIFY4_CHANGE_DIR_ATTRS))

	_, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Simulate concurrent mutations from multiple goroutines
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var notif DirNotification
			switch idx % 4 {
			case 0:
				notif = DirNotification{
					Type:      types.NOTIFY4_ADD_ENTRY,
					EntryName: "add-file.txt",
				}
			case 1:
				notif = DirNotification{
					Type:      types.NOTIFY4_REMOVE_ENTRY,
					EntryName: "rm-file.txt",
				}
			case 2:
				notif = DirNotification{
					Type:      types.NOTIFY4_RENAME_ENTRY,
					EntryName: "old.txt",
					NewName:   "new.txt",
				}
			case 3:
				notif = DirNotification{
					Type: types.NOTIFY4_CHANGE_DIR_ATTRS,
				}
			}
			sm.NotifyDirChange(dirFH, notif)
		}(i)
	}

	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	// Test passes if no data races detected (run with -race flag)
}

// ============================================================================
// Helper
// ============================================================================

// registerTestClientWithName creates a confirmed v4.0 client with a unique name.
func registerTestClientWithName(t *testing.T, sm *StateManager, name string) uint64 {
	t.Helper()
	result, err := sm.SetClientID(name, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, CallbackInfo{
		Addr:    "127.0.0.1",
		Program: 0x40000000,
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("SetClientID(%s) failed: %v", name, err)
	}
	if err := sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(%s) failed: %v", name, err)
	}
	sm.mu.Lock()
	if client, ok := sm.clientsByID[result.ClientID]; ok {
		client.CBPathUp = true
	}
	sm.mu.Unlock()
	return result.ClientID
}
