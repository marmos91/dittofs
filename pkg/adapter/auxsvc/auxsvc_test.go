package auxsvc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
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

func idx(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
