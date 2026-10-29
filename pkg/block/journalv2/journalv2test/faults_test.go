package journalv2test

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

// TestFailingFailsTheCountedCallsOfItsKeyOnly pins that a Failing rule fails
// only the calls of its op on a file its key matches, only as many as its
// count, and every one with a count of 0. It catches a rule built with the
// key, the count or the error dropped, which would fail calls the test never
// meant to fail or let through the ones it did.
func TestFailingFailsTheCountedCallsOfItsKeyOnly(t *testing.T) {
	s := newSeam(t)
	a := durableFile(t, s, "a.seg", "xx")
	b := durableFile(t, s, "b.tmp", "xx")

	schedule(t, s, Failing(OpWrite, "*.seg", 1, syscall.ENOSPC))
	write(t, b, "B", 0)
	if _, err := a.WriteAt([]byte("A"), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the first write of a.seg returned %v, want ENOSPC", err)
	}
	write(t, a, "A", 1)

	if got := readAt(t, a, 8); got != "xA" {
		t.Fatalf("a.seg holds %q, want %q: the failed write landed, or the next one failed too", got, "xA")
	}

	schedule(t, s, Failing(OpWrite, "*.seg", 0, syscall.EIO))
	for range 3 {
		if _, err := a.WriteAt([]byte("A"), 0); !errors.Is(err, syscall.EIO) {
			t.Fatalf("a write of a.seg under a count of 0 returned %v, want EIO on every call", err)
		}
	}
}

// TestDroppingLosesTheCountedWritesAtACrash pins that a Dropping rule
// reports its write done and the write does not survive a crash, even after
// a sync, while the next write, past its count, does. It catches a rule that
// fails the write instead, or drops more writes than its count.
func TestDroppingLosesTheCountedWritesAtACrash(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxx")

	schedule(t, s, Dropping(OpWrite, "seg", 1))
	if n, err := f.WriteAt([]byte("AB"), 0); n != 2 || err != nil {
		t.Fatalf("a dropped write returned %d, %v; want 2, nil", n, err)
	}
	write(t, f, "CD", 2)
	syncFile(t, f)

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xxCD" {
		t.Fatalf("after a sync and a crash the file holds %q, want %q: only the first write dropped", got, "xxCD")
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

// TestScheduleRefusesABadRuleAndKeepsTheFault pins that Schedule returns the
// schedule's refusal of a rule it cannot run, a fail with no error, an empty
// key or a negative tear, and leaves the fault set before it in place. It
// catches a Schedule that sets a broken fault, or clears the old one, on a
// refusal.
func TestScheduleRefusesABadRuleAndKeepsTheFault(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "x")

	schedule(t, s, Failing(OpWrite, "*", 0, syscall.EIO))
	for _, r := range []Rule{Failing(OpWrite, "*", 0, nil), Dropping(OpWrite, "", 0), Tearing(OpWrite, "*", 0, -1, nil)} {
		if err := s.Schedule(r); err == nil {
			t.Fatalf("Schedule(%+v) returned nil, want the rule refused", r)
		}
	}

	if _, err := f.WriteAt([]byte("A"), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("a write after the refused schedules returned %v, want EIO from the fault set before them", err)
	}
}
