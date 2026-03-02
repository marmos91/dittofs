package memory

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// memoryDurableStore implements lock.DurableHandleStore using in-memory storage.
// Secondary lookups use linear scans, acceptable since durable handle counts
// are typically low (hundreds at most).
type memoryDurableStore struct {
	mu      sync.RWMutex
	handles map[string]*lock.PersistedDurableHandle
}

func newMemoryDurableStore() *memoryDurableStore {
	return &memoryDurableStore{
		handles: make(map[string]*lock.PersistedDurableHandle),
	}
}

func (s *memoryDurableStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.handles[handle.ID] = cloneDurableHandle(handle)
	return nil
}

func (s *memoryDurableStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	handle, exists := s.handles[id]
	if !exists {
		return nil, nil
	}

	return cloneDurableHandle(handle), nil
}

func (s *memoryDurableStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Zero FileID would match all handles without a real FileID — reject early
	if fileID == ([16]byte{}) {
		return nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, handle := range s.handles {
		if handle.FileID == fileID {
			return cloneDurableHandle(handle), nil
		}
	}

	return nil, nil
}

func (s *memoryDurableStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Zero GUID matches all V1 handles and unrelated handles — reject early
	if createGuid == ([16]byte{}) {
		return nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, handle := range s.handles {
		if handle.CreateGuid == createGuid {
			return cloneDurableHandle(handle), nil
		}
	}

	return nil, nil
}

func (s *memoryDurableStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Zero AppInstanceId would match all handles without an AppInstanceId — reject early
	if appInstanceId == ([16]byte{}) {
		return nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*lock.PersistedDurableHandle
	for _, handle := range s.handles {
		if handle.AppInstanceId == appInstanceId {
			result = append(result, cloneDurableHandle(handle))
		}
	}

	return result, nil
}

func (s *memoryDurableStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*lock.PersistedDurableHandle
	for _, handle := range s.handles {
		if bytes.Equal(handle.MetadataHandle, fileHandle) {
			result = append(result, cloneDurableHandle(handle))
		}
	}

	return result, nil
}

func (s *memoryDurableStore) DeleteDurableHandle(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.handles, id)
	return nil
}

func (s *memoryDurableStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*lock.PersistedDurableHandle, 0, len(s.handles))
	for _, handle := range s.handles {
		result = append(result, cloneDurableHandle(handle))
	}

	return result, nil
}

func (s *memoryDurableStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*lock.PersistedDurableHandle
	for _, handle := range s.handles {
		if handle.ShareName == shareName {
			result = append(result, cloneDurableHandle(handle))
		}
	}

	return result, nil
}

func (s *memoryDurableStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, handle := range s.handles {
		expiresAt := handle.DisconnectedAt.Add(time.Duration(handle.TimeoutMs) * time.Millisecond)
		if !expiresAt.After(now) {
			delete(s.handles, id)
			count++
		}
	}

	return count, nil
}

// cloneDurableHandle creates a deep copy of a PersistedDurableHandle.
func cloneDurableHandle(h *lock.PersistedDurableHandle) *lock.PersistedDurableHandle {
	if h == nil {
		return nil
	}

	clone := &lock.PersistedDurableHandle{
		ID:              h.ID,
		FileID:          h.FileID,
		Path:            h.Path,
		ShareName:       h.ShareName,
		DesiredAccess:   h.DesiredAccess,
		ShareAccess:     h.ShareAccess,
		CreateOptions:   h.CreateOptions,
		PayloadID:       h.PayloadID,
		OplockLevel:     h.OplockLevel,
		LeaseKey:        h.LeaseKey,
		LeaseState:      h.LeaseState,
		CreateGuid:      h.CreateGuid,
		AppInstanceId:   h.AppInstanceId,
		Username:        h.Username,
		SessionKeyHash:  h.SessionKeyHash,
		IsV2:            h.IsV2,
		CreatedAt:       h.CreatedAt,
		DisconnectedAt:  h.DisconnectedAt,
		TimeoutMs:       h.TimeoutMs,
		ServerStartTime: h.ServerStartTime,
	}

	clone.MetadataHandle = bytes.Clone(h.MetadataHandle)

	return clone
}

// MemoryMetadataStore DurableHandleStore delegation

var _ lock.DurableHandleStore = (*MemoryMetadataStore)(nil)

// getDurableStore returns the durable store, creating it on first access.
func (s *MemoryMetadataStore) getDurableStore() *memoryDurableStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durableStore == nil {
		s.durableStore = newMemoryDurableStore()
	}
	return s.durableStore
}

func (s *MemoryMetadataStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	return s.getDurableStore().PutDurableHandle(ctx, handle)
}

func (s *MemoryMetadataStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandle(ctx, id)
}

func (s *MemoryMetadataStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandleByFileID(ctx, fileID)
}

func (s *MemoryMetadataStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandleByCreateGuid(ctx, createGuid)
}

func (s *MemoryMetadataStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandlesByAppInstanceId(ctx, appInstanceId)
}

func (s *MemoryMetadataStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandlesByFileHandle(ctx, fileHandle)
}

func (s *MemoryMetadataStore) DeleteDurableHandle(ctx context.Context, id string) error {
	return s.getDurableStore().DeleteDurableHandle(ctx, id)
}

func (s *MemoryMetadataStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().ListDurableHandles(ctx)
}

func (s *MemoryMetadataStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().ListDurableHandlesByShare(ctx, shareName)
}

func (s *MemoryMetadataStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	return s.getDurableStore().DeleteExpiredDurableHandles(ctx, now)
}

// DurableHandleStore returns this store as a DurableHandleStore.
func (s *MemoryMetadataStore) DurableHandleStore() lock.DurableHandleStore {
	return s
}
