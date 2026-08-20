package memory

import (
	"bytes"
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// memoryDurableStore implements lock.DurableHandleStore using in-memory storage.
// Secondary lookups use linear scans, acceptable since durable handle counts
// are typically low (hundreds at most).
//
// It carries no lock of its own: every method is reached through a
// MemoryMetadataStore wrapper that holds the store-wide mutex for the whole
// call, so handles is guarded by that one lock — the same lock a snapshot
// holds while it encodes the map.
type memoryDurableStore struct {
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

	s.handles[handle.ID] = cloneDurableHandle(handle)
	return nil
}

func (s *memoryDurableStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

	handle := s.lowestHandleForFileID(fileID)
	if handle == nil {
		return nil, nil
	}
	return cloneDurableHandle(handle), nil
}

// lowestHandleForFileID returns the handle with the smallest ID among those
// held on a file, or nil when the file holds none. A file can carry several
// durable handles at once, so the pick is by ID rather than by map order to
// stay stable across repeated lookups. Callers hold the lock.
func (s *memoryDurableStore) lowestHandleForFileID(fileID [16]byte) *lock.PersistedDurableHandle {
	var lowest *lock.PersistedDurableHandle
	for _, handle := range s.handles {
		if handle.FileID == fileID && (lowest == nil || handle.ID < lowest.ID) {
			lowest = handle
		}
	}
	return lowest
}

func (s *memoryDurableStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Zero GUID matches all V1 handles and unrelated handles — reject early
	if createGuid == ([16]byte{}) {
		return nil, nil
	}

	for _, handle := range s.handles {
		if handle.CreateGuid == createGuid {
			return cloneDurableHandle(handle), nil
		}
	}

	return nil, nil
}

func (s *memoryDurableStore) ConsumeDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	handle, exists := s.handles[id]
	if !exists {
		return nil, nil
	}
	delete(s.handles, id)
	return cloneDurableHandle(handle), nil
}

func (s *memoryDurableStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Zero AppInstanceId would match all handles without an AppInstanceId — reject early
	if appInstanceId == ([16]byte{}) {
		return nil, nil
	}

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

	delete(s.handles, id)
	return nil
}

func (s *memoryDurableStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		ID:                 h.ID,
		FileID:             h.FileID,
		Path:               h.Path,
		ShareName:          h.ShareName,
		DesiredAccess:      h.DesiredAccess,
		GrantedAccess:      h.GrantedAccess,
		ShareAccess:        h.ShareAccess,
		CreateOptions:      h.CreateOptions,
		PayloadID:          h.PayloadID,
		OplockLevel:        h.OplockLevel,
		LeaseKey:           h.LeaseKey,
		LeaseState:         h.LeaseState,
		LeaseEpoch:         h.LeaseEpoch,
		CreateGuid:         h.CreateGuid,
		AppInstanceId:      h.AppInstanceId,
		IsPersistent:       h.IsPersistent,
		Username:           h.Username,
		SessionKeyHash:     h.SessionKeyHash,
		IsV2:               h.IsV2,
		CreatedAt:          h.CreatedAt,
		DisconnectedAt:     h.DisconnectedAt,
		TimeoutMs:          h.TimeoutMs,
		ServerStartTime:    h.ServerStartTime,
		DeletePending:      h.DeletePending,
		FileName:           h.FileName,
		IsDirectory:        h.IsDirectory,
		PositionInfo:       h.PositionInfo,
		OriginalFileID:     h.OriginalFileID,
		ClientGUID:         h.ClientGUID,
		RequestedAllocSize: h.RequestedAllocSize,
	}

	clone.MetadataHandle = bytes.Clone(h.MetadataHandle)
	clone.ParentHandle = bytes.Clone(h.ParentHandle)

	return clone
}

// MemoryMetadataStore DurableHandleStore delegation

var _ lock.DurableHandleStore = (*MemoryMetadataStore)(nil)

// initDurableStore ensures the durable handle store is initialized.
// Must be called with the store's write lock held.
func (s *MemoryMetadataStore) initDurableStore() {
	if s.durableStore == nil {
		s.durableStore = newMemoryDurableStore()
	}
}

// PutDurableHandle stores or replaces a durable handle.
func (s *MemoryMetadataStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initDurableStore()
	return s.durableStore.PutDurableHandle(ctx, handle)
}

// GetDurableHandle retrieves a durable handle by ID.
func (s *MemoryMetadataStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.GetDurableHandle(ctx, id)
}

// GetDurableHandleByFileID retrieves the lowest-ID handle held on a file.
func (s *MemoryMetadataStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.GetDurableHandleByFileID(ctx, fileID)
}

// GetDurableHandleByCreateGuid retrieves a V2 durable handle by its create GUID.
func (s *MemoryMetadataStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.GetDurableHandleByCreateGuid(ctx, createGuid)
}

// ConsumeDurableHandle retrieves and removes a durable handle by ID.
func (s *MemoryMetadataStore) ConsumeDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.ConsumeDurableHandle(ctx, id)
}

// GetDurableHandlesByAppInstanceId returns every handle for an app instance.
func (s *MemoryMetadataStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.GetDurableHandlesByAppInstanceId(ctx, appInstanceId)
}

// GetDurableHandlesByFileHandle returns every handle on a metadata file handle.
func (s *MemoryMetadataStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.GetDurableHandlesByFileHandle(ctx, fileHandle)
}

// DeleteDurableHandle removes a durable handle by ID.
func (s *MemoryMetadataStore) DeleteDurableHandle(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durableStore == nil {
		return nil
	}
	return s.durableStore.DeleteDurableHandle(ctx, id)
}

// ListDurableHandles returns every stored durable handle.
func (s *MemoryMetadataStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return []*lock.PersistedDurableHandle{}, nil
	}
	return s.durableStore.ListDurableHandles(ctx)
}

// ListDurableHandlesByShare returns every durable handle on a share.
func (s *MemoryMetadataStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.durableStore == nil {
		return nil, nil
	}
	return s.durableStore.ListDurableHandlesByShare(ctx, shareName)
}

// DeleteExpiredDurableHandles drops every handle whose timeout has elapsed.
func (s *MemoryMetadataStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durableStore == nil {
		return 0, nil
	}
	return s.durableStore.DeleteExpiredDurableHandles(ctx, now)
}

// DurableHandleStore returns this store as a DurableHandleStore.
func (s *MemoryMetadataStore) DurableHandleStore() lock.DurableHandleStore {
	return s
}
