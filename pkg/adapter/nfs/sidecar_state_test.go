package nfs

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

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

// Closing the UDP socket only unblocks the read: the loop and every datagram
// handler it spawned are still running, and they read adapter and runtime state
// that the rest of teardown is about to dismantle. Stop must not report the
// transport down while one is still in flight.
func TestUDPSidecarStopWaitsForInFlightHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &NFSAdapter{sidecars: auxsvc.NewGroup()}
	a.sidecars.SetBaseContext(ctx)
	udpEnabled := true
	a.config.UDP.Enabled = &udpEnabled
	a.config.Port = 0 // let the kernel pick a free port

	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool

	restore := udpDispatch
	udpDispatch = func(*NFSConnection, context.Context, *net.UDPConn, *net.UDPAddr, []byte) {
		close(entered)
		<-release
		finished.Store(true)
	}
	t.Cleanup(func() { udpDispatch = restore })

	if err := a.startUDP(ctx); err != nil {
		t.Fatalf("startUDP: %v", err)
	}

	a.sidecarMu.Lock()
	addr := a.udpConn.LocalAddr().(*net.UDPAddr)
	a.sidecarMu.Unlock()

	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: addr.Port})
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("datagram")); err != nil {
		t.Fatalf("write udp: %v", err)
	}
	<-entered // the handler is now in flight

	stopped := make(chan error, 1)
	go func() {
		stopped <- (udpSidecar{a}).Stop(context.Background())
	}()

	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while a handler was still running (err=%v, handler finished=%v)", err, finished.Load())
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the handler completed")
	}
	if !finished.Load() {
		t.Fatal("Stop returned before the in-flight handler completed")
	}
}

// A handler that never returns must not hold shutdown open: the wait is bounded
// by the context the lifecycle group supplies.
func TestUDPSidecarStopGivesUpOnWedgedHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &NFSAdapter{sidecars: auxsvc.NewGroup()}
	a.sidecars.SetBaseContext(ctx)
	udpEnabled := true
	a.config.UDP.Enabled = &udpEnabled
	a.config.Port = 0

	entered := make(chan struct{})
	release := make(chan struct{})
	restore := udpDispatch
	udpDispatch = func(*NFSConnection, context.Context, *net.UDPConn, *net.UDPAddr, []byte) {
		close(entered)
		<-release
	}
	t.Cleanup(func() {
		udpDispatch = restore
		close(release)
	})

	if err := a.startUDP(ctx); err != nil {
		t.Fatalf("startUDP: %v", err)
	}
	a.sidecarMu.Lock()
	addr := a.udpConn.LocalAddr().(*net.UDPAddr)
	a.sidecarMu.Unlock()

	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: addr.Port})
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("datagram")); err != nil {
		t.Fatalf("write udp: %v", err)
	}
	<-entered

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopCancel()
	if err := (udpSidecar{a}).Stop(stopCtx); err == nil {
		t.Fatal("expected Stop to report the wedged handler rather than block")
	}
}
