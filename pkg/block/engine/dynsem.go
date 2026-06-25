package engine

import (
	"context"
	gosync "sync"
)

// dynamicSemaphore is a counting semaphore whose limit can change while slots
// are held. The adaptive upload controller (#1407) resizes it every control
// interval, so the fixed-size golang.org/x/sync/semaphore.Weighted (size set at
// construction) does not fit. Growing the limit wakes blocked Acquire callers
// immediately; shrinking never preempts existing holders — it only withholds
// new slots until in-flight falls below the new limit.
//
// It is also context-aware: Acquire returns ctx.Err() if the context is
// cancelled while waiting, so a failing/cancelled mirror pass does not strand a
// goroutine on a slot that will never free.
type dynamicSemaphore struct {
	mu       gosync.Mutex
	cond     *gosync.Cond
	limit    int
	inflight int
	peak     int // high-water mark of inflight since the last TakePeak
}

func newDynamicSemaphore(limit int) *dynamicSemaphore {
	if limit < 1 {
		limit = 1
	}
	s := &dynamicSemaphore{limit: limit}
	s.cond = gosync.NewCond(&s.mu)
	return s
}

// Acquire blocks until a slot is available or ctx is done. On ctx cancellation
// it returns ctx.Err() without consuming a slot.
func (s *dynamicSemaphore) Acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	// Fast path: a slot is free now — take it without parking or spawning a
	// cancellation watcher. This is the common case in the mirror loop (one
	// Acquire per chunk), so it must not allocate a goroutine per call.
	if s.inflight < s.limit {
		s.inflight++
		if s.inflight > s.peak {
			s.peak = s.inflight
		}
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// Slow path: park on the cond. A watcher wakes us if ctx is cancelled while
	// parked. It broadcasts UNDER s.mu so the wakeup cannot fire in the gap
	// between our ctx re-check and cond.Wait — Wait atomically releases s.mu, so
	// holding the lock around Broadcast serializes it against that window and
	// prevents a lost wakeup. The watcher lives only for this wait.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	for s.inflight >= s.limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.inflight++
	if s.inflight > s.peak {
		s.peak = s.inflight
	}
	return nil
}

// TakePeak returns the high-water mark of in-flight slots since the last call
// and resets it to the current in-flight count. The adaptive controller uses it
// to tell window-limited intervals (peak reached the limit) from app-limited
// ones (peak stayed below it) — see goodputController.observe.
func (s *dynamicSemaphore) TakePeak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peak
	s.peak = s.inflight
	return p
}

// Release returns a slot and wakes waiters. Broadcast (not Signal) under the
// lock: a woken waiter may bail on a cancelled context without consuming the
// freed slot, so waking exactly one risks stranding it — Broadcast lets the
// next live waiter take the slot. Signalling under s.mu serializes it against a
// waiter's ctx-check/Wait window (no lost wakeup).
func (s *dynamicSemaphore) Release() {
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// SetLimit changes the maximum concurrency. Growing wakes blocked waiters;
// shrinking takes effect for future acquires only (current holders run to
// completion).
func (s *dynamicSemaphore) SetLimit(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.limit = n
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Limit returns the current maximum concurrency.
func (s *dynamicSemaphore) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

// InFlight returns the number of currently held slots.
func (s *dynamicSemaphore) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}
