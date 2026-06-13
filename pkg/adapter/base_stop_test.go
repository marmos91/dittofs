package adapter

import (
	"context"
	"net"
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
