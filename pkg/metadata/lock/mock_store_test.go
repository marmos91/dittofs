package lock

import (
	"context"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

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
