package segment

// MutexHeld reports whether c's mutex is held, for the leaf-lock check: a
// storage call made under it is a lock held across I/O.
func MutexHeld(c *Descriptors) bool {
	if c.mu.TryLock() {
		c.mu.Unlock()
		return false
	}
	return true
}
