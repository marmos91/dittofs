package state

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// newBackchannelClient registers a v4.1 client and gives it n sessions, each
// with a back channel and its own sender, as production does per session.
func newBackchannelClient(t *testing.T, sm *StateManager, name string, n int) (uint64, []types.SessionId4) {
	t.Helper()

	var verifier [8]byte
	copy(verifier[:], name)
	eid, err := sm.ExchangeID([]byte(name), verifier, 0, nil, "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}

	sessionIDs := make([]types.SessionId4, 0, n)
	for i := 0; i < n; i++ {
		res, _, cerr := sm.CreateSession(
			eid.ClientID, eid.SequenceID+uint32(i),
			types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
			types.ChannelAttrs{
				MaxRequestSize: 1048576, MaxResponseSize: 1048576,
				MaxResponseSizeCached: 4096, MaxOperations: 16, MaxRequests: 64,
			},
			types.ChannelAttrs{
				MaxRequestSize: 4096, MaxResponseSize: 4096,
				MaxOperations: 2, MaxRequests: 8,
			},
			0x40000000,
			[]types.CallbackSecParms4{{CbSecFlavor: 0}},
			1,
		)
		if cerr != nil {
			t.Fatalf("CreateSession: %v", cerr)
		}
		session := sm.GetSession(res.SessionID)
		if session == nil {
			t.Fatalf("session %x not found after CreateSession", res.SessionID)
		}
		session.backchannelSender = NewBackchannelSender(
			res.SessionID, eid.ClientID, 0x40000000, nil, session.BackChannelSlots, 1, sm)
		sessionIDs = append(sessionIDs, res.SessionID)
	}
	return eid.ClientID, sessionIDs
}

// answerOneCBNull replies to the single CB_NULL the server is about to send.
func answerOneCBNull(t *testing.T, clientConn net.Conn, pending *PendingCBReplies) {
	t.Helper()
	go func() {
		xid, _, err := readCBCall(clientConn)
		if err != nil {
			return
		}
		pending.Deliver(xid, buildMockCBNullReplyBody(xid))
	}()
}

// cbPathUp reads the client's recorded callback verdict.
func cbPathUp(sm *StateManager, clientID uint64) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	record := sm.clientRecordLocked(clientID)
	return record != nil && record.CBPathUp
}

// TestUnbindConnection_ClearsTheVerdictWithTheLastBackBinding covers the grant
// that follows a close.
//
// The verdict is client-wide and records what a CB_NULL found; the connection
// that carried that CB_NULL is not. A client that answers the probe and then
// closes its only back-bound connection used to keep the verdict, so the next
// OPEN was offered a delegation whose CB_RECALL had nothing left to travel on —
// a grant that can only end in a failed recall and a revocation.
func TestUnbindConnection_ClearsTheVerdictWithTheLastBackBinding(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()

	const connID = uint64(3001)
	clientConn, pending := bindBackchannel(t, sm, sessionID, connID)
	answerOneCBNull(t, clientConn, pending)

	sm.probeV41CallbackPath(context.Background(), sender)

	fileHandle := []byte("unbind-grant-file")
	if _, granted := sm.ShouldGrantDelegation(sender.clientID, fileHandle, types.OPEN4_SHARE_ACCESS_READ); !granted {
		t.Fatal("no delegation offered after a successful CB_NULL, so this starts from the wrong state")
	}

	sm.UnbindConnection(connID)

	if cbPathUp(sm, sender.clientID) {
		t.Error("the callback verdict survived the loss of the client's last back-bound connection")
	}
	if _, granted := sm.ShouldGrantDelegation(sender.clientID, fileHandle, types.OPEN4_SHARE_ACCESS_READ); granted {
		t.Error("a delegation was granted to a client with no route for its CB_RECALL")
	}
}

// TestUnbindConnection_KeepsTheVerdictWhileAnotherSessionIsBackBound is the
// half that stops the clear from becoming a blanket one.
//
// The verdict is per client, while the close that invalidates it is per
// connection, so a client running several sessions loses one connection at a
// time. Clearing on the first of them would withhold delegations from a client
// whose sibling session still answers callbacks.
func TestUnbindConnection_KeepsTheVerdictWhileAnotherSessionIsBackBound(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	clientID, sessions := newBackchannelClient(t, sm, "partial-close-client", 2)

	connIDs := []uint64{3101, 3102}
	for i, connID := range connIDs {
		sm.RegisterConnWriter(connID, func([]byte) error { return nil })
		if _, err := sm.BindConnToSession(connID, sessions[i], types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
	}
	sm.setCBPathUp(clientID, true)

	sm.UnbindConnection(connIDs[0])
	if !cbPathUp(sm, clientID) {
		t.Fatal("the verdict was cleared while a sibling session still held a back-bound connection")
	}

	sm.UnbindConnection(connIDs[1])
	if cbPathUp(sm, clientID) {
		t.Error("the verdict survived the loss of the client's last back-bound connection")
	}
}

// TestDestroySession_ClearsTheVerdictWhenItHeldTheLastBackBinding covers the
// teardown no socket close follows.
//
// One connection can carry several sessions, so destroying the only back-bound
// one leaves the socket open for the others. Nothing unbinds it, and the
// verdict would stay as the destroyed session left it while the client goes on
// issuing OPENs over the same connection.
func TestDestroySession_ClearsTheVerdictWhenItHeldTheLastBackBinding(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	clientID, sessions := newBackchannelClient(t, sm, "destroy-session-client", 2)
	backSession, foreSession := sessions[0], sessions[1]

	const connID = uint64(3201)
	if _, err := sm.BindConnToSession(connID, foreSession, types.CDFC4_FORE); err != nil {
		t.Fatalf("BindConnToSession(fore): %v", err)
	}
	if _, err := sm.BindConnToSession(connID, backSession, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession(back): %v", err)
	}
	sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	sm.setCBPathUp(clientID, true)

	if err := sm.DestroySession(backSession); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	if cbPathUp(sm, clientID) {
		t.Error("the verdict survived the destruction of the client's only back-bound session")
	}
	if _, granted := sm.ShouldGrantDelegation(clientID, []byte("destroy-session-file"), types.OPEN4_SHARE_ACCESS_READ); granted {
		t.Error("a delegation was granted over a connection that carries only a fore-channel binding")
	}
}

// TestReprobeCallbackPath_IsInertWithoutASender pins the one case the caller
// cannot distinguish for itself: a session whose sender has not been created
// yet probes when StartBackchannelSender creates it, so probing here as well
// would run two CB_NULLs for one registration.
func TestReprobeCallbackPath_IsInertWithoutASender(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	clientID, sessions := newBackchannelClient(t, sm, "no-sender-client", 1)
	session := sm.GetSession(sessions[0])
	session.backchannelSender = nil
	sm.setCBPathUp(clientID, true)

	sm.ReprobeCallbackPath(sessions[0])
	sm.ReprobeCallbackPath(types.SessionId4{0xFF})

	// A probe would have found no back-bound connection and cleared this.
	time.Sleep(50 * time.Millisecond)
	if !cbPathUp(sm, clientID) {
		t.Error("a session with no sender was probed anyway, publishing a verdict for a path nothing wrote to")
	}
}
