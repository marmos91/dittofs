package sim

import (
	"sync"
	"testing"
	"time"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fired(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestAfterNeverFiresWithoutAdvance(t *testing.T) {
	c := NewTestClock(start)
	ch := c.After(time.Nanosecond)

	// A real-time wait is the only way to catch an After backed by a real
	// timer: one armed for this nanosecond would have fired well within it.
	time.Sleep(20 * time.Millisecond)

	if fired(ch) {
		t.Fatal("After fired with no Advance")
	}

	if got := c.Elapsed(); got != 0 {
		t.Fatalf("Elapsed = %v with no Advance, want 0", got)
	}

	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v with no Advance, want %v", got, start)
	}
}

func TestAdvanceFiresExactlyTheTimersDue(t *testing.T) {
	c := NewTestClock(start)
	ten := c.After(10 * time.Second)
	twenty := c.After(20 * time.Second)

	c.Advance(15 * time.Second)
	if !fired(ten) {
		t.Fatal("a timer due at 10 s did not fire at 15 s")
	}
	if fired(twenty) {
		t.Fatal("a timer due at 20 s fired at 15 s")
	}

	// The deadline counts from the After call, not from the clock's origin.
	late := c.After(10 * time.Second) // due at 25 s

	c.Advance(5 * time.Second)
	if !fired(twenty) {
		t.Fatal("a timer due at 20 s did not fire on reaching 20 s exactly")
	}
	if fired(late) {
		t.Fatal("a 10 s timer armed at 15 s fired at 20 s")
	}

	c.Advance(5 * time.Second)
	if !fired(late) {
		t.Fatal("a 10 s timer armed at 15 s did not fire at 25 s")
	}
}

func TestSetRateScalesAdvance(t *testing.T) {
	c := NewTestClock(start)
	at104 := c.After(104 * time.Second)
	at105 := c.After(105 * time.Second)
	at106 := c.After(106 * time.Second)

	c.SetRate(1.05)
	c.Advance(100 * time.Second)

	if !fired(at104) || !fired(at105) {
		t.Fatal("at rate 1.05, an Advance of 100 s did not fire the timers due by 105 s")
	}
	if fired(at106) {
		t.Fatal("at rate 1.05, an Advance of 100 s fired a timer due at 106 s")
	}

	if got := c.Elapsed(); got != 105*time.Second {
		t.Fatalf("Elapsed = %v, want 105s", got)
	}
	if got := c.Now(); !got.Equal(start.Add(105 * time.Second)) {
		t.Fatalf("Now = %v, want start + 105s", got)
	}

	// A rate applies to later Advances only, never to time already passed.
	c.SetRate(1)
	c.Advance(time.Second)
	if got := c.Elapsed(); got != 106*time.Second {
		t.Fatalf("Elapsed after a further 1 s at rate 1 = %v, want 106s", got)
	}
}

func TestSetRateLosesNoNanosecond(t *testing.T) {
	// 3 s at 0.7 is just under 2.1 s in floating point.
	c := NewTestClock(start)
	ch := c.After(2100 * time.Millisecond)

	c.SetRate(0.7)
	c.Advance(3 * time.Second)

	if !fired(ch) {
		t.Fatalf("at rate 0.7, an Advance of 3 s reached %v and missed a timer due at 2.1 s", c.Elapsed())
	}
}

func TestAfterZeroOrLessIsAlreadyClosed(t *testing.T) {
	c := NewTestClock(start)

	if !fired(c.After(0)) {
		t.Fatal("After(0) returned a channel not yet closed")
	}
	if !fired(c.After(-1)) {
		t.Fatal("After(-1) returned a channel not yet closed")
	}
}

func TestTimerArmedByAWokenGoroutineWaitsForTheNextAdvance(t *testing.T) {
	c := NewTestClock(start)
	first := c.After(5 * time.Second)

	armed := make(chan (<-chan struct{}))
	go func() {
		<-first
		armed <- c.After(time.Nanosecond)
	}()

	// An Advance that stepped through deadlines and let woken goroutines run
	// would reach 5 s, see the new timer due at 5 s + 1 ns, and fire it well
	// before 10 s.
	c.Advance(10 * time.Second)
	second := <-armed

	if fired(second) {
		t.Fatal("a timer armed by a goroutine the Advance woke fired within that Advance")
	}

	c.Advance(time.Nanosecond)
	if !fired(second) {
		t.Fatal("a timer armed after an Advance did not fire on the next one")
	}
}

func TestMisusePanics(t *testing.T) {
	cases := map[string]func(c *TestClock){
		"Advance(-1)": func(c *TestClock) { c.Advance(-1) },
		"SetRate(0)":  func(c *TestClock) { c.SetRate(0) },
		"SetRate(-1)": func(c *TestClock) { c.SetRate(-1) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", name)
				}
			}()
			f(NewTestClock(start))
		})
	}
}

// TestConcurrentAfterAndAdvance guards against data races only when run under
// -race; without it, an unlocked clock usually passes.
func TestConcurrentAfterAndAdvance(t *testing.T) {
	c := NewTestClock(start)

	const n = 64
	chans := make([]<-chan struct{}, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			chans[i] = c.After(time.Duration(i+1) * time.Second)
		})
	}
	wg.Go(func() {
		for range n {
			c.Advance(time.Millisecond)
		}
	})
	wg.Wait()

	c.Advance(n * time.Second)
	for i, ch := range chans {
		if !fired(ch) {
			t.Fatalf("timer %d did not fire once the clock passed every deadline", i)
		}
	}
}

func TestPendingCountsOpenTimers(t *testing.T) {
	c := NewTestClock(start)
	c.After(time.Second)
	c.After(2 * time.Second)
	c.After(0) // closed at once, never pending

	if got := c.Pending(); got != 2 {
		t.Fatalf("Pending = %d with two timers set, want 2", got)
	}

	c.Advance(time.Second)
	if got := c.Pending(); got != 1 {
		t.Fatalf("Pending = %d after one timer fired, want 1", got)
	}
}
