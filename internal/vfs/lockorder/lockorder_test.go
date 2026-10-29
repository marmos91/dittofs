package lockorder

import (
	"context"
	"sync"
	"testing"
	"time"
)

var lo, hi, dir = Key{1}, Key{2}, Key{3}

// free reports whether s can be taken at once, releasing it if so.
func free(l *Locks, s Set) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	release, err := l.Take(ctx, s)
	if err != nil {
		return false
	}
	release()
	return true
}

// Two calls naming the same files in opposite orders race 10^4 times: since
// both take them in file-ID order, neither waits for its deadline.
func TestOppositeOrdersNeverWaitForTheDeadline(t *testing.T) {
	var l Locks
	sets := [2]Set{
		{Admit: []Key{lo, hi}, Enter: []Key{hi}, Drain: []Key{lo}, Accept: []Key{lo, hi}},
		{Admit: []Key{hi, lo}, Enter: []Key{lo}, Drain: []Key{hi}, Accept: []Key{hi, lo}},
	}
	var wg sync.WaitGroup
	for _, s := range sets {
		wg.Go(func() {
			for range 10_000 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				release, err := l.Take(ctx, s)
				cancel()
				if err != nil {
					t.Errorf("Take(%v) waited for its deadline: %v", s, err)
					return
				}
				release()
			}
		})
	}
	wg.Wait()
}

// A call queued at fold admission holds no accept lock, and one queued at an
// admission lock holds no fold admission.
func TestAWaitingCallHoldsNothingOfALaterLevel(t *testing.T) {
	var l Locks
	unlock := l.Admission(lo)
	waiting, err := l.Take(context.Background(), Set{Drain: []Key{dir}})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		release, err := l.Take(context.Background(), Set{Admit: []Key{hi}, Enter: []Key{dir}, Accept: []Key{lo}})
		if err == nil {
			release()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if !free(&l, Set{Accept: []Key{lo}}) {
		t.Error("a call waiting at fold admission holds an accept lock")
	}
	if free(&l, Set{Admit: []Key{hi}}) {
		t.Error("a call waiting at fold admission holds none of its admission locks")
	}
	waiting()
	<-done

	go func() {
		release, err := l.Take(context.Background(), Set{Admit: []Key{lo}, Drain: []Key{dir}})
		if err == nil {
			release()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if !free(&l, Set{Enter: []Key{dir}}) {
		t.Error("a call waiting at an admission lock holds a fold admission")
	}
	unlock()
}

// A call that cannot take a fold admission by its deadline releases the
// admission locks it took.
func TestADeadlineReleasesEverythingTaken(t *testing.T) {
	var l Locks
	release, err := l.Take(context.Background(), Set{Drain: []Key{dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Take(ctx, Set{Admit: []Key{lo}, Enter: []Key{dir}}); err == nil {
		t.Fatal("a fold admission held by a drain was taken")
	}
	if !free(&l, Set{Admit: []Key{lo}}) {
		t.Error("a call refused at fold admission kept its admission lock")
	}
}

// A draining admission waiting for the changes in flight closes the
// directory: no new change enters until it has run.
func TestADrainClosesTheDirectory(t *testing.T) {
	var l Locks
	inFlight, err := l.Take(context.Background(), Set{Enter: []Key{dir}})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		release, err := l.Take(context.Background(), Set{Drain: []Key{dir}})
		if err == nil {
			close(drained)
			release()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if free(&l, Set{Enter: []Key{dir}}) {
		t.Error("a change entered a directory a drain waits on")
	}
	inFlight()
	<-drained
	if !free(&l, Set{Enter: []Key{dir}}) {
		t.Error("the directory stayed closed after its drain ran")
	}
}

// A key named twice, or named both to enter and to drain, is taken once,
// exclusive where any was, so the call never waits on itself.
func TestARepeatedKeyIsTakenOnce(t *testing.T) {
	var l Locks
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := l.Take(ctx, Set{Admit: []Key{lo, lo}, Enter: []Key{dir}, Drain: []Key{dir}, Accept: []Key{hi, hi}})
	if err != nil {
		t.Fatalf("a call waited on itself: %v", err)
	}
	if free(&l, Set{Enter: []Key{dir}}) {
		t.Error("a key named to enter and to drain was taken shared")
	}
	release()
	if !free(&l, Set{Admit: []Key{lo}, Drain: []Key{dir}, Accept: []Key{hi}}) {
		t.Error("a release left a lock held")
	}
}

// A drain that gives up waiting for the changes in flight reopens the
// directory: a change queued behind it enters while those changes are still
// held, and the lock is freed with its last user.
func TestADrainThatGivesUpReopensTheDirectory(t *testing.T) {
	var l Locks
	inFlight, err := l.Take(context.Background(), Set{Enter: []Key{dir}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	gaveUp := make(chan error)
	go func() {
		_, err := l.Take(ctx, Set{Drain: []Key{dir}})
		gaveUp <- err
	}()
	time.Sleep(20 * time.Millisecond)
	queued := make(chan struct{})
	go func() {
		release, err := l.Take(context.Background(), Set{Enter: []Key{dir}})
		if err == nil {
			release()
		}
		close(queued)
	}()
	if err := <-gaveUp; err == nil {
		t.Fatal("a drain was taken with a change in flight")
	}
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("a change queued behind the drain did not enter once it gave up")
	}
	inFlight()
	if n := len(l.fold.m); n != 0 {
		t.Errorf("the fold table keeps %d locks with no user", n)
	}
}

// Of two drains waiting, the one that gives up leaves the directory closed
// for the other.
func TestADrainGivingUpLeavesAnotherWaiting(t *testing.T) {
	var l Locks
	inFlight, err := l.Take(context.Background(), Set{Enter: []Key{dir}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	go func() { _, _ = l.Take(ctx, Set{Drain: []Key{dir}}) }()
	drained := make(chan struct{})
	go func() {
		release, err := l.Take(context.Background(), Set{Drain: []Key{dir}})
		if err == nil {
			release()
		}
		close(drained)
	}()
	time.Sleep(50 * time.Millisecond)
	if free(&l, Set{Enter: []Key{dir}}) {
		t.Error("a change entered while a second drain waited")
	}
	inFlight()
	<-drained
}
