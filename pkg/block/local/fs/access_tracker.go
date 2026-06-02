package fs

import (
	"sync"
	"time"
)

// accessTracker maintains per-file last-access times for eviction policy enforcement.
//
// Access times are batched in memory (no synchronous I/O per operation). The tracker
// is updated on read and write paths via Touch(), and queried during eviction to
// determine which files are oldest-accessed (LRU) or have expired (TTL).
//
// Thread-safe: uses RWMutex for concurrent read-heavy access (Touch is write, but
// eviction queries are read).
type accessTracker struct {
	mu    sync.RWMutex
	times map[string]time.Time // payloadID -> lastAccess
}

// newAccessTracker creates a new empty access tracker.
func newAccessTracker() *accessTracker {
	return &accessTracker{
		times: make(map[string]time.Time),
	}
}

// Touch updates the last-access time for a file to now.
// Called on read and write paths.
func (at *accessTracker) Touch(payloadID string) {
	at.mu.Lock()
	at.times[payloadID] = time.Now()
	at.mu.Unlock()
}

// TouchIfAbsent seeds the access time for a file only if not already tracked.
// Used on local store read hits to restore access times from FileBlock.LastAccess
// after a restart, without overwriting times established by active I/O.
func (at *accessTracker) TouchIfAbsent(payloadID string, t time.Time) {
	at.mu.Lock()
	if _, ok := at.times[payloadID]; !ok {
		at.times[payloadID] = t
	}
	at.mu.Unlock()
}

// LastAccess returns the last-access time for a file.
// Returns zero time if the file has never been accessed via Touch.
func (at *accessTracker) LastAccess(payloadID string) time.Time {
	at.mu.RLock()
	t := at.times[payloadID]
	at.mu.RUnlock()
	return t
}

// Remove deletes the access time entry for a file.
// Called when a file is deleted or evicted from memory.
func (at *accessTracker) Remove(payloadID string) {
	at.mu.Lock()
	delete(at.times, payloadID)
	at.mu.Unlock()
}

// FileAccessTimes returns a snapshot (copy) of all file access times.
// Used during eviction to sort files by access time without holding the lock
// during potentially slow eviction operations.
func (at *accessTracker) FileAccessTimes() map[string]time.Time {
	at.mu.RLock()
	defer at.mu.RUnlock()
	result := make(map[string]time.Time, len(at.times))
	for k, v := range at.times {
		result[k] = v
	}
	return result
}
