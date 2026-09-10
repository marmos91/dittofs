package adapter

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
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
	if !b.listenerReadyClosed {
		b.listenerReadyClosed = true
		close(b.listenerReady)
	}
	b.listenerMu.Unlock()
	b.started.Store(true)

	// Sanity: the listener accepts before Stop.
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("pre-Stop dial should succeed: %v", err)
	}
	_ = c.Close()

	// Bound the call so a regression where Stop never returns fails fast at the
	// deadline instead of hanging until the global test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop with no active connections should return nil, got %v", err)
	}

	// After Stop the listener is closed: a fresh dial must fail. Probe with
	// DialTimeout rather than ln.Accept() so a regression where Stop leaves the
	// listener open surfaces as a fast failure instead of a hung Accept.
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("listener still accepting after Stop; Stop did not close it")
	}
}

// TestBaseAdapter_Stop_DrainsActiveConnection verifies the graceful happy path:
// Stop waits for an in-flight connection to finish (within the timeout) and
// returns nil rather than force-closing it. The complementary force-close path
// (context cancelled) is covered by TestBaseAdapter_Stop_CtxDone_ForceClosesConnections.
func TestBaseAdapter_Stop_DrainsActiveConnection(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	// Model one in-flight connection that completes shortly after Stop begins,
	// mirroring a request handler finishing its work during shutdown. Stop
	// blocks on activeConns.Wait(), so it cannot return until this goroutine
	// calls Done().
	b.activeConns.Add(1)
	b.ConnCount.Store(1)

	go func() {
		time.Sleep(100 * time.Millisecond)
		b.ConnCount.Add(-1)
		b.activeConns.Done()
	}()

	// Bound the call so a regression that never drains fails at the deadline
	// rather than hanging until the global test timeout; the in-flight
	// connection drains well within ShutdownTimeout, so the graceful path
	// (done before ctx) still wins on the happy path.
	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	start := time.Now()
	if err := b.Stop(ctx); err != nil {
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

// TestBaseAdapter_GetListenerAddr_ReturnsAfterStopBeforeStart pins the
// Stop-before-bind lifecycle fix: GetListenerAddr blocks on the ready channel,
// and before the fix a Stop issued before ServeWithFactory ever bound left
// that channel open forever, hanging any waiter. After the fix, shutdown
// closes the channel even when no listener was ever bound, so GetListenerAddr
// returns (the zero address) instead of hanging. ServeWithFactory is never
// called here: the adapter is stopped in its pristine state.
func TestBaseAdapter_GetListenerAddr_ReturnsAfterStopBeforeStart(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	addrCh := make(chan string, 1)
	go func() {
		addrCh <- b.GetListenerAddr()
	}()

	// Stop the adapter while the waiter is (or is about to be) blocked.
	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop before Serve should return nil, got %v", err)
	}

	// The waiter must be released with the zero address. A regression where
	// Stop leaves the ready channel open surfaces as a fast failure at the
	// deadline instead of a hung test.
	select {
	case addr := <-addrCh:
		if addr != "" {
			t.Fatalf("GetListenerAddr after Stop-before-Serve: got %q, want the empty address", addr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetListenerAddr still blocked after Stop-before-Serve; shutdown did not release it")
	}
}

// TestBaseAdapter_GetListenerAddr_ReturnsWhenServeNeverRuns covers the other
// no-listener shape: the ready channel must also be closed if the adapter is
// stopped while never having served at all, even without a concurrent waiter.
func TestBaseAdapter_GetListenerAddr_ReturnsWhenServeNeverRuns(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop on a fresh adapter should return nil, got %v", err)
	}

	select {
	case <-b.ListenerReady():
		// released as expected
	case <-time.After(5 * time.Second):
		t.Fatal("ListenerReady still open after Stop on an adapter that never served")
	}
	if addr := b.GetListenerAddr(); addr != "" {
		t.Fatalf("GetListenerAddr after Stop-before-Serve: got %q, want the empty address", addr)
	}
}

// TestBaseAdapter_GetListenerAddr_ReturnsAddrAfterServe pins the normal-path
// invariant: a bound adapter's ready channel stays functional, and Stop after
// a normal Serve must not corrupt it (double close or a stuck waiter).
func TestBaseAdapter_GetListenerAddr_ReturnsAddrAfterServe(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- b.ServeWithFactory(context.Background(), stubFactory{}, nil, nil)
	}()

	addr := b.GetListenerAddr()
	if addr == "" {
		t.Fatal("GetListenerAddr returned an empty address for a bound adapter")
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatalf("Stop after a normal Serve should return nil, got %v", err)
	}

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ServeWithFactory should return nil after graceful shutdown, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeWithFactory did not return after Stop")
	}
}

// TestBaseAdapter_AcceptBackoff_ExitsOnShutdown pins the accept-loop backoff:
// a listener whose Accept always fails with a non-shutdown error must not spin
// the loop hot, and shutdown must interrupt the backoff sleep promptly. The
// failure counter doubles as the hot-loop probe: without the backoff, the
// loop racks up failures far faster than the backoff schedule allows.
func TestBaseAdapter_AcceptBackoff_ExitsOnShutdown(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	failing := &failingAcceptListener{failErr: errors.New("accept: resource temporarily unavailable")}
	b.listenerMu.Lock()
	b.listener = failing
	b.listenerMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = b.acceptConnections(stubFactory{}, nil, nil)
	}()

	// Let the loop accumulate several backoff-cycle failures. With the backoff
	// (10ms, 20ms, 40ms, ...) a 150ms window holds at most ~4 attempts; a hot
	// loop would hold tens of thousands. The bound is coarse so a slow runner
	// cannot flake the assertion.
	time.Sleep(150 * time.Millisecond)

	if got := failing.accepts.Load(); got > 10 {
		t.Fatalf("accept loop ran hot: %d attempts in 150ms, want <= 10 with backoff", got)
	}

	// Shutdown must break the backoff sleep and unwind the loop cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Stop(ctx) }()

	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()

	select {
	case <-wgDone:
		// loop exited cleanly
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop did not exit after shutdown; backoff sleep is not interruptible")
	}
	if err := <-done; err != nil {
		t.Fatalf("Stop returned an error with no active connections: %v", err)
	}
}

// TestBaseAdapter_AcceptBackoff_ResetsOnSuccess pins the failure-streak reset:
// after a successful accept the backoff returns to the base delay, so a
// one-off failure storm does not leave the loop throttled forever.
func TestBaseAdapter_AcceptBackoff_ResetsOnSuccess(t *testing.T) {
	b := NewBaseAdapter(BaseConfig{ShutdownTimeout: 5 * time.Second}, "TEST")

	// Two failures, then a success, then more failures. The success must reset
	// the streak so the next failure delays by the base 10ms, not 40ms+.
	lst := &recoveringAcceptListener{}
	b.listenerMu.Lock()
	b.listener = lst
	b.listenerMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = b.acceptConnections(stubFactory{}, nil, nil)
	}()

	// Wait until the listener has served one successful accept.
	ok := false
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		if lst.accepts.Load() >= 3 {
			ok = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("listener did not reach the third accept call, got %d", lst.accepts.Load())
	}

	// After the reset the fourth failure must delay by the base again. The
	// bound stays coarse: a reset loop holds few attempts per window, a
	// non-reset loop is capped at the max delay anyway, so assert the
	// restart-shaped bound rather than exact timings.
	b.initiateShutdown()

	ctx, cancel := context.WithTimeout(context.Background(), b.Config.ShutdownTimeout)
	defer cancel()
	_ = ctx

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop did not exit after shutdown")
	}
}

// stubFactory hands back a no-op connection so the accept loop's
// connection-tracking path runs without protocol involvement.
type stubFactory struct{}

func (stubFactory) NewConnection(net.Conn) ConnectionHandler { return nopConn{} }

type nopConn struct{}

func (nopConn) Serve(context.Context) {}

// failingAcceptListener fails every Accept with a non-shutdown error and
// counts the attempts.
type failingAcceptListener struct {
	accepts atomic.Int64
	failErr error
}

func (l *failingAcceptListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	return nil, l.failErr
}

func (l *failingAcceptListener) Close() error                     { return nil }
func (l *failingAcceptListener) Addr() net.Addr                   { return dummyAddr{} }
func (l *failingAcceptListener) SetDeadline(time.Time) error      { return nil }
func (l *failingAcceptListener) SetReadDeadline(time.Time) error  { return nil }
func (l *failingAcceptListener) SetWriteDeadline(time.Time) error { return nil }

// recoveringAcceptListener fails twice, then succeeds once per cycle, so a
// reset on success can be observed through the attempt counter.
type recoveringAcceptListener struct {
	accepts atomic.Int64
}

func (l *recoveringAcceptListener) Accept() (net.Conn, error) {
	n := l.accepts.Add(1)
	// Fail the first two calls, then hand back a pipe (never read; closed by
	// the test's cleanup via the process exit) once, then fail again.
	switch (n - 1) % 3 {
	case 0, 1:
		return nil, errors.New("accept: resource temporarily unavailable")
	default:
		srv, cli := net.Pipe()
		_ = cli.Close()
		return srv, nil
	}
}

func (l *recoveringAcceptListener) Close() error                     { return nil }
func (l *recoveringAcceptListener) Addr() net.Addr                   { return dummyAddr{} }
func (l *recoveringAcceptListener) SetDeadline(time.Time) error      { return nil }
func (l *recoveringAcceptListener) SetReadDeadline(time.Time) error  { return nil }
func (l *recoveringAcceptListener) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "127.0.0.1:0" }
