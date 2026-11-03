package segment

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// MutexHeld reports whether c's mutex is held, for the leaf-lock check: a
// storage call made under it is a lock held across I/O.
func MutexHeld(c *Descriptors) bool {
	if c.mu.TryLock() {
		c.mu.Unlock()
		return false
	}
	return true
}

// EntryCount reports how many entries c's map holds, for checks that the map
// stays bounded by the live segments: an entry no Forget can drop leaks the
// map, one stale id at a time.
func EntryCount(c *Descriptors) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Refs reports the references outstanding on a segment's entry, for sequencing
// a concurrent call's arrival deterministically: the count rises under the
// same mutex the call's own steps serialize with.
func Refs(c *Descriptors, id core.SegmentID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[id].refs
}

// EntryGone reports whether Close(id) has started, for sequencing a drain
// against an in-flight open without an acquire of the draining segment: an
// acquire before the drain starts would park on the open it must not.
func EntryGone(c *Descriptors, id core.SegmentID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[id] != nil && c.entries[id].gone
}

// WaitArmed reports whether Shutdown's wait is armed: its signal channel is
// set, after the wait has been taken back against a total a reclaimer's
// reference moved. Checks that the wait loops rather than answering one
// signal read it.
func WaitArmed(c *Descriptors) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shutDone != nil
}
