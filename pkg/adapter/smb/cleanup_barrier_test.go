package smb

import (
	"net"
	"testing"
	"time"
)

// cleanupBarrierOpen reports whether a SESSION_SETUP-style barrier wait gets
// through within d. False means the barrier is holding new sessions back.
func cleanupBarrierOpen(c *Connection, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		c.server.handler.WaitForCleanup()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestConnectionClose_BarrierCoversRequestDrain pins the ordering that keeps a
// reconnecting client from seeing the previous connection's open files.
//
// The close path drains in-flight request goroutines (c.wg.Wait()) before it
// reaches cleanupSessions, which is where the per-session cleanup counts are
// added. If the barrier is only armed there, a new connection's SESSION_SETUP
// that lands during the drain passes straight through while the dying
// connection's handles are still in the handle table — long enough for a CREATE
// to be refused with STATUS_DELETE_PENDING by a leftover delete-on-close
// handle. The barrier must therefore be armed before the drain, not after.
func TestConnectionClose_BarrierCoversRequestDrain(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	c := newTestConnection(server)

	// Model a request goroutine that has not finished yet: the read loop has
	// exited (handleConnectionClose runs) but the drain cannot complete.
	c.wg.Add(1)

	closed := make(chan struct{})
	go func() {
		c.handleConnectionClose()
		close(closed)
	}()

	// The close path parks in c.wg.Wait() and stays there. It must arm the
	// barrier before doing so; if it only arms inside cleanupSessions, that
	// never happens while the drain is held and this loop runs out.
	armed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !cleanupBarrierOpen(c, 50*time.Millisecond) {
			armed = true
			break
		}
	}
	if !armed {
		t.Fatal("cleanup barrier stayed open while the closing connection was draining in-flight requests")
	}

	c.wg.Done()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConnectionClose did not finish after the drain completed")
	}

	// Once the close path is done the barrier must open again, or every later
	// SESSION_SETUP eats the 3s WaitForCleanup timeout.
	if !cleanupBarrierOpen(c, 2*time.Second) {
		t.Fatal("cleanup barrier still closed after connection close completed")
	}
}
