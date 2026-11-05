package journaltest

import (
	"testing"
	"time"
)

// TestNewStopwatchClockStartsAtTheUnixOrigin pins the double the tree's
// internal tests take from this package: its wall time starts at the Unix
// origin — a stable value a check can name — and its Elapsed starts at
// zero, so the ages a check advances are whole from the first.
func TestNewStopwatchClockStartsAtTheUnixOrigin(t *testing.T) {
	c := NewStopwatchClock()
	if got, want := c.Now(), time.Unix(0, 0); !got.Equal(want) {
		t.Fatalf("the stopwatch clock's Now = %v, want %v", got, want)
	}
	if got := c.Elapsed(); got != 0 {
		t.Fatalf("the stopwatch clock's Elapsed = %d, want 0", got)
	}
	c.Advance(time.Second)
	if got, want := c.Now(), time.Unix(1, 0); !got.Equal(want) {
		t.Fatalf("the stopwatch clock's Now after a 1 s advance = %v, want %v", got, want)
	}
	if got := c.Elapsed(); got != time.Second {
		t.Fatalf("the stopwatch clock's Elapsed after a 1 s advance = %d, want 1 s", got)
	}
	// The double answers the driven face a rig keeps it by: an advance arms
	// nothing until After is called, and Pending counts the armed waits.
	var driven DrivenClock = c
	if driven.Pending() != 0 {
		t.Fatalf("the stopwatch clock holds %d armed waits, want none", driven.Pending())
	}
	armed := driven.After(time.Second)
	if driven.Pending() != 1 {
		t.Fatalf("the stopwatch clock holds %d armed waits after After, want one", driven.Pending())
	}
	driven.Advance(time.Second)
	select {
	case <-armed:
	default:
		t.Fatal("the 1 s wait did not fire when the advance reached it")
	}
	if driven.Pending() != 0 {
		t.Fatalf("the stopwatch clock holds %d armed waits after firing, want none", driven.Pending())
	}
}
