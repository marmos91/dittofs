// Package memory provides an in-memory block store implementation for testing.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/pkg/store/block"
)

// Store is an in-memory implementation of block.Store for testing.
type Store struct {
	mu     sync.RWMutex
	blocks map[string][]byte
	closed bool
}

// New creates a new in-memory block store.
func New() *Store {
	return &Store{
		blocks: make(map[string][]byte),
	}
}

// WriteBlock writes a single block to memory.
func (s *Store) WriteBlock(ctx context.Context, blockKey string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return block.ErrStoreClosed
	}

	// Make a copy of the data to prevent mutation
	copied := make([]byte, len(data))
	copy(copied, data)
	s.blocks[blockKey] = copied

	return nil
}

// ReadBlock reads a complete block from memory.
func (s *Store) ReadBlock(ctx context.Context, blockKey string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, block.ErrStoreClosed
	}

	data, ok := s.blocks[blockKey]
	if !ok {
		return nil, block.ErrBlockNotFound
	}

	// Return a copy to prevent mutation
	copied := make([]byte, len(data))
	copy(copied, data)
	return copied, nil
}

// ReadBlockRange reads a byte range from a block.
func (s *Store) ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, block.ErrStoreClosed
	}

	data, ok := s.blocks[blockKey]
	if !ok {
		return nil, block.ErrBlockNotFound
	}

	// Bounds checking
	if offset < 0 || offset >= int64(len(data)) {
		return nil, block.ErrBlockNotFound
	}

	end := min(offset+length, int64(len(data)))

	// Return a copy of the requested range
	result := make([]byte, end-offset)
	copy(result, data[offset:end])
	return result, nil
}

// DeleteBlock removes a single block from memory.
func (s *Store) DeleteBlock(ctx context.Context, blockKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return block.ErrStoreClosed
	}

	delete(s.blocks, blockKey)
	return nil
}

// DeleteByPrefix removes all blocks with a given prefix.
func (s *Store) DeleteByPrefix(ctx context.Context, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return block.ErrStoreClosed
	}

	for key := range s.blocks {
		if strings.HasPrefix(key, prefix) {
			delete(s.blocks, key)
		}
	}

	return nil
}

// ListByPrefix lists all block keys with a given prefix.
func (s *Store) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, block.ErrStoreClosed
	}

	var keys []string
	for key := range s.blocks {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	// Sort for deterministic output
	sort.Strings(keys)
	return keys, nil
}

// Close marks the store as closed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.blocks = nil
	return nil
}

// HealthCheck verifies the store is accessible and operational.
func (s *Store) HealthCheck(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return block.ErrStoreClosed
	}
	return nil
}

// BlockCount returns the number of blocks stored (for testing).
func (s *Store) BlockCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blocks)
}

// TotalSize returns the total size of all blocks stored (for testing).
func (s *Store) TotalSize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	for _, data := range s.blocks {
		total += int64(len(data))
	}
	return total
}

// Ensure Store implements block.Store.
var _ block.Store = (*Store)(nil)
