// Package lock provides directory lease management and change notification.
//
// This file implements the DirChangeNotifier interface and the recently-broken
// cache that prevents lease grant storms on frequently-changed directories.
//
// When a directory entry changes (add, remove, rename), all directory leases
// on that directory are broken to None (except those held by the originating client).
// The recently-broken cache then blocks new directory lease grants for a short
// TTL window to prevent immediate re-grant followed by another break.
//
// Reference: MS-SMB2 3.3.4.7 Object Store Indicates a Lease Break
package lock

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// Default TTL for the recently-broken cache.
const defaultRecentlyBrokenTTL = 5 * time.Second

// DirChangeType describes the type of directory entry change.
type DirChangeType int

const (
	// DirChangeAddEntry indicates a new entry was added to the directory.
	DirChangeAddEntry DirChangeType = iota

	// DirChangeRemoveEntry indicates an entry was removed from the directory.
	DirChangeRemoveEntry

	// DirChangeRenameEntry indicates an entry was renamed within the directory.
	DirChangeRenameEntry
)

// String returns a human-readable name for the directory change type.
func (d DirChangeType) String() string {
	switch d {
	case DirChangeAddEntry:
		return "add"
	case DirChangeRemoveEntry:
		return "remove"
	case DirChangeRenameEntry:
		return "rename"
	default:
		return "unknown"
	}
}

// DirChangeNotifier is notified when directory entries change.
//
// Protocol adapters or metadata services call OnDirChange when a directory's
// contents are modified. This triggers directory lease breaks for all clients
// except the originator, enabling cache coherency for directory listings.
type DirChangeNotifier interface {
	// OnDirChange is called when a directory entry changes.
	//
	// Parameters:
	//   - parentHandle: The file handle of the directory that changed
	//   - changeType: The type of change (add, remove, rename)
	//   - originClientID: The client that caused the change (excluded from breaks)
	OnDirChange(parentHandle FileHandle, changeType DirChangeType, originClientID string)
}

// Verify Manager satisfies DirChangeNotifier at compile time.
var _ DirChangeNotifier = (*Manager)(nil)

// recentlyBrokenCache tracks directories that have had their leases broken recently.
//
// This prevents grant-break storms on busy directories. When a directory lease
// is broken, the directory is marked in this cache for a configurable TTL.
// During that TTL, new directory lease requests are denied (return None).
type recentlyBrokenCache struct {
	mu      sync.RWMutex
	entries map[string]time.Time
	ttl     time.Duration
}

// newRecentlyBrokenCache creates a new recently-broken cache with the given TTL.
func newRecentlyBrokenCache(ttl time.Duration) *recentlyBrokenCache {
	return &recentlyBrokenCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
}

// IsRecentlyBroken returns true if the directory was recently broken and
// should not have new directory leases granted.
func (c *recentlyBrokenCache) IsRecentlyBroken(handleKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	ts, ok := c.entries[handleKey]
	if !ok {
		return false
	}
	if time.Since(ts) > c.ttl {
		delete(c.entries, handleKey)
		return false
	}
	return true
}

// Mark records that a directory lease was broken at the current time.
func (c *recentlyBrokenCache) Mark(handleKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[handleKey] = time.Now()

	// Lazy cleanup: remove expired entries when we add new ones
	c.cleanupLocked()
}

// cleanupLocked removes expired entries. Must be called with mu held for write.
func (c *recentlyBrokenCache) cleanupLocked() {
	now := time.Now()
	for key, ts := range c.entries {
		if now.Sub(ts) > c.ttl {
			delete(c.entries, key)
		}
	}
}

// OnDirChange handles directory entry changes by breaking directory leases.
//
// For each directory lease on parentHandle (excluding those owned by originClientID),
// a break to LeaseStateNone is dispatched via BreakCallbacks.OnOpLockBreak.
// The directory is then marked in the recently-broken cache.
func (lm *Manager) OnDirChange(parentHandle FileHandle, changeType DirChangeType, originClientID string) {
	handleKey := string(parentHandle)

	// Collect directory leases to break (under read lock)
	lm.mu.RLock()
	locks := lm.unifiedLocks[handleKey]

	var toBreak []*UnifiedLock
	for _, lock := range locks {
		if lock.Lease == nil || !lock.Lease.IsDirectory {
			continue // Skip non-directory leases
		}
		if lock.Owner.ClientID == originClientID {
			continue // Skip originator
		}
		toBreak = append(toBreak, lock)
	}
	lm.mu.RUnlock()

	if len(toBreak) == 0 {
		return
	}

	logger.Debug("OnDirChange: breaking directory leases",
		"parentHandle", handleKey,
		"changeType", changeType.String(),
		"originClient", originClientID,
		"leaseCount", len(toBreak))

	// Dispatch breaks outside the lock
	for _, lock := range toBreak {
		lm.dispatchOpLockBreak(handleKey, lock, LeaseStateNone)
	}

	// Mark directory as recently broken
	if lm.recentlyBroken != nil {
		lm.recentlyBroken.Mark(handleKey)
	}
}
