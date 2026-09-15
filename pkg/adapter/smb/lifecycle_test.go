package smb

import (
	"context"
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
