package vfs

import (
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/marmos91/dittofs/internal/vfs/lockorder"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/sim"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// fixture is a built filesystem over an in-memory store, a fake engine and an
// open-state table the test started as the node does: with the filesystem as
// its Sink and Admission as its Order.
type fixture struct {
	*Filesystem
	tab *openstate.Table
	fs  *kvstoretest.FS
}

func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}

func newFixture(t *testing.T, events vfs.Events) *fixture {
	t.Helper()
	return newFixtureOf(t, Deps{Events: events})
}

// newFixtureOf is newFixture over d's Events and Metrics.
func newFixtureOf(t *testing.T, d Deps) *fixture {
	t.Helper()
	fs := kvstoretest.NewFS(t, memKV)
	tab, err := openstate.New(openstate.Deps{Records: fs.Store, Clock: sim.NewTestClock(time.Unix(0, 0)), Rand: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	d.Meta, d.Open, d.Engine = fs.Store, tab, newFakeContent()
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Start(t.Context(), s, s.Admission); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tab.Stop)
	return &fixture{Filesystem: s, tab: tab, fs: fs}
}

// newFilesystem is a serving filesystem, drained and stopped when the test
// ends.
func newFilesystem(t *testing.T) *Filesystem {
	t.Helper()
	f := newFixture(t, nil)
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.Drain(context.Background())
		_ = f.Stop()
	})
	return f.Filesystem
}

// startedHere matches a goroutine running the filesystem's own code, a frame
// in one of its non-test files, whatever started it.
var startedHere = regexp.MustCompile(`goroutine \d+ \[[^\n]*\n(?:[^\n]+\n)*?[^\n]*/internal/vfs/(?:[a-z]+/)*[a-z]+\.go:`)

// filesystemGoroutines is how many goroutines the filesystem started are
// running.
func filesystemGoroutines() int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	return len(startedHere.FindAll(buf, -1))
}

// settles reports whether filesystemGoroutines reaches want within a second.
func settles(want int) bool {
	for range 100 {
		if filesystemGoroutines() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// callAll calls every Service method of s with zero arguments, but one
// reference in each list, and answers each one's errors by method name,
// nil included.
func callAll(s vfs.Service) map[string][]error {
	got := map[string][]error{}
	v := reflect.ValueOf(s)
	ctx := reflect.ValueOf(context.Background())
	it := reflect.TypeFor[vfs.Service]()
	for i := range it.NumMethod() {
		name := it.Method(i).Name
		m := v.MethodByName(name)
		args := []reflect.Value{ctx}
		for j := 1; j < m.Type().NumIn(); j++ {
			in := m.Type().In(j)
			a := reflect.Zero(in)
			if in.Kind() == reflect.Slice && in.Elem().Kind() == reflect.Struct {
				a = reflect.MakeSlice(in, 1, 1)
			}
			args = append(args, a)
		}
		for _, r := range m.Call(args) {
			switch r.Type() {
			case reflect.TypeFor[error]():
				err, _ := r.Interface().(error)
				got[name] = append(got[name], err)
			case reflect.TypeFor[[]error]():
				got[name] = append(got[name], r.Interface().([]error)...)
			}
		}
	}
	return got
}

// Neither New nor Start starts a goroutine; Start starts the filesystem once.
func TestNewStartsNothingAndStartRunsOnce(t *testing.T) {
	f := newFixture(t, nil)
	if n := filesystemGoroutines(); n != 0 {
		t.Fatalf("New started %d goroutines", n)
	}
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(t.Context()); err == nil {
		t.Fatal("a serving filesystem started again")
	}
	if n := filesystemGoroutines(); n != 0 {
		t.Errorf("the filesystem runs %d goroutines after Start, want none", n)
	}
	_ = f.Drain(t.Context())
	_ = f.Stop()
}

// After Stop none of the filesystem's goroutines run, and the store and open
// state it was handed still serve a direct call.
func TestStopLeavesNoGoroutineAndClosesNothing(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err != nil {
		t.Fatal(err)
	}
	if !settles(0) {
		t.Errorf("%d of the filesystem's goroutines run after Stop", filesystemGoroutines())
	}
	if _, err := f.tab.Clients().AddSession(t.Context(), 1, entity.NodeID{}, nil, nil); errors.Is(err, openstate.ErrStopped) {
		t.Errorf("open state is stopped after the filesystem's Stop: %v", err)
	}
	if _, err := f.fs.Store.Files().Get(t.Context(), f.fs.Admin, f.fs.Root); err != nil {
		t.Errorf("the store no longer serves after the filesystem's Stop: %v", err)
	}
}

// The filesystem moves through built, serving, draining and stopped once each,
// in that order; Stop also ends a filesystem never started.
func TestTheLifecycleRunsOnceInOrder(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.Drain(t.Context()); err == nil {
		t.Error("a built filesystem drained")
	}
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err == nil {
		t.Error("a serving filesystem stopped without draining")
	}
	if err := f.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Drain(t.Context()); err == nil {
		t.Error("a draining filesystem drained again")
	}
	if err := f.Start(t.Context()); err == nil {
		t.Error("a draining filesystem started")
	}
	if err := f.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err == nil {
		t.Error("a stopped filesystem stopped again")
	}

	g := newFixture(t, nil)
	if err := g.Stop(); err != nil {
		t.Errorf("a filesystem never started did not stop: %v", err)
	}
	if err := g.Start(t.Context()); err == nil {
		t.Error("a stopped filesystem started")
	}
}

// Drain returns only once every admitted call has answered.
func TestDrainJoinsEveryAdmittedCall(t *testing.T) {
	s := newFilesystem(t)
	var dones []func()
	for range 64 {
		_, done, err := s.enter(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		dones = append(dones, done)
	}
	if n := testutil.ToFloat64(s.inflight); n != 64 {
		t.Errorf("vfs_inflight_calls is %v with 64 calls admitted", n)
	}
	drained := make(chan error)
	go func() { drained <- s.Drain(context.Background()) }()
	for _, done := range dones[:63] {
		done()
	}
	select {
	case err := <-drained:
		t.Fatalf("Drain returned with a call still to answer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	dones[63]()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if n := testutil.ToFloat64(s.inflight); n != 0 {
		t.Errorf("vfs_inflight_calls is %v with every call answered", n)
	}
}

// Before Start, from the moment Drain begins and after Stop, every call is
// refused ErrUnavailable before its body runs.
func TestACallOutsideServingIsUnavailable(t *testing.T) {
	f := newFixture(t, nil)
	refused := func(when string) {
		for m, errs := range callAll(f) {
			for _, err := range errs {
				if !errors.Is(err, vfs.ErrUnavailable) || errors.Is(err, errUnbuilt) {
					t.Errorf("%s: %s answered %v, want ErrUnavailable before its body", when, m, err)
				}
			}
		}
	}
	refused("built")
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for m, errs := range callAll(f) {
		for _, err := range errs {
			if !errors.Is(err, errUnbuilt) {
				t.Errorf("serving: %s answered %v, want its body's errUnbuilt", m, err)
			}
		}
	}

	_, done, err := f.enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error)
	go func() { drained <- f.Drain(context.Background()) }()
	for f.inState() != draining {
		runtime.Gosched()
	}
	refused("draining")
	done()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err != nil {
		t.Fatal(err)
	}
	refused("stopped")
}

func (fs *Filesystem) inState() state {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.state
}

// At its context's deadline Drain ends the admitted calls' contexts, and
// still returns only once they have answered.
func TestDrainEndsAdmittedCallsAtItsDeadline(t *testing.T) {
	s := newFilesystem(t)
	ctx, done, err := s.enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	drained := make(chan error)
	go func() { drained <- s.Drain(dctx) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("an admitted call's context outlived Drain's deadline")
	}
	select {
	case err := <-drained:
		t.Fatalf("Drain returned before the call it cancelled answered: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	done()
	if err := <-drained; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain past its deadline answered %v", err)
	}
}

// Drain with nothing admitted answers nil, even on a context already ended,
// whether no call was ever admitted or every admitted call has answered.
func TestDrainWithNothingAdmittedAnswersNil(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 20 {
		f := newFixture(t, nil)
		if err := f.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if i%2 == 1 {
			_, done, err := f.enter(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			done()
		}
		err := f.Drain(ended)
		_ = f.Stop()
		if err != nil {
			t.Fatalf("drain %d of an idle filesystem on an ended context: %v", i, err)
		}
	}
}

// Stop refuses while Drain still waits for an admitted call, and leaves that
// call's context alive; once Drain returns, Stop succeeds.
func TestStopRefusesWhileDrainWaits(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, done, err := f.enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error)
	go func() { drained <- f.Drain(context.Background()) }()
	for f.inState() != draining {
		runtime.Gosched()
	}
	if err := f.Stop(); err == nil {
		t.Error("Stop succeeded while Drain waited for an admitted call")
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("a refused Stop ended an admitted call's context: %v", err)
	}
	done()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(); err != nil {
		t.Errorf("Stop after Drain returned: %v", err)
	}
}

// The filesystem registers its in-flight gauge with the registerer it is
// handed.
func TestNewRegistersTheInflightGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	newFixtureOf(t, Deps{Metrics: reg})
	if n, err := testutil.GatherAndCount(reg, "dittofs_vfs_inflight_calls"); n != 1 {
		t.Errorf("dittofs_vfs_inflight_calls registered %d times (%v), want once", n, err)
	}
}

// Admission holds f's admission lock in the filesystem's own lock tables until
// its unlock.
func TestAdmissionHoldsTheFilesAdmissionLock(t *testing.T) {
	s := newFilesystem(t)
	unlock := s.Admission(entity.FileID{1})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if release, err := s.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{{1}}}); err == nil {
		release()
		t.Error("a call took an admission lock open state holds")
	}
	unlock()
	release, err := s.locks.Take(t.Context(), lockorder.Set{Admit: []lockorder.Key{{1}}})
	if err != nil {
		t.Fatalf("a call could not take an admission lock once open state unlocked it: %v", err)
	}
	release()
}
