// Package memory provides an in-memory RemoteStore implementation for testing.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// Compile-time interface satisfaction check.
var _ remote.RemoteStore = (*Store)(nil)

// memBlock holds the per-object body plus metadata captured at Put time.
type memBlock struct {
	data         []byte
	lastModified time.Time
}

// Store is an in-memory implementation of remote.RemoteStore for testing.
// The map is keyed by blockstore.ContentHash (the unified CAS key from
// ); previous versions of this package used opaque string
// blockKeys and stamped x-amz-meta-content-hash separately.
type Store struct {
	mu     sync.RWMutex
	blocks map[blockstore.ContentHash]*memBlock
	// nowFn returns the current time for the store. Tests can override
	// this to manipulate LastModified deterministically.
	nowFn  func() time.Time
	closed bool
}

// New creates a new in-memory remote block store.
func New() *Store {
	return &Store{
		blocks: make(map[blockstore.ContentHash]*memBlock),
		nowFn:  time.Now,
	}
}

// SetNowFnForTest overrides the time source used by Put to stamp
// LastModified. Test-only helper for the GC sweep grace TTL test.
func (s *Store) SetNowFnForTest(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn == nil {
		s.nowFn = time.Now
		return
	}
	s.nowFn = fn
}

// Put writes data under the CAS-shaped key derived from hash. The
// in-memory backend stamps time.Now() (via nowFn) into LastModified at
// Put time — every Meta.LastModified surfaced by Head / Walk is non-zero
func (s *Store) Put(_ context.Context, hash blockstore.ContentHash, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
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
// blockstore.ErrChunkNotFound on miss. The returned slice is a defensive
// copy.
func (s *Store) Get(_ context.Context, hash blockstore.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return nil, blockstore.ErrChunkNotFound
	}

	copied := make([]byte, len(mb.data))
	copy(copied, mb.data)
	return copied, nil
}

// ReadBlockVerified mirrors s3.Store.ReadBlockVerified for in-memory
// testing. The in-memory backend has no header to pre-check —
// it always re-hashes the stored bytes so a test that mutates the
// in-memory blob still surfaces ErrCASContentMismatch.
//
// Both hash arguments are intentional and match the RemoteStore
// signature: hash derives the lookup key; expected is the body BLAKE3 to
// assert.
func (s *Store) ReadBlockVerified(_ context.Context, hash blockstore.ContentHash, expected blockstore.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return nil, blockstore.ErrChunkNotFound
	}

	// Body recompute. No streaming response body here.
	got := blake3.Sum256(mb.data)
	var gotHash blockstore.ContentHash
	copy(gotHash[:], got[:])
	if gotHash != expected {
		return nil, fmt.Errorf("%w: got %s, want %s",
			blockstore.ErrCASContentMismatch, gotHash.CASKey(), expected.CASKey())
	}

	copied := make([]byte, len(mb.data))
	copy(copied, mb.data)
	return copied, nil
}

// GetRange reads a byte range from a CAS object.
func (s *Store) GetRange(_ context.Context, hash blockstore.ContentHash, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return nil, blockstore.ErrChunkNotFound
	}

	// Bounds checking. Per the BlockStore.GetRange contract, a negative
	// or past-EOF offset is ErrInvalidOffset and a non-positive length is
	// ErrInvalidSize; an in-range offset whose length runs past EOF is
	// clamped to the chunk's remaining bytes (no error).
	if offset < 0 || offset >= int64(len(mb.data)) {
		return nil, blockstore.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, blockstore.ErrInvalidSize
	}

	end := offset + length
	if end > int64(len(mb.data)) {
		end = int64(len(mb.data))
	}

	result := make([]byte, end-offset)
	copy(result, mb.data[offset:end])
	return result, nil
}

// Has reports whether the CAS object addressed by hash exists in the
// store. Implements the blockstore.BlockStore contract.
func (s *Store) Has(_ context.Context, hash blockstore.ContentHash) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return false, blockstore.ErrStoreClosed
	}
	_, ok := s.blocks[hash]
	return ok, nil
}

// Head returns blockstore.Meta{Size, LastModified} for the CAS object
// addressed by hash. Returns blockstore.ErrChunkNotFound on miss.
func (s *Store) Head(_ context.Context, hash blockstore.ContentHash) (blockstore.Meta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return blockstore.Meta{}, blockstore.ErrStoreClosed
	}

	mb, ok := s.blocks[hash]
	if !ok {
		return blockstore.Meta{}, blockstore.ErrChunkNotFound
	}

	return blockstore.Meta{
		Size:         int64(len(mb.data)),
		LastModified: mb.lastModified,
	}, nil
}

// Delete removes the CAS object addressed by hash. Idempotent: deleting
// an absent hash returns nil.
func (s *Store) Delete(_ context.Context, hash blockstore.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	delete(s.blocks, hash)
	return nil
}

// Walk enumerates every CAS object in the store. Honors
// blockstore.ErrStopWalk for clean early exit (Walk returns nil); any
// other callback error is wrapped as "walk halted at %s: %w".
// Context cancellation aborts immediately.
func (s *Store) Walk(ctx context.Context, fn func(hash blockstore.ContentHash, meta blockstore.Meta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Snapshot under read lock so the callback can take its time without
	// blocking writers; ordering is unspecified per the BlockStore.Walk
	// contract.
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return blockstore.ErrStoreClosed
	}
	type entry struct {
		hash blockstore.ContentHash
		meta blockstore.Meta
	}
	snap := make([]entry, 0, len(s.blocks))
	for h, mb := range s.blocks {
		snap = append(snap, entry{
			hash: h,
			meta: blockstore.Meta{
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
			if errors.Is(cberr, blockstore.ErrStopWalk) {
				return nil
			}
			return fmt.Errorf("walk halted at %s: %w", e.hash, cberr)
		}
	}
	return nil
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
//
// Legacy error-returning probe used by the syncer's HealthMonitor.
// Public callers should prefer Healthcheck (lowercase 'c') which
// returns a structured [health.Report] and satisfies [health.Checker].
func (s *Store) HealthCheck(_ context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}
	return nil
}

// Healthcheck implements [health.Checker] by wrapping HealthCheck in
// a [health.Report] with measured latency. The in-memory remote has
// no external dependencies, so the only failure mode is "store has
// been closed".
//
// We check ctx.Err() explicitly because the legacy HealthCheck above
// ignores its context argument; without this guard, a caller passing a
// canceled context would still receive [health.StatusHealthy] from a
// store that wasn't actually probed.
func (s *Store) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}
	return health.ReportFromError(s.HealthCheck(ctx), time.Since(start))
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
	for _, mb := range s.blocks {
		total += int64(len(mb.data))
	}
	return total
}
