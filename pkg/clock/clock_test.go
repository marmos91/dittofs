package clock

import (
	"testing"
	"time"
)

var _ Clock = System{}

func TestSystemElapsedIsMonotonic(t *testing.T) {
	c := NewSystem()

	first := c.Elapsed()
	if first < 0 || first > time.Second {
		t.Fatalf("Elapsed right after NewSystem = %v, want close to zero", first)
	}

	prev := first
	for range 1000 {
		next := c.Elapsed()
		if next < prev {
			t.Fatalf("Elapsed went backwards: %v after %v", next, prev)
		}
		prev = next
	}
}

func TestSystemAfterFires(t *testing.T) {
	c := NewSystem()
	for _, d := range []time.Duration{0, time.Millisecond} {
		start := c.Elapsed()
		select {
		case <-c.After(d):
			if got := c.Elapsed() - start; got < d {
				t.Errorf("After(%v) fired after %v", d, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("After(%v) did not fire", d)
		}
	}
}
