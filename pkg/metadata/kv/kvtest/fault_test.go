package kvtest_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/sim"
)

// errInjected is the error the schedules below inject, so a test can tell
// the injected failure from any other.
var errInjected = errors.New("injected")

// memStore is a fresh in-memory store, closed when the test ends.
func memStore(t *testing.T) kv.KV {
	t.Helper()

	s, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	return s
}

// schedule builds a schedule over clock from rules, failing the test on a
// rule the schedule refuses.
func schedule(t *testing.T, clock sim.Waiter, rules ...sim.Rule) *sim.Schedule {
	t.Helper()

	s, err := sim.NewSchedule(1, clock, rules...)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// failOn is a rule that fails every call of op on the key Describe renders
// for key.
func failOn(op string, key []byte, err error) sim.Rule {
	return sim.Rule{
		Op:     op,
		Key:    keys.Describe(key),
		Action: sim.FaultAction{Err: err},
	}
}

// stored reports whether key holds a value in s, read past any wrapper.
func stored(t *testing.T, s kv.KV, key []byte) bool {
	t.Helper()

	var ok bool
	err := s.View(t.Context(), func(r kv.Reader) error {
		_, err := r.Get(key)
		if errors.Is(err, kv.ErrNotFound) {
			return nil
		}

		ok = err == nil

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return ok
}

// put commits key=value through s.
func put(ctx context.Context, s kv.KV, key []byte) error {
	return s.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(key, []byte("v"))
	})
}

// armed blocks until clock holds n waits, bounded in real time, so the test
// never advances the clock before the stalled calls have started waiting on
// it.
func armed(t *testing.T, clock *sim.TestClock, n int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for clock.Pending() < n {
		if time.Now().After(deadline) {
			t.Fatalf(
				"the clock holds %d waits, want %d: a delay not consulted, or a call held off it",
				clock.Pending(),
				n,
			)
		}

		runtime.Gosched()
	}
}

// released advances clock until every channel in exited is closed, bounded in
// real time, so a test that fails with a call still stalled leaves no
// goroutine behind it.
func released(t *testing.T, clock *sim.TestClock, exited ...<-chan struct{}) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for _, ch := range exited {
		for done := false; !done; {
			select {
			case <-ch:
				done = true
			default:
				if time.Now().After(deadline) {
					t.Error("a stalled call never returned")

					return
				}

				clock.Advance(time.Hour)
				runtime.Gosched()
			}
		}
	}
}

// A delay on update holds the call before it reaches the store: nothing is
// committed while the clock stands still, and the call commits once the clock
// passes the delay.
func TestFaultStallOnUpdate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	mem := memStore(t)
	clock := sim.NewTestClock(time.Unix(0, 0))
	f := kvtest.NewFaultKV(mem)
	f.SetFault(schedule(t, clock, sim.Rule{
		Op:     kvtest.OpUpdate,
		Key:    "*",
		Action: sim.FaultAction{Delay: 31 * time.Second},
	}))

	key := []byte("stalled")
	done := make(chan error, 1)

	go func() {
		done <- put(t.Context(), f, key)
	}()

	armed(t, clock, 1)

	select {
	case err := <-done:
		t.Fatalf("Update returned %v before the clock passed the delay", err)
	default:
	}

	if stored(t, mem, key) {
		t.Fatal("the stalled Update committed before its delay passed")
	}

	clock.Advance(31 * time.Second)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if !stored(t, mem, key) {
		t.Fatal("the Update did not commit once the delay passed")
	}
}

// An error on update fails exactly the call the rule names, counted per call
// and not per attempt, before fn runs, so nothing it writes commits.
func TestFaultErrorOnSecondUpdate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	mem := memStore(t)
	f := kvtest.NewFaultKV(mem)
	read := []byte("read")
	f.SetFault(schedule(t, nil,
		sim.Rule{
			Op:     kvtest.OpUpdate,
			Key:    "*",
			From:   2,
			Count:  1,
			Action: sim.FaultAction{Err: errInjected},
		},
		// The first Update retries once: a wrapper consulting update per
		// attempt would count that retry as the second call.
		sim.Rule{
			Op:     kvtest.OpGet,
			Key:    keys.Describe(read),
			Count:  1,
			Action: sim.FaultAction{Err: kv.ErrConflict},
		},
	))

	first, second, third := []byte("first"), []byte("second"), []byte("third")
	err := unpanicked(t, kvtest.OpGet, func() error {
		return f.Update(t.Context(), func(tx kv.Txn) error {
			if _, err := tx.Get(read); err != nil && !errors.Is(err, kv.ErrNotFound) {
				return err
			}

			return tx.Set(first, []byte("v"))
		})
	})
	if err != nil {
		t.Fatalf("first Update: %v", err)
	}

	ran := 0
	err = f.Update(t.Context(), func(tx kv.Txn) error {
		ran++

		return tx.Set(second, []byte("v"))
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("second Update returned %v, want the injected error", err)
	}

	if ran != 0 {
		t.Errorf("the failed Update ran fn %d times, want 0", ran)
	}

	if stored(t, mem, second) {
		t.Error("the failed Update's write is in the store")
	}

	if err := put(t.Context(), f, third); err != nil {
		t.Fatalf("third Update: %v", err)
	}

	if !stored(t, mem, first) || !stored(t, mem, third) {
		t.Error("the first and third Updates did not both commit")
	}
}

// A delay stalls only its own call: while an Update waits out its delay, a
// View on the same wrapper reaches the fault and waits out its own, so the
// clock holds both waits, and both return once the clock passes them.
func TestFaultStallHoldsOnlyItsOwnCall(t *testing.T) {
	prof.Watch(t, prof.Options{})

	clock := sim.NewTestClock(time.Unix(0, 0))
	f := kvtest.NewFaultKV(memStore(t))
	f.SetFault(schedule(t, clock,
		sim.Rule{
			Op:     kvtest.OpUpdate,
			Key:    "*",
			Action: sim.FaultAction{Delay: 31 * time.Second},
		},
		sim.Rule{
			Op:     kvtest.OpView,
			Key:    "*",
			Action: sim.FaultAction{Delay: 31 * time.Second},
		},
	))

	updated, viewed := make(chan error, 1), make(chan error, 1)
	updateExited, viewExited := make(chan struct{}), make(chan struct{})
	started := []<-chan struct{}{updateExited}

	t.Cleanup(func() {
		released(t, clock, started...)
	})

	go func() {
		defer close(updateExited)

		updated <- put(t.Context(), f, outside)
	}()

	armed(t, clock, 1)
	started = append(started, viewExited)

	go func() {
		defer close(viewExited)

		viewed <- f.View(t.Context(), func(kv.Reader) error {
			return nil
		})
	}()

	armed(t, clock, 2)
	clock.Advance(31 * time.Second)

	if err := <-updated; err != nil {
		t.Errorf("Update: %v", err)
	}

	if err := <-viewed; err != nil {
		t.Errorf("View: %v", err)
	}
}

// An error on view fails the call before fn runs.
func TestFaultErrorOnView(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := kvtest.NewFaultKV(memStore(t))
	f.SetFault(schedule(t, nil, sim.Rule{
		Op:     kvtest.OpView,
		Key:    "*",
		Action: sim.FaultAction{Err: errInjected},
	}))

	ran := 0
	err := f.View(t.Context(), func(kv.Reader) error {
		ran++

		return nil
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("View returned %v, want the injected error", err)
	}

	if ran != 0 {
		t.Errorf("the failed View ran fn %d times, want 0", ran)
	}
}

// An error on get, versions and a GetMany batch holding the key reaches the
// caller unchanged, a read of another key in the same transaction succeeds,
// and a refused read never reaches the store: with Counting inside the
// wrapper, only the reads that passed are counted.
func TestFaultErrorOnReads(t *testing.T) {
	prof.Watch(t, prof.Options{})

	mem := memStore(t)
	bad, good := []byte("bad"), []byte("good")

	for _, k := range [][]byte{bad, good} {
		if err := put(t.Context(), mem, k); err != nil {
			t.Fatal(err)
		}
	}

	counts := &kv.Counting{KV: mem}
	f := kvtest.NewFaultKV(counts)
	f.SetFault(schedule(t, nil,
		failOn(kvtest.OpGet, bad, errInjected),
		failOn(kvtest.OpVersions, bad, errInjected),
	))

	err := f.Update(t.Context(), func(tx kv.Txn) error {
		if _, err := tx.Get(bad); !errors.Is(err, errInjected) {
			t.Errorf("Get of the failing key returned %v, want the injected error", err)
		}

		if _, err := tx.Get(good); err != nil {
			t.Errorf("Get of another key: %v", err)
		}

		if _, err := tx.GetMany([][]byte{good}); err != nil {
			t.Errorf("GetMany without the failing key: %v", err)
		}

		if _, err := tx.GetMany([][]byte{good, bad}); !errors.Is(err, errInjected) {
			t.Errorf("GetMany with the failing key second: %v, want the injected error", err)
		}

		if _, err := tx.Versions([][]byte{good}); err != nil {
			t.Errorf("Versions without the failing key: %v", err)
		}

		if _, err := tx.Versions([][]byte{good, bad}); !errors.Is(err, errInjected) {
			t.Errorf("Versions with the failing key second: %v, want the injected error", err)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	got := counts.Take()

	if got.Gets != 2 || got.Versions != 1 {
		t.Errorf(
			"the store counted %d Gets and %d Versions, want 2 and 1: a refused read reached it",
			got.Gets,
			got.Versions,
		)
	}
}

// A chosen file's failure: an absent record injected on that file's record
// key alone becomes its entry in Existence.Commit's map, as a missing File
// record is, and the other file in the same commit is committed.
func TestFaultChosenFileFailsAlone(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var f *kvtest.FaultKV
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		mem, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}

		tb.Cleanup(func() {
			if err := mem.Close(); err != nil {
				tb.Error(err)
			}
		})

		f = kvtest.NewFaultKV(mem)

		return f
	})

	share := fs.Share.ID
	failing, failingOp := newContentFile(t, fs, "failing")
	other, otherOp := newContentFile(t, fs, "other")

	// The exact text of the failing file's record key: a trailing '*' would
	// also match its sub-keys, and an error there fails the whole commit.
	f.SetFault(schedule(t, nil, failOn(kvtest.OpGet, fileKey(share, failing), kv.ErrNotFound)))

	failed, err := fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{
		{
			Share: share,
			File:  failing,
			Ops:   []kvstore.PendingOp{failingOp},
		},
		{
			Share: share,
			File:  other,
			Ops:   []kvstore.PendingOp{otherOp},
		},
	}, nil)

	f.SetFault(nil)

	if err != nil {
		t.Fatalf("Commit failed whole: %v", err)
	}

	if len(failed) != 1 || !errors.Is(failed[failing], entity.ErrStale) {
		t.Fatalf("Commit reported %v, want only the failing file, as stale", failed)
	}

	applied, err := fs.Store.Existence().Applied(t.Context(), share, other)
	if err != nil {
		t.Fatal(err)
	}

	if applied != otherOp.Version {
		t.Errorf(
			"the other file's applied is %v, want %v: its existence was not committed",
			applied,
			otherOp.Version,
		)
	}

	applied, err = fs.Store.Existence().Applied(t.Context(), share, failing)
	if err != nil {
		t.Fatal(err)
	}

	if applied == failingOp.Version {
		t.Error("the failing file's existence was committed")
	}
}

// newContentFile creates a regular file under the share's root and returns
// its ID and a write above its version for an existence commit.
func newContentFile(
	t *testing.T,
	fs *kvstoretest.FS,
	name string,
) (entity.FileID, kvstore.PendingOp) {
	t.Helper()

	h, made := fs.Create(t, fs.Admin, fs.Root, name, kvstore.NewFile{
		Type: entity.Regular,
		Mode: 0o644,
	})

	_, id, err := h.Parse()
	if err != nil {
		t.Fatal(err)
	}

	return id, kvstore.PendingOp{
		Version: entity.VersionOf(made.Version.Counter() + 1),
		Kind:    kvstore.OpWrite,
		Extent: entity.Extent{
			Length: 4096,
		},
		Mtime: time.Unix(1_900_000_000, 0),
		Ctime: time.Unix(1_900_000_000, 0),
	}
}

// fileKey is a file's record key, F‖share‖file, as the store lays it out.
func fileKey(share entity.ShareID, file entity.FileID) []byte {
	return keys.ID(keys.ID([]byte{keys.KindF}, share[:]), file[:])
}

// A conflict injected on a read inside an Update is the backend's to retry:
// the Update runs fn again until the rule stops firing, then commits.
func TestFaultConflictInsideUpdateRetries(t *testing.T) {
	prof.Watch(t, prof.Options{})

	mem := memStore(t)
	f := kvtest.NewFaultKV(mem)
	read, written := []byte("read"), []byte("written")
	f.SetFault(schedule(t, nil, sim.Rule{
		Op:     kvtest.OpGet,
		Key:    keys.Describe(read),
		From:   1,
		Count:  2,
		Action: sim.FaultAction{Err: kv.ErrConflict},
	}))

	attempts := 0
	err := unpanicked(t, kvtest.OpGet, func() error {
		return f.Update(t.Context(), func(tx kv.Txn) error {
			attempts++

			if _, err := tx.Get(read); err != nil && !errors.Is(err, kv.ErrNotFound) {
				return err
			}

			return tx.Set(written, []byte("v"))
		})
	})
	if err != nil {
		t.Fatalf("Update returned %v, want the conflicts retried", err)
	}

	if attempts != 3 {
		t.Errorf("fn ran %d times, want 3: two conflicts, then the commit", attempts)
	}

	if !stored(t, mem, written) {
		t.Error("the retried Update did not commit")
	}
}

// unpanicked runs call, which injects a conflict on op inside an Update, and
// fails the test if it panics: the backend's retry handles that conflict, so
// a wrapper that refuses it is wrong, and the test says so instead of the
// panic stopping every test after it.
func unpanicked(t *testing.T, op string, call func() error) error {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a conflict on %s inside an Update panicked with %v, want it retried", op, r)
		}
	}()

	return call()
}

// opCall runs one wrapper operation and returns its error.
type opCall struct {
	name string // the subtest's name: the op, under update/ or view/ for one inside a transaction
	op   string
	key  []byte // the key the wrapper consults with, rendered by Describe; nil for none
	run  func(t *testing.T, f *kvtest.FaultKV) error
}

var (
	prefix  = []byte("p")
	inside  = []byte("p1") // the one record under prefix
	seek    = []byte("p2") // what before seeks below, under prefix
	outside = []byte("q")
)

// inUpdate runs call on a transaction of f.
func inUpdate(t *testing.T, f *kvtest.FaultKV, call func(kv.Txn) error) error {
	t.Helper()

	var got error
	err := f.Update(t.Context(), func(tx kv.Txn) error {
		got = call(tx)

		return nil
	})
	if err != nil {
		return err
	}

	return got
}

// inView runs call on a read transaction of f.
func inView(t *testing.T, f *kvtest.FaultKV, call func(kv.Reader) error) error {
	t.Helper()

	var got error
	err := f.View(t.Context(), func(r kv.Reader) error {
		got = call(r)

		return nil
	})
	if err != nil {
		return err
	}

	return got
}

// getMany reads a batch without inside, which a rule on inside must pass,
// then one with it, and returns that batch's error. The first batch's error
// is returned without its chain, so a rule wrongly firing on it never reads
// as the injected error.
func getMany(r kv.Reader) error {
	if _, err := r.GetMany([][]byte{seek}); err != nil {
		return fmt.Errorf("a batch without the rule's key failed: %v", err)
	}

	_, err := r.GetMany([][]byte{inside, seek})

	return err
}

// firstScanErr is the error of the first pair a scan of prefix yields.
func firstScanErr(r kv.Reader) error {
	for _, err := range r.Scan(prefix, nil) {
		return err
	}

	return nil
}

// txnCalls are the operations a Txn offers, each with the key it consults
// with: the key for get, versions, set, delete, guard and before, the prefix
// for scan and guard_range.
var txnCalls = []opCall{
	{
		name: "update/get",
		op:   kvtest.OpGet,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				_, err := tx.Get(inside)

				return err
			})
		},
	},
	{
		name: "update/getmany",
		op:   kvtest.OpGet,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				return getMany(tx)
			})
		},
	},
	{
		name: "update/versions",
		op:   kvtest.OpVersions,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				_, err := tx.Versions([][]byte{inside})

				return err
			})
		},
	},
	{
		name: "update/scan",
		op:   kvtest.OpScan,
		key:  prefix,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				return firstScanErr(tx)
			})
		},
	},
	{
		name: "update/before",
		op:   kvtest.OpBefore,
		key:  seek,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				_, _, err := tx.Before(prefix, seek)

				return err
			})
		},
	},
	{
		name: "update/set",
		op:   kvtest.OpSet,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				return tx.Set(inside, []byte("w"))
			})
		},
	},
	{
		name: "update/delete",
		op:   kvtest.OpDelete,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				return tx.Delete(inside)
			})
		},
	},
	{
		name: "update/guard",
		op:   kvtest.OpGuard,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				return tx.Guard(inside)
			})
		},
	},
	{
		name: "update/guard_range",
		op:   kvtest.OpGuardRange,
		key:  prefix,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inUpdate(t, f, func(tx kv.Txn) error {
				return tx.GuardRange(prefix)
			})
		},
	},
}

// viewCalls are the reads inside a View, which consult under the same names
// as inside an Update.
var viewCalls = []opCall{
	{
		name: "view/get",
		op:   kvtest.OpGet,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inView(t, f, func(r kv.Reader) error {
				_, err := r.Get(inside)

				return err
			})
		},
	},
	{
		name: "view/getmany",
		op:   kvtest.OpGet,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inView(t, f, func(r kv.Reader) error {
				return getMany(r)
			})
		},
	},
	{
		name: "view/versions",
		op:   kvtest.OpVersions,
		key:  inside,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inView(t, f, func(r kv.Reader) error {
				_, err := r.Versions([][]byte{inside})

				return err
			})
		},
	},
	{
		name: "view/scan",
		op:   kvtest.OpScan,
		key:  prefix,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inView(t, f, func(r kv.Reader) error {
				return firstScanErr(r)
			})
		},
	},
	{
		name: "view/before",
		op:   kvtest.OpBefore,
		key:  seek,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return inView(t, f, func(r kv.Reader) error {
				_, _, err := r.Before(prefix, seek)

				return err
			})
		},
	},
}

// The whole-call operations, which consult with an empty key.
var callCalls = []opCall{
	{
		name: "update",
		op:   kvtest.OpUpdate,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return put(t.Context(), f, outside)
		},
	},
	{
		name: "view",
		op:   kvtest.OpView,
		run: func(t *testing.T, f *kvtest.FaultKV) error {
			return f.View(t.Context(), func(kv.Reader) error {
				return nil
			})
		},
	},
}

// withPrefixData is a store holding one key under prefix, so a scan or a
// before that is not failed yields a record and no error.
func withPrefixData(t *testing.T) kv.KV {
	t.Helper()

	mem := memStore(t)

	if err := put(t.Context(), mem, inside); err != nil {
		t.Fatal(err)
	}

	return mem
}

// ruleFor is a rule matching call's op on the key it consults with.
func ruleFor(c opCall, a sim.FaultAction) sim.Rule {
	key := "*"

	if c.key != nil {
		key = keys.Describe(c.key)
	}

	return sim.Rule{
		Op:     c.op,
		Key:    key,
		Action: a,
	}
}

// Every operation inside a transaction, in an Update or a View, consults the
// fault with the key it acts on, and an injected error reaches its caller
// unchanged; the same call with no fault set succeeds, so the error is the
// injected one.
func TestFaultErrorOnEachTxnOp(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range slices.Concat(txnCalls, viewCalls) {
		t.Run(c.name, func(t *testing.T) {
			f := kvtest.NewFaultKV(withPrefixData(t))

			if err := c.run(t, f); err != nil {
				t.Fatalf("with no fault: %v", err)
			}

			f.SetFault(schedule(t, nil, ruleFor(c, sim.FaultAction{Err: errInjected})))

			if err := c.run(t, f); !errors.Is(err, errInjected) {
				t.Errorf("returned %v, want the injected error", err)
			}
		})
	}
}

// A scan consults when its iteration starts, not when it is made: a scan
// made and never iterated does not count in a rule, so a rule that fails the
// first scan fails the first one iterated. That scan yields the error as its
// first pair, with no record, and nothing after it, though a record is under
// the prefix.
func TestFaultScanErrorIsFirstPair(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := kvtest.NewFaultKV(withPrefixData(t))
	f.SetFault(schedule(t, nil, sim.Rule{
		Op:     kvtest.OpScan,
		Key:    keys.Describe(prefix),
		Count:  1,
		Action: sim.FaultAction{Err: errInjected},
	}))

	err := inView(t, f, func(r kv.Reader) error {
		_ = r.Scan(prefix, nil)

		pairs := 0

		for kvp, err := range r.Scan(prefix, nil) {
			pairs++

			if pairs == 1 && (!errors.Is(err, errInjected) || kvp.Key != nil || kvp.Value != nil) {
				t.Errorf("first pair (%q, %v), want no record and the injected error", kvp.Key, err)
			}
		}

		if pairs != 1 {
			t.Errorf("the failed scan yielded %d pairs, want 1", pairs)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// panicOf runs call and returns what it panicked with, or nil.
func panicOf(t *testing.T, f *kvtest.FaultKV, c opCall) (r any) {
	defer func() {
		r = recover()
	}()

	_ = c.run(t, f)

	return nil
}

// names reports whether msg holds op as a whole name, so that guard is not
// found inside guard_range, nor get inside a longer word.
func names(msg, op string) bool {
	return regexp.MustCompile(`(^|[^a-z_])` + regexp.QuoteMeta(op) + `([^a-z_]|$)`).MatchString(msg)
}

// A result the wrapper cannot apply is a mistake in the test, and panics
// naming the op: any tear or loss on any op, and a bare conflict where the kv
// contract says none reaches a caller (update, view, and every read inside a
// View).
func TestFaultRefusesWhatItCannotApply(t *testing.T) {
	prof.Watch(t, prof.Options{})

	type refused struct {
		name   string
		call   opCall
		kind   string // the result the panic must name besides the op
		action sim.FaultAction
	}

	var cases []refused

	for _, c := range slices.Concat(txnCalls, viewCalls, callCalls) {
		cases = append(cases,
			refused{
				name: c.name + "/tear",
				call: c,
				kind: "tear",
				action: sim.FaultAction{
					TearAfter: 1,
				},
			},
			refused{
				name: c.name + "/lose",
				call: c,
				kind: "loss",
				action: sim.FaultAction{
					TearAfter: sim.Lose,
				},
			},
		)
	}

	for _, c := range slices.Concat(viewCalls, callCalls) {
		cases = append(cases, refused{
			name: c.name + "/conflict",
			call: c,
			kind: "conflict",
			action: sim.FaultAction{
				Err: kv.ErrConflict,
			},
		})
	}

	// A conflict wrapped in another error is still one, as a backend that maps
	// its own retryable errors returns it.
	for _, c := range callCalls {
		if c.op != kvtest.OpView {
			continue
		}

		cases = append(cases, refused{
			name: c.name + "/conflict-wrapped",
			call: c,
			kind: "conflict",
			action: sim.FaultAction{
				Err: fmt.Errorf("backend: %w", kv.ErrConflict),
			},
		})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := kvtest.NewFaultKV(withPrefixData(t))
			f.SetFault(schedule(t, nil, ruleFor(c.call, c.action)))

			r := panicOf(t, f, c.call)

			if r == nil {
				t.Fatal("did not panic")
			}

			msg := fmt.Sprint(r)

			if !names(msg, c.call.op) || !strings.Contains(msg, c.kind) {
				t.Errorf(
					"panicked with %q, which does not name both %s and the %s",
					msg,
					c.call.op,
					c.kind,
				)
			}
		})
	}
}

// SetFault(nil) drops a failing schedule: the next call passes.
func TestFaultSetNilPasses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := kvtest.NewFaultKV(memStore(t))
	f.SetFault(schedule(t, nil, sim.Rule{
		Op:     kvtest.OpUpdate,
		Key:    "*",
		Action: sim.FaultAction{Err: errInjected},
	}))

	if err := put(t.Context(), f, outside); !errors.Is(err, errInjected) {
		t.Fatalf("with the schedule set: %v, want the injected error", err)
	}

	f.SetFault(nil)

	if err := put(t.Context(), f, outside); err != nil {
		t.Fatalf("after SetFault(nil): %v", err)
	}
}

// SetFault may be called while calls run: under -race, swapping the fault
// beside Updates and Views is no race, and a schedule that never fires fails
// none of them.
func TestFaultSetFaultConcurrent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := kvtest.NewFaultKV(memStore(t))
	never := schedule(t, nil, sim.Rule{
		Op:     "never",
		Key:    "*",
		Action: sim.FaultAction{Err: errInjected},
	})

	var wg sync.WaitGroup
	errs := make(chan error, 8)

	for i := range 4 {
		wg.Go(func() {
			key := []byte{byte('a' + i)}

			for range 50 {
				if err := put(t.Context(), f, key); err != nil {
					errs <- err

					return
				}

				if err := inView(t, f, func(r kv.Reader) error {
					_, err := r.Get(key)

					return err
				}); err != nil {
					errs <- err

					return
				}
			}
		})
	}

	for i := range 200 {
		if i%2 == 0 {
			f.SetFault(never)
		} else {
			f.SetFault(nil)
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}
