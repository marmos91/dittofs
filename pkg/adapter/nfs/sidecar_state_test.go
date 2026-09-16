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

// udpAdapterWithHandlerInFlight brings the UDP sidecar up on a kernel-chosen
// port, stands in handler for the real datagram dispatch, and returns once one
// datagram is being handled. The caller controls when handler returns, so it
// controls how long the handler stays in flight.
func udpAdapterWithHandlerInFlight(t *testing.T, handler func()) *NFSAdapter {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &NFSAdapter{sidecars: auxsvc.NewGroup()}
	a.sidecars.SetBaseContext(ctx)
	udpEnabled := true
	a.config.UDP.Enabled = &udpEnabled
	a.config.Port = 0 // let the kernel pick a free port

	entered := make(chan struct{})
	restore := udpDispatch
	udpDispatch = func(*NFSConnection, context.Context, *net.UDPConn, *net.UDPAddr, []byte) {
		close(entered)
		handler()
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
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("datagram")); err != nil {
		t.Fatalf("write udp: %v", err)
	}
	<-entered // the handler is now in flight
	return a
}

// Closing the UDP socket only unblocks the read: the loop and every datagram
// handler it spawned are still running, and they read adapter and runtime state
// that the rest of teardown is about to dismantle. Stop must not report the
// transport down while one is still in flight.
func TestUDPSidecarStopWaitsForInFlightHandler(t *testing.T) {
	release := make(chan struct{})
	var finished atomic.Bool
	a := udpAdapterWithHandlerInFlight(t, func() {
		<-release
		finished.Store(true)
	})

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
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	a := udpAdapterWithHandlerInFlight(t, func() { <-release })

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopCancel()
	if err := (udpSidecar{a}).Stop(stopCtx); err == nil {
		t.Fatal("expected Stop to report the wedged handler rather than block")
	}
}

// A Stop that gives up must not strand the generation. If the claimed done
// channel were cleared along with the conn, the handlers would outlive the
// timeout with nothing left to join them, and the next Stop — the one with more
// time, or the one the lifecycle group issues during shutdown — would report
// the transport down while they were still running.
func TestUDPSidecarStopAfterTimeoutStillJoins(t *testing.T) {
	release := make(chan struct{})
	var released bool
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	var finished atomic.Bool
	a := udpAdapterWithHandlerInFlight(t, func() {
		<-release
		finished.Store(true)
	})

	// First Stop gives up: the handler is still blocked.
	timedOut, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := (udpSidecar{a}).Stop(timedOut); err == nil {
		t.Fatal("expected the first Stop to report the still-running handler")
	}

	// A second Stop must still wait for that same handler, not wave it through.
	stopped := make(chan error, 1)
	go func() { stopped <- (udpSidecar{a}).Stop(context.Background()) }()

	select {
	case err := <-stopped:
		t.Fatalf("second Stop reported the transport down while the handler was still running (err=%v, finished=%v)",
			err, finished.Load())
	case <-time.After(100 * time.Millisecond):
	}

	released = true
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("second Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Stop did not return after the handler completed")
	}
	if !finished.Load() {
		t.Fatal("second Stop returned before the in-flight handler completed")
	}
}
