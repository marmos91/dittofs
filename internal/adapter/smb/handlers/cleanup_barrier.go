package handlers

import "sync"

// alreadyIdle is the channel a barrier with nothing outstanding hands out.
var alreadyIdle = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// cleanupBarrier counts in-progress session cleanups and lets waiters block
// until the count is back to zero. The zero value is ready to use.
//
// A sync.WaitGroup cannot serve here: arming happens on whichever connection
// goroutine just lost its socket, while the waiter is a SESSION_SETUP on an
// unrelated connection, so Add routinely runs from zero with a Wait already in
// flight. That is the one WaitGroup usage the race detector rejects, and the
// reason it rejects it is exactly the failure that matters — a Wait that has
// already sampled the counter as zero returns even though the Add happened
// first, letting the new session through before the old one's handles are gone.
//
// The idle channel carries the state instead: it is open while cleanups are
// outstanding and closed while there are none, so a waiter that reads it after
// an Add is guaranteed to see the open channel.
type cleanupBarrier struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

// Add registers count more in-progress cleanups.
func (b *cleanupBarrier) Add(count int) {
	if count <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 {
		b.idle = make(chan struct{})
	}
	b.n += count
}

// Done retires one in-progress cleanup, releasing waiters when it was the last.
func (b *cleanupBarrier) Done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 {
		return
	}
	b.n--
	if b.n == 0 {
		close(b.idle)
	}
}

// Idle returns a channel that is closed once no cleanup is outstanding.
func (b *cleanupBarrier) Idle() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idle == nil {
		return alreadyIdle
	}
	return b.idle
}
