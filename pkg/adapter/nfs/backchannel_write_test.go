package nfs

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter"

	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// newPipeConnection wires an NFSConnection to one end of a net.Pipe. net.Pipe
// has no buffer at all: a write blocks until the peer reads every byte of it,
// which is what a full TCP send buffer looks like from the writer's side.
func newPipeConnection(t *testing.T, srv *NFSAdapter, connID uint64) (*NFSConnection, net.Conn) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	t.Cleanup(func() { _ = serverConn.Close() })

	return &NFSConnection{
		server:       srv,
		conn:         serverConn,
		connectionID: connID,
		requestSem:   make(chan struct{}, 4),
	}, clientConn
}

// TestCallbackWrite_BoundedSoCloseCannotWedge covers the shutdown deadlock a
// callback write with no deadline opens.
//
// The callback writer holds writeMu for as long as the socket refuses the
// bytes. A fore-channel reply behind it is an in-flight request, and
// handleConnectionClose waits for every in-flight request before it closes the
// socket — the one event that would have released the write. Unbounded, the
// three wait on each other forever.
func TestCallbackWrite_BoundedSoCloseCannotWedge(t *testing.T) {
	srv := &NFSAdapter{
		BaseAdapter: &adapter.BaseAdapter{},
		config:      NFSConfig{Timeouts: NFSTimeoutsConfig{Write: 150 * time.Millisecond}},
	}
	c, clientConn := newPipeConnection(t, srv, 4100)

	// The callback writer takes writeMu and blocks on the socket.
	cbDone := make(chan error, 1)
	go func() { cbDone <- c.write([]byte("callback-bytes"), c.callbackWriteTimeout()) }()

	// Read one byte so the writer has provably entered Write (and so holds
	// writeMu); the rest of the message still has nowhere to go.
	one := make([]byte, 1)
	if _, err := io.ReadFull(clientConn, one); err != nil {
		t.Fatalf("read first byte of the callback: %v", err)
	}

	// An in-flight request waiting for writeMu. This is what the close path
	// waits on.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		_ = c.writeReply(1, []byte("reply-bytes"))
	}()

	closed := make(chan struct{})
	go func() {
		c.handleConnectionClose()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("handleConnectionClose never returned: the callback write is holding writeMu " +
			"against an in-flight reply, and the wait for that reply holds the socket close")
	}

	if err := <-cbDone; err == nil {
		t.Fatal("the callback write reported success although the peer never took the bytes")
	}
}

// TestCallbackWriteTimeout_CappedBySenderBudget pins the two ends of the write
// bound. The configured fore-channel timeout applies when it is shorter, and
// the sender's own per-attempt budget caps it otherwise — including when the
// fore-channel timeout is disabled, which is the case that leaves the write
// unbounded.
func TestCallbackWriteTimeout_CappedBySenderBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"shorter than the budget", time.Second, time.Second},
		{"longer than the budget", time.Hour, v4state.CallbackWriteTimeout},
		{"disabled", 0, v4state.CallbackWriteTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &NFSConnection{server: &NFSAdapter{
				BaseAdapter: &adapter.BaseAdapter{},
				config:      NFSConfig{Timeouts: NFSTimeoutsConfig{Write: tc.configured}},
			}}
			if got := c.callbackWriteTimeout(); got != tc.want {
				t.Errorf("callbackWriteTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestBackchannelDemux_FollowsTheCurrentReplyTable covers the connection
// keeping its own copy of the reply demultiplexer.
//
// A DESTROY_SESSION retires the table while a COMPOUND is registering, and the
// COMPOUND that wins installs a fresh one; a connection holding a copy then
// demuxes into the table it captured while every sender registers its waiters
// in the live one, and each callback times out into a revoked delegation. The
// read loop must route by what the StateManager holds now.
func TestBackchannelDemux_FollowsTheCurrentReplyTable(t *testing.T) {
	sm := newBackchannelStateManager(t)
	srv := &NFSAdapter{
		BaseAdapter: &adapter.BaseAdapter{},
		config:      NFSConfig{Timeouts: NFSTimeoutsConfig{Read: 5 * time.Second, Write: time.Second}},
		v4Handler:   v4handlers.NewHandler(nil, nil, sm),
	}

	const connID = 4200
	c, clientConn := newPipeConnection(t, srv, connID)

	// Whatever the liveness probe puts on the wire has to go somewhere, or the
	// unread bytes hold the writer until its deadline.
	go func() { _, _ = io.Copy(io.Discard, clientConn) }()

	sessionID := createBackchannelSession(t, sm)
	if _, err := sm.BindConnToSession(connID, sessionID, v4types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}
	c.maybeRegisterBackchannel(context.Background())

	retired := sm.GetPendingCBReplies(connID)
	if retired == nil {
		t.Fatal("no reply table was registered for a back-bound connection")
	}

	// The teardown, and the registration that raced it.
	sm.UnbindConnection(connID)
	current := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	if current == retired {
		t.Fatal("the retired table came back, so the test is not exercising a replacement")
	}

	const xid = 0x5151
	replyCh, ok := current.Register(xid)
	if !ok {
		t.Fatal("the current table refused the waiter")
	}

	go func() { _, _ = clientConn.Write(backchannelReplyRecord(xid)) }()

	_, _, err := c.readRequest(context.Background())
	if !errors.Is(err, errBackchannelReply) {
		t.Fatalf("readRequest did not recognise the backchannel reply: %v", err)
	}

	select {
	case <-replyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the reply never reached the table the sender registered with")
	}
}

// backchannelReplyRecord builds a single-fragment RPC record whose msg_type is
// REPLY, which is all the demultiplexer inspects.
func backchannelReplyRecord(xid uint32) []byte {
	body := make([]byte, 24)
	binary.BigEndian.PutUint32(body[0:4], xid)
	binary.BigEndian.PutUint32(body[4:8], 1) // msg_type = REPLY

	record := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(record, 0x80000000|uint32(len(body)))
	return append(record, body...)
}

// newBackchannelStateManager returns a state manager that stops its goroutines
// when the test ends.
func newBackchannelStateManager(t *testing.T) *v4state.StateManager {
	t.Helper()
	sm := v4state.NewStateManager(90 * time.Second)
	t.Cleanup(sm.Shutdown)
	return sm
}

// createBackchannelSession registers a v4.1 client and gives it a session with
// a back channel.
func createBackchannelSession(t *testing.T, sm *v4state.StateManager) v4types.SessionId4 {
	t.Helper()

	var verifier [8]byte
	copy(verifier[:], "demuxvf1")
	eid, err := sm.ExchangeID([]byte("demux-test-client"), verifier, 0, nil, "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}

	cs, _, err := sm.CreateSession(
		eid.ClientID,
		eid.SequenceID,
		v4types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
		v4types.ChannelAttrs{
			MaxRequestSize: 1048576, MaxResponseSize: 1048576,
			MaxResponseSizeCached: 4096, MaxOperations: 16, MaxRequests: 64,
		},
		v4types.ChannelAttrs{
			MaxRequestSize: 4096, MaxResponseSize: 4096,
			MaxOperations: 2, MaxRequests: 8,
		},
		0x40000000,
		[]v4types.CallbackSecParms4{{CbSecFlavor: 0}},
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return cs.SessionID
}
