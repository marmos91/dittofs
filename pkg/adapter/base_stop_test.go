package adapter

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// TestBaseAdapter_Stop_CtxDone_ForceClosesConnections verifies that when Stop
// is called with an already-cancelled context, active connections are
// force-closed rather than abandoned. Before the fix the ctx.Done branch of
// Stop returned immediately without calling forceCloseConnections, leaking the
// underlying TCP connection.
func TestBaseAdapter_Stop_CtxDone_ForceClosesConnections(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	// Register a fake active connection so ActiveConnections is non-empty.
	srv, cli := net.Pipe()
	defer func() { _ = cli.Close() }()
	b.ActiveConnections.Store("127.0.0.1:9999", srv)

	// Simulate one in-flight connection so activeConns.Wait() blocks and Stop's
	// select is forced onto the ctx.Done branch. We never call Done(); the wait
	// goroutine is harmlessly leaked when the test process exits.
	b.activeConns.Add(1)
	b.ConnCount.Store(1)

	// Already-cancelled context drives Stop straight to the ctx.Done branch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Stop(ctx); err != context.Canceled {
		t.Fatalf("Stop with cancelled ctx: got err=%v, want context.Canceled", err)
	}

	// After Stop returns, srv must have been closed by forceCloseConnections.
	// Writing to a closed net.Pipe end returns io.ErrClosedPipe.
	_ = srv.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	if _, writeErr := srv.Write([]byte("probe")); writeErr == nil {
		t.Fatal("Stop with cancelled context did not force-close the active connection")
	}
}

// TestBaseAdapter_Stop_ClosesListener verifies that Stop closes the TCP
// listener so the adapter stops accepting new connections. This is the first
// step of a graceful SIGTERM shutdown (issue #1313): a restarting server must
// stop admitting clients before it drains in-flight work.
func TestBaseAdapter_Stop_ClosesListener(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	addr := ln.Addr().String()

	// Wire the listener into the adapter the way ServeWithFactory does and mark
	// readiness so any concurrent probe observes a started adapter.
	b.listenerMu.Lock()
	b.listener = ln
	b.listenerMu.Unlock()
	b.started.Store(true)
	close(b.ListenerReady)

	// Sanity: the listener accepts before Stop.
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("pre-Stop dial should succeed: %v", err)
	}
	_ = c.Close()

	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop with no active connections should return nil, got %v", err)
	}

	// After Stop the listener is closed: a fresh accept must fail.
	if _, err := ln.Accept(); err == nil {
		t.Fatal("listener still accepting after Stop; Stop did not close it")
	}
}

// TestBaseAdapter_Stop_DrainsActiveConnection verifies the graceful happy path:
// Stop waits for an in-flight connection to finish (within the timeout) and
// returns nil rather than force-closing it. The complementary force-close path
// (timeout exceeded) is covered by TestBaseAdapter_Stop_CtxDone_ForceClosesConnections.
func TestBaseAdapter_Stop_DrainsActiveConnection(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	// Model one in-flight connection that completes shortly after Stop begins,
	// mirroring a request handler finishing its work during shutdown.
	b.activeConns.Add(1)
	b.ConnCount.Store(1)

	var once sync.Once
	finish := func() {
		once.Do(func() {
			b.ConnCount.Add(-1)
			b.activeConns.Done()
		})
	}
	defer finish() // safety net if the test fails before the goroutine runs

	go func() {
		time.Sleep(100 * time.Millisecond)
		finish()
	}()

	start := time.Now()
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("graceful Stop should return nil once the connection drains, got %v", err)
	}
	elapsed := time.Since(start)

	// Stop must have actually waited for the drain (~100ms), not returned
	// instantly, and must not have run to the full timeout.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("Stop returned too quickly (%v); it did not wait for the connection to drain", elapsed)
	}
	if elapsed >= b.Config.ShutdownTimeout {
		t.Fatalf("Stop took %v (>= timeout); it did not drain gracefully", elapsed)
	}
	if remaining := b.ConnCount.Load(); remaining != 0 {
		t.Fatalf("expected 0 active connections after graceful drain, got %d", remaining)
	}
}

// TestBaseAdapter_InitiateShutdown_InterruptsBlockingReads verifies that
// shutdown sets a read deadline on active connections so a goroutine blocked
// in Read unblocks promptly instead of hanging until SIGKILL. This is what
// lets the SMB/NFS read loops notice shutdown and run their clean
// per-connection teardown (session cleanup, TCP FIN) within the drain window.
func TestBaseAdapter_InitiateShutdown_InterruptsBlockingReads(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()
	b.ActiveConnections.Store("127.0.0.1:9999", srv)

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := srv.Read(buf) // blocks until a deadline is set
		readErr <- err
	}()

	// Give the goroutine a moment to enter the blocking Read.
	time.Sleep(20 * time.Millisecond)

	b.initiateShutdown()

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("blocked Read returned without error after shutdown; expected a deadline-exceeded error")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Read was not interrupted by initiateShutdown within 1s")
	}
}
