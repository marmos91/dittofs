// Package memory provides an in-memory RemoteStore implementation for testing.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
)

// Compile-time interface satisfaction check.
var _ remote.RemoteStore = (*Store)(nil)

// Store is an in-memory implementation of remote.RemoteStore for testing.
type Store struct {
	mu     sync.RWMutex
	blocks map[string][]byte
	closed bool
}

// New creates a new in-memory remote block store.
func New() *Store {
	return &Store{
		blocks: make(map[string][]byte),
	}
}

// WriteBlock writes a single block to memory.
func (s *Store) WriteBlock(_ context.Context, blockKey string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	// Make a copy of the data to prevent mutation
	copied := make([]byte, len(data))
	copy(copied, data)
	s.blocks[blockKey] = copied

	return nil
}

// ReadBlock reads a complete block from memory.
func (s *Store) ReadBlock(_ context.Context, blockKey string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	data, ok := s.blocks[blockKey]
	if !ok {
		return nil, blockstore.ErrBlockNotFound
	}

	// Return a copy to prevent mutation
	copied := make([]byte, len(data))
	copy(copied, data)
	return copied, nil
}

// ReadBlockRange reads a byte range from a block.
func (s *Store) ReadBlockRange(_ context.Context, blockKey string, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	data, ok := s.blocks[blockKey]
	if !ok {
		return nil, blockstore.ErrBlockNotFound
	}

	// Bounds checking
	if offset < 0 || offset >= int64(len(data)) {
		return nil, blockstore.ErrBlockNotFound
	}

	end := min(offset+length, int64(len(data)))

	// Return a copy of the requested range
	result := make([]byte, end-offset)
	copy(result, data[offset:end])
	return result, nil
}

// DeleteBlock removes a single block from memory.
func (s *Store) DeleteBlock(_ context.Context, blockKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	delete(s.blocks, blockKey)
	return nil
}

// DeleteByPrefix removes all blocks with a given prefix.
func (s *Store) DeleteByPrefix(_ context.Context, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	for key := range s.blocks {
		if strings.HasPrefix(key, prefix) {
			delete(s.blocks, key)
		}
	}

	return nil
}

// ListByPrefix lists all block keys with a given prefix.
func (s *Store) ListByPrefix(_ context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
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
func (s *Store) HealthCheck(_ context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return blockstore.ErrStoreClosed
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
