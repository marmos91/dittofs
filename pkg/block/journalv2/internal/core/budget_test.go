package core

import (
	"sync"
	"testing"
	"time"
)

// deadlockGuard is how long a Reclaim whose reclaimer cannot make progress
// takes to be called wedged. It is waited only on the failure path.
const deadlockGuard = 5 * time.Second

// TestDescriptorBudgetDefaultLimitChecksQuarter checks that a budget of limit
// zero resolves to a quarter of the process's open-file limit, clamped to
// [1, 65536]: a default not derived from the process defeats the bound's
// purpose, protecting nothing on a process whose open-file limit is low.
func TestDescriptorBudgetDefaultLimitChecksQuarter(t *testing.T) {
	b := NewDescriptorBudget(0)

	// The default is compared against the platform's own reading of the
	// open-file limit, not recomputed from it, so a budget that resolved the
	// limit from anything else reads as a mismatch.
	cur, known := platformOpenFileLimit()
	if !known {
		t.Log("the open-file limit is unknown here; the default is the fixed fallback")
		want := 1024
		if got := b.Limit(); got != want {
			t.Fatalf("Limit = %d, want the fallback %d where the open-file limit is unknown", got, want)
		}
		return
	}

	want := int(cur / 4)
	if want < 1 {
		want = 1
	}
	if want > 1<<16 {
		want = 1 << 16
	}
	if got := b.Limit(); got != want {
		t.Fatalf("Limit = %d, want %d: a quarter of the open-file limit %d, clamped to [1, %d]",
			got, want, cur, 1<<16)
	}
}

// TestDescriptorBudgetLifetimeDefaultLimit checks that a budget of no explicit
// limit keeps the resolved limit for its lifetime: a limit recomputed per
// call, or reset by a later Take or Give, would let the bound drift while the
// journals hold their descriptors.
func TestDescriptorBudgetLifetimeDefaultLimit(t *testing.T) {
	b := NewDescriptorBudget(0)
	first := b.Limit()
	b.TryTake()
	b.Give()
	if b.Limit() != first {
		t.Fatalf("Limit = %d after a take and a give, want the %d it resolved at construction",
			b.Limit(), first)
	}
}

// TestDescriptorBudgetTakeGiveBoundsSlots checks the slot arithmetic that
// makes the bound hold: a slot is taken while one is open, taking past the
// limit fails without changing the count, and Give returns one. A negative
// limit, the invalid setting refused later by Init and Open, never takes one,
// so nothing can wedge under it.
func TestDescriptorBudgetTakeGiveBoundsSlots(t *testing.T) {
	const limit = 3
	b := NewDescriptorBudget(limit)
	if b.Limit() != limit {
		t.Fatalf("Limit = %d, want %d", b.Limit(), limit)
	}

	for i := 0; i < limit; i++ {
		if !b.TryTake() {
			t.Fatalf("TryTake %d of %d failed, want it to succeed", i+1, limit)
		}
		if got := b.Open(); got != i+1 {
			t.Fatalf("Open = %d after %d takes, want %d", got, i+1, i+1)
		}
	}
	if b.TryTake() {
		t.Fatal("TryTake past the limit succeeded, want it to fail")
	}
	if got := b.Open(); got != limit {
		t.Fatalf("Open = %d after a refused take, want %d", got, limit)
	}

	b.Give()
	if got := b.Open(); got != limit-1 {
		t.Fatalf("Open = %d after Give, want %d", got, limit-1)
	}
	if !b.TryTake() {
		t.Fatal("TryTake after Give failed, want the returned slot")
	}

	neg := NewDescriptorBudget(-1)
	if neg.TryTake() {
		t.Fatal("TryTake under a negative limit succeeded, want it to always fail")
	}
	if neg.Limit() >= 0 {
		t.Fatalf("Limit = %d, want the invalid value kept as is, below zero", neg.Limit())
	}
	if neg.Open() != 0 {
		t.Fatalf("Open = %d under a negative limit, want 0", neg.Open())
	}
}

// TestDescriptorBudgetGiveBelowZeroPanics checks that Give of a slot nobody
// took fails loudly rather than driving the count below zero, where TryTake
// would admit descriptors again and the bound would silently stop holding.
func TestDescriptorBudgetGiveBelowZeroPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Give below zero did not panic, want the accounting bug to fail loudly")
		}
	}()
	b := NewDescriptorBudget(1)
	b.Give()
}

// TestDescriptorBudgetReclaimReleasesMutex checks that Reclaim releases the
// budget's mutex before it calls a reclaimer: a reclaimer that takes and gives
// a slot itself — what every cache's eviction does — would deadlock
// otherwise. The call is bounded by a guard a clean Reclaim, which holds no
// mutex and does no I/O, reaches in microseconds.
func TestDescriptorBudgetReclaimReleasesMutex(t *testing.T) {
	closed := 0
	b := NewDescriptorBudget(1)
	unregister := b.Register(func() bool {
		if !b.TryTake() { // re-enters the budget under its own mutex, when held
			t.Error("a reclaimer cannot take a slot: the budget's mutex is held across the call")
			return false
		}
		b.Give()
		closed++
		return true
	})
	defer unregister()

	done := make(chan bool, 1)
	go func() { done <- b.Reclaim() }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Reclaim returned false with a reclaimer that closes one, want true")
		}
	case <-time.After(deadlockGuard):
		t.Fatal("Reclaim did not return: the budget's mutex is held across the reclaimer call")
	}
	if closed != 1 {
		t.Fatalf("the reclaimer ran %d times, want once in one Reclaim", closed)
	}
}

// TestDescriptorBudgetReclaimRoundRobin checks that successive Reclaims start
// after the last reclaimer that succeeded, in registration order, so no one
// journal's cache is asked twice before every other one has been asked once:
// a reclaim that always restarted at the first would starve a busy journal's
// cache in favour of a quiet one's.
func TestDescriptorBudgetReclaimRoundRobin(t *testing.T) {
	var mu sync.Mutex
	var order []int
	declines := map[int]bool{0: true} // the first reclaimer holds only dirty or referenced descriptors
	b := NewDescriptorBudget(1)
	for i := 0; i < 3; i++ {
		i := i
		b.Register(func() bool {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return !declines[i]
		})
	}

	for round := 0; round < 3; round++ {
		if !b.Reclaim() {
			t.Fatalf("Reclaim %d failed, want it to reach the second reclaimer, which closes one", round+1)
		}
		mu.Lock()
		got := append([]int(nil), order...)
		mu.Unlock()
		// The declining reclaimer is asked once per Reclaim, then the next
		// one in registration order closes; the next Reclaim resumes from it,
		// so the asks run [0 1] [2] [0 1] [2] and so on — each Reclaim
		// alternates, and the third reclaimer is reached on the second one.
		// A reclaim that restarts at the head asks [0 1] [0 1] [0 1] — the
		// third reclaimer is never asked, which the second Reclaim already
		// catches.
		want := [][]int{{0, 1}, {0, 1, 2}, {0, 1, 2, 0}}
		for i := range want[round] {
			if got[i] != want[round][i] {
				t.Fatalf("round %d asked %v, want %v", round+1, got, want[round])
			}
		}
	}
}

// TestDescriptorBudgetReclaimRefusesWhenNothingCloses checks that Reclaim
// returns false when no reclaimer, or none registered, closes a descriptor:
// a caller that treated that as an available slot would hold more
// descriptors than the bound.
func TestDescriptorBudgetReclaimRefusesWhenNothingCloses(t *testing.T) {
	b := NewDescriptorBudget(1)
	if b.Reclaim() {
		t.Fatal("Reclaim with no reclaimer returned true, want false")
	}
	unregister := b.Register(func() bool { return false })
	defer unregister()
	if b.Reclaim() {
		t.Fatal("Reclaim with a reclaimer that closes nothing returned true, want false")
	}
}

// TestDescriptorBudgetReclaimSkipsUnregistered checks that an unregistered
// reclaimer is no longer asked: a cache that had shut down would otherwise be
// asked to evict descriptors it no longer holds, and its TryTake/Give calls
// from beyond the grave would corrupt the count.
func TestDescriptorBudgetReclaimSkipsUnregistered(t *testing.T) {
	var mu sync.Mutex
	asked := []int{}
	closed := 0
	b := NewDescriptorBudget(1)
	first := b.Register(func() bool {
		mu.Lock()
		asked = append(asked, 0)
		closed++
		mu.Unlock()
		return true
	})
	second := b.Register(func() bool {
		mu.Lock()
		asked = append(asked, 1)
		mu.Unlock()
		return false
	})
	defer second()

	if !b.Reclaim() {
		t.Fatal("Reclaim failed with two reclaimers, want the first to close one")
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != 0 {
		t.Fatalf("the first Reclaim asked %v, want it to stop at the first reclaimer, which closes one", asked)
	}
	mu.Unlock()

	// The first is what shuts down, so unregistering it is what must take it
	// out of the round-robin; the second is what proves the walk went on: it
	// is asked exactly once, as the only one left.
	first()
	if b.Reclaim() {
		t.Fatal("Reclaim succeeded, but the only reclaimer left closes nothing")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 || asked[1] != 1 {
		t.Fatalf("after the shutdown, Reclaim asked %v, want the remaining reclaimer once", asked)
	}
	if closed != 1 {
		t.Fatalf("the unregistered reclaimer closed %d times, want its one before shutdown", closed)
	}
}

// TestDescriptorBudgetReclaimersGauge checks the registry gauge Register adds
// to and its unregister removes from: the count of reclaimers the budget will
// ask, which is what a journal's Shutdown must drop by one.
func TestDescriptorBudgetReclaimersGauge(t *testing.T) {
	b := NewDescriptorBudget(1)
	if got := b.Reclaimers(); got != 0 {
		t.Fatalf("Reclaimers = %d with none registered, want 0", got)
	}
	unregister := b.Register(func() bool { return true })
	if got := b.Reclaimers(); got != 1 {
		t.Fatalf("Reclaimers = %d after one Register, want 1", got)
	}
	second := b.Register(func() bool { return true })
	if got := b.Reclaimers(); got != 2 {
		t.Fatalf("Reclaimers = %d after two Registers, want 2", got)
	}
	unregister()
	if got := b.Reclaimers(); got != 1 {
		t.Fatalf("Reclaimers = %d after one unregister, want 1", got)
	}
	second()
	if got := b.Reclaimers(); got != 0 {
		t.Fatalf("Reclaimers = %d after both unregisters, want 0", got)
	}
}

// TestDescriptorBudgetReclaimResumesAfterUnregister checks that the
// round-robin resumes after the reclaimer that succeeded, not after its
// position in an order that a concurrent unregister has since shifted. The
// reclaimer that succeeds is unregistered, and the next Reclaim must ask the
// second — the one after it in the order where it stood — not the third, which
// is what a cursor resolved against the shrunk order would ask.
func TestDescriptorBudgetReclaimResumesAfterUnregister(t *testing.T) {
	var mu sync.Mutex
	var order []int
	b := NewDescriptorBudget(1)
	var unregisters []func()
	for i := 0; i < 3; i++ {
		i := i
		unregisters = append(unregisters, b.Register(func() bool {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return true
		}))
	}

	if !b.Reclaim() {
		t.Fatal("the first Reclaim failed, want the first reclaimer to close one")
	}
	mu.Lock()
	first := append([]int(nil), order...)
	mu.Unlock()
	if len(first) != 1 || first[0] != 0 {
		t.Fatalf("the first Reclaim asked %v, want it to stop at the first reclaimer, which closes one", first)
	}

	// The first reclaimer is the one that shut down: unregistering it shifts
	// the order to [1 2], and a cursor stored as an index into the old order
	// would now point past the second straight at the third.
	unregisters[0]()

	if !b.Reclaim() {
		t.Fatal("the second Reclaim failed, want a reclaimer to close one")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := order; len(got) != 2 || got[1] != 1 {
		t.Fatalf("after the unregister, Reclaim asked %v, want the second reclaimer next: the round-robin resumes after the reclaimer that succeeded", got)
	}
}
