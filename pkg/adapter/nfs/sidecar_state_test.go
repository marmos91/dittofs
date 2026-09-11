package nfs

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter/auxsvc"
)

// The lifecycle group reserves the sidecar name under the lock and runs Start
// outside it (Start binds listeners and can block), so a disable can steal the
// reservation mid-Start and tear down concurrently. These hammers Start and
// Stop of the same sidecar; with -race this fails on any unsynchronized
// concurrent access to the adapter's sidecar shutdown state.
func TestUDPSidecarStateConcurrentStartStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &NFSAdapter{sidecars: auxsvc.NewGroup()}
	a.sidecars.SetBaseContext(ctx)
	udpEnabled := true
	a.config.UDP.Enabled = &udpEnabled

	const iterations = 20
	for i := 0; i < iterations; i++ {
		startDone := make(chan struct{})
		go func() {
			defer close(startDone)
			if err := a.startUDP(ctx); err != nil {
				t.Errorf("startUDP: %v", err)
			}
		}()
		if err := (udpSidecar{a}).Stop(context.Background()); err != nil {
			t.Errorf("udpSidecar.Stop: %v", err)
		}
		<-startDone
		// Idempotent: repeat Stop sees the cleared conn and is a no-op.
		if err := (udpSidecar{a}).Stop(context.Background()); err != nil {
			t.Errorf("udpSidecar.Stop (repeat): %v", err)
		}
	}
}

// Same shape for the embedded portmapper: the server is published only once
// WaitReady has fired, so a concurrent disable either sees nothing (no-op —
// the abandoned start tears itself down via its per-start cancellation) or
// claims a ready server (which handles a closed listener) — never a server
// mid-bind.
func TestPortmapSidecarStateConcurrentStartStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &NFSAdapter{sidecars: auxsvc.NewGroup()}
	a.sidecars.SetBaseContext(ctx)
	enabled := true
	a.config.Portmapper.Enabled = &enabled

	const iterations = 20
	for i := 0; i < iterations; i++ {
		startDone := make(chan struct{})
		go func() {
			defer close(startDone)
			if err := a.startPortmapper(ctx); err != nil {
				t.Errorf("startPortmapper: %v", err)
			}
		}()
		if err := (portmapSidecar{a}).Stop(context.Background()); err != nil {
			t.Errorf("portmapSidecar.Stop: %v", err)
		}
		<-startDone
		// Idempotent: repeat stopPortmapper sees the cleared server and is a no-op.
		(a.stopPortmapper())
	}
}
