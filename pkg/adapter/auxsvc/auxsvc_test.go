package auxsvc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeService is a controllable Service for exercising Group.
type fakeService struct {
	name      string
	startErr  error
	stopErr   error
	started   atomic.Bool
	stopped   atomic.Bool
	startCtx  context.Context
	stopCalls atomic.Int32
	onStop    func() // optional hook invoked inside Stop
}

func (f *fakeService) Name() string { return f.name }

func (f *fakeService) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.startCtx = ctx
	f.started.Store(true)
	return nil
}

func (f *fakeService) Stop(ctx context.Context) error {
	f.stopCalls.Add(1)
	f.stopped.Store(true)
	if f.onStop != nil {
		f.onStop()
	}
	return f.stopErr
}

// blockingService is a fakeService whose Start parks until released, so a test
// can hold a Start call in flight while other Group operations run.
type blockingService struct {
	name         string
	startErr     error
	release      chan struct{}
	startEntered chan struct{}

	started   atomic.Bool
	stopCalls atomic.Int32
}

func (b *blockingService) Name() string { return b.name }

func (b *blockingService) Stop(ctx context.Context) error {
	b.stopCalls.Add(1)
	return nil
}

func (b *blockingService) Start(ctx context.Context) error {
	b.started.Store(true)
	// Signal entry, then park until the test releases the Start call; the
	// injected error (if any) is returned after the park, so a failure can be
	// observed as a live reservation before it rolls back.
	close(b.startEntered)
	<-b.release
	return b.startErr
}

func TestGroup_StartBlockedDoesNotWedgeOtherOps(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())

	blocking := &blockingService{name: "slow", release: make(chan struct{}), startEntered: make(chan struct{})}
	other := &fakeService{name: "other"}
	if err := g.Start(other); err != nil {
		t.Fatalf("Start(other): %v", err)
	}

	startDone := make(chan error, 1)
	go func() { startDone <- g.Start(blocking) }()
	<-blocking.startEntered // Start is now parked inside s.Start

	// Every other Group op must return promptly while the first Start is wedged.
	opsDone := make(chan struct{})
	go func() {
		defer close(opsDone)
		if !g.Ready() {
			t.Error("Ready() should be true while a Start is in flight")
		}
		if g.IsRunning("never-started") {
			t.Error("IsRunning on an unknown name should be false")
		}
		if err := g.StopOne("other"); err != nil {
			t.Errorf("StopOne(other) while Start is in flight: %v", err)
		}
		// The same-name Start in flight must lose with ErrAlreadyRunning.
		err := g.Start(&fakeService{name: "slow"})
		if !errors.Is(err, ErrAlreadyRunning) {
			t.Errorf("same-name Start during in-flight Start: got %v, want ErrAlreadyRunning", err)
		}
	}()
	select {
	case <-opsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Group ops wedged while a Start was blocked inside s.Start")
	}

	// Release the blocked Start and confirm success-path tracking.
	close(blocking.release)
	if err := <-startDone; err != nil {
		t.Fatalf("blocking Start: %v", err)
	}
	if !g.IsRunning("slow") {
		t.Fatal("service should be tracked after its Start returns")
	}
}

func TestGroup_StartFailureAfterParkUntracks(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())

	// A Start that parks, then fails: the reservation taken while it was in
	// flight must be rolled back once Start returns an error.
	failing := &blockingService{name: "fails", startErr: errors.New("bind failed"), release: make(chan struct{}), startEntered: make(chan struct{})}
	startDone := make(chan error, 1)
	go func() {
		startDone <- g.Start(failing)
	}()
	<-failing.startEntered // parked inside s.Start, reservation is in the map

	// While parked, the name is reserved: a duplicate Start must lose.
	if err := g.Start(&fakeService{name: "fails"}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("same-name Start during parked Start: got %v, want ErrAlreadyRunning", err)
	}

	close(failing.release) // the parked Start now fails via the injected error
	if err := <-startDone; err == nil {
		t.Fatal("expected start error")
	}
	if g.IsRunning("fails") {
		t.Fatal("failed service must not stay tracked after rollback")
	}
	// The name is free again: a retry succeeds cleanly.
	if err := g.Start(&fakeService{name: "fails"}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestGroup_StartRacingStopAllDoesNotLeakService(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())

	// s.Start parks; StopAll runs while it is in flight and clears the base
	// context. The Start must roll the reservation back and stop the service
	// instead of leaving it tracked by a group that will never tear it down.
	blocking := &blockingService{name: "slow", release: make(chan struct{}), startEntered: make(chan struct{})}
	startDone := make(chan error, 1)
	go func() { startDone <- g.Start(blocking) }()
	<-blocking.startEntered // parked inside s.Start, reservation is in the map

	if err := g.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	close(blocking.release)

	if err := <-startDone; err == nil {
		t.Fatal("Start must fail when the group stopped during the start")
	}
	if g.IsRunning("slow") {
		t.Fatal("service must not stay tracked after the group stopped")
	}
	if got := blocking.stopCalls.Load(); got != 2 {
		t.Fatalf("Stop called %d times after raced shutdown, want 2 (StopAll snapshot + raced rollback)", got)
	}
	// The raced path can Stop a second time when StopAll's snapshot already
	// included the reservation (it does here): Service.Stop must be idempotent.
	// The group is shut down: a fresh reconcile no-ops.
	if g.Ready() {
		t.Fatal("StopAll should have cleared the base context")
	}
}

func TestGroup_StartReservationStolenByStopOneNotLeaked(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())

	// StopOne deletes the reservation while the Start is parked in s.Start;
	// the successfully-started service must not leak: it is either re-tracked
	// or stopped, never left serving outside the group.
	blocking := &blockingService{name: "slow", release: make(chan struct{}), startEntered: make(chan struct{})}
	startDone := make(chan error, 1)
	go func() { startDone <- g.Start(blocking) }()
	<-blocking.startEntered // parked inside s.Start, reservation is in the map

	if err := g.StopOne("slow"); err != nil {
		t.Fatalf("StopOne during in-flight Start: %v", err)
	}
	close(blocking.release)

	err := <-startDone
	if err == nil {
		// The Start believed it succeeded; the group must not have silently
		// dropped it from tracking.
		if !g.IsRunning("slow") {
			t.Fatal("successful Start whose reservation was stolen must stay tracked, not leak")
		}
	} else {
		// The Start reported the theft as a failure; it must have stopped the
		// service it had already bound.
		if got := blocking.stopCalls.Load(); got < 1 {
			t.Fatalf("failed Start must stop the service it started, Stop calls = %d", got)
		}
		if g.IsRunning("slow") {
			t.Fatal("failed Start must not leave the service tracked")
		}
		_ = err
	}
}

func TestGroup_StartRequiresBaseContext(t *testing.T) {
	g := NewGroup()
	if err := g.Start(&fakeService{name: "a"}); err == nil {
		t.Fatal("expected error starting before SetBaseContext")
	}
}

func TestGroup_StartTracksAndBindsBaseContext(t *testing.T) {
	g := NewGroup()
	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "base")
	g.SetBaseContext(base)

	svc := &fakeService{name: "portmapper"}
	if err := g.Start(svc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !g.IsRunning("portmapper") {
		t.Fatal("service should be running")
	}
	if !svc.started.Load() {
		t.Fatal("Start not called")
	}
	if svc.startCtx.Value(ctxKey{}) != "base" {
		t.Fatal("service was not started with the group's base context")
	}
}

func TestGroup_StartDuplicateNameFails(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	if err := g.Start(&fakeService{name: "dup"}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := g.Start(&fakeService{name: "dup"}); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestGroup_StartErrorNotTracked(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	err := g.Start(&fakeService{name: "boom", startErr: errors.New("bind failed")})
	if err == nil {
		t.Fatal("expected start error")
	}
	if g.IsRunning("boom") {
		t.Fatal("failed service must not be tracked")
	}
}

func TestGroup_StopOneIdempotent(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	svc := &fakeService{name: "x"}
	_ = g.Start(svc)

	if err := g.StopOne("x"); err != nil {
		t.Fatalf("StopOne: %v", err)
	}
	if g.IsRunning("x") {
		t.Fatal("service should be stopped")
	}
	// Second StopOne is a no-op.
	if err := g.StopOne("x"); err != nil {
		t.Fatalf("StopOne (repeat): %v", err)
	}
	if got := svc.stopCalls.Load(); got != 1 {
		t.Fatalf("Stop called %d times, want 1", got)
	}
}

func TestGroup_StopAllReverseOrder(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())

	var mu sync.Mutex
	var stopOrder []string
	mk := func(name string) *fakeService {
		return &fakeService{name: name, onStop: func() {
			mu.Lock()
			stopOrder = append(stopOrder, name)
			mu.Unlock()
		}}
	}
	// Start order mirrors NFS: portmapper, sysreg, nfs-udp, nsm.
	for _, s := range []*fakeService{mk("portmapper"), mk("sysreg"), mk("nfs-udp"), mk("nsm")} {
		if err := g.Start(s); err != nil {
			t.Fatalf("Start %s: %v", s.name, err)
		}
	}

	if err := g.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}

	want := []string{"nsm", "nfs-udp", "sysreg", "portmapper"}
	if len(stopOrder) != len(want) {
		t.Fatalf("stopped %v, want %v", stopOrder, want)
	}
	for i := range want {
		if stopOrder[i] != want[i] {
			t.Fatalf("stop order %v, want %v", stopOrder, want)
		}
	}
	// The important invariant: sysreg unregisters before portmapper stops.
	if idx(stopOrder, "sysreg") > idx(stopOrder, "portmapper") {
		t.Fatal("sysreg must stop before portmapper")
	}
}

func TestGroup_StopAllReturnsFirstError(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	_ = g.Start(&fakeService{name: "ok1"})
	_ = g.Start(&fakeService{name: "bad", stopErr: errors.New("teardown failed")})
	_ = g.Start(&fakeService{name: "ok2"})

	if err := g.StopAll(context.Background()); err == nil {
		t.Fatal("expected StopAll to surface a Stop error")
	}
	if g.IsRunning("ok1") || g.IsRunning("bad") || g.IsRunning("ok2") {
		t.Fatal("StopAll must clear all services even when one errors")
	}
}

// TestGroup_StopOneFromCallbackWhileOthersRun models a live settings toggle
// disabling one service (from a callback goroutine) while another keeps running.
func TestGroup_StopOneFromCallbackWhileOthersRun(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	keep := &fakeService{name: "mdns"}
	drop := &fakeService{name: "wsd"}
	_ = g.Start(keep)
	_ = g.Start(drop)

	done := make(chan struct{})
	go func() { // simulate the settings-watcher callback goroutine
		_ = g.StopOne("wsd")
		close(done)
	}()
	<-done

	if g.IsRunning("wsd") {
		t.Fatal("wsd should be stopped")
	}
	if !g.IsRunning("mdns") {
		t.Fatal("mdns should still be running")
	}
}

func TestGroup_ReconcileStartsAndStops(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	svc := &fakeService{name: "mdns"}

	if err := g.Reconcile("mdns", true, func(context.Context) Service { return svc }); err != nil {
		t.Fatalf("Reconcile(start): %v", err)
	}
	if !g.IsRunning("mdns") {
		t.Fatal("Reconcile(want=true) should have started the service")
	}
	if err := g.Reconcile("mdns", false, func(context.Context) Service { return svc }); err != nil {
		t.Fatalf("Reconcile(stop): %v", err)
	}
	if g.IsRunning("mdns") {
		t.Fatal("Reconcile(want=false) should have stopped the service")
	}
}

func TestGroup_ReconcilePassesBaseContextToBuilder(t *testing.T) {
	type key struct{}
	base := context.WithValue(context.Background(), key{}, "seeded")

	g := NewGroup()
	g.SetBaseContext(base)

	var got context.Context
	if err := g.Reconcile("mdns", true, func(ctx context.Context) Service {
		got = ctx
		return &fakeService{name: "mdns"}
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got == nil || got.Value(key{}) != "seeded" {
		t.Fatal("Reconcile must hand the builder the base context, not a fresh one")
	}
}

func TestGroup_ReconcileNoOpBeforeReady(t *testing.T) {
	g := NewGroup() // no SetBaseContext
	built := false
	if err := g.Reconcile("mdns", true, func(context.Context) Service { built = true; return &fakeService{name: "mdns"} }); err != nil {
		t.Fatalf("Reconcile before Ready should be a no-op, got %v", err)
	}
	if built || g.IsRunning("mdns") {
		t.Fatal("Reconcile must not build or start a service before SetBaseContext")
	}
}

func TestGroup_StopAllClearsBaseContextSoLateReconcileNoOps(t *testing.T) {
	g := NewGroup()
	g.SetBaseContext(context.Background())
	_ = g.Start(&fakeService{name: "a"})

	if err := g.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if g.Ready() {
		t.Fatal("StopAll should clear the base context so Ready() is false")
	}
	// A live reconcile racing shutdown must not start a service the now-stopped
	// group would never tear down.
	built := false
	if err := g.Reconcile("b", true, func(context.Context) Service { built = true; return &fakeService{name: "b"} }); err != nil {
		t.Fatalf("post-StopAll Reconcile: %v", err)
	}
	if built || g.IsRunning("b") {
		t.Fatal("post-StopAll Reconcile must no-op")
	}
}

func idx(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
