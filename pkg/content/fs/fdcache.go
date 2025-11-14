package content

import (
	"container/list"
	"fmt"
	"os"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// FDCache provides an LRU cache for open file descriptors.
type FDCache struct {
	maxSize   int
	mu        sync.Mutex
	cache     map[metadata.ContentID]*list.Element
	lru       *list.List
	fileLocks sync.Map
}

type cacheEntry struct {
	id   metadata.ContentID
	file *os.File
	path string
}

func NewFDCache(maxSize int) *FDCache {
	if maxSize < 1 {
		maxSize = 256
	}
	return &FDCache{
		maxSize: maxSize,
		cache:   make(map[metadata.ContentID]*list.Element),
		lru:     list.New(),
	}
}

func (c *FDCache) Get(id metadata.ContentID) (*os.File, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.cache[id]
	if !exists {
		return nil, false
	}

	c.lru.MoveToFront(elem)
	entry := elem.Value.(*cacheEntry)
	return entry.file, true
}

func (c *FDCache) Put(id metadata.ContentID, file *os.File, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, exists := c.cache[id]; exists {
		c.lru.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		if entry.file != file {
			if err := entry.file.Close(); err != nil {
				// Non-fatal
			}
			entry.file = file
			entry.path = path
		}
		return nil
	}

	if c.lru.Len() >= c.maxSize {
		if err := c.evictLRU(); err != nil {
			return fmt.Errorf("evict LRU: %w", err)
		}
	}

	entry := &cacheEntry{
		id:   id,
		file: file,
		path: path,
	}
	elem := c.lru.PushFront(entry)
	c.cache[id] = elem

	return nil
}

func (c *FDCache) Remove(id metadata.ContentID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.cache[id]
	if !exists {
		return nil
	}

	entry := elem.Value.(*cacheEntry)
	if err := entry.file.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}

	c.lru.Remove(elem)
	delete(c.cache, id)
	c.fileLocks.Delete(id)

	return nil
}

func (c *FDCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error
	for c.lru.Len() > 0 {
		elem := c.lru.Back()
		entry := elem.Value.(*cacheEntry)

		if err := entry.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}

		c.lru.Remove(elem)
		delete(c.cache, entry.id)
	}

	c.fileLocks.Range(func(key, value interface{}) bool {
		c.fileLocks.Delete(key)
		return true
	})

	return firstErr
}

func (c *FDCache) evictLRU() error {
	elem := c.lru.Back()
	if elem == nil {
		return nil
	}

	entry := elem.Value.(*cacheEntry)
	if err := entry.file.Close(); err != nil {
		return fmt.Errorf("close evicted file %s: %w", entry.path, err)
	}

	c.lru.Remove(elem)
	delete(c.cache, entry.id)
	c.fileLocks.Delete(entry.id)

	return nil
}

func (c *FDCache) LockFile(id metadata.ContentID) {
	value, _ := c.fileLocks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
}

func (c *FDCache) UnlockFile(id metadata.ContentID) {
	value, exists := c.fileLocks.Load(id)
	if !exists {
		return
	}
	mu := value.(*sync.Mutex)
	mu.Unlock()
}

func (c *FDCache) Stats() (size int, maxSize int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.maxSize
}
