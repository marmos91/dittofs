package smb

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// TestStop_ResolverUnsubsRaceConfigChange is a -race guard. Stop read and nil'd
// identityUnsub, identityProviderUnsub and foreignSIDProviderUnsub with no
// lock, while wireIdentityResolver and wireForeignSIDResolver write them under
// resolverMu from SetRuntime, SetKerberosProvider and an identity-provider
// config change — all on other goroutines. A config change concurrent with
// shutdown is therefore a data race on all three.
//
// Run without -race this test only checks that both paths complete; the
// detector is what makes it a guard.
func TestStop_ResolverUnsubsRaceConfigChange(t *testing.T) {
	for range 4 {
		a := New(Config{})
		rt := runtime.New(nil)

		// Stands in for wireIdentityResolver's locked writes: a Kerberos
		// provider is not needed to reproduce the unlocked read on the other
		// side, and the writers' discipline is exactly this — hold resolverMu,
		// assign the field.
		writer := make(chan struct{})
		go func() {
			defer close(writer)
			a.resolverMu.Lock()
			a.identityUnsub = func() {}
			a.identityProviderUnsub = func() {}
			a.foreignSIDProviderUnsub = func() {}
			a.resolverMu.Unlock()
			// The real writer, which takes the same lock and assigns
			// foreignSIDProviderUnsub when a pipe manager is configured.
			a.wireForeignSIDResolver(rt)
			// shareUnsubscribers is the same class in the same function: it is
			// appended by SetRuntime and read by Stop, and it is the slice that
			// carries the auth-cache-invalidate subscription whose removal is
			// what keeps a sweep from being queued during teardown. Recorded
			// last so no other lock hold orders it against Stop's read.
			a.addShareUnsubscriber(func() {})
		}()

		stopper := make(chan struct{})
		go func() {
			defer close(stopper)
			if err := a.Stop(context.Background()); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}()

		<-writer
		<-stopper
	}
}

// TestStop_JoinsScavenger pins the durable-handle scavenger's missing
// join: the goroutine ended only when its context was cancelled, and nothing
// waited for it, so it could outlive Stop and touch DurableStore and the
// handler during teardown.
func TestStop_JoinsScavenger(t *testing.T) {
	a := New(Config{})

	started := make(chan struct{})
	var returned atomic.Bool
	a.startScavenger(context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		// Model the teardown work the scavenger does after its context ends:
		// an unjoined Stop returns straight through this window.
		time.Sleep(50 * time.Millisecond)
		returned.Store(true)
	})
	<-started

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !returned.Load() {
		t.Fatal("Stop returned while the scavenger loop was still running")
	}

	a.resolverMu.Lock()
	cancel := a.scavengerCancel
	a.resolverMu.Unlock()
	if cancel != nil {
		t.Error("Stop left the scavenger loop's cancel attached")
	}
}

// TestStop_WithNoScavenger covers an adapter stopped before Serve ever
// started one: the join must be a no-op rather than a hang.
func TestStop_WithNoScavenger(t *testing.T) {
	a := New(Config{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung with no scavenger loop running")
	}
}

// TestStop_BoundedByContext is the twin guard at the adapter level: a scavenger
// that ignores its cancellation must not turn Stop into a hang. The adapters
// service stops each adapter serially, so one that never returns strands the
// whole shutdown.
//
// It also pins what a shutdown that gives up must NOT do. Logging and carrying
// on converts a hang into a use-after-teardown, because the runtime closes the
// metadata stores once the adapters have stopped — so the failure has to come
// back as an error, and the worker handle has to stay attached for a later Stop
// to join.
func TestStop_BoundedByContext(t *testing.T) {
	a := New(Config{})

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	started := make(chan struct{})
	a.startScavenger(context.Background(), func(context.Context) {
		close(started)
		<-stuck
	})
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Stop(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Errorf("Stop error = %v, want one wrapping ErrShutdownIncomplete: a nil or "+
				"unrelated error reads as \"nothing of mine is still running\"", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung on a scavenger that ignored its cancellation")
	}

	a.resolverMu.Lock()
	cancelFn := a.scavengerCancel
	a.resolverMu.Unlock()
	if cancelFn == nil {
		t.Error("Stop detached the scavenger handle after a failed join; a later Stop cannot join it")
	}
}

// TestStop_NilContextIsBounded: BaseAdapter.Stop treats a nil ctx as "use the
// configured shutdown timeout", and the joins have to agree — otherwise the one
// entry point that is documented as bounded is the one that hangs forever.
func TestStop_NilContextIsBounded(t *testing.T) {
	a := New(Config{Timeouts: TimeoutsConfig{Shutdown: 200 * time.Millisecond}})

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	started := make(chan struct{})
	a.startScavenger(context.Background(), func(context.Context) {
		close(started)
		<-stuck
	})
	<-started

	done := make(chan struct{})
	// The nil context is the subject of this test: Stop documents it as
	// "use the configured shutdown timeout", and that contract is what broke.
	//nolint:staticcheck // SA1012: passing nil is the case under test
	go func() { defer close(done); _ = a.Stop(nil) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop(nil) hung: the joins ignored the configured shutdown timeout")
	}
}

// TestStop_SweepJoinFailureKeepsWorker is the sweep-side twin of the assertions
// in TestStop_BoundedByContext.
func TestStop_SweepJoinFailureKeepsWorker(t *testing.T) {
	a := New(Config{})
	rt := runtime.New(nil)
	a.SetRuntime(rt)

	a.resolverMu.Lock()
	wired := a.authSweep
	a.resolverMu.Unlock()
	wired.stop(context.Background())

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	started := make(chan struct{})
	a.resolverMu.Lock()
	a.authSweep = newAuthSweeper(func(context.Context) { close(started); <-stuck })
	a.authSweep.request()
	a.resolverMu.Unlock()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := a.Stop(ctx)
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Errorf("Stop error = %v, want one wrapping ErrShutdownIncomplete", err)
	}

	a.resolverMu.Lock()
	remaining := a.authSweep
	a.resolverMu.Unlock()
	if remaining == nil {
		t.Error("Stop detached the sweep worker after a failed join")
	}
}

// TestWiringRefusedAfterStop closes the other half of the stopping fence. The
// identity-provider notifier snapshots its callbacks before invoking them, so
// unsubscribing does not recall one already in flight: it can reach the wiring
// functions after Stop has drained the subscriptions and register a fresh one
// that nothing will ever remove.
func TestWiringRefusedAfterStop(t *testing.T) {
	a := New(Config{})
	rt := runtime.New(nil)
	a.SetRuntime(rt)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	a.wireForeignSIDResolver(rt)
	a.wireIdentityResolver(rt)
	a.wireNetlogonReload(rt)

	a.resolverMu.Lock()
	defer a.resolverMu.Unlock()
	// The foreign-SID path is the one this fixture actually drives to the
	// subscription: an adapter built from a bare Config has a pipe manager but
	// no Kerberos provider and no netlogon authenticator, so the other two
	// return at their own guards here whether or not the fence exists. They are
	// asserted anyway because they carry the identical check ahead of those
	// guards, and a fence removed from one of them should still show up.
	if a.foreignSIDProviderUnsub != nil {
		t.Error("wireForeignSIDResolver subscribed after Stop")
	}
	if a.identityUnsub != nil || a.identityProviderUnsub != nil {
		t.Error("wireIdentityResolver subscribed after Stop")
	}
	if a.netlogonProviderUnsub != nil {
		t.Error("wireNetlogonReload subscribed after Stop")
	}
}

// TestStartScavenger_RefusedAfterStop closes the Add-after-Wait window: a Serve
// still in its prologue when shutdown lands would otherwise have the WaitGroup
// counter rise from zero while Stop is already waiting on it, and would leave
// behind a cancel Stop had read past.
func TestStartScavenger_RefusedAfterStop(t *testing.T) {
	a := New(Config{})
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	ran := make(chan struct{})
	a.startScavenger(context.Background(), func(context.Context) { close(ran) })

	select {
	case <-ran:
		t.Fatal("startScavenger ran a loop after Stop")
	case <-time.After(200 * time.Millisecond):
	}

	a.resolverMu.Lock()
	cancel := a.scavengerCancel
	a.resolverMu.Unlock()
	if cancel != nil {
		t.Error("a refused start still published its cancel")
	}
}
