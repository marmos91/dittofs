package journalv2test

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// within waits, in real time, for ch to be closed or to deliver.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen", what)
		var zero T
		return zero
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

// aside makes a seam call on another goroutine and waits for it in real time,
// so a hold that catches the call by mistake fails the test instead of
// parking the test's own goroutine, which no cleanup can release.
func aside(t *testing.T, what string, call func() error) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- call() }()
	if err := within(t, done, what); err != nil {
		t.Fatalf("%s: %v", what, err)
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

	h := s.Hold(t, OpSync, "seg")
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
	h := s.Hold(t, OpWrite, "seg")

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, _ = f.WriteAt([]byte("x"), int64(i)) })
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

	h := s.Hold(t, OpSync, "seg-*")
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

	h := s.Hold(t, OpSync, "*")
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

	h := s.Hold(t, OpSync, "seg")
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

	h := s.Hold(t, OpSync, "seg")
	done := goSync(f)
	within(t, h.Reached(), "the Sync reaching the hold")

	crash(t, s)
	var g storage.File
	aside(t, "an Open of seg, another op, returning", func() (err error) {
		g, err = s.Open("seg")
		return err
	})
	aside(t, "a write of seg, another op, returning", func() error {
		_, err := g.WriteAt([]byte("bbbb"), 0)
		return err
	})

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
				_, _ = f.WriteAt([]byte("x"), 0)
				_ = f.Sync()
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			h := s.Hold(t, OpSync, "*")
			h.Waiting()
			h.Release()
		}
	})

	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()
	within(t, all, "every call returning")
}

func TestAHoldIsReleasedWhenItsTestEnds(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	done := make(chan error, 1)

	t.Run("holds and never releases", func(t *testing.T) {
		h := s.Hold(t, OpSync, "seg")
		go func() { done <- f.Sync() }()
		within(t, h.Reached(), "the Sync reaching the hold")
	})

	if err := within(t, done, "the Sync held by a finished subtest returning"); err != nil {
		t.Fatalf("the Sync released when its subtest ended returned %v", err)
	}
}

func TestHoldRefusesAnEmptyKey(t *testing.T) {
	s := newSeam(t)

	defer func() {
		if recover() == nil {
			t.Fatal("Hold accepted the empty key")
		}
	}()
	s.Hold(t, OpSync, "")
}

func TestAHoldOnStarHoldsEveryKey(t *testing.T) {
	for _, key := range []string{"seg-1", "", "shard/1", "a\x2fb", "\xff/\xfe"} {
		s := newSeam(t)
		h := s.Hold(t, "point", "*")

		held := make(chan struct{})
		go func() { s.hold("point", key); close(held) }()

		select {
		case <-h.Reached():
		case <-held:
			t.Fatalf("a call on %q passed a hold on \"*\"", key)
		case <-time.After(2 * time.Second):
			t.Fatalf("a call on %q neither reached nor passed a hold on \"*\"", key)
		}
		h.Release()
	}
}

func TestAHoldOnStarHoldsEveryOp(t *testing.T) {
	for _, op := range []string{OpSync, OpWrite, OpSyncDir, "point"} {
		s := newSeam(t)
		h := s.Hold(t, "*", "*")

		held := make(chan struct{})
		go func() { s.hold(op, "seg"); close(held) }()

		select {
		case <-h.Reached():
		case <-held:
			t.Fatalf("a %s call passed a hold on op \"*\"", op)
		case <-time.After(2 * time.Second):
			t.Fatalf("a %s call neither reached nor passed a hold on op \"*\"", op)
		}
		h.Release()
	}
}

func TestAHoldOnSyncDoesNotHoldSyncDir(t *testing.T) {
	s := newSeam(t)
	h := s.Hold(t, OpSync, "*")
	defer h.Release()

	aside(t, "a SyncDir under a hold on sync returning", s.SyncDir)
	if h.Waiting() != 0 {
		t.Fatalf("Waiting = %d after a SyncDir under a hold on sync, want 0", h.Waiting())
	}
}

func TestHoldRefusesAnEmptyOp(t *testing.T) {
	s := newSeam(t)

	defer func() {
		if recover() == nil {
			t.Fatal("Hold accepted the empty op")
		}
	}()
	s.Hold(t, "", "*")
}

func TestAHoldOnTheFirstCallLetsLaterOnesPass(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	h := s.HoldFirst(t, OpWrite, "seg")

	first := make(chan error, 1)
	go func() { _, err := f.WriteAt([]byte("a"), 0); first <- err }()
	within(t, h.Reached(), "the first write reaching the hold")

	aside(t, "a second write, made while the first is held", func() error {
		_, err := f.WriteAt([]byte("b"), 1)
		return err
	})
	if returned(first) || h.Waiting() != 1 {
		t.Fatalf("the first write returned or is not held: Waiting = %d", h.Waiting())
	}

	h.Release()
	if err := within(t, first, "the released first write returning"); err != nil {
		t.Fatal(err)
	}
}
