package memory

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/local"
)

// Compile-time interface satisfaction check covering the
// BlockStoreAppend-contributed methods
// (Put/Get/GetRange/Has/Delete/Head/Walk/AppendWrite/DeleteAppendLog).
var (
	_ local.LocalStore          = (*MemoryStore)(nil)
	_ local.ChunkLifecycleHooks = (*MemoryStore)(nil)
	_ block.DurabilityReporter  = (*MemoryStore)(nil)
)

// ErrStoreClosed is an alias for block.ErrStoreClosed for backward compatibility.
var ErrStoreClosed = block.ErrStoreClosed

// casEntry is a single content-addressed chunk plus its metadata.
type casEntry struct {
	data         []byte
	lastModified time.Time
}

// ChunkEmitter is invoked once per CAS chunk freshly emitted by the
// MemoryStore's synchronous rollup. Engine and test wiring use this
// hook to populate downstream FileBlock metadata so the engine's CAS
// read path can resolve (payloadID, offset) → hash. The callback runs
// under MemoryStore's write lock; implementations must not call back
// into the MemoryStore (deadlock).
//
// chunkStart is the absolute byte offset of the chunk's first byte in
// the per-payload append-log stream. size is the chunk length in
// bytes. hash is the BLAKE3-256 content hash already stored in the
// MemoryStore's CAS map.
type ChunkEmitter func(payloadID string, chunkStart uint64, size uint32, hash block.ContentHash)

// MemoryStore is a pure in-memory implementation of local.LocalStore.
// All data lives in maps; nothing touches disk. Useful for testing and
// ephemeral configurations. The on-the-wire surface is exclusively
// the unified BlockStoreAppend (Put / Get / AppendWrite / DeleteAppendLog /
// Walk / Has / Delete / Head / GetRange) plus the LocalStore admin
// methods (lifecycle, retention, observability).
type MemoryStore struct {
	mu sync.RWMutex

	// files tracks file sizes (payloadID -> size). Populated by
	// AppendWrite via updateFileSizeLocked.
	files map[string]uint64

	// cas stores content-addressed chunks for the BlockStore surface.
	cas map[block.ContentHash]casEntry

	// appendLogs buffers per-payload AppendWrite data; the synchronous
	// rollup runs FastCDC over the consolidated buffer at every
	// AppendWrite (cheap for an in-memory test fixture).
	appendLogs map[string]*appendLog

	// chunkEmitter, if non-nil, is invoked once per CAS chunk freshly
	// emitted by rollup. Installed by engine.New (and by test
	// harnesses) so downstream FileBlock metadata can mirror the
	// rollup's chunk boundaries.
	chunkEmitter ChunkEmitter

	// durable reports whether accepted bytes survive a crash/restart
	// (block.DurabilityReporter). In-memory storage is volatile, so the type
	// default is false; an operator could override via SetDurable, though doing
	// so for a memory store is almost always a misconfiguration.
	durable atomic.Bool

	closed bool
}

// SetChunkEmitter wires the rollup-time per-chunk callback. Idempotent
// the most recent setter wins. Mirrors the *fs.FSStore's
// SetObjectIDPersister plumbing pattern. The signature uses the raw
// function type (rather than the ChunkEmitter alias) so the engine's
// interface assertion (engine.New) matches structurally without
// importing the alias.
func (s *MemoryStore) SetChunkEmitter(emit func(payloadID string, chunkStart uint64, size uint32, hash block.ContentHash)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkEmitter = emit
}

// SetObjectIDPersister is a no-op on MemoryStore. MemoryStore mirrors
// FileBlock rows through the per-chunk emitter installed via
// SetChunkEmitter; the rollup-completion persister is only used by the
// FSStore backend whose writes drive the engine's CAS read path through
// the rollup-persisted FileBlock manifest. Implemented to satisfy
// [local.ChunkLifecycleHooks] so engine.New can wire all three hooks
// through a single named-interface assertion.
func (s *MemoryStore) SetObjectIDPersister(_ func(ctx context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error) {
}

// SetOnChunkComplete is a no-op on MemoryStore. MemoryStore's writes
// don't materialize through the CAS chunkstore + read-Cache hot path
// that this callback warms; the in-memory rollup keeps everything in
// the MemoryStore's CAS map and FileBlock rows are emitted via
// SetChunkEmitter. Implemented to satisfy [local.ChunkLifecycleHooks]
// so engine.New can wire all three hooks through a single
// named-interface assertion.
func (s *MemoryStore) SetOnChunkComplete(_ func(hash block.ContentHash, data []byte, path string)) {
}

// appendLog is the per-payload write-absorber buffer. AppendWrite
// extends `buf` (sparse-filling gaps with zeros when offset > len) and
// the synchronous rollup chunks the whole buffer.
type appendLog struct {
	buf []byte
}

// New creates a new MemoryStore.
func New() *MemoryStore {
	return &MemoryStore{
		files:      make(map[string]uint64),
		cas:        make(map[block.ContentHash]casEntry),
		appendLogs: make(map[string]*appendLog),
	}
}

// Durable reports whether accepted bytes survive a crash/restart
// (block.DurabilityReporter). In-memory storage is volatile, so the type
// default is false. The zero value of the atomic field already encodes false.
func (s *MemoryStore) Durable() bool {
	return s.durable.Load()
}

// SetDurable overrides the type-default durability. Provided for symmetry with
// the fs backend so the controlplane can apply config["durable"] uniformly;
// marking a memory store durable is almost always a misconfiguration.
func (s *MemoryStore) SetDurable(durable bool) {
	s.durable.Store(durable)
}

// GetFileSize returns the tracked file size.
func (s *MemoryStore) GetFileSize(_ context.Context, payloadID string) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	size, ok := s.files[payloadID]
	return size, ok
}

// Put writes data under the key derived from hash. Idempotent on
// (hash, identical bytes).
//
// Implements block.BlockStore.
func (s *MemoryStore) Put(_ context.Context, hash block.ContentHash, data []byte) error {
	if hash.IsZero() {
		return fmt.Errorf("blockstore.memory: Put with zero hash")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	if existing, ok := s.cas[hash]; ok {
		if bytes.Equal(existing.data, data) {
			return nil
		}
		// Same-hash different-bytes is undefined per the contract; we
		// adopt the "no-op on first-write-wins" stance (the conformance
		// suite only asserts the same-bytes case).
		return nil
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	s.cas[hash] = casEntry{data: buf, lastModified: time.Now()}
	return nil
}

// Get returns the CAS chunk addressed by hash. The returned slice is a
// fresh copy — backends MUST NOT alias internal storage.
//
// Implements block.BlockStore.
func (s *MemoryStore) Get(_ context.Context, hash block.ContentHash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	entry, ok := s.cas[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	out := make([]byte, len(entry.data))
	copy(out, entry.data)
	return out, nil
}

// GetRange returns a byte sub-range of the chunk addressed by hash.
//
// Implements block.BlockStore.
func (s *MemoryStore) GetRange(_ context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	if offset < 0 {
		return nil, fmt.Errorf("blockstore.memory: GetRange: %w: offset %d", block.ErrInvalidOffset, offset)
	}
	if length <= 0 {
		return nil, fmt.Errorf("blockstore.memory: GetRange: %w: length %d", block.ErrInvalidSize, length)
	}
	entry, ok := s.cas[hash]
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	size := int64(len(entry.data))
	if offset >= size {
		return nil, fmt.Errorf("blockstore.memory: GetRange: offset %d beyond size %d", offset, size)
	}
	// offset < size is guaranteed above, so size-offset is positive; compare
	// against it instead of offset+length (which can overflow int64).
	if length > size-offset {
		length = size - offset
	}
	out := make([]byte, length)
	copy(out, entry.data[offset:offset+length])
	return out, nil
}

// Has reports whether the store currently holds an object addressed by hash.
//
// Implements block.BlockStore.
func (s *MemoryStore) Has(_ context.Context, hash block.ContentHash) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false, ErrStoreClosed
	}
	_, ok := s.cas[hash]
	return ok, nil
}

// Delete removes the object addressed by hash. Idempotent.
//
// Implements block.BlockStore.
func (s *MemoryStore) Delete(_ context.Context, hash block.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	delete(s.cas, hash)
	return nil
}

// Head returns Meta for the object addressed by hash.
//
// Implements block.BlockStore.
func (s *MemoryStore) Head(_ context.Context, hash block.ContentHash) (block.Meta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return block.Meta{}, ErrStoreClosed
	}
	entry, ok := s.cas[hash]
	if !ok {
		return block.Meta{}, block.ErrChunkNotFound
	}
	return block.Meta{
		Size:         int64(len(entry.data)),
		LastModified: entry.lastModified,
	}, nil
}

// Walk enumerates every CAS object in deterministic hash order. The
// callback may return block.ErrStopWalk for clean early exit.
//
// Implements block.BlockStore.
func (s *MemoryStore) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrStoreClosed
	}
	// Snapshot under read lock to avoid holding it across user callback.
	hashes := make([]block.ContentHash, 0, len(s.cas))
	for h := range s.cas {
		hashes = append(hashes, h)
	}
	snapshot := make(map[block.ContentHash]casEntry, len(s.cas))
	for h, e := range s.cas {
		snapshot[h] = e
	}
	s.mu.RUnlock()

	sort.Slice(hashes, func(i, j int) bool {
		return bytes.Compare(hashes[i][:], hashes[j][:]) < 0
	})
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := snapshot[h]
		meta := block.Meta{Size: int64(len(entry.data)), LastModified: entry.lastModified}
		if err := fn(h, meta); err != nil {
			if err == block.ErrStopWalk {
				return nil
			}
			return fmt.Errorf("walk halted at %s: %w", h, err)
		}
	}
	return nil
}

// ListUnsynced satisfies the local.LocalStore interface with an
// empty-yield implementation. MemoryStore is used only for tests and
// ephemeral configurations (see package godoc); it has no CAS
// persistence model that a Syncer would mirror to a remote store, so
// there is nothing to enumerate as "unsynced". Returning an empty
// iterator keeps the interface satisfied without inventing a
// hash-walk-plus-IsSynced loop that no production caller exercises.
//
// Implements local.LocalStore.
func (s *MemoryStore) ListUnsynced(ctx context.Context) iter.Seq2[block.ContentHash, error] {
	return func(yield func(block.ContentHash, error) bool) {
		_ = ctx
		_ = yield
	}
}

// AppendWrite stages random-offset bytes for payloadID into a
// per-file in-memory buffer and synchronously runs FastCDC over the
// consolidated buffer, emitting any newly-discovered chunks via the
// internal CAS map. Backends with on-disk logs use an asynchronous
// rollup goroutine; the in-memory backend runs it inline because the
// test surface needs deterministic post-AppendWrite Walk visibility.
//
// A prior DeleteAppendLog on the same payloadID does NOT permanently
// block subsequent AppendWrites: the memory store's rollup is
// synchronous under the same write lock as DeleteAppendLog, so there is no
// async-rollup-completion race to guard against. Recreate-at-same-id is
// supported. Files created after #1166 PR-3 get a UUID-based PayloadID
// (metadata/file_helpers.go buildPayloadID), so 'unlink + create at same
// path' yields a fresh content_id; DeleteAppendLog still runs on delete
// with the deleted file's own PayloadID to reclaim its append-log state.
//
// Implements block.BlockStoreAppend.
func (s *MemoryStore) AppendWrite(_ context.Context, payloadID string, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	log, ok := s.appendLogs[payloadID]
	if !ok {
		log = &appendLog{}
		s.appendLogs[payloadID] = log
	}
	end := offset + uint64(len(data))
	if uint64(len(log.buf)) < end {
		grown := make([]byte, end)
		copy(grown, log.buf)
		log.buf = grown
	}
	copy(log.buf[offset:end], data)

	// Track file size so GetFileSize / Truncate see writes done via
	// AppendWrite (engine.WriteAt routes here post-Phase-18).
	if end > s.files[payloadID] {
		s.files[payloadID] = end
	}

	// Synchronous FastCDC rollup over the entire log buffer.
	s.rollupLocked(payloadID, log.buf)
	return nil
}

// rollupLocked runs FastCDC over the consolidated payload buffer
// stores the resulting chunks in the CAS map, and invokes the
// chunkEmitter callback (if wired) once per chunk so downstream
// FileBlock metadata can mirror the rollup's chunk boundaries.
// Caller MUST hold s.mu for write.
func (s *MemoryStore) rollupLocked(payloadID string, buf []byte) {
	if len(buf) == 0 {
		return
	}
	c := chunker.NewChunker()
	pos := 0
	for pos < len(buf) {
		end, _ := c.Next(buf[pos:], true)
		if end <= 0 {
			break
		}
		chunk := buf[pos : pos+end]
		sum := blake3.Sum256(chunk)
		var h block.ContentHash
		copy(h[:], sum[:])
		if _, exists := s.cas[h]; !exists {
			cp := make([]byte, len(chunk))
			copy(cp, chunk)
			s.cas[h] = casEntry{data: cp, lastModified: time.Now()}
		}
		if s.chunkEmitter != nil {
			s.chunkEmitter(payloadID, uint64(pos), uint32(end), h)
		}
		pos += end
	}
}

// ReadPayloadAt serves bytes for [offset, offset+len(dest)) from the
// per-payload append-log buffer. The memory backend's append log is the
// authoritative byte source post-Phase-18 — rollup runs inline on every
// AppendWrite, so the buf field always reflects the most recent
// AppendWrite, including pre-rollup bytes (synchronous rollup means
// there is no real pre-rollup window in the in-memory backend, but
// AppendWrite still extends buf before invoking rollupLocked so a
// concurrent ReadPayloadAt observes the new bytes immediately).
//
// Returns (len(dest), nil) when the requested window lies entirely
// inside the buffer. Returns (0, block.ErrFileBlockNotFound) when
// the payload is unknown OR the offset is past the buffer end.
// Partial-coverage requests (offset inside, offset+len past end) are
// treated as a miss so the engine falls back to remote — the local
// store's contract is "all-or-nothing for the requested window".
//
// Implements local.LocalStore.
func (s *MemoryStore) ReadPayloadAt(_ context.Context, payloadID string, dest []byte, offset uint64) (int, error) {
	if len(dest) == 0 {
		return 0, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, ErrStoreClosed
	}
	log, ok := s.appendLogs[payloadID]
	if !ok || log == nil {
		return 0, block.ErrFileBlockNotFound
	}
	end := offset + uint64(len(dest))
	if end > uint64(len(log.buf)) {
		// Requested window extends past what we have locally — surface as
		// a miss so the caller can fall back to remote.
		return 0, block.ErrFileBlockNotFound
	}
	copy(dest, log.buf[offset:end])
	return len(dest), nil
}

// DeleteAppendLog removes the per-payload append-log buffer and clears
// the tracked file-size entry. Already-rolled-up CAS chunks remain in
// the store — orphan-chunk cleanup is GC's responsibility per the
// contract. Subsequent AppendWrites for the same payloadID resurrect
// a fresh log. Files created after #1166 PR-3 get a UUID-based
// PayloadID, so this runs on delete to reclaim the deleted file's own
// log; a recreate-at-same-path gets a fresh content_id.
//
// Implements block.BlockStoreAppend.
func (s *MemoryStore) DeleteAppendLog(_ context.Context, payloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	delete(s.appendLogs, payloadID)
	delete(s.files, payloadID)
	return nil
}

// DrainRollups is a no-op on the memory store. Its rollup runs inline on
// every AppendWrite (rollupLocked), so there is never a pending async
// rollup to force — the CAS map and any wired chunkEmitter are always
// current. Returns nil. Implements local.LocalStore.
func (s *MemoryStore) DrainRollups(_ context.Context) error { return nil }

// ResetLocalState clears the per-payload append-log buffers and tracked
// file sizes so a subsequent ReadPayloadAt cannot serve stale post-restore
// bytes from the in-memory buffer. The CAS map is intentionally retained:
// restored content is resolved through the CAS chunks the manifest
// references, exactly as the on-disk backend resolves through its CAS
// store after dropping the append log. Implements local.LocalStore.
func (s *MemoryStore) ResetLocalState(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	s.appendLogs = make(map[string]*appendLog)
	s.files = make(map[string]uint64)
	return nil
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

// Truncate discards append-log bytes beyond newSize and updates the
// tracked file size. CAS chunks are reclaimed by the engine's refcount
// → GC path, not by Truncate directly.
func (s *MemoryStore) Truncate(_ context.Context, payloadID string, newSize uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if log, ok := s.appendLogs[payloadID]; ok && uint64(len(log.buf)) > newSize {
		log.buf = log.buf[:newSize]
	}
	s.files[payloadID] = newSize
	return nil
}

// EvictMemory drops the per-file append-log buffer and forgets the
// tracked file size. CAS chunks are not removed here (they may be
// shared via file-level dedup); the engine's refcount → GC path
// reclaims orphaned chunks.
func (s *MemoryStore) EvictMemory(_ context.Context, payloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.appendLogs, payloadID)
	delete(s.files, payloadID)
	return nil
}

// SetEvictionEnabled satisfies the local.LocalStore interface. The
// in-memory backend does not implement eviction (chunks live for the
// process lifetime), so the toggle is a no-op.
func (s *MemoryStore) SetEvictionEnabled(_ bool) {}

// SetRetentionPolicy is a no-op in the memory store (memory store doesn't do eviction).
func (s *MemoryStore) SetRetentionPolicy(_ block.RetentionPolicy, _ time.Duration) {}

// Stats returns local store statistics. The in-memory backend reports
// per-payload append-log byte usage under MemUsed and the count of
// payloads with active logs under MemBlockCount (a coarse signal — the
// memory backend has no notion of fixed-size 8 MiB block frames).
func (s *MemoryStore) Stats() local.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var memUsed int64
	var memBlockCount int
	for _, log := range s.appendLogs {
		if log == nil {
			continue
		}
		memBlockCount++
		memUsed += int64(len(log.buf))
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

// GetStoredFileSize returns the size of the per-payload append log
// buffer. The append log is the authoritative byte source for the
// memory backend post-Phase-18 (legacy per-block blocks map removed).
func (s *MemoryStore) GetStoredFileSize(_ context.Context, payloadID string) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	log, ok := s.appendLogs[payloadID]
	if !ok || log == nil {
		return 0, nil
	}
	return uint64(len(log.buf)), nil
}
