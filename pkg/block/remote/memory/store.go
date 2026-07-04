// Package memory provides an in-memory RemoteStore implementation for testing.
package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// Compile-time interface satisfaction check.
var (
	_ remote.RemoteStore       = (*Store)(nil)
	_ remote.RemoteBlockStore  = (*Store)(nil)
	_ remote.ChunkReader       = (*Store)(nil)
	_ remote.ChunkSealer       = (*Store)(nil)
	_ block.DurabilityReporter = (*Store)(nil)
)

// memBlock holds the per-object body plus metadata captured at Put time.
type memBlock struct {
	data         []byte
	lastModified time.Time
}

// Store is an in-memory implementation of remote.RemoteStore for testing.
// The map is keyed by block.ContentHash (the unified CAS key from
// ); previous versions of this package used opaque string
// blockKeys and stamped x-amz-meta-content-hash separately.
type Store struct {
	mu     sync.RWMutex
	blocks map[block.ContentHash]*memBlock
	// blocksByID holds block objects keyed by BlockID (#1414). Populated via PutBlock
	// (used by tests and the future packer); read by ReadChunk and the new
	// block-keyed methods. Separate from blocks because block objects are keyed
	// by an opaque BlockID string, not a content hash.
	blocksByID map[string]*memBlock
	// nowFn returns the current time for the store. Tests can override
	// this to manipulate LastModified deterministically.
	nowFn  func() time.Time
	closed bool

	// durable reports whether accepted bytes survive a crash/restart
	// (block.DurabilityReporter). This is a test/dev fixture whose contents are
	// volatile, so the type default is false.
	durable atomic.Bool
}

// New creates a new in-memory remote block store.
func New() *Store {
	return &Store{
		blocks:     make(map[block.ContentHash]*memBlock),
		blocksByID: make(map[string]*memBlock),
		nowFn:      time.Now,
	}
}

// PutBlock writes the content of r under blocks/<blockID>. Implements
// remote.RemoteBlockStore. Idempotent: a second call overwrites silently.
// A defensive copy is taken via io.ReadAll; callers may reuse r after return.
func (s *Store) PutBlock(_ context.Context, blockID string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("memory put block %s: %w", blockID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	if s.blocksByID == nil {
		s.blocksByID = make(map[string]*memBlock)
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	s.blocksByID[blockID] = &memBlock{data: copied, lastModified: s.nowFn()}
	return nil
}

// GetBlock returns the full bytes of the block object identified by blockID.
// Returns block.ErrChunkNotFound when the block is absent.
func (s *Store) GetBlock(_ context.Context, blockID string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, block.ErrStoreClosed
	}
	mb, ok := s.blocksByID[blockID]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	copied := make([]byte, len(mb.data))
	copy(copied, mb.data)
	return copied, nil
}

// GetBlockRange returns [offset, offset+length) bytes of the block object
// identified by blockID. Bounds semantics mirror GetRange.
func (s *Store) GetBlockRange(_ context.Context, blockID string, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, block.ErrStoreClosed
	}
	mb, ok := s.blocksByID[blockID]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	if offset < 0 || offset >= int64(len(mb.data)) {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}
	size := int64(len(mb.data))
	end := size
	if length <= size-offset {
		end = offset + length
	}
	result := make([]byte, end-offset)
	copy(result, mb.data[offset:end])
	return result, nil
}

// DeleteBlock removes the block object keyed by blockID. Idempotent:
// deleting an absent blockID returns nil.
func (s *Store) DeleteBlock(_ context.Context, blockID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return block.ErrStoreClosed
	}
	delete(s.blocksByID, blockID)
	return nil
}

// WalkBlocks enumerates every block object in the store. The callback receives
// the blockID and block.Meta. Honors block.ErrStopWalk; any other callback
// error halts the walk and is wrapped as "walk halted at <blockID>: %w".
// Context cancellation aborts immediately.
func (s *Store) WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return block.ErrStoreClosed
	}
	type entry struct {
		id   string
		meta block.Meta
	}
	snap := make([]entry, 0, len(s.blocksByID))
	for id, mb := range s.blocksByID {
		snap = append(snap, entry{
			id: id,
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
		if cberr := fn(e.id, e.meta); cberr != nil {
			if errors.Is(cberr, block.ErrStopWalk) {
				return nil
			}
			return fmt.Errorf("walk halted at %s: %w", e.id, cberr)
		}
	}
	return nil
}

// ReadChunk returns the wire bytes [offset, offset+length) from the block
// object blockID. As a base store there is no transform to invert and no
// verification here. Implements remote.ChunkReader; hash is unused at this
// layer. Bounds semantics mirror GetRange.
func (s *Store) ReadChunk(_ context.Context, blockID string, offset, length int64, _ block.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, block.ErrStoreClosed
	}
	mb, ok := s.blocksByID[blockID]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	data := mb.data
	if offset < 0 || offset >= int64(len(data)) {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}
	size := int64(len(data))
	end := size
	if length <= size-offset {
		end = offset + length
	}
	result := make([]byte, end-offset)
	copy(result, data[offset:end])
	return result, nil
}

// SealChunk implements remote.ChunkSealer as the identity transform: the base
// store stores chunk bodies verbatim, so the wire bytes equal the plaintext.
// The compression/encryption decorators wrap this to seal their own layer.
// A defensive copy is returned so the carver may retain it independently of the
// caller's plaintext buffer.
func (s *Store) SealChunk(_ context.Context, _ block.ContentHash, plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	copy(out, plaintext)
	return out, nil
}

// Durable reports whether accepted bytes survive a crash/restart
// (block.DurabilityReporter). The in-memory remote fixture is volatile, so the
// type default is false (the zero value of the atomic field).
func (s *Store) Durable() bool {
	return s.durable.Load()
}

// SetDurable overrides the type-default durability, applied by the controlplane
// when the per-store config carries an explicit "durable".
func (s *Store) SetDurable(durable bool) {
	s.durable.Store(durable)
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

// Close marks the store as closed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.blocks = nil
	s.blocksByID = nil
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
		return block.ErrStoreClosed
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
