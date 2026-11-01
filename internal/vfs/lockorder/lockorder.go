// Package lockorder takes the locks a call needs in the one order every call
// takes them: the admission locks of every file it touches, then fold
// admission at every directory whose deltas or own attributes it changes,
// then the accept locks of every file it draws a version for, each kind in
// file-ID order. Since every call takes them in that order, no two calls each
// hold what the other waits for.
package lockorder

import (
	"bytes"
	"context"
	"slices"
	"sync"
)

// Key names the file a lock is held on: its file ID's bytes.
type Key [16]byte

// Set is every lock one call takes. A key may repeat, within a list or, for a
// directory, across Enter and Drain, where Drain wins.
type Set struct {
	Admit  []Key // admission locks: the outer level of each file's open-state step
	Enter  []Key // fold admission, shared: a create, link or rename writing the directory's deltas
	Drain  []Key // fold admission, draining: a fold, the directory's own attribute change, a rename moving it, a recount
	Accept []Key // accept locks: the files the call draws a version for
}

// Locks is the filesystem's three lock tables. The zero value is ready.
type Locks struct {
	admit, fold, accept table
}

// Admission takes f's admission lock, waiting as long as it takes, and
// returns its unlock. It is open state's Order: the table takes it itself
// where it serves a file the call did not name, so its caller holds no lock
// of the three tables; taken after one, it can break the one order and wait
// on a call that waits on it.
func (l *Locks) Admission(f Key) (unlock func()) {
	_ = l.admit.acquire(context.Background(), f, true)
	return func() { l.admit.release(f, true) }
}

// lockOf is one lock a call takes.
type lockOf struct {
	t    *table
	k    Key
	excl bool
}

// Take takes every lock of s, in the one order, and returns their release. A
// lock not taken by ctx's end releases every lock the call took, and Take
// answers ctx's error. A call answers a deadline met here as ErrDelay with
// DelayConflict, never as ctx's error.
//
// So a call waiting at fold admission holds its admission locks and no
// accept lock, and one waiting at an accept lock holds every admission lock
// and fold admission it needs.
func (l *Locks) Take(ctx context.Context, s Set) (release func(), err error) {
	var want []lockOf
	want = append(want, ordered(&l.admit, nil, s.Admit)...)
	want = append(want, ordered(&l.fold, s.Enter, s.Drain)...)
	want = append(want, ordered(&l.accept, nil, s.Accept)...)

	var held []lockOf
	release = func() {
		for _, h := range slices.Backward(held) {
			h.t.release(h.k, h.excl)
		}
	}
	for _, w := range want {
		if err := w.t.acquire(ctx, w.k, w.excl); err != nil {
			release()
			return nil, err
		}
		held = append(held, w)
	}
	return release, nil
}

// ordered is the locks of t on shared and excl, by key, one per key,
// exclusive where the key is in excl.
func ordered(t *table, shared, excl []Key) []lockOf {
	ls := make([]lockOf, 0, len(shared)+len(excl))
	for _, k := range excl {
		ls = append(ls, lockOf{t, k, true})
	}
	for _, k := range shared {
		ls = append(ls, lockOf{t, k, false})
	}
	// Exclusive first among equal keys, so the one kept is exclusive.
	slices.SortStableFunc(ls, func(a, b lockOf) int { return bytes.Compare(a.k[:], b.k[:]) })
	return slices.CompactFunc(ls, func(a, b lockOf) bool { return a.k == b.k })
}

// table is one kind of lock, held per key.
//
// ponytail: one mutex guards every key's lock of a table, held only to read
// or change a lock's counts, never while a caller waits. Stripe it by key, as
// open state stripes its files, once a profile shows it contended.
type table struct {
	mu sync.Mutex
	m  map[Key]*lock
}

// lock is one key's lock: exclusive, or shared by any number of holders.
type lock struct {
	users   int           // callers holding or waiting on it; the lock is freed at zero
	shared  int           // shared holders
	held    bool          // an exclusive holder
	closing int           // exclusive waiters: while one waits, no new shared holder is admitted
	wake    chan struct{} // closed, and replaced, whenever a waiter may now proceed
}

// acquire takes k's lock, exclusive or shared, waiting until ctx ends.
func (t *table) acquire(ctx context.Context, k Key, excl bool) error {
	t.mu.Lock()
	if t.m == nil {
		t.m = map[Key]*lock{}
	}
	l := t.m[k]
	if l == nil {
		l = &lock{wake: make(chan struct{})}
		t.m[k] = l
	}
	l.users++
	if excl {
		l.closing++
	}
	for {
		if excl && !l.held && l.shared == 0 {
			l.closing--
			l.held = true
			t.mu.Unlock()
			return nil
		}
		if !excl && !l.held && l.closing == 0 {
			l.shared++
			t.mu.Unlock()
			return nil
		}
		wake := l.wake
		t.mu.Unlock()
		select {
		case <-wake:
			t.mu.Lock()
		case <-ctx.Done():
			t.mu.Lock()
			if excl {
				// Shared waiters this one held back may now proceed.
				l.closing--
				l.signal()
			}
			t.drop(k, l)
			t.mu.Unlock()
			return ctx.Err()
		}
	}
}

// release releases one hold of k's lock, taken as excl says.
func (t *table) release(k Key, excl bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.m[k]
	if excl {
		l.held = false
	} else {
		l.shared--
	}
	l.signal()
	t.drop(k, l)
}

// signal wakes every waiter on l, each to look again.
func (l *lock) signal() {
	close(l.wake)
	l.wake = make(chan struct{})
}

// drop ends one caller's use of l, freeing it with the last.
func (t *table) drop(k Key, l *lock) {
	if l.users--; l.users == 0 {
		delete(t.m, k)
	}
}
