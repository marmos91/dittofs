package nfs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
)

// fakeSysreg stands in for the host rpcbind. It records the ordered sequence of
// register/unregister operations and can hold either one open, so a test can
// arrange an overlap instead of waiting for one to happen.
type fakeSysreg struct {
	mu      sync.Mutex
	held    map[xdr.Mapping]bool
	events  []string
	entered chan struct{} // closed once Register has applied its first mapping
	release chan struct{} // closed by the test to let Register finish
	// unregEntered is closed when Unregister is reached, and unregRelease lets
	// it delete. Holding the deletion open is what makes the ordering
	// deterministic: a registration that resumes after the unregistration has
	// been entered leaves its later mappings behind in the event log, which is
	// the leak, without the test betting on goroutine scheduling.
	unregEntered chan struct{}
	unregRelease chan struct{}
}

func newFakeSysreg() *fakeSysreg {
	return &fakeSysreg{
		held:         map[xdr.Mapping]bool{},
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
		unregEntered: make(chan struct{}),
		unregRelease: make(chan struct{}),
	}
}

func (f *fakeSysreg) Ping(context.Context, string) error { return nil }

func (f *fakeSysreg) Register(ctx context.Context, _ string, mappings []*xdr.Mapping) error {
	for i, m := range mappings {
		f.mu.Lock()
		f.held[*m] = true
		f.events = append(f.events, "reg")
		f.mu.Unlock()

		// Hold the registration open after its first mapping so the shutdown
		// below is issued while it is genuinely in flight. A fake that returned
		// immediately would let the shutdown land after it every time, and the
		// test would pass on a build with no serialisation at all.
		if i == 0 {
			close(f.entered)
			select {
			case <-f.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func (f *fakeSysreg) Unregister(ctx context.Context, _ string, mappings []*xdr.Mapping) error {
	f.mu.Lock()
	f.events = append(f.events, "unreg")
	f.mu.Unlock()

	close(f.unregEntered)
	select {
	case <-f.unregRelease:
	case <-ctx.Done():
		return ctx.Err()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range mappings {
		delete(f.held, *m)
	}
	return nil
}

func (f *fakeSysreg) snapshot() (held int, events []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.held), append([]string(nil), f.events...)
}

// newSysregRaceAdapter returns an adapter with system registration enabled and
// the given registrar injected.
func newSysregRaceAdapter(reg sysregRegistrar) *NFSAdapter {
	enabled := true
	a := &NFSAdapter{sysregAddr: "127.0.0.1:111", sysregRegistrar: reg}
	a.config.Portmapper.RegisterWithSystem = &enabled
	return a
}

// A shutdown landing while a registration is in flight must not leave mappings
// registered. The two are serialised, so the shutdown runs entirely before the
// registration (unregistering nothing) or entirely after it (unregistering
// everything that landed). Letting them interleave is how Register's remaining
// SETs land after the Unregister and leak with nothing left to clean them up.
//
// The assertion is that the shutdown cannot reach rpcbind while Register holds
// the lock, plus the consequence: no mapping lands after the unregistration.
// Both are checked with Register provably mid-flight, so a build with no
// serialisation fails here rather than passing on a lucky ordering.
func TestSysregShutdownRacingRegistrationLeavesNothingRegistered(t *testing.T) {
	fake := newFakeSysreg()
	a := newSysregRaceAdapter(fake)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.startSystemPortmapRegistration(context.Background())
	}()

	// Issue the shutdown only once Register is provably mid-flight and holding
	// the lock, so this exercises the overlap rather than a lucky ordering.
	<-fake.entered
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		a.stopSystemPortmapRegistration()
	}()

	// While Register holds the lock the shutdown must not reach rpcbind. Give it
	// every chance to: an unserialised build gets there promptly, and reaching it
	// is the defect. The fixed build blocks on the lock, so the wait times out
	// and the ordering check below still runs.
	select {
	case <-fake.unregEntered:
		t.Fatal("shutdown reached rpcbind while a registration was still in flight")
	case <-time.After(250 * time.Millisecond):
	}

	// Let Register finish its remaining mappings, then let the shutdown run.
	close(fake.release)
	<-done
	close(fake.unregRelease)
	<-stopped

	held, events := fake.snapshot()
	if held != 0 {
		t.Fatalf("%d mappings left registered after shutdown: %v", held, events)
	}

	// Every register must precede the unregister. A "reg" after the "unreg" is
	// the interleaving that leaks.
	seenUnreg := false
	for _, e := range events {
		if e == "unreg" {
			seenUnreg = true
			continue
		}
		if seenUnreg {
			t.Fatalf("registration interleaved with the shutdown: %v", events)
		}
	}
}

// A shutdown with nothing registered is a no-op and must not call rpcbind.
func TestSysregShutdownWithoutRegistrationDoesNotUnregister(t *testing.T) {
	fake := newFakeSysreg()
	a := newSysregRaceAdapter(fake)

	a.stopSystemPortmapRegistration()

	held, events := fake.snapshot()
	if held != 0 || len(events) != 0 {
		t.Fatalf("shutdown touched rpcbind with nothing registered: held=%d events=%v", held, events)
	}
}
