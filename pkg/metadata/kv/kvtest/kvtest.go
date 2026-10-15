// Package kvtest is the KV conformance suite. Every kv.KV backend runs it; a
// backend that passes it is interchangeable with any other for the entity
// layer. Durability across a power loss and Now across a restart need a
// backend's own harness, so its own tests cover them.
package kvtest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Run runs the suite. open returns a new, empty store, closed by the caller's
// cleanup.
func Run(t *testing.T, open func(t *testing.T) kv.KV) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, kv.KV)
	}{
		{"Atomic", testAtomic},
		{"CloseTwice", testCloseTwice},
		{"UpdateRacingClose", testUpdateRacingClose},
		{"OwnWrites", testOwnWrites},
		{"ConcurrentIncrements", testConcurrentIncrements},
		{"LostUpdate", testLostUpdate},
		{"WriteSkew", testWriteSkew},
		{"BlindWrites", testBlindWrites},
		{"GuardWriterFirst", testGuardVsWrite},
		{"GuardFirst", testGuardFirst},
		{"GuardOfAbsentKey", testGuardOfAbsentKey},
		{"GuardsShared", testGuardsShared},
		{"GuardRangeConflictsWithInsert", testGuardRangeVsInsert},
		{"GuardRangeOnlyItsPrefix", testGuardRangeOnlyPrefix},
		{"GuardRangesShared", testGuardRangesShared},
		{"ScanUntracked", testScanUntracked},
		{"EmptyDirectoryRemoval", testEmptyDirectoryRemoval},
		{"Scan", testScan},
		{"Before", testBefore},
		{"BeforeSeesOwnWrites", testBeforeOwnWrites},
		{"BeforeUntracked", testBeforeUntracked},
		{"ScanSeesNothingUncommitted", testScanUncommitted},
		{"Versions", testVersions},
		{"Now", testNow},
		{"Limits", testLimits},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open(t)) })
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func set(t *testing.T, s kv.KV, kvs ...string) {
	t.Helper()
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		for i := 0; i < len(kvs); i += 2 {
			if err := tx.Set([]byte(kvs[i]), []byte(kvs[i+1])); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// get reads key in a View; "" with ok false when absent.
func get(t *testing.T, s kv.KV, key string) (string, bool) {
	t.Helper()
	var v []byte
	err := s.View(ctx(t), func(r kv.Reader) error {
		var err error
		v, err = r.Get([]byte(key))
		return err
	})
	if errors.Is(err, kv.ErrNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(v), true
}

func scan(t *testing.T, r kv.Reader, prefix, after string) []string {
	t.Helper()
	var a []byte
	if after != "" {
		a = []byte(after)
	}
	var out []string
	for e, err := range r.Scan([]byte(prefix), a) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(e.Key)+"="+string(e.Value))
	}
	return out
}

// race runs a and b as two Updates whose first attempts both take their
// snapshot and run their body before either commits. It returns how many
// attempts they took together: 2 when they did not conflict, 3 when one was
// retried.
func race(t *testing.T, s kv.KV, a, b func(kv.Txn) error) int {
	t.Helper()
	var attempts atomic.Int32
	var ready sync.WaitGroup
	ready.Add(2)
	run := func(fn func(kv.Txn) error) error {
		first := true
		return s.Update(ctx(t), func(tx kv.Txn) error {
			attempts.Add(1)
			if err := fn(tx); err != nil {
				return err
			}
			if first {
				first = false
				ready.Done()
				ready.Wait()
			}
			return nil
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- run(a) }()
	go func() { errs <- run(b) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	return int(attempts.Load())
}

// ordered runs first and second as two Updates whose first attempts both take
// their snapshot and run their body before either commits, then lets first
// commit before second. It returns how many attempts each took.
func ordered(t *testing.T, s kv.KV, first, second func(kv.Txn) error) (int, int) {
	t.Helper()
	type run struct {
		in, commit chan struct{}
		done       chan error
		attempts   int
	}
	start := func(fn func(kv.Txn) error) *run {
		r := &run{in: make(chan struct{}), commit: make(chan struct{}), done: make(chan error, 1)}
		go func() {
			r.done <- s.Update(ctx(t), func(tx kv.Txn) error {
				r.attempts++
				if err := fn(tx); err != nil {
					return err
				}
				if r.attempts == 1 {
					close(r.in)
					<-r.commit
				}
				return nil
			})
		}()
		return r
	}
	a, b := start(first), start(second)
	<-a.in
	<-b.in
	close(a.commit)
	if err := <-a.done; err != nil {
		t.Fatal(err)
	}
	close(b.commit)
	if err := <-b.done; err != nil {
		t.Fatal(err)
	}
	return a.attempts, b.attempts
}

func setter(key, value string) func(kv.Txn) error {
	return func(tx kv.Txn) error { return tx.Set([]byte(key), []byte(value)) }
}

func testAtomic(t *testing.T, s kv.KV) {
	boom := errors.New("boom")
	attempts := 0
	err := s.Update(ctx(t), func(tx kv.Txn) error {
		attempts++
		if err := tx.Set([]byte("a"), []byte("1")); err != nil {
			return err
		}
		if err := tx.Set([]byte("b"), []byte("1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || attempts != 1 {
		t.Fatalf("got %v after %d attempts, want boom after 1", err, attempts)
	}
	for _, k := range []string{"a", "b"} {
		if _, ok := get(t, s, k); ok {
			t.Errorf("%s written by a failed Update", k)
		}
	}
	set(t, s, "a", "1", "b", "2")
	if a, _ := get(t, s, "a"); a != "1" {
		t.Errorf("a = %q", a)
	}
	if b, _ := get(t, s, "b"); b != "2" {
		t.Errorf("b = %q", b)
	}
}

func testOwnWrites(t *testing.T, s kv.KV) {
	set(t, s, "k", "old", "d", "x")
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		if err := tx.Set([]byte("k"), []byte("new")); err != nil {
			return err
		}
		if err := tx.Delete([]byte("d")); err != nil {
			return err
		}
		if v, err := tx.Get([]byte("k")); err != nil || string(v) != "new" {
			return fmt.Errorf("own Set: got %q, %v", v, err)
		}
		if _, err := tx.Get([]byte("d")); !errors.Is(err, kv.ErrNotFound) {
			return fmt.Errorf("own Delete: got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(t, s, "d"); ok {
		t.Error("d survived its Delete")
	}
}

// Conflicts on one shared key are retried, never surfaced.
func testConcurrentIncrements(t *testing.T, s kv.KV) {
	const workers, each = 8, 25
	var attempts atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				errs <- s.Update(ctx(t), func(tx kv.Txn) error {
					attempts.Add(1)
					return increment(tx, "n")
				})
			}
		}()
	}
	go func() { wg.Wait(); close(errs) }()
	for err := range errs {
		if err != nil {
			t.Fatalf("a conflict reached the caller: %v", err)
		}
	}
	if n, _ := get(t, s, "n"); n != strconv.Itoa(workers*each) {
		t.Fatalf("n = %s, want %d", n, workers*each)
	}
	t.Logf("%d attempts for %d increments", attempts.Load(), workers*each)
}

func increment(tx kv.Txn, key string) error {
	n := 0
	v, err := tx.Get([]byte(key))
	switch {
	case err == nil:
		n, _ = strconv.Atoi(string(v))
	case !errors.Is(err, kv.ErrNotFound):
		return err
	}
	return tx.Set([]byte(key), []byte(strconv.Itoa(n+1)))
}

func testLostUpdate(t *testing.T, s kv.KV) {
	set(t, s, "n", "0")
	inc := func(tx kv.Txn) error { return increment(tx, "n") }
	if got := race(t, s, inc, inc); got != 3 {
		t.Errorf("%d attempts, want 3: one side must retry", got)
	}
	if n, _ := get(t, s, "n"); n != "2" {
		t.Errorf("n = %s, want 2", n)
	}
}

func testWriteSkew(t *testing.T, s kv.KV) {
	set(t, s, "a", "0", "b", "0")
	readThenSet := func(read, write string) func(kv.Txn) error {
		return func(tx kv.Txn) error {
			if _, err := tx.Get([]byte(read)); err != nil {
				return err
			}
			return tx.Set([]byte(write), []byte("1"))
		}
	}
	if got := race(t, s, readThenSet("a", "b"), readThenSet("b", "a")); got != 3 {
		t.Errorf("%d attempts, want 3: write skew must abort one side", got)
	}
}

func testBlindWrites(t *testing.T, s kv.KV) {
	if got := race(t, s, setter("k", "1"), setter("k", "2")); got != 3 {
		t.Errorf("%d attempts, want 3: two blind writes of one key must conflict", got)
	}
}

func guardThenSet(guard, key string) func(kv.Txn) error {
	return func(tx kv.Txn) error {
		if err := tx.Guard([]byte(guard)); err != nil {
			return err
		}
		return tx.Set([]byte(key), []byte("1"))
	}
}

// versions reads the change sequences of keys in a View.
func versions(t *testing.T, s kv.KV, keys ...string) []uint64 {
	t.Helper()
	var vs []uint64
	if err := s.View(ctx(t), func(r kv.Reader) error {
		var err error
		vs, err = r.Versions(byteKeys(keys))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return vs
}

func byteKeys(keys []string) [][]byte {
	b := make([][]byte, len(keys))
	for i, k := range keys {
		b[i] = []byte(k)
	}
	return b
}

// Writer first: a write committed after the guard's snapshot aborts the guard.
func testGuardVsWrite(t *testing.T, s kv.KV) {
	set(t, s, "parent", "dir")
	if _, got := ordered(t, s, setter("parent", "changed"), guardThenSet("parent", "child")); got != 2 {
		t.Errorf("guard took %d attempts, want 2: a write committed after its snapshot must abort it", got)
	}
}

// Guard first: a guard binds only its own transaction. The guard commits, then
// the writer of the guarded key commits too, ordered after it.
func testGuardFirst(t *testing.T, s kv.KV) {
	set(t, s, "parent", "dir")
	g, w := ordered(t, s, guardThenSet("parent", "child"), setter("parent", "changed"))
	if g != 1 || w != 1 {
		t.Fatalf("guard took %d attempts and writer %d, want 1 and 1: a guard binds only its own transaction", g, w)
	}
	if vs := versions(t, s, "child", "parent"); vs[1] <= vs[0] {
		t.Errorf("the writer committed second but has version %d, not above the guard's %d", vs[1], vs[0])
	}
}

// guardAfter runs second as an Update that takes its snapshot, waits for first
// to commit, then runs its body and commits. It returns second's attempts.
func guardAfter(t *testing.T, s kv.KV, first, second func(kv.Txn) error) int {
	t.Helper()
	snapped, resume := make(chan struct{}), make(chan struct{})
	attempts := 0
	done := make(chan error, 1)
	go func() {
		done <- s.Update(ctx(t), func(tx kv.Txn) error {
			attempts++
			// A read pins the snapshot on a backend that takes it lazily.
			if _, err := tx.Get([]byte("snapshot")); err != nil && !errors.Is(err, kv.ErrNotFound) {
				return err
			}
			if attempts == 1 {
				close(snapped)
				<-resume
			}
			return second(tx)
		})
	}()
	<-snapped
	if err := s.Update(ctx(t), first); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return attempts
}

// sharedGuards checks that two transactions guarding one key or range, each
// writing a key of its own, both commit in either commit order, and when one
// commits between the other's snapshot and its guard.
func sharedGuards(t *testing.T, s kv.KV, a, b func(kv.Txn) error) {
	t.Helper()
	if x, y := ordered(t, s, a, b); x != 1 || y != 1 {
		t.Errorf("a then b: %d and %d attempts, want 1 and 1: two guards must not conflict", x, y)
	}
	if x, y := ordered(t, s, b, a); x != 1 || y != 1 {
		t.Errorf("b then a: %d and %d attempts, want 1 and 1: two guards must not conflict", x, y)
	}
	if got := guardAfter(t, s, a, b); got != 1 {
		t.Errorf("a committing between b's snapshot and its guard: b took %d attempts, want 1", got)
	}
}

// The create-if-absent gate: a guard of a missing key conflicts with its
// creation.
func testGuardOfAbsentKey(t *testing.T, s kv.KV) {
	if _, got := ordered(t, s, setter("name", "1"), guardThenSet("name", "other")); got != 2 {
		t.Errorf("guard took %d attempts, want 2: a guard of an absent key must conflict with its create", got)
	}
}

func testGuardsShared(t *testing.T, s kv.KV) {
	set(t, s, "parent", "dir")
	sharedGuards(t, s, guardThenSet("parent", "a"), guardThenSet("parent", "b"))
}

func testScanUntracked(t *testing.T, s kv.KV) {
	set(t, s, "p/1", "x")
	scanThenSet := func(tx kv.Txn) error {
		for _, err := range tx.Scan([]byte("p/"), nil) {
			if err != nil {
				return err
			}
		}
		return tx.Set([]byte("elsewhere"), []byte("1"))
	}
	if got := race(t, s, scanThenSet, setter("p/2", "y")); got != 2 {
		t.Errorf("%d attempts, want 2: a Scan must register nothing", got)
	}
}

func rangeGuardThenSet(prefix, key string) func(kv.Txn) error {
	return func(tx kv.Txn) error {
		if err := tx.GuardRange([]byte(prefix)); err != nil {
			return err
		}
		return tx.Set([]byte(key), []byte("1"))
	}
}

func testGuardRangeVsInsert(t *testing.T, s kv.KV) {
	if _, got := ordered(t, s, setter("e/1", "y"), rangeGuardThenSet("e/", "elsewhere")); got != 2 {
		t.Errorf("range guard of an empty prefix took %d attempts, want 2: a create under it must abort it", got)
	}
	set(t, s, "p/1", "x")
	if _, got := ordered(t, s, setter("p/2", "y"), rangeGuardThenSet("p/", "elsewhere")); got != 2 {
		t.Errorf("range guard took %d attempts, want 2: an insert under its prefix must abort it", got)
	}
	if _, got := ordered(t, s, func(tx kv.Txn) error { return tx.Delete([]byte("p/1")) },
		rangeGuardThenSet("p/", "elsewhere")); got != 2 {
		t.Errorf("range guard took %d attempts, want 2: a delete under its prefix must abort it", got)
	}
}

func testGuardRangeOnlyPrefix(t *testing.T, s kv.KV) {
	if _, got := ordered(t, s, setter("q/1", "y"), rangeGuardThenSet("p/", "elsewhere")); got != 1 {
		t.Errorf("range guard took %d attempts, want 1: a write outside its prefix must not abort it", got)
	}
}

func testGuardRangesShared(t *testing.T, s kv.KV) {
	set(t, s, "p/k", "x")
	sharedGuards(t, s, rangeGuardThenSet("p/", "a"), rangeGuardThenSet("p/", "b"))
}

// "The directory is empty, so remove it", in both commit orders: a create
// guards the directory's record and writes its entry; the removal guards the
// entries' range, finds it empty and deletes the record. Whichever commits
// second retries, and no entry is left under a removed directory. Without the
// removal's range guard, the create committing first lets both commit.
func testEmptyDirectoryRemoval(t *testing.T, s kv.KV) {
	for _, c := range []struct {
		createFirst, rangeGuard bool
	}{{true, true}, {false, true}, {true, false}} {
		t.Run(fmt.Sprintf("createFirst=%v,rangeGuard=%v", c.createFirst, c.rangeGuard), func(t *testing.T) {
			dir := fmt.Sprintf("d-%v-%v", c.createFirst, c.rangeGuard)
			entry := dir + "/f"
			set(t, s, dir, "dir")
			removed, created := false, false
			rmdir := func(tx kv.Txn) error {
				removed = false
				if c.rangeGuard {
					if err := tx.GuardRange([]byte(dir + "/")); err != nil {
						return err
					}
				}
				for _, err := range tx.Scan([]byte(dir+"/"), nil) {
					return err // not empty: nothing to remove
				}
				removed = true
				return tx.Delete([]byte(dir))
			}
			create := func(tx kv.Txn) error {
				created = false
				if _, err := tx.Get([]byte(dir)); errors.Is(err, kv.ErrNotFound) {
					return nil // no directory: nothing to create in
				} else if err != nil {
					return err
				}
				if err := tx.Guard([]byte(dir)); err != nil {
					return err
				}
				created = true
				return tx.Set([]byte(entry), []byte("1"))
			}
			first, second := rmdir, create
			if c.createFirst {
				first, second = create, rmdir
			}
			if !c.rangeGuard {
				ordered(t, s, first, second)
				if !removed || !created {
					t.Errorf("removed %v, created %v: without the range guard both should commit, or this case proves nothing", removed, created)
				}
				return
			}
			if _, got := ordered(t, s, first, second); got != 2 {
				t.Errorf("the second to commit took %d attempts, want 2", got)
			}
			if removed == created {
				t.Errorf("removed %v, created %v: exactly one must win", removed, created)
			}
			_, dirLeft := get(t, s, dir)
			_, entryLeft := get(t, s, entry)
			if !dirLeft && entryLeft {
				t.Error("an entry exists under a removed directory")
			}
		})
	}
}

func testScan(t *testing.T, s kv.KV) {
	set(t, s, "a", "-", "p/1", "1", "p/2", "2", "p/3", "3", "p0", "-", "q", "-")
	want := func(got []string, w ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("got %v, want %v", got, w)
		}
	}
	if err := s.View(ctx(t), func(r kv.Reader) error {
		want(scan(t, r, "p/", ""), "p/1=1", "p/2=2", "p/3=3")
		want(scan(t, r, "p/", "p/1"), "p/2=2", "p/3=3")
		want(scan(t, r, "p/", "p/3"), []string{}...)
		want(scan(t, r, "p/", "a"), "p/1=1", "p/2=2", "p/3=3")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		for _, k := range []string{"p/0", "p/25"} {
			if err := tx.Set([]byte(k), []byte("new")); err != nil {
				return err
			}
		}
		if err := tx.Set([]byte("p/1"), []byte("changed")); err != nil {
			return err
		}
		if err := tx.Delete([]byte("p/2")); err != nil {
			return err
		}
		want(scan(t, tx, "p/", ""), "p/0=new", "p/1=changed", "p/25=new", "p/3=3")
		want(scan(t, tx, "p/", "p/1"), "p/25=new", "p/3=3")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Stopping early is allowed.
	if err := s.View(ctx(t), func(r kv.Reader) error {
		for range r.Scan([]byte("p/"), nil) {
			break
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testScanUncommitted(t *testing.T, s kv.KV) {
	written := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		once := false
		done <- s.Update(ctx(t), func(tx kv.Txn) error {
			if err := tx.Set([]byte("p/pending"), []byte("1")); err != nil {
				return err
			}
			if !once {
				once = true
				close(written)
				<-release
			}
			return nil
		})
	}()
	<-written
	if err := s.View(ctx(t), func(r kv.Reader) error {
		if got := scan(t, r, "p/", ""); len(got) != 0 {
			t.Errorf("an uncommitted write was scanned: %v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Versions orders one key's writes as their commits, reports 0 for an absent
// key, and inside Update reports the snapshot's version of a key the
// transaction has not written and ErrUncommittedVersion for one it set or
// deleted.
func testVersions(t *testing.T, s kv.KV) {
	var seen []uint64
	for _, v := range []string{"1", "2", "3"} {
		set(t, s, "k", v)
		set(t, s, "other", v)
		seen = append(seen, versions(t, s, "k")[0])
	}
	if seen[0] == 0 || seen[1] <= seen[0] || seen[2] <= seen[1] {
		t.Errorf("versions of k across commits in order: %v, want increasing and above 0", seen)
	}
	if vs := versions(t, s, "k", "absent"); vs[1] != 0 {
		t.Errorf("absent key has version %d, want 0", vs[1])
	}
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		vs, err := tx.Versions(byteKeys([]string{"k"}))
		if err != nil {
			return err
		}
		if vs[0] != seen[2] {
			return fmt.Errorf("k before any write has version %d, want the snapshot's %d", vs[0], seen[2])
		}
		if err := tx.Set([]byte("k"), []byte("4")); err != nil {
			return err
		}
		if err := tx.Delete([]byte("other")); err != nil {
			return err
		}
		for _, key := range []string{"k", "other"} {
			if _, err := tx.Versions(byteKeys([]string{"absent", key})); !errors.Is(err, kv.ErrUncommittedVersion) {
				return fmt.Errorf("versions of %s after this transaction wrote it: %v, want ErrUncommittedVersion", key, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v := versions(t, s, "k")[0]; v <= seen[2] {
		t.Errorf("k committed again has version %d, not above %d", v, seen[2])
	}
}

func testNow(t *testing.T, s kv.KV) {
	var prev time.Time
	for range 50 {
		var now time.Time
		if err := s.Update(ctx(t), func(tx kv.Txn) error {
			now = tx.Now()
			if !tx.Now().Equal(now) {
				return errors.New("store time moved within one transaction")
			}
			return tx.Set([]byte("k"), []byte("v"))
		}); err != nil {
			t.Fatal(err)
		}
		if now.Before(prev) {
			t.Fatalf("Now ran backward across commits in order: %v after %v", now, prev)
		}
		prev = now
	}
	if d := time.Since(prev); d > 5*time.Second || d < -5*time.Second {
		t.Errorf("Now is %v from real time", d)
	}
	// On an idle store it still advances with real time.
	now := func() time.Time {
		var n time.Time
		if err := s.Update(ctx(t), func(tx kv.Txn) error { n = tx.Now(); return nil }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := now()
	time.Sleep(1500 * time.Millisecond)
	if d := now().Sub(before); d < time.Second {
		t.Errorf("Now advanced %v over 1.5s of an idle store", d)
	}
}

// A transaction at each limit commits, and one past it is refused; conflict
// ranges count toward TxnBytes; one held open past Age aborts and is retried.
// The parts whose limits are too large to reach in a test are skipped.
func testLimits(t *testing.T, s kv.KV) {
	l := s.Limits()
	if l.TxnEntries < 0 || l.TxnBytes <= 0 || l.Key <= 0 || l.Value <= 0 || l.Age <= 0 {
		t.Fatalf("limits %+v", l)
	}
	key := func(i int) []byte { return fmt.Appendf(nil, "e/%08d", i) }
	t.Run("TxnEntries", func(t *testing.T) {
		if l.TxnEntries == 0 || l.TxnEntries > 1<<20 {
			t.Skipf("TxnEntries %d unbounded or too large to reach", l.TxnEntries)
		}
		if err := s.Update(ctx(t), func(tx kv.Txn) error {
			for i := range l.TxnEntries {
				if err := tx.Set(key(i), nil); err != nil {
					return fmt.Errorf("entry %d of %d: %w", i+1, l.TxnEntries, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("a transaction at the entry limit: %v", err)
		}
		if err := s.Update(ctx(t), func(tx kv.Txn) error {
			for i := range l.TxnEntries + 1 {
				if err := tx.Set(key(i), nil); err != nil {
					return err
				}
			}
			return nil
		}); !errors.Is(err, kv.ErrTooLarge) {
			t.Fatalf("a transaction past the entry limit: got %v, want ErrTooLarge", err)
		}
	})

	t.Run("TxnBytes", func(t *testing.T) {
		if l.TxnBytes > 64<<20 {
			t.Skipf("TxnBytes %d too large to reach", l.TxnBytes)
		}
		// fill writes exactly total bytes, in entries no larger than Value
		// allows; the last key is cut short when fewer bytes are left.
		fill := func(tx kv.Txn, total int) error {
			for i := 0; total > 0; i++ {
				k := key(i)
				if total < len(k) {
					k = k[:total]
				}
				n := min(total-len(k), l.Value)
				if err := tx.Set(k, make([]byte, n)); err != nil {
					return fmt.Errorf("%d bytes left: %w", total, err)
				}
				total -= len(k) + n
			}
			return nil
		}
		if err := s.Update(ctx(t), func(tx kv.Txn) error { return fill(tx, l.TxnBytes) }); err != nil {
			t.Fatalf("a transaction at the byte limit: %v", err)
		}
		if err := s.Update(ctx(t), func(tx kv.Txn) error { return fill(tx, l.TxnBytes+1) }); !errors.Is(err, kv.ErrTooLarge) {
			t.Fatalf("a transaction past the byte limit: got %v, want ErrTooLarge", err)
		}
		// Writes that fit, with the guard that brings them to the limit.
		guard := []byte("guarded/")
		if err := s.Update(ctx(t), func(tx kv.Txn) error {
			if err := fill(tx, l.TxnBytes-len(guard)); err != nil {
				return err
			}
			return tx.Guard(guard)
		}); err != nil {
			t.Fatalf("writes and a guard at the byte limit: %v", err)
		}
		for name, past := range map[string]func(kv.Txn) error{
			"Guard":      func(tx kv.Txn) error { return tx.Guard(append(guard, 'x')) },
			"GuardRange": func(tx kv.Txn) error { return tx.GuardRange(append(guard, 'x')) },
			"Get": func(tx kv.Txn) error {
				_, err := tx.Get(append(guard, 'x'))
				return err
			},
		} {
			if err := s.Update(ctx(t), func(tx kv.Txn) error {
				if err := fill(tx, l.TxnBytes-len(guard)); err != nil {
					return err
				}
				return past(tx)
			}); !errors.Is(err, kv.ErrTooLarge) {
				t.Errorf("writes that fit and a %s past the byte limit: got %v, want ErrTooLarge", name, err)
			}
		}
	})

	t.Run("KeyAndValue", func(t *testing.T) {
		for _, c := range []struct {
			name       string
			key, value int // at the limit; one more byte of the limited part is past it
			pastKey    bool
		}{{"key", l.Key, 0, true}, {"value", 1, l.Value, false}} {
			if err := s.Update(ctx(t), func(tx kv.Txn) error {
				return tx.Set(make([]byte, c.key), make([]byte, c.value))
			}); err != nil {
				t.Errorf("a %s at its limit: %v", c.name, err)
			}
			k, v := c.key, c.value+1
			if c.pastKey {
				k, v = c.key+1, c.value
			}
			if err := s.Update(ctx(t), func(tx kv.Txn) error {
				return tx.Set(make([]byte, k), make([]byte, v))
			}); !errors.Is(err, kv.ErrTooLarge) {
				t.Errorf("a %s past its limit: got %v, want ErrTooLarge", c.name, err)
			}
		}
	})

	t.Run("Age", func(t *testing.T) {
		if l.Age > 10*time.Second {
			t.Skipf("Age %v too long to wait out", l.Age)
		}
		attempts := 0
		if err := s.Update(ctx(t), func(tx kv.Txn) error {
			attempts++
			if attempts == 1 {
				time.Sleep(l.Age + 100*time.Millisecond)
			}
			return tx.Set([]byte("aged"), []byte("1"))
		}); err != nil {
			t.Fatal(err)
		}
		if attempts != 2 {
			t.Errorf("%d attempts, want 2: a transaction held open past Age must abort and be retried", attempts)
		}
	})
}

func before(r kv.Reader, prefix, key string) (string, error) {
	got, ok, err := r.Before([]byte(prefix), []byte(key))
	if err != nil || !ok {
		return "", err
	}
	return string(got.Key) + "=" + string(got.Value), nil
}

func testBefore(t *testing.T, s kv.KV) {
	set(t, s, "a/1", "x", "a/3", "y", "a/5", "z", "b/1", "w", "a", "outside")
	if err := s.View(ctx(t), func(r kv.Reader) error {
		for _, c := range []struct{ prefix, key, want string }{
			{"a/", "a/4", "a/3=y"}, // the record below, not at, the key
			{"a/", "a/3", "a/1=x"}, // strictly below
			{"a/", "a/1", ""},      // nothing under the prefix below it
			{"a/", "a/9", "a/5=z"}, // past the last record
			{"a/", "b/0", "a/5=z"}, // a key past the prefix's range
			{"a/", "a/", ""},       // the prefix itself: nothing sorts below it in range
			{"c/", "c/9", ""},      // an empty prefix
			{"b/", "b/2", "b/1=w"}, // never a record of another prefix
		} {
			got, err := before(r, c.prefix, c.key)
			if err != nil {
				return err
			}
			if got != c.want {
				t.Errorf("Before(%q, %q) = %q, want %q", c.prefix, c.key, got, c.want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testBeforeOwnWrites(t *testing.T, s kv.KV) {
	set(t, s, "a/1", "x", "a/3", "y", "a/5", "z")
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		for _, step := range []struct {
			op, key, want string
		}{
			{"set", "a/4", "a/4=new"},   // its own insert is the nearest below a/5
			{"del", "a/4", "a/3=y"},     // and its own delete of it uncovers a/3
			{"del", "a/3", "a/1=x"},     // a delete hides a stored record
			{"set", "a/3", "a/3=again"}, // and a later set brings it back
		} {
			var err error
			if step.op == "set" {
				err = tx.Set([]byte(step.key), []byte(strings.TrimPrefix(step.want, step.key+"=")))
			} else {
				err = tx.Delete([]byte(step.key))
			}
			if err != nil {
				return err
			}
			got, err := before(tx, "a/", "a/5")
			if err != nil {
				return err
			}
			if got != step.want {
				t.Errorf("after %s %s: Before = %q, want %q", step.op, step.key, got, step.want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testBeforeUntracked(t *testing.T, s kv.KV) {
	set(t, s, "p/1", "x")
	seekThenSet := func(tx kv.Txn) error {
		if _, _, err := tx.Before([]byte("p/"), []byte("p/9")); err != nil {
			return err
		}
		return tx.Set([]byte("elsewhere"), []byte("1"))
	}
	if got := race(t, s, seekThenSet, setter("p/2", "y")); got != 2 {
		t.Errorf("%d attempts, want 2: a Before must register nothing", got)
	}
}

// Close is safe to call twice, and every call after it fails with
// ErrClosed rather than reading or writing a released store.
func testCloseTwice(t *testing.T, s kv.KV) {
	ctx := ctx(t)
	if err := s.Update(ctx, func(tx kv.Txn) error { return tx.Set([]byte("k"), []byte("v")) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("second Close panicked: %v", p)
			}
		}()
		if err := s.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()
	if err := s.Update(ctx, func(tx kv.Txn) error { return tx.Set([]byte("k"), []byte("w")) }); !errors.Is(err, kv.ErrClosed) {
		t.Errorf("Update after Close: %v", err)
	}
	if err := s.View(ctx, func(kv.Reader) error { return nil }); !errors.Is(err, kv.ErrClosed) {
		t.Errorf("View after Close: %v", err)
	}
}

// Writers racing Close all return: each commit in flight completes as it
// would have, and each later one fails with ErrClosed. None is left
// waiting on a store that has stopped.
func testUpdateRacingClose(t *testing.T, s kv.KV) {
	ctx := ctx(t)
	var started atomic.Int32
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				started.Add(1)
				if err := s.Update(ctx, func(tx kv.Txn) error {
					return tx.Set(fmt.Appendf(nil, "k%d-%d", w, i), []byte("v"))
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for started.Load() < 64 {
		time.Sleep(time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writers still blocked 5s after Close")
	}
	close(errs)
	for err := range errs {
		if !errors.Is(err, kv.ErrClosed) {
			t.Errorf("a writer racing Close: %v", err)
		}
	}
}
