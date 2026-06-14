package lock

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// mockOpLockBreakCallback implements OpLockBreakCallback for testing.
type mockOpLockBreakCallback struct {
	mu           sync.Mutex
	timedOutKeys []string // hex strings of timed-out lease keys
}

func (m *mockOpLockBreakCallback) OnLeaseBreakTimeout(leaseKey [16]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timedOutKeys = append(m.timedOutKeys, string(leaseKey[:]))
}

func (m *mockOpLockBreakCallback) getTimedOutKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.timedOutKeys))
	copy(result, m.timedOutKeys)
	return result
}

// mockLockStore implements LockStore for testing.
type mockLockStore struct {
	mu    sync.Mutex
	locks map[string]*PersistedLock
}

func newMockLockStore() *mockLockStore {
	return &mockLockStore{
		locks: make(map[string]*PersistedLock),
	}
}

func (s *mockLockStore) PutLock(ctx context.Context, lock *PersistedLock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locks[lock.ID] = lock
	return nil
}

func (s *mockLockStore) GetLock(ctx context.Context, lockID string) (*PersistedLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock, ok := s.locks[lockID]; ok {
		return lock, nil
	}
	return nil, &errors.StoreError{
		Code:    errors.ErrLockNotFound,
		Message: fmt.Sprintf("lock %s not found", lockID),
	}
}

func (s *mockLockStore) DeleteLock(ctx context.Context, lockID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.locks[lockID]; !ok {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: fmt.Sprintf("lock %s not found", lockID),
		}
	}
	delete(s.locks, lockID)
	return nil
}

func (s *mockLockStore) ListLocks(ctx context.Context, query LockQuery) ([]*PersistedLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []*PersistedLock
	for _, lock := range s.locks {
		if query.MatchesLock(lock) {
			result = append(result, lock)
		}
	}
	return result, nil
}

func (s *mockLockStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, lock := range s.locks {
		if lock.ClientID == clientID {
			delete(s.locks, id)
			count++
		}
	}
	return count, nil
}

func (s *mockLockStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, lock := range s.locks {
		if lock.FileID == fileID {
			delete(s.locks, id)
			count++
		}
	}
	return count, nil
}

func (s *mockLockStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	return 1, nil
}

func (s *mockLockStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	return 2, nil
}

func (s *mockLockStore) GetCleanShutdown(_ context.Context) (bool, error) {
	return false, nil
}

func (s *mockLockStore) SetCleanShutdown(_ context.Context, _ bool) error {
	return nil
}

func (s *mockLockStore) ReclaimLease(_ context.Context, _ FileHandle, _ [16]byte, _ string) (*UnifiedLock, error) {
	// Mock implementation returns not found - reclaim not supported in mock
	return nil, nil
}

// Helper to create a breaking lease
func createBreakingLease(id string, leaseKey [16]byte, breakStart time.Time) *PersistedLock {
	return &PersistedLock{
		ID:           id,
		ShareName:    "share1",
		FileID:       "file1",
		OwnerID:      "smb:lease:" + id,
		ClientID:     "client1",
		LeaseKey:     leaseKey[:],
		LeaseState:   LeaseStateRead | LeaseStateWrite,
		Breaking:     true,
		BreakToState: LeaseStateRead,
		AcquiredAt:   breakStart,
		BreakStarted: breakStart, // break-clock, not grant-clock
	}
}

// Helper to create a non-breaking lease
func createNonBreakingLease(id string, leaseKey [16]byte) *PersistedLock {
	return &PersistedLock{
		ID:         id,
		ShareName:  "share1",
		FileID:     "file1",
		OwnerID:    "smb:lease:" + id,
		ClientID:   "client1",
		LeaseKey:   leaseKey[:],
		LeaseState: LeaseStateRead,
		Breaking:   false,
		AcquiredAt: time.Now(),
	}
}

func TestOpLockBreakScanner_StartStop(t *testing.T) {
	store := newMockLockStore()
	scanner := NewOpLockBreakScanner(store, nil, 100*time.Millisecond)

	// Initially not running
	if scanner.IsRunning() {
		t.Error("Scanner should not be running initially")
	}

	// Start
	scanner.Start()
	if !scanner.IsRunning() {
		t.Error("Scanner should be running after Start()")
	}

	// Double start should be no-op
	scanner.Start()
	if !scanner.IsRunning() {
		t.Error("Scanner should still be running after double Start()")
	}

	// Stop
	scanner.Stop()
	if scanner.IsRunning() {
		t.Error("Scanner should not be running after Stop()")
	}

	// Double stop should be no-op
	scanner.Stop()
	if scanner.IsRunning() {
		t.Error("Scanner should still not be running after double Stop()")
	}

	// Restart should work
	scanner.Start()
	if !scanner.IsRunning() {
		t.Error("Scanner should be running after restart")
	}
	scanner.Stop()
}

func TestOpLockBreakScanner_DefaultTimeout(t *testing.T) {
	store := newMockLockStore()

	// Test default timeout (0 should use DefaultOpLockBreakTimeout)
	scanner := NewOpLockBreakScanner(store, nil, 0)
	if scanner.GetTimeout() != DefaultOpLockBreakTimeout {
		t.Errorf("Expected default timeout %v, got %v", DefaultOpLockBreakTimeout, scanner.GetTimeout())
	}

	// Test custom timeout
	scanner2 := NewOpLockBreakScanner(store, nil, 10*time.Second)
	if scanner2.GetTimeout() != 10*time.Second {
		t.Errorf("Expected custom timeout 10s, got %v", scanner2.GetTimeout())
	}
}

func TestOpLockBreakScanner_SetTimeout(t *testing.T) {
	store := newMockLockStore()
	scanner := NewOpLockBreakScanner(store, nil, 5*time.Second)

	scanner.SetTimeout(10 * time.Second)
	if scanner.GetTimeout() != 10*time.Second {
		t.Errorf("Expected timeout 10s after SetTimeout, got %v", scanner.GetTimeout())
	}
}

func TestOpLockBreakScanner_ExpiredBreakTriggersCallback(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	// Use a short timeout and scan interval for testing
	scanner := NewOpLockBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

	// Create a lease that started breaking 100ms ago (already expired)
	var leaseKey [16]byte
	copy(leaseKey[:], []byte("expiredlease1234"))
	lease := createBreakingLease("lease1", leaseKey, time.Now().Add(-100*time.Millisecond))
	_ = store.PutLock(context.Background(), lease)

	// Start scanner
	scanner.Start()
	defer scanner.Stop()

	// Wait for scanner to detect and process the expired break
	time.Sleep(100 * time.Millisecond)

	// Check callback was called
	timedOut := callback.getTimedOutKeys()
	if len(timedOut) != 1 {
		t.Errorf("Expected 1 timed out key, got %d", len(timedOut))
		return
	}
	if timedOut[0] != string(leaseKey[:]) {
		t.Errorf("Expected key %v, got %v", leaseKey[:], []byte(timedOut[0]))
	}

	// Verify lease was deleted
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 0 {
		t.Errorf("Expected lease to be deleted, but found %d locks", len(locks))
	}
}

func TestOpLockBreakScanner_NonBreakingLeasesNotAffected(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	scanner := NewOpLockBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

	// Create a non-breaking lease
	var leaseKey [16]byte
	copy(leaseKey[:], []byte("nonbreakinglse12"))
	lease := createNonBreakingLease("lease1", leaseKey)
	_ = store.PutLock(context.Background(), lease)

	// Start scanner
	scanner.Start()
	defer scanner.Stop()

	// Wait for multiple scan cycles
	time.Sleep(100 * time.Millisecond)

	// Callback should NOT have been called
	timedOut := callback.getTimedOutKeys()
	if len(timedOut) != 0 {
		t.Errorf("Expected 0 timed out keys, got %d", len(timedOut))
	}

	// Lease should still exist
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 1 {
		t.Errorf("Expected lease to still exist, but found %d locks", len(locks))
	}
}

func TestOpLockBreakScanner_NotExpiredBreakNotAffected(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	// Long timeout (5 seconds), short scan interval
	scanner := NewOpLockBreakScannerWithInterval(store, callback, 5*time.Second, 10*time.Millisecond)

	// Create a breaking lease that just started (not expired)
	var leaseKey [16]byte
	copy(leaseKey[:], []byte("recentbreakkey12"))
	lease := createBreakingLease("lease1", leaseKey, time.Now())
	_ = store.PutLock(context.Background(), lease)

	// Start scanner
	scanner.Start()
	defer scanner.Stop()

	// Wait for multiple scan cycles
	time.Sleep(100 * time.Millisecond)

	// Callback should NOT have been called (not expired yet)
	timedOut := callback.getTimedOutKeys()
	if len(timedOut) != 0 {
		t.Errorf("Expected 0 timed out keys, got %d", len(timedOut))
	}

	// Lease should still exist
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 1 {
		t.Errorf("Expected lease to still exist, but found %d locks", len(locks))
	}
}

func TestOpLockBreakScanner_MultipleLeases(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	scanner := NewOpLockBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

	// Create multiple leases:
	// 1. Expired breaking lease (should be revoked)
	// 2. Non-expired breaking lease (should remain) - use 5 second timeout
	// 3. Non-breaking lease (should remain)

	var key1, key2, key3 [16]byte
	copy(key1[:], []byte("expiredkey123456"))
	copy(key2[:], []byte("recentbreakkey12"))
	copy(key3[:], []byte("normalleasekey12"))

	lease1 := createBreakingLease("lease1", key1, time.Now().Add(-100*time.Millisecond))
	lease2 := createBreakingLease("lease2", key2, time.Now().Add(5*time.Second)) // Far in the future
	lease3 := createNonBreakingLease("lease3", key3)

	_ = store.PutLock(context.Background(), lease1)
	_ = store.PutLock(context.Background(), lease2)
	_ = store.PutLock(context.Background(), lease3)

	// Start scanner
	scanner.Start()
	defer scanner.Stop()

	// Wait for scanner
	time.Sleep(100 * time.Millisecond)

	// Only lease1 should have timed out
	timedOut := callback.getTimedOutKeys()
	if len(timedOut) != 1 {
		t.Errorf("Expected 1 timed out key, got %d", len(timedOut))
	}

	// Two leases should remain
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 2 {
		t.Errorf("Expected 2 leases to remain, but found %d", len(locks))
	}
}

func TestOpLockBreakScanner_NilCallback(t *testing.T) {
	store := newMockLockStore()

	// Scanner with nil callback should not panic
	scanner := NewOpLockBreakScannerWithInterval(store, nil, 50*time.Millisecond, 10*time.Millisecond)

	// Create an expired breaking lease
	var leaseKey [16]byte
	copy(leaseKey[:], []byte("expiredlease1234"))
	lease := createBreakingLease("lease1", leaseKey, time.Now().Add(-100*time.Millisecond))
	_ = store.PutLock(context.Background(), lease)

	// Start scanner
	scanner.Start()
	defer scanner.Stop()

	// Wait for scanner
	time.Sleep(100 * time.Millisecond)

	// Lease should still be deleted even without callback
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 0 {
		t.Errorf("Expected lease to be deleted, but found %d locks", len(locks))
	}
}

func TestOpLockBreakScanner_ByteRangeLocksIgnored(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	scanner := NewOpLockBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

	// Create a byte-range lock (no LeaseKey)
	byteRangeLock := &PersistedLock{
		ID:         "lock1",
		ShareName:  "share1",
		FileID:     "file1",
		OwnerID:    "nlm:client1:123",
		ClientID:   "client1",
		LockType:   1, // Exclusive
		Offset:     0,
		Length:     100,
		AcquiredAt: time.Now().Add(-100 * time.Millisecond),
		// No LeaseKey - this is a byte-range lock
	}
	_ = store.PutLock(context.Background(), byteRangeLock)

	// Start scanner
	scanner.Start()
	defer scanner.Stop()

	// Wait for scanner
	time.Sleep(100 * time.Millisecond)

	// Callback should NOT have been called (byte-range lock, not a lease)
	timedOut := callback.getTimedOutKeys()
	if len(timedOut) != 0 {
		t.Errorf("Expected 0 timed out keys for byte-range lock, got %d", len(timedOut))
	}

	// Lock should still exist
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 1 {
		t.Errorf("Expected byte-range lock to still exist, but found %d locks", len(locks))
	}
}

// TestOpLockBreakScanner_UsesBreakStartedNotAcquiredAt verifies that the
// scanner measures timeout from BreakStarted, not AcquiredAt. A lease held
// for a long time (large AcquiredAt offset) must NOT be revoked if
// BreakStarted is recent.
func TestOpLockBreakScanner_UsesBreakStartedNotAcquiredAt(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	// 200ms timeout, 10ms scan interval.
	scanner := NewOpLockBreakScannerWithInterval(store, callback, 200*time.Millisecond, 10*time.Millisecond)

	var leaseKey [16]byte
	copy(leaseKey[:], []byte("longheldbreaklse"))

	// Lease was granted 10 minutes ago (AcquiredAt far in the past),
	// but the break was only initiated just now (BreakStarted = now).
	pl := &PersistedLock{
		ID:           "lease-longheld",
		ShareName:    "share1",
		FileID:       "file1",
		OwnerID:      "smb:client1",
		ClientID:     "client1",
		LeaseKey:     leaseKey[:],
		LeaseState:   LeaseStateRead | LeaseStateWrite,
		Breaking:     true,
		BreakToState: LeaseStateRead,
		AcquiredAt:   time.Now().Add(-10 * time.Minute), // very old grant
		BreakStarted: time.Now(),                        // break JUST started
	}
	_ = store.PutLock(context.Background(), pl)

	scanner.Start()
	defer scanner.Stop()

	// Wait 3 scan cycles — well under the 200ms break timeout.
	time.Sleep(50 * time.Millisecond)

	// Callback must NOT have fired: the break just started, not yet expired.
	if len(callback.getTimedOutKeys()) != 0 {
		t.Fatal("scanner revoked a long-held lease whose break just started (using AcquiredAt instead of BreakStarted)")
	}
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 1 {
		t.Fatalf("expected lease to still exist, got %d locks", len(locks))
	}
}

// TestOpLockBreakScanner_LegacyZeroBreakStartedNotRevoked verifies the
// backwards-compatibility path: a breaking lease persisted before BreakStarted
// existed has a zero BreakStarted. The scanner must treat that conservatively
// (reference = now) and NOT immediately revoke it, even though AcquiredAt is far
// in the past. Otherwise every legacy in-flight break would be revoked on the
// first scan after upgrade.
func TestOpLockBreakScanner_LegacyZeroBreakStartedNotRevoked(t *testing.T) {
	store := newMockLockStore()
	callback := &mockOpLockBreakCallback{}

	// 5s timeout so a "now" reference cannot expire during the test window.
	scanner := NewOpLockBreakScannerWithInterval(store, callback, 5*time.Second, 10*time.Millisecond)

	var leaseKey [16]byte
	copy(leaseKey[:], []byte("legacynobreakset"))

	pl := &PersistedLock{
		ID:           "legacy-lease",
		ShareName:    "share1",
		FileID:       "file1",
		OwnerID:      "smb:client1",
		ClientID:     "client1",
		LeaseKey:     leaseKey[:],
		LeaseState:   LeaseStateRead | LeaseStateWrite,
		Breaking:     true,
		BreakToState: LeaseStateRead,
		AcquiredAt:   time.Now().Add(-10 * time.Minute), // old grant
		// BreakStarted intentionally left zero (legacy record).
	}
	_ = store.PutLock(context.Background(), pl)

	scanner.Start()
	defer scanner.Stop()

	time.Sleep(60 * time.Millisecond) // several scan cycles

	if len(callback.getTimedOutKeys()) != 0 {
		t.Fatal("scanner revoked a legacy zero-BreakStarted lease on first scan (should use now as reference)")
	}
	locks, _ := store.ListLocks(context.Background(), LockQuery{})
	if len(locks) != 1 {
		t.Fatalf("expected legacy lease to still exist, got %d locks", len(locks))
	}
}

// TestOpLockBreakScanner_RevokeUnblocksWaitForBreakCompletion verifies that
// after the scanner force-revokes a timed-out lease from the store, any
// goroutine blocked in Manager.WaitForBreakCompletion is unblocked promptly
// (well within the context deadline) rather than waiting for its own deadline.
func TestOpLockBreakScanner_RevokeUnblocksWaitForBreakCompletion(t *testing.T) {
	ctx := context.Background()

	store := newMockLockStore()
	lm := NewManager()
	lm.SetLockStore(store)

	// 100ms break timeout, 20ms scan interval — fast for testing.
	scanner := NewOpLockBreakScannerWithInterval(store, nil, 100*time.Millisecond, 20*time.Millisecond)
	scanner.SetLockManager(lm)

	var leaseKey [16]byte
	copy(leaseKey[:], []byte("waitunblockkey12"))

	// Add breaking lease to both Manager (in-memory) and store (persisted).
	breakingLock := &UnifiedLock{
		ID:         "lease-wait-test",
		Owner:      LockOwner{OwnerID: "smb:client1", ClientID: "client1", ShareName: "share1"},
		FileHandle: FileHandle("file-wait"),
		Type:       LockTypeShared,
		AcquiredAt: time.Now().Add(-5 * time.Minute), // granted long ago
		Lease: &OpLock{
			LeaseKey:     leaseKey,
			LeaseState:   LeaseStateRead | LeaseStateWrite,
			Breaking:     true,
			BreakToState: LeaseStateRead,
			BreakStarted: time.Now().Add(-200 * time.Millisecond), // break already expired
		},
	}
	// Insert directly into in-memory map to bypass conflict-check path.
	lm.mu.Lock()
	lm.unifiedLocks["file-wait"] = append(lm.unifiedLocks["file-wait"], breakingLock)
	lm.mu.Unlock()

	// Persist it so the scanner can find it.
	pl := ToPersistedLock(breakingLock, 1)
	pl.ShareName = "share1"
	_ = store.PutLock(ctx, pl)

	scanner.Start()
	defer scanner.Stop()

	// WaitForBreakCompletion with a 2-second deadline: if Bug 2 is present the
	// goroutine blocks for the full 2s and returns DeadlineExceeded.
	// With the fix it returns nil well within 500ms (the scanner fires within
	// 100ms+20ms).
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- lm.WaitForBreakCompletion(waitCtx, "file-wait")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForBreakCompletion returned error after scanner revoke: %v", err)
		}
		// Pass: unblocked before the 2s deadline.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WaitForBreakCompletion was not unblocked within 500ms after scanner force-revoke (Bug 2 not fixed)")
	}

	// Verify in-memory lease is gone.
	lm.mu.RLock()
	remaining := lm.unifiedLocks["file-wait"]
	lm.mu.RUnlock()
	if len(remaining) != 0 {
		t.Fatalf("expected no in-memory leases after revoke, got %d", len(remaining))
	}
}
