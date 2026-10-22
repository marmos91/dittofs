package journalv2test

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"
)

// within waits, in real time, for ch to be closed or to deliver.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen", what)
		panic("unreachable")
	}
}

// returned reports whether ch has delivered, without waiting.
func returned[T any](ch <-chan T) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// waitingReaches waits, in real time, until h holds n calls.
func waitingReaches(t *testing.T, h *Hold, n int) {
	t.Helper()

	for deadline := time.Now().Add(2 * time.Second); h.Waiting() != n; {
		if time.Now().After(deadline) {
			t.Fatalf("Waiting stayed at %d, want %d", h.Waiting(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func goSync(f interface{ Sync() error }) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f.Sync() }()
	return done
}

func TestAHeldCallBlocksUntilRelease(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	h := s.Hold(OpSync, "seg")
	done := goSync(f)

	within(t, h.Reached(), "reaching the hold")
	if returned(done) || h.Waiting() != 1 {
		t.Fatalf("a held Sync returned or is not counted: Waiting = %d", h.Waiting())
	}

	h.Release()
	if err := within(t, done, "the released Sync returning"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitingCountsTheBlockedCalls(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	h := s.Hold(OpWrite, "seg")

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { f.WriteAt([]byte("x"), int64(i)) })
	}

	waitingReaches(t, h, n)

	h.Release()
	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()
	within(t, all, "every released write returning")

	if got := h.Waiting(); got != 0 {
		t.Fatalf("Waiting = %d after every held call returned, want 0", got)
	}
}

func TestOnlyMatchingCallsAreHeld(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg-1")
	g := create(t, s, "other")

	h := s.Hold(OpSync, "seg-*")
	defer h.Release()

	wrote := make(chan error, 1)
	go func() { _, err := f.WriteAt([]byte("x"), 0); wrote <- err }()
	if err := within(t, wrote, "a write of seg-1, another op, returning"); err != nil {
		t.Fatal(err)
	}
	if err := within(t, goSync(g), "a sync of other, another file, returning"); err != nil {
		t.Fatal(err)
	}

	done := goSync(f)
	within(t, h.Reached(), "a sync of seg-1 reaching the hold")
	if returned(done) {
		t.Fatal("a sync of seg-1 passed a hold on seg-*")
	}
}

func TestAReleasedHoldLetsLaterCallsPass(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	h := s.Hold(OpSync, "*")
	h.Release()
	h.Release() // harmless

	if err := within(t, goSync(f), "a Sync after the release returning"); err != nil {
		t.Fatal(err)
	}
}

func TestAHeldCallReachesTheHoldBeforeTheFault(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	failOn(t, s, OpSync)

	h := s.Hold(OpSync, "seg")
	done := goSync(f)

	within(t, h.Reached(), "a failing Sync reaching the hold")
	if returned(done) {
		t.Fatal("a held Sync returned before its release")
	}

	h.Release()
	wantEIO(t, OpSync, within(t, done, "the released Sync returning"))
}

func TestCrashDuringAHoldLeavesTheNewHandleAlone(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "aaaa")

	h := s.Hold(OpSync, "seg")
	done := goSync(f)
	within(t, h.Reached(), "the Sync reaching the hold")

	crash(t, s)
	g, err := s.Open("seg")
	if err != nil {
		t.Fatal(err)
	}
	write(t, g, "bbbb", 0)

	h.Release()
	if err := within(t, done, "the released Sync returning"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("a Sync held across a crash returned %v, want fs.ErrClosed", err)
	}
	wantDurable(t, s, "seg", new("aaaa"))
}

// TestConcurrentHoldsAndCalls guards against data races only when run under
// -race.
func TestConcurrentHoldsAndCalls(t *testing.T) {
	s := newSeam(t)
	files := make([]interface {
		Sync() error
		WriteAt([]byte, int64) (int, error)
	}, 8)
	for i := range files {
		files[i] = create(t, s, fmt.Sprint("seg-", i))
	}

	var wg sync.WaitGroup
	for _, f := range files {
		wg.Go(func() {
			for range 20 {
				f.WriteAt([]byte("x"), 0)
				f.Sync()
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			h := s.Hold(OpSync, "*")
			h.Waiting()
			h.Release()
		}
	})

	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()
	within(t, all, "every call returning")
}

func TestHoldRefusesAMalformedKey(t *testing.T) {
	s := newSeam(t)

	defer func() {
		if recover() == nil {
			t.Fatal("Hold accepted the malformed key \"seg-[\"")
		}
	}()
	s.Hold(OpSync, "seg-[")
}
