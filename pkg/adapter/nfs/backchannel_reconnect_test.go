package nfs

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// cbNullReplyBody builds the RPC reply a client sends for CB_NULL. Procedure 0
// returns void, so the message ends at accept_stat.
func cbNullReplyBody(xid uint32) []byte {
	body := make([]byte, 0, 24)
	body = binary.BigEndian.AppendUint32(body, xid)
	body = binary.BigEndian.AppendUint32(body, uint32(rpc.RPCReply))
	body = binary.BigEndian.AppendUint32(body, uint32(rpc.RPCMsgAccepted))
	body = binary.BigEndian.AppendUint32(body, rpc.AuthNull)
	body = binary.BigEndian.AppendUint32(body, 0)
	return binary.BigEndian.AppendUint32(body, uint32(rpc.RPCSuccess))
}

// answerCBNull reads the one record-marked callback the server sends over this
// connection and routes a successful reply back through the state manager's
// demultiplexer, the way the real read loop does.
func answerCBNull(clientConn net.Conn, sm *v4state.StateManager, connID uint64) error {
	var header [4]byte
	if _, err := io.ReadFull(clientConn, header[:]); err != nil {
		return fmt.Errorf("read record mark: %w", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(header[:])&0x7FFFFFFF)
	if _, err := io.ReadFull(clientConn, body); err != nil {
		return fmt.Errorf("read callback: %w", err)
	}
	if len(body) < 4 {
		return fmt.Errorf("callback too short: %d bytes", len(body))
	}
	pending := sm.GetPendingCBReplies(connID)
	if pending == nil {
		return fmt.Errorf("no reply table for connection %d", connID)
	}
	xid := binary.BigEndian.Uint32(body[0:4])
	if !pending.Deliver(xid, cbNullReplyBody(xid)) {
		return fmt.Errorf("nothing was waiting on xid %#x", xid)
	}
	return nil
}

// waitForDelegationDecision polls the grant decision until it settles on want.
// The probe that moves it runs in its own goroutine, so the verdict lands after
// the registration returns.
func waitForDelegationDecision(t *testing.T, sm *v4state.StateManager, clientID uint64, fileHandle []byte, want bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, granted := sm.ShouldGrantDelegation(clientID, fileHandle, v4types.OPEN4_SHARE_ACCESS_READ); granted == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBackchannelReconnect_ReDerivesTheCallbackVerdict covers a client that
// loses its last back-bound connection and then binds a new one.
//
// The sender is per session and outlives every connection that carries it, and
// it probes the callback path only at the moment it is created. So registering
// a connection is the only point left that can re-derive the verdict: left to
// the sender, a client that reconnected would stay ineligible for delegations
// until an unrelated BACKCHANNEL_CTL happened to probe, and a client whose
// verdict was still standing would be offered delegations on a route nothing
// had tried.
func TestBackchannelReconnect_ReDerivesTheCallbackVerdict(t *testing.T) {
	sm := newBackchannelStateManager(t)
	srv := &NFSAdapter{
		BaseAdapter: &adapter.BaseAdapter{},
		config:      NFSConfig{Timeouts: NFSTimeoutsConfig{Read: 5 * time.Second, Write: 2 * time.Second}},
		v4Handler:   v4handlers.NewHandler(nil, nil, sm),
	}

	sessionID := createBackchannelSession(t, sm)
	session := sm.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found after CreateSession")
	}
	clientID := session.ClientID
	fileHandle := []byte("reconnect-file")

	// bindAndRegister is one client connection arriving: it binds to the
	// session inside a COMPOUND, and the registration that follows the COMPOUND
	// is what makes the connection able to carry a callback.
	bindAndRegister := func(connID uint64) {
		t.Helper()
		c, clientConn := newPipeConnection(t, srv, connID)
		if _, err := sm.BindConnToSession(connID, sessionID, v4types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
		answered := make(chan error, 1)
		go func() { answered <- answerCBNull(clientConn, sm, connID) }()
		c.maybeRegisterBackchannel(context.Background())
		select {
		case err := <-answered:
			if err != nil {
				t.Fatalf("answer CB_NULL on connection %d: %v", connID, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no CB_NULL reached connection %d", connID)
		}
	}

	bindAndRegister(4300)
	waitForDelegationDecision(t, sm, clientID, fileHandle, true,
		"the probe on the first back-bound connection never enabled delegations")

	// The socket goes away, taking the client's only callback route with it.
	sm.UnbindConnection(4300)
	waitForDelegationDecision(t, sm, clientID, fileHandle, false,
		"delegations stayed enabled after the client's last back-bound connection closed")

	// The client reconnects and binds the new socket to the same session. The
	// sender is the one from before, so nothing inside it probes.
	bindAndRegister(4301)
	waitForDelegationDecision(t, sm, clientID, fileHandle, true,
		"the reconnected connection was never probed, so the client stayed ineligible for delegations")
}
