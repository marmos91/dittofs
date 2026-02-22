package state

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ============================================================================
// GrantDirDelegation Tests
// ============================================================================

func TestGrantDirDelegation_Basic(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(50 * time.Millisecond)

	// Create a v4.0 client so it can be found
	clientID := registerTestClient(t, sm)

	dirFH := []byte("dir-handle-1")
	mask := uint32((1 << types.NOTIFY4_ADD_ENTRY) | (1 << types.NOTIFY4_REMOVE_ENTRY))

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}
	if deleg == nil {
		t.Fatal("expected non-nil DelegationState")
	}
	if !deleg.IsDirectory {
		t.Error("IsDirectory should be true")
	}
	if deleg.NotificationMask != mask {
		t.Errorf("NotificationMask = 0x%x, want 0x%x", deleg.NotificationMask, mask)
	}
	if deleg.Stateid.Seqid != 1 {
		t.Errorf("Stateid.Seqid = %d, want 1", deleg.Stateid.Seqid)
	}
	if deleg.Stateid.Other[0] != StateTypeDeleg {
		t.Errorf("Stateid.Other[0] = 0x%02x, want 0x%02x", deleg.Stateid.Other[0], StateTypeDeleg)
	}
	if deleg.CookieVerf == [8]byte{} {
		t.Error("CookieVerf should be non-zero (random)")
	}

	// Verify stored in maps
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if _, exists := sm.delegByOther[deleg.Stateid.Other]; !exists {
		t.Error("delegation should be in delegByOther")
	}
	if delegs := sm.delegByFile[string(dirFH)]; len(delegs) != 1 {
		t.Errorf("delegByFile count = %d, want 1", len(delegs))
	}
}

func TestGrantDirDelegation_DelegationsDisabled(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetDelegationsEnabled(false)

	clientID := registerTestClient(t, sm)

	_, err := sm.GrantDirDelegation(clientID, []byte("dir-handle"), 0xFF)
	if err == nil {
		t.Fatal("expected error when delegations disabled")
	}
}

func TestGrantDirDelegation_LimitExceeded(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(1) // Only allow 1 delegation total

	clientID := registerTestClient(t, sm)

	// Grant first delegation (file delegation via existing method)
	sm.GrantDelegation(clientID, []byte("fh-file"), types.OPEN_DELEGATE_READ)

	// Second should fail (limit=1, already have 1)
	_, err := sm.GrantDirDelegation(clientID, []byte("dir-handle"), 0xFF)
	if err == nil {
		t.Fatal("expected error when delegation limit exceeded")
	}
}

func TestGrantDirDelegation_DoubleGrant(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-handle-double")

	// First grant succeeds
	_, err := sm.GrantDirDelegation(clientID, dirFH, 0xFF)
	if err != nil {
		t.Fatalf("first grant failed: %v", err)
	}

	// Second grant for same client+handle fails
	_, err = sm.GrantDirDelegation(clientID, dirFH, 0xFF)
	if err == nil {
		t.Fatal("expected error for double grant")
	}
}

func TestGrantDirDelegation_ExpiredLease(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)

	// Non-existent client
	_, err := sm.GrantDirDelegation(99999, []byte("dir-handle"), 0xFF)
	if err == nil {
		t.Fatal("expected error for non-existent client")
	}
	// Should be NFS4ERR_EXPIRED
	if nfsErr, ok := err.(*NFS4StateError); ok {
		if nfsErr.Status != types.NFS4ERR_EXPIRED {
			t.Errorf("error status = %d, want NFS4ERR_EXPIRED (%d)", nfsErr.Status, types.NFS4ERR_EXPIRED)
		}
	} else {
		t.Errorf("expected NFS4StateError, got %T", err)
	}
}

// ============================================================================
// NotifyDirChange Tests
// ============================================================================

func TestNotifyDirChange_Basic(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour) // Long window so flush doesn't happen

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-notify-basic")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	notif := DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "new-file.txt",
		Cookie:    42,
	}

	sm.NotifyDirChange(dirFH, notif)

	// Check pending notifications
	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	if len(deleg.PendingNotifs) != 1 {
		t.Fatalf("PendingNotifs count = %d, want 1", len(deleg.PendingNotifs))
	}
	if deleg.PendingNotifs[0].EntryName != "new-file.txt" {
		t.Errorf("entry name = %q, want %q", deleg.PendingNotifs[0].EntryName, "new-file.txt")
	}
}

func TestNotifyDirChange_FilterByMask(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-notify-mask")

	// Subscribe only to ADD_ENTRY
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)
	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Send REMOVE notification (not subscribed)
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_REMOVE_ENTRY,
		EntryName: "old-file.txt",
	})

	// Should not be queued
	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 0 {
		t.Errorf("PendingNotifs = %d, want 0 (filtered by mask)", count)
	}

	// Send ADD notification (subscribed)
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "new-file.txt",
	})

	deleg.NotifMu.Lock()
	count = len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 1 {
		t.Errorf("PendingNotifs = %d, want 1", count)
	}
}

func TestNotifyDirChange_RecalledSkipped(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-notify-recalled")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Mark as recalled
	deleg.RecallSent = true

	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "new-file.txt",
	})

	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 0 {
		t.Errorf("PendingNotifs = %d, want 0 (recalled delegation skipped)", count)
	}
}

func TestNotifyDirChange_BatchFlush(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(50 * time.Millisecond)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-notify-flush")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Send a notification
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "file1.txt",
		Cookie:    1,
	})

	// Wait for batch timer to fire
	time.Sleep(200 * time.Millisecond)

	// After flush, pending should be empty
	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 0 {
		t.Errorf("PendingNotifs after flush = %d, want 0", count)
	}
}

func TestNotifyDirChange_CountFlush(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(10000)
	sm.SetDirDelegBatchWindow(1 * time.Hour) // Very long, should not trigger

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-notify-count")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Send maxBatchSize notifications to trigger count-based flush
	for i := 0; i < maxBatchSize; i++ {
		sm.NotifyDirChange(dirFH, DirNotification{
			Type:      types.NOTIFY4_ADD_ENTRY,
			EntryName: "file.txt",
			Cookie:    uint64(i),
		})
	}

	// After count-based flush, pending should be empty (or near-empty due to timing)
	// Give a tiny bit of time for the flush goroutine
	time.Sleep(10 * time.Millisecond)

	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count >= maxBatchSize {
		t.Errorf("PendingNotifs = %d, expected < %d (count-based flush)", count, maxBatchSize)
	}
}

// ============================================================================
// RecallDirDelegation Tests
// ============================================================================

func TestRecallDirDelegation_FlushBeforeRecall(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour) // Long, so no timer flush

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-recall-flush")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Add pending notifications
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "pending-file.txt",
		Cookie:    99,
	})

	// Recall the delegation (flush should happen before recall)
	sm.RecallDirDelegation(deleg, "conflict")

	// Pending should be flushed
	deleg.NotifMu.Lock()
	count := len(deleg.PendingNotifs)
	deleg.NotifMu.Unlock()
	if count != 0 {
		t.Errorf("PendingNotifs after recall = %d, want 0 (should be flushed)", count)
	}

	// RecallSent should be true
	if !deleg.RecallSent {
		t.Error("RecallSent should be true after recall")
	}
	if deleg.RecallReason != "conflict" {
		t.Errorf("RecallReason = %q, want %q", deleg.RecallReason, "conflict")
	}
}

func TestRecallDirDelegation_DirectoryDeleted(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-deleted")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Recall with "directory_deleted" reason -- should revoke immediately
	sm.RecallDirDelegation(deleg, "directory_deleted")

	if deleg.RecallReason != "directory_deleted" {
		t.Errorf("RecallReason = %q, want %q", deleg.RecallReason, "directory_deleted")
	}

	// Delegation should be revoked (removed from delegByFile)
	sm.mu.RLock()
	delegs := sm.delegByFile[string(dirFH)]
	sm.mu.RUnlock()
	if len(delegs) != 0 {
		t.Errorf("delegByFile count = %d, want 0 (revoked)", len(delegs))
	}
}

// ============================================================================
// Client Cleanup Tests
// ============================================================================

func TestDirDelegation_ClientCleanup(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-cleanup")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Add pending notifications
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "pending.txt",
	})

	// Verify timer is running
	deleg.NotifMu.Lock()
	hasTimer := deleg.BatchTimer != nil
	deleg.NotifMu.Unlock()
	if !hasTimer {
		t.Error("BatchTimer should be set after notification")
	}

	// Evict the client (simulates DESTROY_CLIENTID cleanup)
	if err := sm.EvictV40Client(clientID); err != nil {
		t.Fatalf("EvictV40Client failed: %v", err)
	}

	// After eviction: pending notifications cleared, timer stopped
	deleg.NotifMu.Lock()
	pendingCount := len(deleg.PendingNotifs)
	timerNil := deleg.BatchTimer == nil
	deleg.NotifMu.Unlock()

	if pendingCount != 0 {
		t.Errorf("PendingNotifs after cleanup = %d, want 0", pendingCount)
	}
	if !timerNil {
		t.Error("BatchTimer should be nil after cleanup")
	}

	// Delegation should be removed from maps
	sm.mu.RLock()
	_, inOther := sm.delegByOther[deleg.Stateid.Other]
	delegs := sm.delegByFile[string(dirFH)]
	sm.mu.RUnlock()

	if inOther {
		t.Error("delegation should be removed from delegByOther")
	}
	if len(delegs) != 0 {
		t.Errorf("delegByFile count = %d, want 0", len(delegs))
	}
}

func TestDirDelegation_V41ClientCleanup(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(100)
	sm.SetDirDelegBatchWindow(1 * time.Hour)

	// Register a v4.1 client
	v41Client := registerTestV41Client(t, sm)
	dirFH := []byte("dir-v41-cleanup")
	mask := uint32(1 << types.NOTIFY4_ADD_ENTRY)

	deleg, err := sm.GrantDirDelegation(v41Client, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Add pending notifications
	sm.NotifyDirChange(dirFH, DirNotification{
		Type:      types.NOTIFY4_ADD_ENTRY,
		EntryName: "pending-v41.txt",
	})

	// Evict the v4.1 client
	if err := sm.EvictV41Client(v41Client); err != nil {
		t.Fatalf("EvictV41Client failed: %v", err)
	}

	// Delegation should be cleaned up
	deleg.NotifMu.Lock()
	pendingCount := len(deleg.PendingNotifs)
	timerNil := deleg.BatchTimer == nil
	deleg.NotifMu.Unlock()

	if pendingCount != 0 {
		t.Errorf("PendingNotifs after v4.1 cleanup = %d, want 0", pendingCount)
	}
	if !timerNil {
		t.Error("BatchTimer should be nil after v4.1 cleanup")
	}
}

// ============================================================================
// Concurrent Tests
// ============================================================================

func TestDirDelegation_Concurrent(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetMaxDelegations(10000)
	sm.SetDirDelegBatchWindow(10 * time.Millisecond)

	clientID := registerTestClient(t, sm)
	dirFH := []byte("dir-concurrent")
	mask := uint32((1 << types.NOTIFY4_ADD_ENTRY) | (1 << types.NOTIFY4_REMOVE_ENTRY))

	_, err := sm.GrantDirDelegation(clientID, dirFH, mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation failed: %v", err)
	}

	// Concurrent notifications from multiple goroutines
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			notifType := types.NOTIFY4_ADD_ENTRY
			if idx%2 == 0 {
				notifType = types.NOTIFY4_REMOVE_ENTRY
			}
			sm.NotifyDirChange(dirFH, DirNotification{
				Type:      uint32(notifType),
				EntryName: "concurrent-file.txt",
				Cookie:    uint64(idx),
			})
		}(i)
	}

	wg.Wait()

	// Wait for any pending flushes
	time.Sleep(100 * time.Millisecond)

	// The test passes if no data races are detected (run with -race)
}

// ============================================================================
// Helper Functions
// ============================================================================

// registerTestClient creates a confirmed v4.0 client for testing.
func registerTestClient(t *testing.T, sm *StateManager) uint64 {
	t.Helper()
	return registerTestClientWithName(t, sm, "test-client-dir-deleg")
}

// registerTestV41Client creates a v4.1 client record for testing.
func registerTestV41Client(t *testing.T, sm *StateManager) uint64 {
	t.Helper()
	sm.mu.Lock()
	defer sm.mu.Unlock()

	clientID := sm.generateClientID()
	record := &V41ClientRecord{
		ClientID:   clientID,
		OwnerID:    []byte("test-v41-client-dir-deleg"),
		ClientAddr: "127.0.0.1",
		CreatedAt:  time.Now(),
	}
	sm.v41ClientsByID[clientID] = record
	sm.v41ClientsByOwner[string(record.OwnerID)] = record
	return clientID
}
