package engine

import (
	"container/list"
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// keyKind discriminates the three coexisting key spaces in the read
// buffer during the dual-read window. The kinds share one eviction
// budget but cannot collide because blockKey is value-compared and the
// kind byte differs.
type keyKind uint8

const (
	// keyKindCoord: (payloadID, blockIdx) — the legacy engine.ReadAt
	// entry path that does not consult metadata before serving from
	// cache.
	keyKindCoord keyKind = 0

	// keyKindCAS: ContentHash — cross-file dedup (two callers reading
	// the same content share one cache line).
	keyKindCAS keyKind = 1

	// keyKindLegacy: pre-Phase-11 storeKey string ({payloadID}/block-{N}).
	keyKindLegacy keyKind = 2
)

// blockKey is the composite key for a cached block entry. The kind
// discriminator MUST be checked first; the matching field carries the
// actual key bytes. Cross-space collision is impossible by construction
// because blockKey is value-compared and the kind byte differs.
type blockKey struct {
	kind keyKind

	// keyKindCoord fields:
	payloadID string
	blockIdx  uint64

	// keyKindCAS field:
	cas blockstore.ContentHash

	// keyKindLegacy field:
	legacy string
}

// coordKey constructs a coordinate-space key (legacy engine.ReadAt path).
func coordKey(payloadID string, blockIdx uint64) blockKey {
	return blockKey{kind: keyKindCoord, payloadID: payloadID, blockIdx: blockIdx}
}

// casKey constructs a CAS-space key (D-22 — verified read path).
func casKey(h blockstore.ContentHash) blockKey {
	return blockKey{kind: keyKindCAS, cas: h}
}

// legacyKey constructs a legacy-space key keyed by the legacy
// {payloadID}/block-{N} string.
func legacyKey(storeKey string) blockKey {
	return blockKey{kind: keyKindLegacy, legacy: storeKey}
}

// cacheEntry holds the buffered block data stored in each list.Element.
type cacheEntry struct {
	key      blockKey
	data     []byte // heap-copied block data
	dataSize uint32 // actual bytes of valid data in the block
}

// ReadBuffer is an LRU read buffer that stores full blocks as heap-allocated
// []byte slices. It provides copy-on-read semantics: Get copies data into the
// caller's buffer and never returns internal slices.
//
// Thread safety: reads take RLock, mutations take WLock.
// Eviction is synchronous and inline during Put (O(1) -- just drops []byte ref).
type ReadBuffer struct {
	mu      sync.RWMutex
	entries map[blockKey]*list.Element     // primary index: blockKey -> list element
	lru     *list.List                     // front = most recent, back = LRU victim
	byFile  map[string]map[uint64]struct{} // secondary index: payloadID -> set of blockIdx

	maxBytes int64 // memory budget
	curBytes int64 // current usage

	prefetcher *Prefetcher // optional sequential prefetcher
}

// NewReadBuffer creates a new ReadBuffer with the given memory budget in bytes.
// Returns nil if maxBytes <= 0 (disabled mode).
func NewReadBuffer(maxBytes int64) *ReadBuffer {
	if maxBytes <= 0 {
		return nil
	}
	return &ReadBuffer{
		entries:  make(map[blockKey]*list.Element),
		lru:      list.New(),
		byFile:   make(map[string]map[uint64]struct{}),
		maxBytes: maxBytes,
	}
}

// Get reads a buffered block (coordinate space) into dest starting from
// offset within the block data. Returns bytes copied and whether the
// block was found. Copy-on-read: modifying dest does not affect the
// buffer. See GetCAS / GetLegacy for the dual-read window APIs.
func (c *ReadBuffer) Get(payloadID string, blockIdx uint64, dest []byte, offset uint32) (int, bool) {
	if c == nil {
		return 0, false
	}
	return c.getAt(coordKey(payloadID, blockIdx), dest, offset)
}

// Put inserts or updates a coordinate-space block in the read buffer.
// A heap copy of data is made. LRU entries are evicted synchronously
// when over budget; blocks larger than maxBytes are silently skipped.
// See PutCAS / PutLegacy for the dual-read window APIs.
func (c *ReadBuffer) Put(payloadID string, blockIdx uint64, data []byte, dataSize uint32) {
	c.putAt(coordKey(payloadID, blockIdx), data, dataSize)
}

// putAt is the shared insert path for all three key spaces. Eviction
// follows the existing LRU policy and the byFile secondary index is
// maintained only for coordinate keys.
func (c *ReadBuffer) putAt(key blockKey, data []byte, dataSize uint32) {
	if c == nil {
		return
	}
	if int64(dataSize) > c.maxBytes {
		return
	}
	// Clamp dataSize to len(data) to prevent out-of-bounds panic from callers
	// passing inconsistent values.
	if int(dataSize) > len(data) {
		dataSize = uint32(len(data))
	}

	heapCopy := make([]byte, dataSize)
	copy(heapCopy, data[:dataSize])

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.entries[key]; ok {
		old := elem.Value.(*cacheEntry)
		c.curBytes -= int64(old.dataSize)
		old.data = heapCopy
		old.dataSize = dataSize
		c.curBytes += int64(dataSize)
		c.lru.MoveToFront(elem)
		for c.curBytes > c.maxBytes && c.lru.Len() > 1 {
			c.evictLRU()
		}
		return
	}

	entry := &cacheEntry{
		key:      key,
		data:     heapCopy,
		dataSize: dataSize,
	}
	elem := c.lru.PushFront(entry)
	c.entries[key] = elem
	c.curBytes += int64(dataSize)

	if key.kind == keyKindCoord {
		idxSet, ok := c.byFile[key.payloadID]
		if !ok {
			idxSet = make(map[uint64]struct{})
			c.byFile[key.payloadID] = idxSet
		}
		idxSet[key.blockIdx] = struct{}{}
	}

	for c.curBytes > c.maxBytes && c.lru.Len() > 1 {
		c.evictLRU()
	}
}

// GetCAS reads a CAS-cached block (D-22) into dest. Returns (n, true)
// if the hash is in the cache. The verifier is the source of truth for
// content integrity; the cache only serves bytes that were verified on
// insert. Phase 12 CACHE-02 collapses the buffer to this single API.
func (c *ReadBuffer) GetCAS(hash blockstore.ContentHash, dest []byte, offset uint32) (int, bool) {
	if c == nil {
		return 0, false
	}
	return c.getAt(casKey(hash), dest, offset)
}

// PutCAS inserts data into the CAS key space, keyed by ContentHash.
// Cross-file dedup: two payloads referencing the same hash share one
// cache line. Heap-copies data per the existing copy-on-write semantics.
func (c *ReadBuffer) PutCAS(hash blockstore.ContentHash, data []byte, dataSize uint32) {
	c.putAt(casKey(hash), data, dataSize)
}

// GetLegacy reads a legacy-string-keyed block (dual-read window only,
// D-22). The legacy storeKey is the FormatStoreKey output for
// pre-Phase-11 file blocks. Removed when the dual-read code path
// retires in Phase 15.
func (c *ReadBuffer) GetLegacy(storeKey string, dest []byte, offset uint32) (int, bool) {
	if c == nil {
		return 0, false
	}
	return c.getAt(legacyKey(storeKey), dest, offset)
}

// PutLegacy inserts data into the legacy storeKey space.
func (c *ReadBuffer) PutLegacy(storeKey string, data []byte, dataSize uint32) {
	c.putAt(legacyKey(storeKey), data, dataSize)
}

// getAt is the shared read path for all three key spaces.
func (c *ReadBuffer) getAt(key blockKey, dest []byte, offset uint32) (int, bool) {
	c.mu.RLock()
	elem, ok := c.entries[key]
	if !ok {
		c.mu.RUnlock()
		return 0, false
	}
	entry := elem.Value.(*cacheEntry)
	if offset >= entry.dataSize {
		c.mu.RUnlock()
		return 0, false
	}
	n := copy(dest, entry.data[offset:entry.dataSize])
	c.mu.RUnlock()

	c.mu.Lock()
	if elem2, still := c.entries[key]; still {
		c.lru.MoveToFront(elem2)
	}
	c.mu.Unlock()

	return n, true
}

// InvalidateCAS removes a CAS entry by ContentHash. Returned bool
// indicates whether an entry was present.
func (c *ReadBuffer) InvalidateCAS(hash blockstore.ContentHash) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[casKey(hash)]
	if !ok {
		return false
	}
	c.removeEntry(elem)
	return true
}

// InvalidateLegacy removes a legacy entry by its storeKey string.
func (c *ReadBuffer) InvalidateLegacy(storeKey string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[legacyKey(storeKey)]
	if !ok {
		return false
	}
	c.removeEntry(elem)
	return true
}

// ContainsCAS reports whether a CAS hash is buffered. Does not promote LRU.
func (c *ReadBuffer) ContainsCAS(hash blockstore.ContentHash) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	_, ok := c.entries[casKey(hash)]
	c.mu.RUnlock()
	return ok
}

// ContainsLegacy reports whether a legacy storeKey is buffered. Does not
// promote LRU.
func (c *ReadBuffer) ContainsLegacy(storeKey string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	_, ok := c.entries[legacyKey(storeKey)]
	c.mu.RUnlock()
	return ok
}

// Invalidate removes a single block entry from the read buffer.
func (c *ReadBuffer) Invalidate(payloadID string, blockIdx uint64) {
	if c == nil {
		return
	}

	key := coordKey(payloadID, blockIdx)

	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.entries[key]
	if !ok {
		return
	}
	c.removeEntry(elem)
}

// InvalidateFile removes all buffered blocks for the given payloadID.
// Uses the secondary index for O(entries_for_file) performance.
func (c *ReadBuffer) InvalidateFile(payloadID string) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	idxSet, ok := c.byFile[payloadID]
	if !ok {
		return
	}

	for blockIdx := range idxSet {
		c.unlinkEntry(coordKey(payloadID, blockIdx))
	}
	delete(c.byFile, payloadID)
}

// InvalidateAbove removes all buffered blocks for the given payloadID where
// blockIdx >= threshold. Used for truncate support.
func (c *ReadBuffer) InvalidateAbove(payloadID string, threshold uint64) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	idxSet, ok := c.byFile[payloadID]
	if !ok {
		return
	}

	for blockIdx := range idxSet {
		if blockIdx >= threshold {
			c.unlinkEntry(coordKey(payloadID, blockIdx))
			delete(idxSet, blockIdx)
		}
	}

	if len(idxSet) == 0 {
		delete(c.byFile, payloadID)
	}
}

// Contains checks if a block is present in the read buffer. Does not promote.
func (c *ReadBuffer) Contains(payloadID string, blockIdx uint64) bool {
	if c == nil {
		return false
	}

	key := coordKey(payloadID, blockIdx)

	c.mu.RLock()
	_, ok := c.entries[key]
	c.mu.RUnlock()
	return ok
}

// MaxBytes returns the memory budget of the read buffer.
// Returns 0 if the read buffer is nil (disabled).
func (c *ReadBuffer) MaxBytes() int64 {
	if c == nil {
		return 0
	}
	return c.maxBytes
}

// CacheStats holds read buffer statistics.
type CacheStats struct {
	Entries  int   `json:"entries"`   // Number of buffered blocks
	CurBytes int64 `json:"cur_bytes"` // Current memory usage in bytes
	MaxBytes int64 `json:"max_bytes"` // Memory budget in bytes
}

// Stats returns a snapshot of read buffer statistics.
// Returns zero-value stats if the read buffer is nil (disabled).
func (c *ReadBuffer) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CacheStats{
		Entries:  len(c.entries),
		CurBytes: c.curBytes,
		MaxBytes: c.maxBytes,
	}
}

// SetPrefetcher attaches a prefetcher to this read buffer. The prefetcher is
// created after the read buffer (in BlockStore.Start) so it must be set separately.
func (c *ReadBuffer) SetPrefetcher(p *Prefetcher) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prefetcher = p
}

// BlockDataFn loads a block's raw data from the local store.
// Injected to avoid import cycles with the local package.
type BlockDataFn func(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)

// InvalidateRange invalidates all read buffer entries covering the byte range
// [offset, offset+length) and resets the prefetcher for the file.
// Used by WriteAt to keep the read buffer consistent with writes.
func (c *ReadBuffer) InvalidateRange(payloadID string, offset uint64, length int, blockSize uint64) {
	if c == nil || length <= 0 {
		return
	}
	startBlock := offset / blockSize
	endBlock := (offset + uint64(length) - 1) / blockSize
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		c.Invalidate(payloadID, blockIdx)
	}
	c.resetPrefetcher(payloadID)
}

// InvalidateAboveSize invalidates all read buffer entries for blocks above
// newSize bytes and resets the prefetcher. Used by Truncate.
func (c *ReadBuffer) InvalidateAboveSize(payloadID string, newSize uint64, blockSize uint64) {
	if c == nil {
		return
	}
	newBlockCount := (newSize + blockSize - 1) / blockSize
	c.InvalidateAbove(payloadID, newBlockCount)
	c.resetPrefetcher(payloadID)
}

// InvalidateAndReset invalidates all read buffer entries for a file and resets
// the prefetcher. Used by Delete.
func (c *ReadBuffer) InvalidateAndReset(payloadID string) {
	if c == nil {
		return
	}
	c.InvalidateFile(payloadID)
	c.resetPrefetcher(payloadID)
}

// NotifyRead informs the prefetcher about a read covering the byte range
// [offset, offset+length). Each block in the range is reported individually
// so multi-block reads are correctly detected as sequential.
func (c *ReadBuffer) NotifyRead(payloadID string, offset, length, blockSize uint64) {
	if c == nil || c.prefetcher == nil || length == 0 {
		return
	}
	startBlock := offset / blockSize
	endBlock := (offset + length - 1) / blockSize
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		c.prefetcher.OnRead(payloadID, blockIdx)
	}
}

// FillFromStore reads full blocks from the local store and populates the read
// buffer for the byte range [offset, offset+length). Skips blocks already present.
func (c *ReadBuffer) FillFromStore(ctx context.Context, payloadID string, offset, length, blockSize uint64, getBlockData BlockDataFn) {
	if c == nil || length == 0 {
		return
	}
	startBlock := offset / blockSize
	endBlock := (offset + length - 1) / blockSize
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		if c.Contains(payloadID, blockIdx) {
			continue
		}
		data, dataSize, err := getBlockData(ctx, payloadID, blockIdx)
		if err == nil && data != nil {
			c.Put(payloadID, blockIdx, data, dataSize)
		}
	}
}

// resetPrefetcher resets the prefetcher state for a payloadID.
func (c *ReadBuffer) resetPrefetcher(payloadID string) {
	if c.prefetcher != nil {
		c.prefetcher.Reset(payloadID)
	}
}

// Close clears all read buffer state. After Close, Get returns miss for all keys.
func (c *ReadBuffer) Close() {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[blockKey]*list.Element)
	c.lru.Init()
	c.byFile = make(map[string]map[uint64]struct{})
	c.curBytes = 0
}

// evictLRU removes the least recently used entry. Must be called under WLock.
func (c *ReadBuffer) evictLRU() {
	back := c.lru.Back()
	if back == nil {
		return
	}
	c.removeEntry(back)
}

// unlinkEntry removes an entry from the primary index and LRU list by key.
// Does NOT touch the secondary index (byFile). Must be called under WLock.
func (c *ReadBuffer) unlinkEntry(key blockKey) {
	elem, ok := c.entries[key]
	if !ok {
		return
	}
	entry := elem.Value.(*cacheEntry)
	c.curBytes -= int64(entry.dataSize)
	c.lru.Remove(elem)
	delete(c.entries, key)
}

// removeEntry removes a list element from all data structures including the
// secondary index. Must be called under WLock.
func (c *ReadBuffer) removeEntry(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	c.unlinkEntry(entry.key)

	// Secondary byFile index only tracks coordinate-space entries. CAS
	// and legacy-string entries are not file-scoped (CAS spans files;
	// legacy keys are dual-read-window only and aged out per-key).
	if entry.key.kind != keyKindCoord {
		return
	}
	if idxSet, ok := c.byFile[entry.key.payloadID]; ok {
		delete(idxSet, entry.key.blockIdx)
		if len(idxSet) == 0 {
			delete(c.byFile, entry.key.payloadID)
		}
	}
}
