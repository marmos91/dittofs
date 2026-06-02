package fs

import (
	"container/list"
	"os"
	"sync"
)

// fdPool is an LRU pool of open file descriptors for .blk block files.
// Eliminates open+close syscalls per 4KB random write in tryDirectDiskWrite.
type fdPool struct {
	mu      sync.Mutex
	fds     map[string]*fdEntry // blockID -> open fd
	lru     *list.List
	maxSize int
}

type fdEntry struct {
	f       *os.File
	blockID string
	elem    *list.Element
}

const defaultFDPoolSize = 256

func newFDPool(maxSize int) *fdPool {
	if maxSize <= 0 {
		maxSize = defaultFDPoolSize
	}
	return &fdPool{
		fds:     make(map[string]*fdEntry, maxSize),
		lru:     list.New(),
		maxSize: maxSize,
	}
}

// Get returns a pooled fd for blockID, or nil if not in the pool.
// The returned fd is moved to the front of the LRU list.
func (c *fdPool) Get(blockID string) *os.File {
	c.mu.Lock()
	entry, ok := c.fds[blockID]
	if ok {
		c.lru.MoveToFront(entry.elem)
	}
	c.mu.Unlock()
	if ok {
		return entry.f
	}
	return nil
}

// Put adds an fd to the pool. If the pool is full, the least recently
// used fd is evicted and closed.
func (c *fdPool) Put(blockID string, f *os.File) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Already pooled -- update and promote
	if entry, ok := c.fds[blockID]; ok {
		_ = entry.f.Close()
		entry.f = f
		c.lru.MoveToFront(entry.elem)
		return
	}

	// Evict LRU if full
	for c.lru.Len() >= c.maxSize {
		back := c.lru.Back()
		if back == nil {
			break
		}
		victim := back.Value.(*fdEntry)
		_ = victim.f.Close()
		delete(c.fds, victim.blockID)
		c.lru.Remove(back)
	}

	entry := &fdEntry{f: f, blockID: blockID}
	entry.elem = c.lru.PushFront(entry)
	c.fds[blockID] = entry
}

// Evict removes and closes the fd for blockID if present.
func (c *fdPool) Evict(blockID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.fds[blockID]
	if !ok {
		return
	}
	_ = entry.f.Close()
	delete(c.fds, blockID)
	c.lru.Remove(entry.elem)
}

// CloseAll closes all pooled file descriptors.
func (c *fdPool) CloseAll() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, entry := range c.fds {
		_ = entry.f.Close()
	}
	c.fds = make(map[string]*fdEntry)
	c.lru.Init()
}
