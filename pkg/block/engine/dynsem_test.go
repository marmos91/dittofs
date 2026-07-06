package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// dynamicSemaphore is the concurrency primitive the adaptive upload controller
// resizes at runtime. golang.org/x/sync/semaphore fixes its size at
// construction, so the syncer needs one whose limit can grow and shrink while
// slots are held. These tests pin the contract independently of the syncer.

func TestDynamicSemaphore_BlocksAtLimit(t *testing.T) {
	s := newDynamicSemaphore(2)
	ctx := context.Background()
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if s.InFlight() != 2 {
		t.Fatalf("InFlight = %d, want 2", s.InFlight())
	}

	// Third acquire must block until a slot frees.
	acquired := make(chan struct{})
	go func() {
		_ = s.Acquire(ctx)
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("third Acquire returned while at limit")
	case <-time.After(50 * time.Millisecond):
	}

	s.Release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not unblock after Release")
	}
}

func TestDynamicSemaphore_GrowUnblocksWaiter(t *testing.T) {
	s := newDynamicSemaphore(1)
	ctx := context.Background()
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	go func() {
		_ = s.Acquire(ctx)
		close(acquired)
	}()
	// Waiter is blocked at limit 1.
	select {
	case <-acquired:
		t.Fatal("Acquire returned before grow")
	case <-time.After(50 * time.Millisecond):
	}

	// Raising the limit must wake the waiter without any Release.
	s.SetLimit(2)
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("grow did not unblock the waiter")
	}
}

func TestDynamicSemaphore_ShrinkHoldsNewAcquires(t *testing.T) {
	s := newDynamicSemaphore(4)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := s.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Shrink below the in-flight count. Existing holders are not preempted, but
	// no new slot may be handed out until in-flight drops below the new limit.
	s.SetLimit(2)
	if s.Limit() != 2 {
		t.Fatalf("Limit = %d, want 2", s.Limit())
	}

	acquired := make(chan struct{})
	go func() {
		_ = s.Acquire(ctx)
		close(acquired)
	}()
	// 4 in-flight, limit 2: releasing one (→3) must NOT admit the waiter.
	s.Release()
	select {
	case <-acquired:
		t.Fatal("Acquire admitted while in-flight (3) still above limit (2)")
	case <-time.After(50 * time.Millisecond):
	}
	// Release two more (→1, below limit 2): now the waiter may proceed.
	s.Release()
	s.Release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not proceed after in-flight fell below limit")
	}
}

func TestDynamicSemaphore_TakePeakTracksHighWater(t *testing.T) {
	s := newDynamicSemaphore(8)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Peak reached 5 even though we now release down to 2.
	s.Release()
	s.Release()
	s.Release()
	if p := s.TakePeak(); p != 5 {
		t.Fatalf("TakePeak = %d, want high-water 5", p)
	}
	// After TakePeak the high-water resets to the current in-flight (2).
	if p := s.TakePeak(); p != 2 {
		t.Fatalf("TakePeak after reset = %d, want current in-flight 2", p)
	}
}

func TestDynamicSemaphore_AcquireRespectsContext(t *testing.T) {
	s := newDynamicSemaphore(1)
	if err := s.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Acquire(ctx) }()
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Acquire returned nil after context cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return after context cancel")
	}
}

func TestDynamicSemaphore_ConcurrentNeverExceedsLimit(t *testing.T) {
	const limit = 5
	s := newDynamicSemaphore(limit)
	ctx := context.Background()
	var (
		mu      sync.Mutex
		inFlt   int
		maxSeen int
		wg      sync.WaitGroup
	)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Acquire(ctx); err != nil {
				return
			}
			mu.Lock()
			inFlt++
			if inFlt > maxSeen {
				maxSeen = inFlt
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inFlt--
			mu.Unlock()
			s.Release()
		}()
	}
	wg.Wait()
	if maxSeen > limit {
		t.Fatalf("observed %d concurrent holders, limit was %d", maxSeen, limit)
	}
}
