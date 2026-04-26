// Package memory provides an in-memory RemoteStore implementation for testing.
package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// Compile-time interface satisfaction check.
var _ remote.RemoteStore = (*Store)(nil)

// Store is an in-memory implementation of remote.RemoteStore for testing.
type Store struct {
	mu     sync.RWMutex
	blocks map[string][]byte
	// metadata mirrors the per-object user metadata that S3 stores in
	// `x-amz-meta-*` headers (BSCAS-06). Populated by WriteBlockWithHash;
	// remains nil/absent for legacy WriteBlock entries so the conformance
	// suite can assert the negative case.
	metadata map[string]map[string]string
	// lastModified tracks the per-object write timestamp surfaced by
	// ListByPrefixWithMeta (D-05). Real S3 backends report this from
	// the object's LastModified header; the in-memory backend captures
	// time.Now() at every write so GC sweep tests can apply the same
	// grace TTL filter.
	lastModified map[string]time.Time
	// nowFn returns the current time for the store. Tests can override
	// this to manipulate LastModified deterministically.
	nowFn  func() time.Time
	closed bool
}

// New creates a new in-memory remote block store.
func New() *Store {
	return &Store{
		blocks:       make(map[string][]byte),
		metadata:     make(map[string]map[string]string),
		lastModified: make(map[string]time.Time),
		nowFn:        time.Now,
	}
}

// SetNowFnForTest overrides the time source used by Write/WriteBlockWithHash
// to stamp LastModified. Test-only helper for the GC sweep grace TTL test.
func (s *Store) SetNowFnForTest(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn == nil {
		s.nowFn = time.Now
		return
	}
	s.nowFn = fn
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
	// Legacy WriteBlock writes no per-object metadata. Drop any prior
	// entry so the conformance assertion ("WriteBlock_NoHeader") sees a
	// clean slate even when the caller previously populated the key via
	// WriteBlockWithHash.
	delete(s.metadata, blockKey)
	s.lastModified[blockKey] = s.nowFn()

	return nil
}

// WriteBlockWithHash implements RemoteStore (BSCAS-06). Records the
// content-hash header alongside the data so the remotetest conformance
// suite (and any in-process consumer) can assert it via GetObjectMetadata.
func (s *Store) WriteBlockWithHash(_ context.Context, blockKey string, hash blockstore.ContentHash, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	copied := make([]byte, len(data))
	copy(copied, data)
	s.blocks[blockKey] = copied
	s.metadata[blockKey] = map[string]string{
		"content-hash": hash.CASKey(),
	}
	s.lastModified[blockKey] = s.nowFn()

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

// ReadBlockVerified mirrors s3.Store.ReadBlockVerified for in-memory
// testing (INV-06). The header pre-check uses the recorded
// "content-hash" entry (set by WriteBlockWithHash); the body-recompute
// always re-hashes the stored bytes so a Test that mutates the in-memory
// blob (cosmically rare but possible in fault-injection setups) still
// surfaces ErrCASContentMismatch.
func (s *Store) ReadBlockVerified(_ context.Context, blockKey string, expected blockstore.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	data, ok := s.blocks[blockKey]
	if !ok {
		return nil, blockstore.ErrBlockNotFound
	}

	// Header pre-check (D-19): if metadata records a content hash that
	// does not match expected, fail fast before recomputing the body.
	if md, ok := s.metadata[blockKey]; ok {
		if hdr, ok := md["content-hash"]; ok && hdr != expected.CASKey() {
			return nil, fmt.Errorf("%w: header %q != expected %q",
				blockstore.ErrCASContentMismatch, hdr, expected.CASKey())
		}
	}

	// Body recompute (D-18). Note this hashes the stored bytes directly
	// because there is no streaming response body in the memory backend.
	got := blake3.Sum256(data)
	var gotHash blockstore.ContentHash
	copy(gotHash[:], got[:])
	if gotHash != expected {
		return nil, fmt.Errorf("%w: got %s, want %s",
			blockstore.ErrCASContentMismatch, gotHash.CASKey(), expected.CASKey())
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

// CopyBlock copies a block from srcKey to dstKey in memory.
func (s *Store) CopyBlock(_ context.Context, srcKey, dstKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	data, ok := s.blocks[srcKey]
	if !ok {
		return blockstore.ErrBlockNotFound
	}

	copied := make([]byte, len(data))
	copy(copied, data)
	s.blocks[dstKey] = copied
	// Mirror S3 CopyObject semantics: metadata is carried with the object
	// by default unless MetadataDirective=REPLACE is requested.
	if md, ok := s.metadata[srcKey]; ok {
		mdCopy := make(map[string]string, len(md))
		for k, v := range md {
			mdCopy[k] = v
		}
		s.metadata[dstKey] = mdCopy
	} else {
		delete(s.metadata, dstKey)
	}
	s.lastModified[dstKey] = s.nowFn()
	return nil
}

// DeleteBlock removes a single block from memory.
func (s *Store) DeleteBlock(_ context.Context, blockKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return blockstore.ErrStoreClosed
	}

	delete(s.blocks, blockKey)
	delete(s.metadata, blockKey)
	delete(s.lastModified, blockKey)
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
			delete(s.metadata, key)
			delete(s.lastModified, key)
		}
	}

	return nil
}

// ListByPrefixWithMeta lists all objects under prefix with per-object
// metadata (Key, Size, LastModified). Used by the GC sweep phase to apply
// the snapshot - GracePeriod TTL filter (D-05).
func (s *Store) ListByPrefixWithMeta(_ context.Context, prefix string) ([]remote.ObjectInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, blockstore.ErrStoreClosed
	}

	out := make([]remote.ObjectInfo, 0)
	for key, data := range s.blocks {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, remote.ObjectInfo{
			Key:          key,
			Size:         int64(len(data)),
			LastModified: s.lastModified[key],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
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
	s.metadata = nil
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
	for _, data := range s.blocks {
		total += int64(len(data))
	}
	return total
}

// GetObjectMetadata returns the per-object user metadata recorded for
// blockKey, or nil if the object was written via legacy WriteBlock (no
// header). Used by the remotetest conformance suite to assert
// x-amz-meta-content-hash presence (BSCAS-06).
//
// The returned map is a defensive copy — mutations by callers do not
// affect the in-memory store.
func (s *Store) GetObjectMetadata(blockKey string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	md, ok := s.metadata[blockKey]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}
