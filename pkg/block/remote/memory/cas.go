package memory

import (
	"context"
	"errors"
	"fmt"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
)

// The memory backend's hash-keyed CAS operations (Put/Get/GetRange/Has/Head/
// Delete/Walk + ReadBlockVerified), keyed by content hash under "cas/". They
// are NOT part of the production RemoteStore surface, which is block-keyed;
// they are reachable only on the concrete type, and today only from tests.

// Put writes data under the CAS-shaped key derived from hash. The
// in-memory backend stamps time.Now() (via nowFn) into LastModified at
// Put time — every Meta.LastModified surfaced by Head / Walk is non-zero
func (s *Store) Put(_ context.Context, hash block.ContentHash, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return block.ErrStoreClosed
	}

	// Defensive copy to prevent caller mutation.
	copied := make([]byte, len(data))
	copy(copied, data)
	s.blocks[hash] = &memBlock{
		data:         copied,
		lastModified: s.nowFn(),
	}

	return nil
}

// Get returns the bytes addressed by hash. Returns
// block.ErrChunkNotFound on miss. The returned slice is a defensive
// copy.
func (s *Store) Get(_ context.Context, hash block.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, block.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}

	copied := make([]byte, len(mb.data))
	copy(copied, mb.data)
	return copied, nil
}

// ReadBlockVerified re-hashes the stored bytes and asserts they match
// expected before returning. The in-memory backend has no header to
// pre-check — it always re-hashes the stored bytes so a test that mutates the
// in-memory blob still surfaces ErrChunkContentMismatch.
//
// Both hash arguments are intentional: hash derives the lookup key; expected
// is the body BLAKE3 to assert.
func (s *Store) ReadBlockVerified(_ context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, block.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}

	// Body recompute. No streaming response body here.
	got := blake3.Sum256(mb.data)
	var gotHash block.ContentHash
	copy(gotHash[:], got[:])
	if gotHash != expected {
		return nil, fmt.Errorf("%w: got %s, want %s",
			block.ErrChunkContentMismatch, gotHash.CASKey(), expected.CASKey())
	}

	copied := make([]byte, len(mb.data))
	copy(copied, mb.data)
	return copied, nil
}

// GetRange reads a byte range from a CAS object.
func (s *Store) GetRange(_ context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, block.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}

	// Bounds checking. Per the BlockStore.GetRange contract, a negative
	// or past-EOF offset is ErrInvalidOffset and a non-positive length is
	// ErrInvalidSize; an in-range offset whose length runs past EOF is
	// clamped to the chunk's remaining bytes (no error).
	if offset < 0 || offset >= int64(len(mb.data)) {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}

	// Clamp past-EOF length without computing offset+length (which can
	// overflow int64 for a hostile length). offset < len is guaranteed above,
	// so len-offset is positive.
	size := int64(len(mb.data))
	end := size
	if length <= size-offset {
		end = offset + length
	}

	result := make([]byte, end-offset)
	copy(result, mb.data[offset:end])
	return result, nil
}

// Has reports whether the CAS object addressed by hash exists in the store.
func (s *Store) Has(_ context.Context, hash block.ContentHash) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return false, block.ErrStoreClosed
	}
	_, ok := s.blocks[hash]
	return ok, nil
}

// Head returns block.Meta{Size, LastModified} for the CAS object
// addressed by hash. Returns block.ErrChunkNotFound on miss.
func (s *Store) Head(_ context.Context, hash block.ContentHash) (block.Meta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return block.Meta{}, block.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return block.Meta{}, block.ErrChunkNotFound
	}

	return block.Meta{
		Size:         int64(len(mb.data)),
		LastModified: mb.lastModified,
	}, nil
}

// Delete removes the CAS object addressed by hash. Idempotent: deleting
// an absent hash returns nil.
func (s *Store) Delete(_ context.Context, hash block.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return block.ErrStoreClosed
	}

	delete(s.blocks, hash)
	return nil
}

// Walk enumerates every CAS object in the store. Honors
// block.ErrStopWalk for clean early exit (Walk returns nil); any
// other callback error is wrapped as "walk halted at %s: %w".
// Context cancellation aborts immediately.
func (s *Store) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Snapshot under read lock so the callback can take its time without
	// blocking writers; ordering is unspecified per the BlockStore.Walk
	// contract.
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return block.ErrStoreClosed
	}
	type entry struct {
		hash block.ContentHash
		meta block.Meta
	}
	snap := make([]entry, 0, len(s.blocks))
	for h, mb := range s.blocks {
		snap = append(snap, entry{
			hash: h,
			meta: block.Meta{
				Size:         int64(len(mb.data)),
				LastModified: mb.lastModified,
			},
		})
	}
	s.mu.RUnlock()

	for _, e := range snap {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cberr := fn(e.hash, e.meta); cberr != nil {
			if errors.Is(cberr, block.ErrStopWalk) {
				return nil
			}
			return fmt.Errorf("walk halted at %s: %w", e.hash, cberr)
		}
	}
	return nil
}
