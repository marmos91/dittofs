package handlers

import (
	"sync"
	"testing"
)

func isIdle(b *cleanupBarrier) bool {
	select {
	case <-b.Idle():
		return true
	default:
		return false
	}
}

func TestCleanupBarrier(t *testing.T) {
	var b cleanupBarrier

	if !isIdle(&b) {
		t.Fatal("zero-value barrier should be idle")
	}
	if b.Add(0); !isIdle(&b) {
		t.Fatal("Add(0) should leave the barrier idle")
	}

	// A waiter that took the channel before the last Done must still be woken.
	b.Add(2)
	held := b.Idle()
	b.Add(1)
	if isIdle(&b) {
		t.Fatal("barrier reported idle with cleanups outstanding")
	}
	for i := 0; i < 3; i++ {
		b.Done()
	}
	select {
	case <-held:
	default:
		t.Fatal("channel captured before the last Done was not closed")
	}
	if !isIdle(&b) {
		t.Fatal("barrier should be idle once every cleanup is retired")
	}

	// Re-arming after going idle hands out a fresh, open channel.
	b.Add(1)
	if isIdle(&b) {
		t.Fatal("re-armed barrier reported idle")
	}
	b.Done()

	// An unbalanced Done must not double-close the idle channel.
	b.Done()
	if !isIdle(&b) {
		t.Fatal("barrier should stay idle after an unbalanced Done")
	}
}

func TestCleanupBarrierConcurrentAddDone(t *testing.T) {
	var b cleanupBarrier
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Add(1)
			_ = isIdle(&b)
			b.Done()
		}()
	}
	wg.Wait()

	if !isIdle(&b) {
		t.Fatal("barrier should be idle after every Add was matched by a Done")
	}
}
