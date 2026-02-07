package memory

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Memory LockStore Implementation
// ============================================================================

// memoryLockStore implements lock.LockStore using in-memory storage.
//
// This implementation is suitable for:
//   - Testing and development environments
//   - Ephemeral deployments where lock persistence is not required
//
// Thread Safety:
// All operations are protected by a read-write mutex, making the store
// safe for concurrent access from multiple goroutines.
type memoryLockStore struct {
	mu sync.RWMutex

	// locks maps lock ID to PersistedLock
	locks map[string]*lock.PersistedLock

	// serverEpoch tracks server restarts
	serverEpoch uint64
}

// newMemoryLockStore creates a new in-memory lock store.
func newMemoryLockStore() *memoryLockStore {
	return &memoryLockStore{
		locks:       make(map[string]*lock.PersistedLock),
		serverEpoch: 0,
	}
}

// PutLock persists a lock. Overwrites if lock with same ID exists.
func (s *memoryLockStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Clone the lock to prevent external modifications
	s.locks[lk.ID] = cloneLock(lk)
	return nil
}

// GetLock retrieves a lock by ID.
func (s *memoryLockStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	lock, exists := s.locks[lockID]
	if !exists {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}

	// Return a clone to prevent external modifications
	return cloneLock(lock), nil
}

// DeleteLock removes a lock by ID.
func (s *memoryLockStore) DeleteLock(ctx context.Context, lockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.locks[lockID]; !exists {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}

	delete(s.locks, lockID)
	return nil
}

// ListLocks returns locks matching the query.
func (s *memoryLockStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*lock.PersistedLock

	for _, lk := range s.locks {
		if matchesQuery(lk, query) {
			result = append(result, cloneLock(lk))
		}
	}

	return result, nil
}

// DeleteLocksByClient removes all locks for a client.
func (s *memoryLockStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, lk := range s.locks {
		if lk.ClientID == clientID {
			delete(s.locks, id)
			count++
		}
	}

	return count, nil
}

// DeleteLocksByFile removes all locks for a file.
func (s *memoryLockStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, lk := range s.locks {
		if lk.FileID == fileID {
			delete(s.locks, id)
			count++
		}
	}

	return count, nil
}

// GetServerEpoch returns current server epoch.
func (s *memoryLockStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.serverEpoch, nil
}

// IncrementServerEpoch increments and returns new epoch.
func (s *memoryLockStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.serverEpoch++
	return s.serverEpoch, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// matchesQuery returns true if the lock matches all non-empty query fields.
func matchesQuery(lk *lock.PersistedLock, query lock.LockQuery) bool {
	// Use the centralized MatchesLock method from LockQuery
	return query.MatchesLock(lk)
}

// cloneLock creates a deep copy of a PersistedLock.
func cloneLock(lk *lock.PersistedLock) *lock.PersistedLock {
	clone := &lock.PersistedLock{
		ID:               lk.ID,
		ShareName:        lk.ShareName,
		FileID:           lk.FileID,
		OwnerID:          lk.OwnerID,
		ClientID:         lk.ClientID,
		LockType:         lk.LockType,
		Offset:           lk.Offset,
		Length:           lk.Length,
		ShareReservation: lk.ShareReservation,
		AcquiredAt:       lk.AcquiredAt,
		ServerEpoch:      lk.ServerEpoch,
		// Lease fields
		LeaseState:   lk.LeaseState,
		LeaseEpoch:   lk.LeaseEpoch,
		BreakToState: lk.BreakToState,
		Breaking:     lk.Breaking,
	}
	// Deep copy LeaseKey slice if present
	if len(lk.LeaseKey) > 0 {
		clone.LeaseKey = make([]byte, len(lk.LeaseKey))
		copy(clone.LeaseKey, lk.LeaseKey)
	}
	return clone
}

// ============================================================================
// MemoryMetadataStore LockStore Integration
// ============================================================================

// Ensure MemoryMetadataStore implements LockStore
var _ lock.LockStore = (*MemoryMetadataStore)(nil)

// The memory store has a per-store lock store (not global).
// This is initialized lazily via initLockStore().

// initLockStore ensures the lock store is initialized.
// Must be called with the store's write lock held.
func (s *MemoryMetadataStore) initLockStore() {
	if s.lockStore == nil {
		s.lockStore = newMemoryLockStore()
	}
}

// PutLock persists a lock.
func (s *MemoryMetadataStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLockStore()
	return s.putLockLocked(ctx, lk)
}

// GetLock retrieves a lock by ID.
func (s *MemoryMetadataStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lockStore == nil {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	return s.getLockLocked(ctx, lockID)
}

// DeleteLock removes a lock by ID.
func (s *MemoryMetadataStore) DeleteLock(ctx context.Context, lockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockStore == nil {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	return s.deleteLockLocked(ctx, lockID)
}

// ListLocks returns locks matching the query.
func (s *MemoryMetadataStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lockStore == nil {
		return []*lock.PersistedLock{}, nil
	}
	return s.listLocksLocked(ctx, query)
}

// DeleteLocksByClient removes all locks for a client.
func (s *MemoryMetadataStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockStore == nil {
		return 0, nil
	}
	return s.deleteLocksByClientLocked(ctx, clientID)
}

// DeleteLocksByFile removes all locks for a file.
func (s *MemoryMetadataStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockStore == nil {
		return 0, nil
	}
	return s.deleteLocksByFileLocked(ctx, fileID)
}

// GetServerEpoch returns current server epoch.
func (s *MemoryMetadataStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lockStore == nil {
		return 0, nil
	}
	return s.getServerEpochLocked(ctx)
}

// IncrementServerEpoch increments and returns new epoch.
func (s *MemoryMetadataStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLockStore()
	return s.incrementServerEpochLocked(ctx)
}

// ReclaimLease reclaims an existing lease during grace period.
// Memory store returns ErrLockNotFound since leases are not persisted across restarts.
func (s *MemoryMetadataStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.EnhancedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lockStore == nil {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lease not found for reclaim",
			Path:    string(fileHandle),
		}
	}
	return s.reclaimLeaseLocked(ctx, fileHandle, leaseKey, clientID)
}

// ============================================================================
// Locked Helpers (for transaction support)
// ============================================================================
// These methods assume the store's lock is already held.

func (s *MemoryMetadataStore) putLockLocked(_ context.Context, lk *lock.PersistedLock) error {
	s.lockStore.locks[lk.ID] = cloneLock(lk)
	return nil
}

func (s *MemoryMetadataStore) getLockLocked(_ context.Context, lockID string) (*lock.PersistedLock, error) {
	lk, exists := s.lockStore.locks[lockID]
	if !exists {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	return cloneLock(lk), nil
}

func (s *MemoryMetadataStore) deleteLockLocked(_ context.Context, lockID string) error {
	if _, exists := s.lockStore.locks[lockID]; !exists {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	delete(s.lockStore.locks, lockID)
	return nil
}

func (s *MemoryMetadataStore) listLocksLocked(_ context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	var result []*lock.PersistedLock
	for _, lk := range s.lockStore.locks {
		if matchesQuery(lk, query) {
			result = append(result, cloneLock(lk))
		}
	}
	return result, nil
}

func (s *MemoryMetadataStore) deleteLocksByClientLocked(_ context.Context, clientID string) (int, error) {
	count := 0
	for id, lk := range s.lockStore.locks {
		if lk.ClientID == clientID {
			delete(s.lockStore.locks, id)
			count++
		}
	}
	return count, nil
}

func (s *MemoryMetadataStore) deleteLocksByFileLocked(_ context.Context, fileID string) (int, error) {
	count := 0
	for id, lk := range s.lockStore.locks {
		if lk.FileID == fileID {
			delete(s.lockStore.locks, id)
			count++
		}
	}
	return count, nil
}

func (s *MemoryMetadataStore) getServerEpochLocked(_ context.Context) (uint64, error) {
	return s.lockStore.serverEpoch, nil
}

func (s *MemoryMetadataStore) incrementServerEpochLocked(_ context.Context) (uint64, error) {
	s.lockStore.serverEpoch++
	return s.lockStore.serverEpoch, nil
}

// reclaimLeaseLocked searches for a lease matching the criteria.
// Memory store looks for existing leases that match the lease key.
func (s *MemoryMetadataStore) reclaimLeaseLocked(_ context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.EnhancedLock, error) {
	// Search for a persisted lease with matching file handle and lease key
	for _, lk := range s.lockStore.locks {
		// Must be a lease (has 16-byte key)
		if len(lk.LeaseKey) != 16 {
			continue
		}
		// Match file handle
		if lk.FileID != string(fileHandle) {
			continue
		}
		// Match lease key
		var storedKey [16]byte
		copy(storedKey[:], lk.LeaseKey)
		if storedKey != leaseKey {
			continue
		}
		// Found matching lease - convert to EnhancedLock
		enhanced := lock.FromPersistedLock(lk)
		if enhanced.Lease != nil {
			enhanced.Lease.Reclaim = true
		}
		enhanced.Reclaim = true
		return enhanced, nil
	}

	return nil, &errors.StoreError{
		Code:    errors.ErrLockNotFound,
		Message: "lease not found for reclaim",
		Path:    string(fileHandle),
	}
}

// ============================================================================
// Transaction LockStore Support
// ============================================================================
// The memoryTransaction type needs to implement LockStore as well.
// We delegate to the store's locked methods since memory transactions use the store's mutex.

// Ensure memoryTransaction implements LockStore
var _ lock.LockStore = (*memoryTransaction)(nil)

func (tx *memoryTransaction) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	tx.store.initLockStore()
	return tx.store.putLockLocked(ctx, lk)
}

func (tx *memoryTransaction) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	if tx.store.lockStore == nil {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	return tx.store.getLockLocked(ctx, lockID)
}

func (tx *memoryTransaction) DeleteLock(ctx context.Context, lockID string) error {
	if tx.store.lockStore == nil {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	return tx.store.deleteLockLocked(ctx, lockID)
}

func (tx *memoryTransaction) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	if tx.store.lockStore == nil {
		return []*lock.PersistedLock{}, nil
	}
	return tx.store.listLocksLocked(ctx, query)
}

func (tx *memoryTransaction) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	if tx.store.lockStore == nil {
		return 0, nil
	}
	return tx.store.deleteLocksByClientLocked(ctx, clientID)
}

func (tx *memoryTransaction) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	if tx.store.lockStore == nil {
		return 0, nil
	}
	return tx.store.deleteLocksByFileLocked(ctx, fileID)
}

func (tx *memoryTransaction) GetServerEpoch(ctx context.Context) (uint64, error) {
	if tx.store.lockStore == nil {
		return 0, nil
	}
	return tx.store.getServerEpochLocked(ctx)
}

func (tx *memoryTransaction) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	tx.store.initLockStore()
	return tx.store.incrementServerEpochLocked(ctx)
}

func (tx *memoryTransaction) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.EnhancedLock, error) {
	if tx.store.lockStore == nil {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lease not found for reclaim",
			Path:    string(fileHandle),
		}
	}
	return tx.store.reclaimLeaseLocked(ctx, fileHandle, leaseKey, clientID)
}
