package smb

import (
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// newConnWithID builds a test Connection bound to the same Adapter (so they
// share the handler / session manager) with a caller-chosen ConnID, modelling
// two TCP connections of the same multichannel session.
func newConnWithID(adapter *Adapter, conn net.Conn, id uint64) *Connection {
	c := NewConnection(adapter, conn)
	c.ID = id
	return c
}

// addChannel registers a Channel for connID on the session.
func addChannel(t *testing.T, sess *session.Session, connID uint64) {
	t.Helper()
	if !sess.AddChannel(&session.Channel{ConnID: connID, RemoteAddr: "127.0.0.1:0"}) {
		t.Fatalf("AddChannel(%d) failed", connID)
	}
}

// parkCreate registers a pending CREATE for sessionID.
func parkCreate(t *testing.T, h *handlers.Handler, sessionID, connID, asyncID uint64) {
	t.Helper()
	if err := h.PendingCreateRegistry.Register(&handlers.PendingCreate{
		ConnID:    connID,
		SessionID: sessionID,
		MessageID: 1,
		AsyncId:   asyncID,
		Cancel:    func() {},
		Callback:  func(_, _, _ uint64, _ types.Status, _ []byte) error { return nil },
	}); err != nil {
		t.Fatalf("parkCreate Register failed: %v", err)
	}
}

// snapshotTouched mirrors the snapshot-and-clear that cleanupSessions performs
// up front, returning the set of sessions this connection participates in.
func snapshotTouched(c *Connection) []uint64 {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	out := make([]uint64, 0, len(c.sessions)+len(c.boundSessions))
	for id := range c.sessions {
		out = append(out, id)
	}
	for id := range c.boundSessions {
		if _, owned := c.sessions[id]; !owned {
			out = append(out, id)
		}
	}
	return out
}

// TestMultichannelSessionSurvival verifies MS-SMB2 §3.3.7.1 multichannel session
// survival: closing one connection of a multichannel session removes only that
// connection's channel; the session and any parked operation survive while other
// channels remain live, and only the last channel's close marks the session for
// teardown. This is the fix for the smb2.replay.dhv2-pending2*/3*-sane rows of
// #749.
//
// The runtime-backed final teardown (Handler.CleanupSession) is exercised
// elsewhere (handler-package replay tests); here we assert the pure channel
// survival/partition decision.
func TestMultichannelSessionSurvival(t *testing.T) {
	srvA, cliA := net.Pipe()
	defer func() { _ = srvA.Close() }()
	defer func() { _ = cliA.Close() }()
	srvB, cliB := net.Pipe()
	defer func() { _ = srvB.Close() }()
	defer func() { _ = cliB.Close() }()

	adapter := New(Config{})

	origin := newConnWithID(adapter, srvA, 1)
	secondary := newConnWithID(adapter, srvB, 2)

	sess := adapter.handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sessionID := sess.SessionID

	addChannel(t, sess, origin.ID)
	addChannel(t, sess, secondary.ID)

	// Origin owns the lifecycle; secondary merely carries a bound channel.
	origin.TrackSession(sessionID)
	secondary.BindSession(sessionID)

	parkCreate(t, adapter.handler, sessionID, origin.ID, 0xAA)

	// --- Drop the ORIGIN connection (the test disconnects the originating
	// channel mid-test). The session must SURVIVE because the secondary
	// channel remains live: it is NOT in the dying set, the channel is removed,
	// and the parked CREATE stays registered. ---
	dying := origin.removeChannelsAndPartition(snapshotTouched(origin), "origin")
	if len(dying) != 0 {
		t.Fatalf("origin close: dying=%v, want empty (session survives on secondary)", dying)
	}
	if _, ok := adapter.handler.GetSession(sessionID); !ok {
		t.Fatal("session must survive origin-channel close while another channel is live")
	}
	if got := sess.ChannelCount(); got != 1 {
		t.Fatalf("after origin close: channel count = %d, want 1 (secondary only)", got)
	}
	if sess.GetChannel(origin.ID) != nil {
		t.Fatal("origin channel must be removed from the session on its connection close")
	}
	if n := adapter.handler.PendingCreateRegistry.Len(); n != 1 {
		t.Fatalf("parked CREATE registry len = %d, want 1 (survives)", n)
	}

	// --- Drop the SECONDARY connection (the last channel). Now the session is
	// the last-channel case and must be marked dying. ---
	dying = secondary.removeChannelsAndPartition(snapshotTouched(secondary), "secondary")
	if len(dying) != 1 || dying[0] != sessionID {
		t.Fatalf("secondary close: dying=%v, want [%d] (last channel)", dying, sessionID)
	}
	if got := sess.ChannelCount(); got != 0 {
		t.Fatalf("after secondary close: channel count = %d, want 0", got)
	}
}

// TestSingleChannelSessionMarkedDyingOnClose pins the common case: a session
// with exactly one channel is marked for teardown on connection close, exactly
// as before the survival change. Guards against the survival gate accidentally
// leaking single-channel sessions.
func TestSingleChannelSessionMarkedDyingOnClose(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	adapter := New(Config{})
	c := newConnWithID(adapter, srv, 1)

	sess := adapter.handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sessionID := sess.SessionID
	addChannel(t, sess, c.ID)
	c.TrackSession(sessionID)

	dying := c.removeChannelsAndPartition(snapshotTouched(c), "single")
	if len(dying) != 1 || dying[0] != sessionID {
		t.Fatalf("single-channel close: dying=%v, want [%d]", dying, sessionID)
	}
}

// TestBoundChannelCloseKeepsSessionAndRouting verifies the inverse drop order:
// when a non-origin (bound) channel closes first, the session survives on the
// origin channel and the break-routing entry (sessionConns), which points at the
// still-live origin, is left intact. Dropping the origin afterwards both marks
// the session dying and clears the routing entry.
func TestBoundChannelCloseKeepsSessionAndRouting(t *testing.T) {
	srvA, cliA := net.Pipe()
	defer func() { _ = srvA.Close() }()
	defer func() { _ = cliA.Close() }()
	srvB, cliB := net.Pipe()
	defer func() { _ = srvB.Close() }()
	defer func() { _ = cliB.Close() }()

	adapter := New(Config{})
	origin := newConnWithID(adapter, srvA, 1)
	secondary := newConnWithID(adapter, srvB, 2)

	sess := adapter.handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sessionID := sess.SessionID
	addChannel(t, sess, origin.ID)
	addChannel(t, sess, secondary.ID)
	origin.TrackSession(sessionID)
	secondary.BindSession(sessionID)

	// Record a break-routing entry pointing at the origin connection.
	adapter.sessionConns.Store(sessionID, origin.connInfo())

	// Drop the bound secondary first.
	dying := secondary.removeChannelsAndPartition(snapshotTouched(secondary), "secondary")
	if len(dying) != 0 {
		t.Fatalf("secondary close: dying=%v, want empty (origin still live)", dying)
	}
	if _, ok := adapter.handler.GetSession(sessionID); !ok {
		t.Fatal("session must survive bound-channel close while origin is live")
	}
	if got := sess.ChannelCount(); got != 1 {
		t.Fatalf("after secondary close: channel count = %d, want 1 (origin only)", got)
	}
	if _, ok := adapter.sessionConns.Load(sessionID); !ok {
		t.Fatal("break-routing entry pointing at the live origin must be preserved")
	}

	// Now drop the origin (last channel).
	dying = origin.removeChannelsAndPartition(snapshotTouched(origin), "origin")
	if len(dying) != 1 || dying[0] != sessionID {
		t.Fatalf("origin close: dying=%v, want [%d] (last channel)", dying, sessionID)
	}
	if _, ok := adapter.sessionConns.Load(sessionID); ok {
		t.Fatal("break-routing entry must be deleted when the origin closes")
	}
}

// TestAnyTrackedSessionFallsBackToBound verifies that a connection carrying ONLY
// a bound multichannel channel (no owned session) still surfaces a SessionID via
// AnyTrackedSession. SendErrorResponse relies on this to sign a wrong-SessionId
// error on a bound-only transport; without the fallback it returns 0 and the
// error cannot be signed.
func TestAnyTrackedSessionFallsBackToBound(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	adapter := New(Config{})
	c := newConnWithID(adapter, srv, 1)

	const boundID = 0x4242
	c.BindSession(boundID)

	if got := c.AnyTrackedSession(); got != boundID {
		t.Fatalf("AnyTrackedSession on bound-only connection = %d, want %d", got, boundID)
	}

	// With an owned session present, that one is preferred.
	const ownedID = 0x1111
	c.TrackSession(ownedID)
	if got := c.AnyTrackedSession(); got != ownedID {
		t.Fatalf("AnyTrackedSession with owned session = %d, want owned %d", got, ownedID)
	}
}

// TestUntrackSessionClearsBoundChannel verifies that a LOGOFF processed on a
// bound multichannel channel clears the boundSessions entry too. Otherwise a
// stale bound entry makes connection-close still treat the session as
// participating on this transport.
func TestUntrackSessionClearsBoundChannel(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	adapter := New(Config{})
	c := newConnWithID(adapter, srv, 1)

	const sessionID = 0x9090
	c.BindSession(sessionID)

	c.UntrackSession(sessionID)

	if got := snapshotTouched(c); len(got) != 0 {
		t.Fatalf("after UntrackSession, participating sessions = %v, want none", got)
	}
	if got := c.AnyTrackedSession(); got != 0 {
		t.Fatalf("after UntrackSession, AnyTrackedSession = %d, want 0", got)
	}
}
