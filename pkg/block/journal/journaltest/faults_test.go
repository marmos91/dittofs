package journaltest

import (
	"errors"
	"syscall"
	"testing"
)

// schedule sets rules on s through Schedule, failing the test if it refuses
// them.
func schedule(t *testing.T, s *Seam, rules ...Rule) {
	t.Helper()

	if err := s.Schedule(rules...); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
}

// TestTearingCutsTheCountedWriteAfterNBytes pins that a Tearing rule with an
// error is a short write: only its first n bytes land, it returns n and the
// error, and the next write, past its count, lands whole. It catches a rule
// that drops n or the error, or tears more writes than its count.
func TestTearingCutsTheCountedWriteAfterNBytes(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxx")

	schedule(t, s, Tearing(OpWrite, "seg", 1, 2, syscall.EIO))
	if n, err := f.WriteAt([]byte("ABCD"), 0); n != 2 || !errors.Is(err, syscall.EIO) {
		t.Fatalf("a write torn at 2 with EIO returned %d, %v; want 2 and EIO", n, err)
	}
	write(t, f, "EF", 4)

	if got := readAt(t, f, 8); got != "ABxxEF" {
		t.Fatalf("the file holds %q, want %q: the torn write's first 2 bytes and the next write whole", got, "ABxxEF")
	}
}

// TestSilentTearingKeepsNBytesAtACrash pins that a Tearing rule with a nil
// error reports the write whole and lets the file serve it whole, but keeps
// only its first n bytes through a sync and a crash. It catches a rule that
// turns a silent tear into a short write, or into no tear at all.
func TestSilentTearingKeepsNBytesAtACrash(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxx")

	schedule(t, s, Tearing(OpWrite, "seg", 1, 2, nil))
	if n, err := f.WriteAt([]byte("ABCD"), 0); n != 4 || err != nil {
		t.Fatalf("a silently torn write returned %d, %v; want 4, nil", n, err)
	}

	if got := readAt(t, f, 8); got != "ABCDxx" {
		t.Fatalf("the file serves %q before the crash, want the whole write %q", got, "ABCDxx")
	}

	syncFile(t, f)
	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "ABxxxx" {
		t.Fatalf("after a sync and a crash the file holds %q, want only the first 2 bytes of the write: %q", got, "ABxxxx")
	}
}
