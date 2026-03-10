package memory

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// Compile-time interface satisfaction check.
var _ local.LocalStore = (*MemoryStore)(nil)

// ErrStoreClosed is an alias for blockstore.ErrStoreClosed for backward compatibility.
var ErrStoreClosed = blockstore.ErrStoreClosed

// memBlock holds data for a single block in memory.
type memBlock struct {
	data     []byte
	dataSize uint32
	dirty    bool
	state    blockstore.BlockState
}

// MemoryStore is a pure in-memory implementation of local.LocalStore.
// All data lives in maps; nothing touches disk. Useful for testing and
// ephemeral configurations.
type MemoryStore struct {
	mu sync.RWMutex

	// blocks stores block data keyed by "payloadID/blockIdx".
	blocks map[string]*memBlock

	// files tracks file sizes (payloadID -> size).
	files map[string]uint64

	closed          bool
	evictionEnabled bool
	skipFsync       bool
}

// New creates a new MemoryStore.
func New() *MemoryStore {
	return &MemoryStore{
		blocks:          make(map[string]*memBlock),
		files:           make(map[string]uint64),
		evictionEnabled: true,
	}
}

// blockKey returns the map key for a block.
func blockKey(payloadID string, blockIdx uint64) string {
	return blockstore.FormatStoreKey(payloadID, blockIdx)
}

// ReadAt reads data from the in-memory store at the specified offset into dest.
func (s *MemoryStore) ReadAt(_ context.Context, payloadID string, dest []byte, offset uint64) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return false, ErrStoreClosed
	}

	if len(dest) == 0 {
		return true, nil
	}

	remaining := dest
	currentOffset := offset

	for len(remaining) > 0 {
		blockIdx := currentOffset / blockstore.BlockSize
		blockOffset := uint32(currentOffset % blockstore.BlockSize)

		readLen := uint32(len(remaining))
		spaceInBlock := uint32(blockstore.BlockSize) - blockOffset
		if readLen > spaceInBlock {
			readLen = spaceInBlock
		}

		key := blockKey(payloadID, blockIdx)
		mb, ok := s.blocks[key]
		if !ok || mb.data == nil {
			return false, nil // Cache miss
		}
		if blockOffset+readLen > mb.dataSize {
			return false, nil // Beyond written data
		}

		copy(remaining[:readLen], mb.data[blockOffset:blockOffset+readLen])
		remaining = remaining[readLen:]
		currentOffset += uint64(readLen)
	}

	return true, nil
}

// GetFileSize returns the tracked file size.
func (s *MemoryStore) GetFileSize(_ context.Context, payloadID string) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	size, ok := s.files[payloadID]
	return size, ok
}

// IsBlockCached checks if a specific block is available in the store.
func (s *MemoryStore) IsBlockCached(_ context.Context, payloadID string, blockIdx uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := blockKey(payloadID, blockIdx)
	mb, ok := s.blocks[key]
	return ok && mb.data != nil
}

// GetBlockData returns the raw data for a specific block.
func (s *MemoryStore) GetBlockData(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, 0, ErrStoreClosed
	}

	key := blockKey(payloadID, blockIdx)
	mb, ok := s.blocks[key]
	if !ok || mb.data == nil || mb.dataSize == 0 {
		return nil, 0, blockstore.ErrBlockNotFound
	}

	data := make([]byte, mb.dataSize)
	copy(data, mb.data[:mb.dataSize])
	return data, mb.dataSize, nil
}

// WriteAt writes data to the in-memory store at the specified offset.
func (s *MemoryStore) WriteAt(_ context.Context, payloadID string, data []byte, offset uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if len(data) == 0 {
		return nil
	}

	remaining := data
	currentOffset := offset

	for len(remaining) > 0 {
		blockIdx := currentOffset / blockstore.BlockSize
		blockOffset := uint32(currentOffset % blockstore.BlockSize)

		spaceInBlock := uint32(blockstore.BlockSize) - blockOffset
		writeLen := min(uint32(len(remaining)), spaceInBlock)

		key := blockKey(payloadID, blockIdx)
		mb, ok := s.blocks[key]
		if !ok {
			mb = &memBlock{
				data:  make([]byte, blockstore.BlockSize),
				state: blockstore.BlockStateDirty,
			}
			s.blocks[key] = mb
		}

		copy(mb.data[blockOffset:blockOffset+writeLen], remaining[:writeLen])
		end := blockOffset + writeLen
		if end > mb.dataSize {
			mb.dataSize = end
		}
		mb.dirty = true

		remaining = remaining[writeLen:]
		currentOffset += uint64(writeLen)
	}

	// Update file size
	end := offset + uint64(len(data))
	if end > s.files[payloadID] {
		s.files[payloadID] = end
	}

	return nil
}

// WriteFromRemote caches data fetched from the remote block store.
// The block is marked Remote since it already exists remotely.
func (s *MemoryStore) WriteFromRemote(_ context.Context, payloadID string, data []byte, offset uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	blockIdx := offset / blockstore.BlockSize
	key := blockKey(payloadID, blockIdx)

	mb := &memBlock{
		data:     make([]byte, blockstore.BlockSize),
		dataSize: uint32(len(data)),
		dirty:    false,
		state:    blockstore.BlockStateRemote,
	}
	copy(mb.data, data)
	s.blocks[key] = mb

	end := offset + uint64(len(data))
	if end > s.files[payloadID] {
		s.files[payloadID] = end
	}

	return nil
}

// Flush marks all dirty blocks for a file as Local (flushed).
// In the memory store, there is no disk to flush to -- this just transitions state.
func (s *MemoryStore) Flush(_ context.Context, payloadID string) ([]local.FlushedBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	var flushed []local.FlushedBlock

	for key, mb := range s.blocks {
		if !blockstore.KeyBelongsToFile(key, payloadID) {
			continue
		}
		if mb.dirty {
			mb.dirty = false
			if mb.state == blockstore.BlockStateDirty {
				mb.state = blockstore.BlockStateLocal
			}
			blockIdx := blockstore.ParseBlockIdx(key, payloadID)
			flushed = append(flushed, local.FlushedBlock{
				BlockIndex: blockIdx,
				CachePath:  key, // Use key as path in memory store
				DataSize:   mb.dataSize,
			})
		}
	}

	return flushed, nil
}

// GetDirtyBlocks flushes and returns all blocks in Local state as PendingBlocks.
func (s *MemoryStore) GetDirtyBlocks(ctx context.Context, payloadID string) ([]local.PendingBlock, error) {
	// Flush first
	_, err := s.Flush(ctx, payloadID)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []local.PendingBlock

	for key, mb := range s.blocks {
		if !blockstore.KeyBelongsToFile(key, payloadID) {
			continue
		}
		if mb.state == blockstore.BlockStateLocal && mb.data != nil && mb.dataSize > 0 {
			data := make([]byte, mb.dataSize)
			copy(data, mb.data[:mb.dataSize])
			blockIdx := blockstore.ParseBlockIdx(key, payloadID)
			result = append(result, local.PendingBlock{
				BlockIndex: blockIdx,
				Data:       data,
				DataSize:   mb.dataSize,
			})
		}
	}

	return result, nil
}

// SyncFileBlocks is a no-op in the memory store (no persistent store to sync to).
func (s *MemoryStore) SyncFileBlocks(_ context.Context) {}

// SyncFileBlocksForFile is a no-op in the memory store.
func (s *MemoryStore) SyncFileBlocksForFile(_ context.Context, _ string) {}

// Start is a no-op in the memory store (no background goroutines needed).
func (s *MemoryStore) Start(_ context.Context) {}

// Close marks the store as closed.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Truncate discards cached blocks beyond newSize.
func (s *MemoryStore) Truncate(_ context.Context, payloadID string, newSize uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	// Remove blocks beyond newSize
	for key := range s.blocks {
		if !blockstore.KeyBelongsToFile(key, payloadID) {
			continue
		}
		blockIdx := blockstore.ParseBlockIdx(key, payloadID)
		if blockIdx*blockstore.BlockSize >= newSize {
			delete(s.blocks, key)
		}
	}

	s.files[payloadID] = newSize
	return nil
}

// EvictMemory removes all cached data for a file.
func (s *MemoryStore) EvictMemory(_ context.Context, payloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.blocks {
		if blockstore.KeyBelongsToFile(key, payloadID) {
			delete(s.blocks, key)
		}
	}
	delete(s.files, payloadID)
	return nil
}

// DeleteBlockFile removes a single block from the store.
func (s *MemoryStore) DeleteBlockFile(_ context.Context, payloadID string, blockIdx uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := blockKey(payloadID, blockIdx)
	delete(s.blocks, key)
	return nil
}

// DeleteAllBlockFiles removes all blocks for a file.
func (s *MemoryStore) DeleteAllBlockFiles(_ context.Context, payloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.blocks {
		if blockstore.KeyBelongsToFile(key, payloadID) {
			delete(s.blocks, key)
		}
	}
	delete(s.files, payloadID)
	return nil
}

// TruncateBlockFiles removes all blocks whose start offset >= newSize.
func (s *MemoryStore) TruncateBlockFiles(_ context.Context, payloadID string, newSize uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.blocks {
		if !blockstore.KeyBelongsToFile(key, payloadID) {
			continue
		}
		blockIdx := blockstore.ParseBlockIdx(key, payloadID)
		if blockIdx*blockstore.BlockSize >= newSize {
			delete(s.blocks, key)
		}
	}
	return nil
}

// SetSkipFsync is a no-op in the memory store.
func (s *MemoryStore) SetSkipFsync(skip bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipFsync = skip
}

// SetEvictionEnabled controls whether eviction is enabled (no-op effect in memory store).
func (s *MemoryStore) SetEvictionEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictionEnabled = enabled
}

// Stats returns cache statistics.
func (s *MemoryStore) Stats() local.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var memUsed int64
	var memBlockCount int
	for _, mb := range s.blocks {
		if mb.data != nil {
			memBlockCount++
			memUsed += int64(blockstore.BlockSize)
		}
	}

	return local.Stats{
		FileCount:     len(s.files),
		MemBlockCount: memBlockCount,
		MemUsed:       memUsed,
	}
}

// ListFiles returns the payloadIDs of all tracked files.
func (s *MemoryStore) ListFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]string, 0, len(s.files))
	for payloadID := range s.files {
		result = append(result, payloadID)
	}
	return result
}

// MarkBlockRemote marks a block as confirmed in the remote block store.
func (s *MemoryStore) MarkBlockRemote(_ context.Context, payloadID string, blockIdx uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := blockKey(payloadID, blockIdx)
	mb, ok := s.blocks[key]
	if !ok {
		return false
	}
	mb.state = blockstore.BlockStateRemote
	return true
}

// MarkBlockSyncing claims a block for sync to remote (Local -> Syncing).
func (s *MemoryStore) MarkBlockSyncing(_ context.Context, payloadID string, blockIdx uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := blockKey(payloadID, blockIdx)
	mb, ok := s.blocks[key]
	if !ok || mb.state != blockstore.BlockStateLocal {
		return false
	}
	mb.state = blockstore.BlockStateSyncing
	return true
}

// MarkBlockLocal reverts a block to Local state after a failed sync attempt.
func (s *MemoryStore) MarkBlockLocal(_ context.Context, payloadID string, blockIdx uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := blockKey(payloadID, blockIdx)
	mb, ok := s.blocks[key]
	if !ok {
		return false
	}
	mb.state = blockstore.BlockStateLocal
	return true
}

// GetStoredFileSize returns the total stored data size for a file.
func (s *MemoryStore) GetStoredFileSize(_ context.Context, payloadID string) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total uint64
	for key, mb := range s.blocks {
		if blockstore.KeyBelongsToFile(key, payloadID) {
			total += uint64(mb.dataSize)
		}
	}
	return total, nil
}

// ExistsOnDisk always returns false for the memory store (nothing on disk).
func (s *MemoryStore) ExistsOnDisk(_ context.Context, _ string, _ uint64) (bool, error) {
	return false, nil
}
