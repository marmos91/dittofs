package handlers

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// mockDurableStore is a minimal in-memory DurableHandleStore for testing the scavenger.
type mockDurableStore struct {
	mu      sync.RWMutex
	handles map[string]*lock.PersistedDurableHandle
}

func newMockDurableStore() *mockDurableStore {
	return &mockDurableStore{handles: make(map[string]*lock.PersistedDurableHandle)}
}

func (s *mockDurableStore) PutDurableHandle(_ context.Context, h *lock.PersistedDurableHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles[h.ID] = h
	return nil
}

func (s *mockDurableStore) GetDurableHandle(_ context.Context, id string) (*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handles[id], nil
}

func (s *mockDurableStore) GetDurableHandleByFileID(_ context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.handles {
		if h.FileID == fileID {
			return h, nil
		}
	}
	return nil, nil
}

func (s *mockDurableStore) GetDurableHandleByCreateGuid(_ context.Context, guid [16]byte) (*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.handles {
		if h.CreateGuid == guid {
			return h, nil
		}
	}
	return nil, nil
}

func (s *mockDurableStore) GetDurableHandlesByAppInstanceId(_ context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*lock.PersistedDurableHandle
	for _, h := range s.handles {
		if h.AppInstanceId == appInstanceId {
			result = append(result, h)
		}
	}
	return result, nil
}

func (s *mockDurableStore) GetDurableHandlesByFileHandle(_ context.Context, fh []byte) ([]*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*lock.PersistedDurableHandle
	for _, h := range s.handles {
		if bytes.Equal(h.MetadataHandle, fh) {
			result = append(result, h)
		}
	}
	return result, nil
}

func (s *mockDurableStore) DeleteDurableHandle(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handles, id)
	return nil
}

func (s *mockDurableStore) ListDurableHandles(_ context.Context) ([]*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*lock.PersistedDurableHandle
	for _, h := range s.handles {
		result = append(result, h)
	}
	return result, nil
}

func (s *mockDurableStore) ListDurableHandlesByShare(_ context.Context, share string) ([]*lock.PersistedDurableHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*lock.PersistedDurableHandle
	for _, h := range s.handles {
		if h.ShareName == share {
			result = append(result, h)
		}
	}
	return result, nil
}

func (s *mockDurableStore) DeleteExpiredDurableHandles(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for id, h := range s.handles {
		if !h.DisconnectedAt.Add(time.Duration(h.TimeoutMs) * time.Millisecond).After(now) {
			delete(s.handles, id)
			count++
		}
	}
	return count, nil
}

func (s *mockDurableStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.handles)
}

func TestScavengerExpiresTimedOutHandles(t *testing.T) {
	store := newMockDurableStore()
	now := time.Now()

	// Put an expired handle (disconnected 2 seconds ago, timeout 1 second)
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "expired-1",
		Path:            "/test/file1",
		ShareName:       "share1",
		DisconnectedAt:  now.Add(-2 * time.Second),
		TimeoutMs:       1000, // 1 second
		ServerStartTime: now,
	})

	// Put a valid handle (disconnected 1 second ago, timeout 10 seconds)
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "valid-1",
		Path:            "/test/file2",
		ShareName:       "share1",
		DisconnectedAt:  now.Add(-1 * time.Second),
		TimeoutMs:       10000, // 10 seconds
		ServerStartTime: now,
	})

	scavenger := NewDurableHandleScavenger(store, nil, 50*time.Millisecond, 10000, now)
	ctx, cancel := context.WithCancel(context.Background())

	// Run scavenger in background
	done := make(chan struct{})
	go func() {
		scavenger.Run(ctx)
		close(done)
	}()

	// Wait for at least one tick
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	// Expired handle should be removed, valid handle should remain
	if store.count() != 1 {
		t.Fatalf("expected 1 handle remaining, got %d", store.count())
	}

	h, _ := store.GetDurableHandle(context.Background(), "valid-1")
	if h == nil {
		t.Fatal("expected valid-1 to remain in store")
	}

	h, _ = store.GetDurableHandle(context.Background(), "expired-1")
	if h != nil {
		t.Fatal("expected expired-1 to be removed from store")
	}
}

func TestScavengerDoesNotExpireValidHandles(t *testing.T) {
	store := newMockDurableStore()
	now := time.Now()

	// Put two valid handles
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "valid-1",
		DisconnectedAt:  now.Add(-1 * time.Second),
		TimeoutMs:       60000, // 60 seconds
		ServerStartTime: now,
	})
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "valid-2",
		DisconnectedAt:  now.Add(-500 * time.Millisecond),
		TimeoutMs:       60000,
		ServerStartTime: now,
	})

	scavenger := NewDurableHandleScavenger(store, nil, 50*time.Millisecond, 60000, now)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scavenger.Run(ctx)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if store.count() != 2 {
		t.Fatalf("expected 2 handles remaining, got %d", store.count())
	}
}

func TestScavengerStopsOnContextCancellation(t *testing.T) {
	store := newMockDurableStore()
	now := time.Now()

	scavenger := NewDurableHandleScavenger(store, nil, 1*time.Hour, 60000, now)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scavenger.Run(ctx)
		close(done)
	}()

	// Cancel immediately
	cancel()

	// Run should return promptly
	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("scavenger did not stop after context cancellation")
	}
}

func TestScavengerAdjustsTimeoutsForRestart(t *testing.T) {
	store := newMockDurableStore()
	oldServerStart := time.Now().Add(-30 * time.Second)
	newServerStart := time.Now()

	// Handle from previous server instance, disconnected 20s ago with 10s timeout -> should be expired
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "old-expired",
		DisconnectedAt:  oldServerStart.Add(5 * time.Second), // 5s after old start, 25s ago
		TimeoutMs:       10000,                               // 10 second timeout
		ServerStartTime: oldServerStart,
	})

	// Handle from previous server instance, disconnected 2s ago with 60s timeout -> should survive
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "old-valid",
		DisconnectedAt:  newServerStart.Add(-2 * time.Second),
		TimeoutMs:       60000,
		ServerStartTime: oldServerStart,
	})

	// Handle from current server instance -> should not be touched by restart adjustment
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "current",
		DisconnectedAt:  newServerStart.Add(-1 * time.Second),
		TimeoutMs:       60000,
		ServerStartTime: newServerStart,
	})

	scavenger := NewDurableHandleScavenger(store, nil, 50*time.Millisecond, 60000, newServerStart)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		scavenger.Run(ctx)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	// old-expired should be gone (elapsed > timeout)
	h, _ := store.GetDurableHandle(context.Background(), "old-expired")
	if h != nil {
		t.Fatal("expected old-expired to be removed")
	}

	// old-valid should survive (elapsed < timeout)
	h, _ = store.GetDurableHandle(context.Background(), "old-valid")
	if h == nil {
		t.Fatal("expected old-valid to remain")
	}

	// current should remain
	h, _ = store.GetDurableHandle(context.Background(), "current")
	if h == nil {
		t.Fatal("expected current to remain")
	}
}

func TestScavengerForceExpireDurableHandle(t *testing.T) {
	store := newMockDurableStore()
	now := time.Now()

	// Put a valid (not-yet-expired) handle
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "force-me",
		Path:            "/test/file",
		ShareName:       "share1",
		DisconnectedAt:  now.Add(-1 * time.Second),
		TimeoutMs:       60000,
		ServerStartTime: now,
	})

	scavenger := NewDurableHandleScavenger(store, nil, 1*time.Hour, 60000, now)

	err := scavenger.ForceExpireDurableHandle(context.Background(), "force-me")
	if err != nil {
		t.Fatalf("ForceExpireDurableHandle failed: %v", err)
	}

	h, _ := store.GetDurableHandle(context.Background(), "force-me")
	if h != nil {
		t.Fatal("expected force-me to be removed after ForceExpire")
	}
}

func TestScavengerForceExpireNotFound(t *testing.T) {
	store := newMockDurableStore()
	now := time.Now()

	scavenger := NewDurableHandleScavenger(store, nil, 1*time.Hour, 60000, now)

	err := scavenger.ForceExpireDurableHandle(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent handle")
	}
}

func TestScavengerHandleConflictingOpen(t *testing.T) {
	store := newMockDurableStore()
	now := time.Now()

	fileHandle := []byte{0x01, 0x02, 0x03, 0x04}

	// Put an orphaned durable handle for a file
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:              "orphaned",
		MetadataHandle:  fileHandle,
		Path:            "/test/conflict",
		ShareName:       "share1",
		DisconnectedAt:  now.Add(-1 * time.Second),
		TimeoutMs:       60000,
		ServerStartTime: now,
	})

	scavenger := NewDurableHandleScavenger(store, nil, 1*time.Hour, 60000, now)

	expired := scavenger.HandleConflictingOpen(context.Background(), fileHandle)
	if expired != 1 {
		t.Fatalf("expected 1 handle force-expired, got %d", expired)
	}

	h, _ := store.GetDurableHandle(context.Background(), "orphaned")
	if h != nil {
		t.Fatal("expected orphaned handle to be removed after conflicting open")
	}
}
