// Package kvtest is the KV conformance suite. Every kv.KV backend runs it; a
// backend that passes it is interchangeable with any other for the entity
// layer. Durability across a power loss and Now across a restart need a
// backend's own harness, so its own tests cover them.
package kvtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
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
		{"OwnWrites", testOwnWrites},
		{"ConcurrentIncrements", testConcurrentIncrements},
		{"LostUpdate", testLostUpdate},
		{"WriteSkew", testWriteSkew},
		{"BlindWrites", testBlindWrites},
		{"GuardConflictsWithWrite", testGuardVsWrite},
		{"GuardOfAbsentKey", testGuardOfAbsentKey},
		{"GuardsShared", testGuardsShared},
		{"GuardRangeConflictsWithInsert", testGuardRangeVsInsert},
		{"GuardRangeOnlyItsPrefix", testGuardRangeOnlyPrefix},
		{"GuardRangesShared", testGuardRangesShared},
		{"ScanUntracked", testScanUntracked},
		{"EmptyDirectoryRemoval", testEmptyDirectoryRemoval},
		{"Scan", testScan},
		{"ScanSeesNothingUncommitted", testScanUncommitted},
		{"ChangeSequence", testChangeSequence},
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

func testGuardVsWrite(t *testing.T, s kv.KV) {
	set(t, s, "parent", "dir")
	if _, got := ordered(t, s, setter("parent", "changed"), guardThenSet("parent", "child")); got != 2 {
		t.Errorf("guard took %d attempts, want 2: a write committed after its snapshot must abort it", got)
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
	if got := race(t, s, guardThenSet("parent", "a"), guardThenSet("parent", "b")); got != 2 {
		t.Errorf("%d attempts, want 2: two guards of one key must not conflict", got)
	}
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
	if got := race(t, s, rangeGuardThenSet("p/", "a"), rangeGuardThenSet("p/", "b")); got != 2 {
		t.Errorf("%d attempts, want 2: two range guards of one prefix must not conflict", got)
	}
}

// "The directory is empty, so remove it", in both commit orders: a create
// guards the directory's record and writes its entry; the removal guards the
// entries' range, finds it empty and deletes the record. Whichever commits
// second retries, and no entry is left under a removed directory.
func testEmptyDirectoryRemoval(t *testing.T, s kv.KV) {
	for _, createFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("createFirst=%v", createFirst), func(t *testing.T) {
			dir, entry := "d"+strconv.FormatBool(createFirst), "d"+strconv.FormatBool(createFirst)+"/f"
			set(t, s, dir, "dir")
			removed, created := false, false
			rmdir := func(tx kv.Txn) error {
				removed = false
				if err := tx.GuardRange([]byte(dir + "/")); err != nil {
					return err
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
			if createFirst {
				first, second = create, rmdir
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

func testChangeSequence(t *testing.T, s kv.KV) {
	set(t, s, "p/a", "1")
	set(t, s, "p/b", "1")
	set(t, s, "p/a", "2")
	seqs := map[string][]byte{}
	if err := s.View(ctx(t), func(r kv.Reader) error {
		for e, err := range r.Scan([]byte("p/"), nil) {
			if err != nil {
				return err
			}
			if e.Seq == nil {
				return fmt.Errorf("%s: committed record has no change sequence", e.Key)
			}
			seqs[string(e.Key)] = bytes.Clone(e.Seq)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(seqs["p/a"], seqs["p/b"]) <= 0 {
		t.Errorf("p/a rewritten after p/b has sequence %x, not above %x", seqs["p/a"], seqs["p/b"])
	}
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		if err := tx.Set([]byte("p/c"), []byte("1")); err != nil {
			return err
		}
		for e, err := range tx.Scan([]byte("p/c"), nil) {
			if err != nil {
				return err
			}
			if e.Seq != nil {
				return fmt.Errorf("own uncommitted write has sequence %x", e.Seq)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testNow(t *testing.T, s kv.KV) {
	var prev time.Time
	for range 50 {
		var now time.Time
		if err := s.Update(ctx(t), func(tx kv.Txn) error {
			now = tx.Now()
			if !tx.Now().Equal(now) {
				return errors.New("Now moved within one transaction")
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
}

// A transaction at the limit commits, and one past it is refused. Skipped
// for a backend whose limits are too large to reach in a test.
func testLimits(t *testing.T, s kv.KV) {
	l := s.Limits()
	if l.Entries <= 0 || l.Bytes <= 0 || l.Key <= 0 || l.Value <= 0 {
		t.Fatalf("limits %+v", l)
	}
	if l.Entries > 1<<20 || l.Bytes > 64<<20 {
		t.Skipf("limits %+v too large to reach", l)
	}
	key := func(i int) []byte { return fmt.Appendf(nil, "e/%08d", i) }
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		for i := range l.Entries {
			if err := tx.Set(key(i), nil); err != nil {
				return fmt.Errorf("entry %d of %d: %w", i+1, l.Entries, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("a transaction at the entry limit: %v", err)
	}
	if err := s.Update(ctx(t), func(tx kv.Txn) error {
		for i := range l.Entries + 1 {
			if err := tx.Set(key(i), nil); err != nil {
				return err
			}
		}
		return nil
	}); !errors.Is(err, kv.ErrTooLarge) {
		t.Fatalf("a transaction past the entry limit: got %v, want ErrTooLarge", err)
	}

	// Bytes, filled with entries no larger than Value allows.
	fill := func(tx kv.Txn, total int) error {
		for i := 0; total > 0; i++ {
			k := key(i)
			n := min(total-len(k), l.Value)
			if err := tx.Set(k, make([]byte, n)); err != nil {
				return fmt.Errorf("%d bytes left: %w", total, err)
			}
			total -= len(k) + n
		}
		return nil
	}
	if err := s.Update(ctx(t), func(tx kv.Txn) error { return fill(tx, l.Bytes) }); err != nil {
		t.Fatalf("a transaction at the byte limit: %v", err)
	}
	if err := s.Update(ctx(t), func(tx kv.Txn) error { return fill(tx, l.Bytes+1) }); !errors.Is(err, kv.ErrTooLarge) {
		t.Fatalf("a transaction past the byte limit: got %v, want ErrTooLarge", err)
	}

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
}
