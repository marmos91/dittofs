package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileChunkStore Implementation for Memory Store
// ============================================================================
//
// This file implements the FileChunkStore interface for the in-memory metadata store.
// It provides content-addressed file block tracking for deduplication and caching.
//
// The FileChunkStore interface is narrowed to 6 methods. The backend
// retains the legacy GetFileChunk + ListFileChunks helpers as
// concrete methods on the struct (not on the public interface) for
// engine-internal callers (engine/{fetch,dedup,syncer,engine}.go,
// blockstore/local/fs/{recovery,manage,fs}.go) that consume them via
// a wider engine-internal interface.
//
// Thread Safety: All operations are protected by the store's mutex.
//
// ============================================================================

// fileBlockStoreData holds the in-memory data structures for file block tracking.
type fileBlockStoreData struct {
	blocks map[string]*metadata.FileChunk // ID -> FileChunk

	// hashIndex maps content hash -> block ID for dedup lookups.
	// Only populated for finalized blocks (non-zero hash).
	hashIndex map[metadata.ContentHash]string
}

// newFileChunkStoreData creates a new fileBlockStoreData instance.
func newFileChunkStoreData() *fileBlockStoreData {
	return &fileBlockStoreData{
		blocks:    make(map[string]*metadata.FileChunk),
		hashIndex: make(map[metadata.ContentHash]string),
	}
}

// Ensure Store implements FileChunkStore
var _ block.FileChunkStore = (*MemoryMetadataStore)(nil)

// ============================================================================
// FileChunk Operations
// ============================================================================

// GetFileChunk retrieves a file block by its ID. Not on the narrowed
// FileChunkStore interface; kept as a backend
// method for engine-internal callers.
func (s *MemoryMetadataStore) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getFileChunkLocked(ctx, id)
}

// Put stores or updates a file block. Renamed from PutFileChunk to
// match the narrowed interface.
func (s *MemoryMetadataStore) Put(ctx context.Context, block *metadata.FileChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putFileChunkLocked(ctx, block)
}

// Delete removes a file block by its ID. Renamed from DeleteFileChunk.
func (s *MemoryMetadataStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteFileChunkLocked(ctx, id)
}

// IncrementRefCount atomically increments a block's RefCount.
func (s *MemoryMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.incrementRefCountLocked(ctx, id)
}

// DecrementRefCount atomically decrements a block's RefCount.
func (s *MemoryMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decrementRefCountLocked(ctx, id)
}

// DecrementRefCountAndReap atomically decrements RefCount and reaps the row
// (and its hash index entry) when the new count is 0, all under the single
// s.mu Write lock so the decrement-and-delete is TOCTOU-free against a
// concurrent AddRef. Implements the FileChunkStore.DecrementRefCountAndReap
// contract: ErrFileChunkNotFound is tolerated and reported as count 0.
func (s *MemoryMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decrementAndReapLocked(ctx, id)
}

// AddRef atomically increments RefCount on the FileChunk row indexed by
// the given content hash. Implements the FileChunkStore.AddRef contract
// used by the in-memory hash dedup LRU hit path to
// bump RefCount on an already-stored block without creating a new row.
//
// Atomicity: the entire hash→id resolve + RefCount mutation runs under
// a single s.mu Write lock so AddRef is TOCTOU-free against concurrent
// DecrementRefCount cascade.
//
// Returns metadata.ErrUnknownHash when no row exists for the hash;
// caller (LRU hit site) falls back to the full Put path.
//
// RefCount is the ONLY field mutated. BlockState is left
// unchanged — no Pending→Syncing→Remote transition; no new row is
// materialized on either the success or the ErrUnknownHash branch.
func (s *MemoryMetadataStore) AddRef(ctx context.Context, hash block.ContentHash, payloadID string, blockRef block.BlockRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addRefLocked(ctx, hash, payloadID, blockRef)
}

// GetByHash looks up a finalized block by its content hash.
// Returns nil without error if not found. Renamed from FindFileChunkByHash
func (s *MemoryMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findFileChunkByHashLocked(ctx, hash)
}

// ListFileChunks returns all blocks belonging to a file, ordered by block index.
// Not on the narrowed FileChunkStore interface;
// kept as a backend method for engine-internal callers.
func (s *MemoryMetadataStore) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listFileChunksLocked(ctx, payloadID)
}

// EnumerateLivePayloadIDs streams every distinct PayloadID referenced by a
// live inode. It snapshots the distinct PayloadIDs under the read lock, then
// calls fn per payloadID. Hardlinks share one fileData entry, so DISTINCT
// yields one payloadID per content. nlink=0 (unlinked) inodes are excluded
// (#1433): their payload is dead, so the reconcile treats it as stranded.
func (s *MemoryMetadataStore) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
	seen := make(map[string]struct{})
	s.mu.RLock()
	for key, fd := range s.files {
		if fd == nil || fd.Attr == nil {
			continue
		}
		if n, ok := s.linkCounts[key]; ok && n == 0 {
			continue // unlinked: payload is dead, not live (#1433)
		}
		if pid := string(fd.Attr.PayloadID); pid != "" {
			seen[pid] = struct{}{}
		}
	}
	s.mu.RUnlock()
	for payloadID := range seen {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// EnumeratePayloads streams every distinct payloadID that has at least one
// FileChunk row through fn. It collects distinct payloadIDs under the read
// lock (splitting each block.ID on the LAST '/' to recover the payloadID —
// payloadIDs are BuildPayloadID(shareName, filePath) and themselves contain
// slashes, while the trailing component is the chunk offset), releases the
// lock, then calls fn per payloadID so callers can issue further metadata
// operations. Unlike the local store's ListFiles, this enumerates the
// authoritative metadata, so it still yields rolled-up payloads.
func (s *MemoryMetadataStore) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
	seen := make(map[string]struct{})
	s.mu.RLock()
	if s.fileBlockData != nil {
		for id := range s.fileBlockData.blocks {
			i := strings.LastIndex(id, "/")
			if i < 0 {
				continue
			}
			seen[id[:i]] = struct{}{}
		}
	}
	s.mu.RUnlock()
	for payloadID := range seen {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// EnumerateFileChunks streams every FileChunk's ContentHash through fn.
// The memory backend snapshots hashes under the read lock then releases
// the lock before invoking fn so callers can issue further metadata
// operations. Lifted from FileChunkStore to MetadataStore —
// implementation unchanged.
func (s *MemoryMetadataStore) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	s.mu.RLock()
	snapshot := s.snapshotBlockHashesLocked()
	s.mu.RUnlock()
	return enumerateBlockHashes(ctx, snapshot, fn)
}

// snapshotBlockHashesLocked copies every live-set hash, unioning the CAS index
// (fileBlockData.blocks) with the per-file manifest (files[].Attr.Blocks) so
// the GC mark live set is a strict SUPERSET of both structures. The snapshot
// Backup HashSet is built from File.Blocks alone; a hash present only there
// (manifest row without a CAS index row) would otherwise be missed by the mark
// phase and the sweep would reap a still-live chunk (data loss). Caller MUST
// hold at least the read lock.
func (s *MemoryMetadataStore) snapshotBlockHashesLocked() []block.ContentHash {
	var snapshot []block.ContentHash
	if s.fileBlockData != nil {
		snapshot = make([]block.ContentHash, 0, len(s.fileBlockData.blocks))
		for _, b := range s.fileBlockData.blocks {
			snapshot = append(snapshot, b.Hash)
		}
	}
	// Union the per-file manifest (File.Blocks). Duplicates across the two
	// structures are harmless — GCState.Add deduplicates the live set. nlink=0
	// (unlinked) inodes keep their entry but the file is dead, so their manifest
	// blocks are excluded (#1433): including them would pin orphaned chunks live
	// forever. Snapshot-held blocks are protected separately by the GC
	// HoldProvider, not by this manifest. The authoritative link count is the
	// linkCounts map (#1166), not the embedded fd.Attr.Nlink (which SetLinkCount
	// does not rewrite); a missing entry is treated as live (fail-closed).
	for key, fd := range s.files {
		if fd == nil || fd.Attr == nil {
			continue
		}
		if n, ok := s.linkCounts[key]; ok && n == 0 {
			continue
		}
		for _, br := range fd.Attr.Blocks {
			snapshot = append(snapshot, br.Hash)
		}
	}
	return snapshot
}

// enumerateBlockHashes streams the snapshot through fn, honoring ctx
// cancellation. Shared by the public store method and the transaction method
// so the latter does not re-lock (WithTransaction already holds the lock).
func enumerateBlockHashes(ctx context.Context, snapshot []block.ContentHash, fn func(block.ContentHash) error) error {
	for _, h := range snapshot {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file blocks: %w", err)
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// EnumerateSyncingBlocks returns every FileChunk currently in
// BlockStateSyncing. the engine.Syncer janitor uses this to
// requeue rows abandoned by a previous syncer instance. The memory backend
// implements this via direct map iteration; other backends may opt in
// when their query surface allows.
func (s *MemoryMetadataStore) EnumerateSyncingBlocks(_ context.Context) ([]*metadata.FileChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fileBlockData == nil {
		return nil, nil
	}
	var result []*metadata.FileChunk
	for _, block := range s.fileBlockData.blocks {
		if block.State == metadata.BlockStateSyncing {
			b := *block
			result = append(result, &b)
		}
	}
	return result, nil
}

// ============================================================================
// Helper Methods
// ============================================================================

// initFileChunkData initializes the fileBlockStoreData if needed.
// Must be called with the write lock held.
func (s *MemoryMetadataStore) initFileChunkData() {
	if s.fileBlockData == nil {
		s.fileBlockData = newFileChunkStoreData()
	}
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure memoryTransaction implements FileChunkStore
var _ block.FileChunkStore = (*memoryTransaction)(nil)

func (tx *memoryTransaction) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	return tx.store.getFileChunkLocked(ctx, id)
}

func (tx *memoryTransaction) Put(ctx context.Context, block *metadata.FileChunk) error {
	return tx.store.putFileChunkLocked(ctx, block)
}

func (tx *memoryTransaction) Delete(ctx context.Context, id string) error {
	return tx.store.deleteFileChunkLocked(ctx, id)
}

func (tx *memoryTransaction) IncrementRefCount(ctx context.Context, id string) error {
	return tx.store.incrementRefCountLocked(ctx, id)
}

func (tx *memoryTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return tx.store.decrementRefCountLocked(ctx, id)
}

func (tx *memoryTransaction) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	return tx.store.decrementAndReapLocked(ctx, id)
}

func (tx *memoryTransaction) AddRef(ctx context.Context, hash metadata.ContentHash, payloadID string, blockRef block.BlockRef) error {
	return tx.store.addRefLocked(ctx, hash, payloadID, blockRef)
}

func (tx *memoryTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	return tx.store.findFileChunkByHashLocked(ctx, hash)
}

func (tx *memoryTransaction) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	return tx.store.listFileChunksLocked(ctx, payloadID)
}

func (tx *memoryTransaction) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	// WithTransaction already holds the write lock; snapshot without
	// re-locking (the public method's RLock would deadlock) so the enumerate
	// observes uncommitted tx writes.
	snapshot := tx.store.snapshotBlockHashesLocked()
	return enumerateBlockHashes(ctx, snapshot, fn)
}

// ============================================================================
// Locked Helpers (for transaction support)
// ============================================================================

func (s *MemoryMetadataStore) getFileChunkLocked(_ context.Context, id string) (*metadata.FileChunk, error) {
	if s.fileBlockData == nil {
		return nil, metadata.ErrFileChunkNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return nil, metadata.ErrFileChunkNotFound
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) putFileChunkLocked(_ context.Context, block *metadata.FileChunk) error {
	s.initFileChunkData()
	stored := *block
	s.fileBlockData.blocks[block.ID] = &stored

	// Update hash index for finalized blocks
	if block.IsFinalized() {
		s.fileBlockData.hashIndex[block.Hash] = block.ID
	}
	return nil
}

func (s *MemoryMetadataStore) deleteFileChunkLocked(_ context.Context, id string) error {
	if s.fileBlockData == nil {
		return metadata.ErrFileChunkNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return metadata.ErrFileChunkNotFound
	}

	// Remove from hash index
	if block.IsFinalized() {
		if s.fileBlockData.hashIndex[block.Hash] == id {
			delete(s.fileBlockData.hashIndex, block.Hash)
		}
	}

	delete(s.fileBlockData.blocks, id)
	return nil
}

func (s *MemoryMetadataStore) incrementRefCountLocked(_ context.Context, id string) error {
	if s.fileBlockData == nil {
		return metadata.ErrFileChunkNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return metadata.ErrFileChunkNotFound
	}
	block.RefCount++
	return nil
}

// addRefLocked resolves hash→id via the secondary index and bumps
// RefCount on the resolved row. Caller MUST hold s.mu Write lock so
// the entire resolve+mutate sequence is atomic (TOCTOU-free
// against concurrent DecrementRefCount cascade).
func (s *MemoryMetadataStore) addRefLocked(_ context.Context, hash block.ContentHash, _ string, _ block.BlockRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// memory backend records ref count only — parameters intentionally
	// blanked.
	// No data → no rows → hash is unknown by definition.
	if s.fileBlockData == nil {
		return metadata.ErrUnknownHash
	}
	id, ok := s.fileBlockData.hashIndex[hash]
	if !ok || id == "" {
		return metadata.ErrUnknownHash
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		// Index/blocks desync — defend by treating the hash as unknown.
		return metadata.ErrUnknownHash
	}
	block.RefCount++
	return nil
}

func (s *MemoryMetadataStore) decrementRefCountLocked(_ context.Context, id string) (uint32, error) {
	if s.fileBlockData == nil {
		return 0, metadata.ErrFileChunkNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return 0, metadata.ErrFileChunkNotFound
	}
	if block.RefCount > 0 {
		block.RefCount--
	}
	return block.RefCount, nil
}

// decrementAndReapLocked decrements RefCount and, when the new count is 0,
// deletes the row plus its hash-index entry. Caller MUST hold s.mu Write lock
// so the decrement-and-delete is a single atomic critical section. Returns
// (0, nil) when the row is already absent (a swept row is not a caller error).
func (s *MemoryMetadataStore) decrementAndReapLocked(ctx context.Context, id string) (uint32, error) {
	if s.fileBlockData == nil {
		return 0, nil
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return 0, nil
	}
	if block.RefCount > 0 {
		block.RefCount--
	}
	if block.RefCount == 0 {
		// Reap via the shared teardown (drops the row and, when finalized,
		// its hash-index entry) — runs in this same lock region so the
		// decrement-and-delete is TOCTOU-free vs AddRef. The row exists
		// (looked up above), so the NotFound branch cannot fire here.
		_ = s.deleteFileChunkLocked(ctx, id)
		return 0, nil
	}
	return block.RefCount, nil
}

func (s *MemoryMetadataStore) findFileChunkByHashLocked(_ context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	id, ok := s.fileBlockData.hashIndex[hash]
	if !ok {
		return nil, nil
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return nil, nil
	}
	// Only return remote blocks for dedup safety — prevents matching against
	// blocks that are dirty, being re-written, or mid-sync.
	if !block.IsRemote() {
		return nil, nil
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) listFileChunksLocked(_ context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	if s.fileBlockData == nil {
		return []*metadata.FileChunk{}, nil
	}
	prefix := payloadID + "/"
	type indexedBlock struct {
		block *metadata.FileChunk
		idx   int
	}
	var candidates []indexedBlock
	for id, block := range s.fileBlockData.blocks {
		if strings.HasPrefix(id, prefix) {
			suffix := id[len(prefix):]
			blockIdx, err := strconv.Atoi(suffix)
			if err != nil {
				continue // Skip entries with non-numeric suffix
			}
			b := *block
			candidates = append(candidates, indexedBlock{block: &b, idx: blockIdx})
		}
	}
	// Sort by block index ascending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].idx < candidates[j].idx
	})
	result := make([]*metadata.FileChunk, len(candidates))
	for i, c := range candidates {
		result[i] = c.block
	}
	return result, nil
}

// InjectRefCountLeak is a test-only capability hook implementing the
// storetest.RefCountLeakInjector interface (audit).
// It bumps the named block's RefCount by leakAmount without touching any
// FileAttr.Blocks reference, deliberately violating the global
// invariant so the leak-injection scenario can verify the reconciliation
// arithmetic detects the drift. NEVER call from production code.
func (s *MemoryMetadataStore) InjectRefCountLeak(_ context.Context, blockID string, leakAmount uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileBlockData == nil {
		return metadata.ErrFileChunkNotFound
	}
	block, ok := s.fileBlockData.blocks[blockID]
	if !ok {
		return metadata.ErrFileChunkNotFound
	}
	block.RefCount += leakAmount
	return nil
}
