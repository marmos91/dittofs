package lock

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// mockLeaseBreakCallback implements LeaseBreakCallback for testing.
type mockLeaseBreakCallback struct {
	mu          sync.Mutex
	timedOutKeys []string // hex strings of timed-out lease keys
}

func (m *mockLeaseBreakCallback) OnLeaseBreakTimeout(leaseKey [16]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timedOutKeys = append(m.timedOutKeys, string(leaseKey[:]))
}

func (m *mockLeaseBreakCallback) getTimedOutKeys() []string {
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
		AcquiredAt:   breakStart, // Break start time
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

func TestLeaseBreakScanner_StartStop(t *testing.T) {
	store := newMockLockStore()
	scanner := NewLeaseBreakScanner(store, nil, 100*time.Millisecond)

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

func TestLeaseBreakScanner_DefaultTimeout(t *testing.T) {
	store := newMockLockStore()

	// Test default timeout (0 should use DefaultLeaseBreakTimeout)
	scanner := NewLeaseBreakScanner(store, nil, 0)
	if scanner.GetTimeout() != DefaultLeaseBreakTimeout {
		t.Errorf("Expected default timeout %v, got %v", DefaultLeaseBreakTimeout, scanner.GetTimeout())
	}

	// Test custom timeout
	scanner2 := NewLeaseBreakScanner(store, nil, 10*time.Second)
	if scanner2.GetTimeout() != 10*time.Second {
		t.Errorf("Expected custom timeout 10s, got %v", scanner2.GetTimeout())
	}
}

func TestLeaseBreakScanner_SetTimeout(t *testing.T) {
	store := newMockLockStore()
	scanner := NewLeaseBreakScanner(store, nil, 5*time.Second)

	scanner.SetTimeout(10 * time.Second)
	if scanner.GetTimeout() != 10*time.Second {
		t.Errorf("Expected timeout 10s after SetTimeout, got %v", scanner.GetTimeout())
	}
}

func TestLeaseBreakScanner_ExpiredBreakTriggersCallback(t *testing.T) {
	store := newMockLockStore()
	callback := &mockLeaseBreakCallback{}

	// Use a short timeout and scan interval for testing
	scanner := NewLeaseBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

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

func TestLeaseBreakScanner_NonBreakingLeasesNotAffected(t *testing.T) {
	store := newMockLockStore()
	callback := &mockLeaseBreakCallback{}

	scanner := NewLeaseBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

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

func TestLeaseBreakScanner_NotExpiredBreakNotAffected(t *testing.T) {
	store := newMockLockStore()
	callback := &mockLeaseBreakCallback{}

	// Long timeout (5 seconds), short scan interval
	scanner := NewLeaseBreakScannerWithInterval(store, callback, 5*time.Second, 10*time.Millisecond)

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

func TestLeaseBreakScanner_MultipleLeases(t *testing.T) {
	store := newMockLockStore()
	callback := &mockLeaseBreakCallback{}

	scanner := NewLeaseBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

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

func TestLeaseBreakScanner_NilCallback(t *testing.T) {
	store := newMockLockStore()

	// Scanner with nil callback should not panic
	scanner := NewLeaseBreakScannerWithInterval(store, nil, 50*time.Millisecond, 10*time.Millisecond)

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

func TestLeaseBreakScanner_ByteRangeLocksIgnored(t *testing.T) {
	store := newMockLockStore()
	callback := &mockLeaseBreakCallback{}

	scanner := NewLeaseBreakScannerWithInterval(store, callback, 50*time.Millisecond, 10*time.Millisecond)

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
