package readcache

import (
	"container/list"
	"sync"
)

// blockKey is the composite key for a cached block entry.
type blockKey struct {
	payloadID string
	blockIdx  uint64
}

// cacheEntry holds the cached block data stored in each list.Element.
type cacheEntry struct {
	key      blockKey
	data     []byte // heap-copied block data
	dataSize uint32 // actual bytes of valid data in the block
}

// ReadCache is an LRU block cache that stores full blocks as heap-allocated
// []byte slices. It provides copy-on-read semantics: Get copies data into the
// caller's buffer and never returns internal slices.
//
// Thread safety: reads take RLock, mutations take WLock.
// Eviction is synchronous and inline during Put (O(1) -- just drops []byte ref).
type ReadCache struct {
	mu      sync.RWMutex
	entries map[blockKey]*list.Element // primary index: blockKey -> list element
	lru     *list.List                // front = most recent, back = LRU victim
	byFile  map[string]map[uint64]struct{}  // secondary index: payloadID -> set of blockIdx

	maxBytes int64 // memory budget
	curBytes int64 // current usage
}

// New creates a new ReadCache with the given memory budget in bytes.
// Returns nil if maxBytes <= 0 (disabled mode).
func New(maxBytes int64) *ReadCache {
	if maxBytes <= 0 {
		return nil
	}
	return &ReadCache{
		entries:  make(map[blockKey]*list.Element),
		lru:      list.New(),
		byFile:   make(map[string]map[uint64]struct{}),
		maxBytes: maxBytes,
	}
}

// Get reads a cached block into dest starting from offset within the block data.
// Returns the number of bytes copied and whether the block was found.
// If offset >= dataSize, returns (0, false).
// Copy-on-read: modifying dest does not affect cached data.
func (c *ReadCache) Get(payloadID string, blockIdx uint64, dest []byte, offset uint32) (int, bool) {
	if c == nil {
		return 0, false
	}

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

	// Lookup under RLock.
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

	// Promote under WLock (separate lock acquisition to avoid holding RLock
	// while taking WLock, which would deadlock).
	c.mu.Lock()
	// Re-check: entry may have been evicted between RUnlock and WLock.
	if elem2, still := c.entries[key]; still {
		c.lru.MoveToFront(elem2)
	}
	c.mu.Unlock()

	return n, true
}

// Put inserts or updates a block in the cache. A heap copy of data is made.
// If the cache exceeds maxBytes, LRU entries are evicted synchronously.
func (c *ReadCache) Put(payloadID string, blockIdx uint64, data []byte, dataSize uint32) {
	if c == nil {
		return
	}

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

	// Make a heap copy of the data.
	heapCopy := make([]byte, dataSize)
	copy(heapCopy, data[:dataSize])

	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry.
	if elem, ok := c.entries[key]; ok {
		old := elem.Value.(*cacheEntry)
		c.curBytes -= int64(old.dataSize)
		old.data = heapCopy
		old.dataSize = dataSize
		c.curBytes += int64(dataSize)
		c.lru.MoveToFront(elem)
		// Evict if over budget after update (e.g., replacement with larger data).
		for c.curBytes > c.maxBytes && c.lru.Len() > 1 {
			c.evictLRU()
		}
		return
	}

	// Insert new entry.
	entry := &cacheEntry{
		key:      key,
		data:     heapCopy,
		dataSize: dataSize,
	}
	elem := c.lru.PushFront(entry)
	c.entries[key] = elem
	c.curBytes += int64(dataSize)

	// Update secondary index.
	idxSet, ok := c.byFile[payloadID]
	if !ok {
		idxSet = make(map[uint64]struct{})
		c.byFile[payloadID] = idxSet
	}
	idxSet[blockIdx] = struct{}{}

	// Evict LRU entries until under budget.
	for c.curBytes > c.maxBytes && c.lru.Len() > 1 {
		c.evictLRU()
	}
}

// Invalidate removes a single block entry from the cache.
func (c *ReadCache) Invalidate(payloadID string, blockIdx uint64) {
	if c == nil {
		return
	}

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.entries[key]
	if !ok {
		return
	}
	c.removeEntry(elem)
}

// InvalidateFile removes all cached blocks for the given payloadID.
// Uses the secondary index for O(entries_for_file) performance.
func (c *ReadCache) InvalidateFile(payloadID string) {
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
		c.unlinkEntry(blockKey{payloadID: payloadID, blockIdx: blockIdx})
	}
	delete(c.byFile, payloadID)
}

// InvalidateAbove removes all cached blocks for the given payloadID where
// blockIdx >= threshold. Used for truncate support.
func (c *ReadCache) InvalidateAbove(payloadID string, threshold uint64) {
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
			c.unlinkEntry(blockKey{payloadID: payloadID, blockIdx: blockIdx})
			delete(idxSet, blockIdx)
		}
	}

	if len(idxSet) == 0 {
		delete(c.byFile, payloadID)
	}
}

// Contains checks if a block is present in the cache. Does not promote.
func (c *ReadCache) Contains(payloadID string, blockIdx uint64) bool {
	if c == nil {
		return false
	}

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

	c.mu.RLock()
	_, ok := c.entries[key]
	c.mu.RUnlock()
	return ok
}

// Close clears all cache state. After Close, Get returns miss for all keys.
func (c *ReadCache) Close() {
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
func (c *ReadCache) evictLRU() {
	back := c.lru.Back()
	if back == nil {
		return
	}
	c.removeEntry(back)
}

// unlinkEntry removes an entry from the primary index and LRU list by key.
// Does NOT touch the secondary index (byFile). Must be called under WLock.
func (c *ReadCache) unlinkEntry(key blockKey) {
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
func (c *ReadCache) removeEntry(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	c.unlinkEntry(entry.key)

	// Clean up secondary index.
	if idxSet, ok := c.byFile[entry.key.payloadID]; ok {
		delete(idxSet, entry.key.blockIdx)
		if len(idxSet) == 0 {
			delete(c.byFile, entry.key.payloadID)
		}
	}
}
