package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// BackchannelSender Integration Tests
// ============================================================================

// createTestBackchannelSender creates a BackchannelSender with a StateManager,
// session with backchannel slots, and returns the sender/sm/sessionID.
func createTestBackchannelSender(t *testing.T) (*BackchannelSender, *StateManager, types.SessionId4) {
	t.Helper()

	sm := NewStateManager(90 * time.Second)

	// Register a v4.1 client via ExchangeID
	ownerID := []byte("backchannel-test-client")
	var verifier [8]byte
	copy(verifier[:], "bcverf01")
	eidResult, err := sm.ExchangeID(ownerID, verifier, 0, nil, "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	clientID := eidResult.ClientID

	// Create session with CONN_BACK_CHAN flag
	csResult, _, err := sm.CreateSession(
		clientID,
		eidResult.SequenceID, // EXCHANGE_ID returns the value CREATE_SESSION expects
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
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sessionID := csResult.SessionID

	// Get the session to access its BackChannelSlots
	session := sm.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found after CreateSession")
	}
	if session.BackChannelSlots == nil {
		t.Fatal("BackChannelSlots should not be nil for CONN_BACK_CHAN session")
	}

	// Create sender directly (not via StartBackchannelSender to avoid goroutine)
	sender := NewBackchannelSender(
		sessionID,
		clientID,
		0x40000000,
		nil,
		session.BackChannelSlots,
		sm,
	)

	return sender, sm, sessionID
}

// TestBackchannelSender_SendCallback verifies the full backchannel send path:
// BackchannelSender -> TCP write -> mock client receives CB_COMPOUND -> reply -> sender validates
func TestBackchannelSender_SendCallback(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)

	// Create a pipe for the ConnWriter
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	connID := uint64(1000)

	// Register ConnWriter that writes to the pipe
	pending := sm.RegisterConnWriter(connID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})

	// Bind connection as back-channel
	_, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	// Start mock client reader in goroutine
	done := make(chan error, 1)
	go func() {
		// Read fragment header
		var headerBuf [4]byte
		if _, err := io.ReadFull(clientConn, headerBuf[:]); err != nil {
			done <- fmt.Errorf("read header: %w", err)
			return
		}
		header := binary.BigEndian.Uint32(headerBuf[:])
		fragLen := header & 0x7FFFFFFF

		// Read body
		body := make([]byte, fragLen)
		if _, err := io.ReadFull(clientConn, body); err != nil {
			done <- fmt.Errorf("read body: %w", err)
			return
		}

		// Extract XID
		xid := binary.BigEndian.Uint32(body[0:4])

		// Validate msg_type = CALL (0)
		msgType := binary.BigEndian.Uint32(body[4:8])
		if msgType != rpc.RPCCall {
			done <- fmt.Errorf("expected CALL msg_type=0, got %d", msgType)
			return
		}

		// Validate RPC version = 2
		rpcVers := binary.BigEndian.Uint32(body[8:12])
		if rpcVers != 2 {
			done <- fmt.Errorf("expected RPC version 2, got %d", rpcVers)
			return
		}

		// Validate program = 0x40000000
		prog := binary.BigEndian.Uint32(body[12:16])
		if prog != 0x40000000 {
			done <- fmt.Errorf("expected program 0x40000000, got 0x%x", prog)
			return
		}

		// Build and deliver reply via PendingCBReplies
		replyBody := buildMockCBCompoundReplyBody(xid)
		pending.Deliver(xid, replyBody)

		done <- nil
	}()

	// Send a callback
	recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01, 0x02, 0x03})
	err = sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: recallOp,
	})
	if err != nil {
		t.Fatalf("sendCallback error: %v", err)
	}

	// Wait for mock client
	if clientErr := <-done; clientErr != nil {
		t.Fatalf("mock client error: %v", clientErr)
	}
}

// TestBackchannelSender_SendTimeout verifies that sender times out when client never replies.
func TestBackchannelSender_SendTimeout(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	sender.callbackTimeout = 200 * time.Millisecond

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	connID := uint64(2000)
	sm.RegisterConnWriter(connID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})
	_, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	// Read from client side but never reply
	go func() {
		buf := make([]byte, 4096)
		_, _ = clientConn.Read(buf)
		// Never deliver reply -- let it timeout
	}()

	recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01})
	err = sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: recallOp,
	})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("timed out")) {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

// TestBackchannelSender_RetryOnFailure verifies retry when first connection write fails,
// and sender succeeds on an alternate back-bound connection.
func TestBackchannelSender_RetryOnFailure(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	sender.callbackTimeout = 2 * time.Second

	// First connection: will fail on write
	failConnID := uint64(3000)
	sm.RegisterConnWriter(failConnID, func(data []byte) error {
		return fmt.Errorf("connection closed")
	})
	_, err := sm.BindConnToSession(failConnID, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("BindConnToSession (fail): %v", err)
	}

	// Second connection: will succeed
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	okConnID := uint64(3001)
	pending2 := sm.RegisterConnWriter(okConnID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})
	_, err = sm.BindConnToSession(okConnID, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("BindConnToSession (ok): %v", err)
	}

	// Mock client on second connection
	go func() {
		var headerBuf [4]byte
		if _, err := io.ReadFull(clientConn, headerBuf[:]); err != nil {
			return
		}
		header := binary.BigEndian.Uint32(headerBuf[:])
		fragLen := header & 0x7FFFFFFF
		body := make([]byte, fragLen)
		if _, err := io.ReadFull(clientConn, body); err != nil {
			return
		}
		xid := binary.BigEndian.Uint32(body[0:4])
		replyBody := buildMockCBCompoundReplyBody(xid)
		pending2.Deliver(xid, replyBody)
	}()

	recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01})
	err = sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: recallOp,
	})
	if err != nil {
		t.Fatalf("sendCallback should succeed on alternate connection: %v", err)
	}
}

// TestBackchannelSender_QueueFull verifies Enqueue returns false when queue is full.
func TestBackchannelSender_QueueFull(t *testing.T) {
	sender, _, _ := createTestBackchannelSender(t)

	// Fill the queue
	for i := 0; i < backchannelQueueSize; i++ {
		ok := sender.Enqueue(CallbackRequest{
			OpCode:  types.OP_CB_RECALL,
			Payload: []byte{byte(i)},
		})
		if !ok {
			t.Fatalf("Enqueue failed at index %d, queue should not be full yet", i)
		}
	}

	// Next enqueue should fail
	ok := sender.Enqueue(CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: []byte{0xFF},
	})
	if ok {
		t.Fatal("Enqueue should return false when queue is full")
	}
}

// TestBackchannelSender_Stop verifies the sender goroutine exits cleanly on Stop.
func TestBackchannelSender_Stop(t *testing.T) {
	sender, _, _ := createTestBackchannelSender(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		sender.Run(ctx)
		close(stopped)
	}()

	// Give Run a moment to start
	time.Sleep(10 * time.Millisecond)

	sender.Stop()

	select {
	case <-stopped:
		// OK -- goroutine exited
	case <-time.After(2 * time.Second):
		t.Fatal("BackchannelSender.Run did not exit after Stop")
	}
}

// TestBackchannelSender_CbProgramRace exercises the two real call sites that
// touch BackchannelSender.cbProgram concurrently: UpdateBackchannelParams
// (BACKCHANNEL_CTL) writes it, while sendCallback (the Run-goroutine read path)
// reads it on every callback. Before the fix cbProgram was a plain uint32 read
// without sm.mu, so `go test -race` flagged a data race here. With cbProgram as
// an atomic.Uint32 this test must pass cleanly under -race.
func TestBackchannelSender_CbProgramRace(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)

	// Attach the sender to the session so UpdateBackchannelParams targets it.
	sm.mu.Lock()
	sm.sessionsByID[sessionID].backchannelSender = sender
	sm.mu.Unlock()

	const iterations = 500
	done := make(chan struct{}, 2)

	// Writer: BACKCHANNEL_CTL updating the callback program number.
	go func() {
		defer func() { done <- struct{}{} }()
		for i := 0; i < iterations; i++ {
			if err := sm.UpdateBackchannelParams(sessionID, 0x40000000+uint32(i), nil); err != nil {
				t.Errorf("UpdateBackchannelParams: %v", err)
				return
			}
		}
	}()

	// Reader: sendCallback reads cbProgram before failing (no bound connection),
	// which is the exact unsynchronised read the race covered.
	go func() {
		defer func() { done <- struct{}{} }()
		recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01})
		for i := 0; i < iterations; i++ {
			_ = sender.sendCallback(context.Background(), CallbackRequest{
				OpCode:  types.OP_CB_RECALL,
				Payload: recallOp,
			})
		}
	}()

	<-done
	<-done
}

// TestBackchannelSender_SequenceIDIncrement verifies seqID increments between callbacks.
func TestBackchannelSender_SequenceIDIncrement(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	connID := uint64(4000)
	pending := sm.RegisterConnWriter(connID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})
	_, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH)
	if err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	// Helper to read one CB_COMPOUND, extract seqid, and reply
	readAndReply := func() uint32 {
		var headerBuf [4]byte
		if _, err := io.ReadFull(clientConn, headerBuf[:]); err != nil {
			t.Fatalf("read header: %v", err)
		}
		header := binary.BigEndian.Uint32(headerBuf[:])
		fragLen := header & 0x7FFFFFFF
		body := make([]byte, fragLen)
		if _, err := io.ReadFull(clientConn, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		xid := binary.BigEndian.Uint32(body[0:4])

		seqID := cbSequenceSeqID(t, body)

		replyBody := buildMockCBCompoundReplyBody(xid)
		pending.Deliver(xid, replyBody)

		return seqID
	}

	// Send first callback
	go func() {
		recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01})
		_ = sender.sendCallback(context.Background(), CallbackRequest{
			OpCode:  types.OP_CB_RECALL,
			Payload: recallOp,
		})
	}()
	seqID1 := readAndReply()

	// Send second callback
	go func() {
		recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 2}, false, []byte{0x02})
		_ = sender.sendCallback(context.Background(), CallbackRequest{
			OpCode:  types.OP_CB_RECALL,
			Payload: recallOp,
		})
	}()
	seqID2 := readAndReply()

	// RFC 8881 §2.10.6.1: CB_SEQUENCE seqID must increment by exactly 1 per send.
	if seqID2 != seqID1+1 {
		t.Errorf("CB_SEQUENCE seqID must increment by exactly 1: seqID1=%d seqID2=%d (want %d)", seqID1, seqID2, seqID1+1)
	}
}

// TestBackchannelSender_SeqIDAndXIDAreIndependent proves the CB_SEQUENCE seqID
// counter and the RPC XID counter are fully decoupled. A shared counter (the
// prior bug) made CB_SEQUENCE seqIDs skip by 2, violating RFC 8881 §2.10.6.1.
func TestBackchannelSender_SeqIDAndXIDAreIndependent(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)

	// Pre-skew the XID counter to prove the two counters diverge and do not
	// share state: after this, XIDs run ahead while CB seqIDs still start at 1.
	nextCallbackXID.Add(5)

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	connID := uint64(4100)
	pending := sm.RegisterConnWriter(connID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})
	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	// Helper to read one CB_COMPOUND frame and extract both the RPC XID and the
	// CB_SEQUENCE seqID, then deliver a mock reply.
	readAndReply := func() (xid, seqID uint32) {
		var headerBuf [4]byte
		if _, err := io.ReadFull(clientConn, headerBuf[:]); err != nil {
			t.Fatalf("read header: %v", err)
		}
		header := binary.BigEndian.Uint32(headerBuf[:])
		fragLen := header & 0x7FFFFFFF
		body := make([]byte, fragLen)
		if _, err := io.ReadFull(clientConn, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		if len(body) < 4 {
			t.Fatalf("body too short: %d bytes", len(body))
		}
		xid = binary.BigEndian.Uint32(body[0:4])
		seqID = cbSequenceSeqID(t, body)

		pending.Deliver(xid, buildMockCBCompoundReplyBody(xid))
		return xid, seqID
	}

	go func() {
		recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01})
		_ = sender.sendCallback(context.Background(), CallbackRequest{
			OpCode:  types.OP_CB_RECALL,
			Payload: recallOp,
		})
	}()
	xid1, seqID1 := readAndReply()

	go func() {
		recallOp := EncodeCBRecallOp(&types.Stateid4{Seqid: 2}, false, []byte{0x02})
		_ = sender.sendCallback(context.Background(), CallbackRequest{
			OpCode:  types.OP_CB_RECALL,
			Payload: recallOp,
		})
	}()
	xid2, seqID2 := readAndReply()

	// CB_SEQUENCE seqID increments by exactly 1, independent of XID activity.
	if seqID2 != seqID1+1 {
		t.Errorf("CB_SEQUENCE seqID must increment by exactly 1: seqID1=%d seqID2=%d (want %d)", seqID1, seqID2, seqID1+1)
	}
	// RPC XID increments by exactly 1, independently.
	if xid2 != xid1+1 {
		t.Errorf("RPC XID must increment by exactly 1: xid1=%d xid2=%d (want %d)", xid1, xid2, xid1+1)
	}
	// The two counters are genuinely independent: with the XID pre-skewed, the
	// first XID and first seqID must not coincide.
	if xid1 == seqID1 {
		t.Errorf("seqID and XID counters must be independent: xid1=%d seqID1=%d", xid1, seqID1)
	}
}

// TestPendingCBReplies_RegisterDeliverCancel tests XID routing:
// register, deliver, verify channel receives data; also test Cancel cleanup.
func TestPendingCBReplies_RegisterDeliverCancel(t *testing.T) {
	p := NewPendingCBReplies()

	// Register and deliver
	ch, registered := p.Register(42, types.SessionId4{})
	if !registered {
		t.Fatal("Register refused on an open table")
	}
	delivered := p.Deliver(42, []byte("reply-data"))
	if !delivered {
		t.Fatal("Deliver should return true for registered XID")
	}

	select {
	case data := <-ch:
		if !bytes.Equal(data, []byte("reply-data")) {
			t.Errorf("received data = %q, want %q", data, "reply-data")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for delivered reply")
	}

	// Deliver to unknown XID
	delivered = p.Deliver(999, []byte("unknown"))
	if delivered {
		t.Fatal("Deliver should return false for unknown XID")
	}

	// Register and cancel
	ch2, registered2 := p.Register(100, types.SessionId4{})
	if !registered2 {
		t.Fatal("Register refused on an open table")
	}
	p.Cancel(100)

	// Deliver after cancel should fail
	delivered = p.Deliver(100, []byte("after-cancel"))
	if delivered {
		t.Fatal("Deliver should return false after Cancel")
	}

	// Channel should be empty (no data delivered)
	select {
	case <-ch2:
		t.Fatal("channel should be empty after Cancel")
	default:
		// OK -- channel is empty
	}
}

// TestEncodeCBCompoundV41 verifies the wire format of CB_COMPOUND encoding.
func TestEncodeCBCompoundV41(t *testing.T) {
	dummyOp := []byte{0x00, 0x00, 0x00, 0x01}
	result := encodeCBCompoundV41([][]byte{dummyOp})

	reader := bytes.NewReader(result)

	// Tag: empty opaque (length = 0)
	var tagLen uint32
	if err := binary.Read(reader, binary.BigEndian, &tagLen); err != nil {
		t.Fatalf("read tag length: %v", err)
	}
	if tagLen != 0 {
		t.Errorf("tag length = %d, want 0", tagLen)
	}

	// Minorversion: 1
	var minorVersion uint32
	if err := binary.Read(reader, binary.BigEndian, &minorVersion); err != nil {
		t.Fatalf("read minorversion: %v", err)
	}
	if minorVersion != 1 {
		t.Errorf("minorversion = %d, want 1", minorVersion)
	}

	// Callback ident: 0
	var callbackIdent uint32
	if err := binary.Read(reader, binary.BigEndian, &callbackIdent); err != nil {
		t.Fatalf("read callback_ident: %v", err)
	}
	if callbackIdent != 0 {
		t.Errorf("callback_ident = %d, want 0", callbackIdent)
	}

	// Op count: 1
	var opCount uint32
	if err := binary.Read(reader, binary.BigEndian, &opCount); err != nil {
		t.Fatalf("read op count: %v", err)
	}
	if opCount != 1 {
		t.Errorf("op count = %d, want 1", opCount)
	}

	// Op payload
	remaining := make([]byte, reader.Len())
	if _, err := io.ReadFull(reader, remaining); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if !bytes.Equal(remaining, dummyOp) {
		t.Errorf("op payload = %x, want %x", remaining, dummyOp)
	}
}

// TestCallbackRouting_V41VsV40 verifies v4.1 uses BackchannelSender, v4.0 uses dial-out.
func TestCallbackRouting_V41VsV40(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Register a v4.0 client with callback info
	v40ClientID := sm.generateClientID()
	sm.mu.Lock()
	sm.clientsByID[v40ClientID] = &ClientRecord{
		ClientID: v40ClientID,
		Callback: CallbackInfo{
			Program: 0x40000000,
			NetID:   "tcp",
			Addr:    "127.0.0.1.0.1",
		},
		Confirmed:      true,
		ClientIDString: "v40-client",
	}
	sm.clientsByName["v40-client"] = sm.clientsByID[v40ClientID]
	sm.mu.Unlock()

	// Register a v4.1 client
	ownerID := []byte("v41-routing-client")
	var verifier [8]byte
	copy(verifier[:], "routerf1")
	eidResult, err := sm.ExchangeID(ownerID, verifier, 0, nil, "127.0.0.1:5555")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}
	v41ClientID := eidResult.ClientID

	csResult, _, err := sm.CreateSession(
		v41ClientID,
		eidResult.SequenceID,
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
	)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Start backchannel sender
	ctx := context.Background()
	sm.StartBackchannelSender(ctx, csResult.SessionID)

	// Verify v4.1 client has a BackchannelSender
	v41Sender := sm.getBackchannelSender(v41ClientID)
	if v41Sender == nil {
		t.Fatal("v4.1 client should have a BackchannelSender")
	}

	// Verify v4.0 client does NOT have a BackchannelSender
	v40Sender := sm.getBackchannelSender(v40ClientID)
	if v40Sender != nil {
		t.Fatal("v4.0 client should not have a BackchannelSender")
	}

	// Clean up
	v41Sender.Stop()
}

// ============================================================================
// Test Helpers
// ============================================================================

// buildMockCBCompoundReplyBody builds a CB_COMPOUND4res body (without record mark).
func buildMockCBCompoundReplyBody(xid uint32) []byte {
	var reply bytes.Buffer

	// XID
	_ = binary.Write(&reply, binary.BigEndian, xid)
	// MsgType = REPLY (1)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
	// reply_stat = MSG_ACCEPTED (0)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCMsgAccepted))
	// Verifier: AUTH_NULL (flavor=0, body_len=0)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.AuthNull))
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))
	// accept_stat = SUCCESS (0)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCSuccess))
	// CB_COMPOUND4res: nfsstat4 = NFS4_OK (0)
	_ = binary.Write(&reply, binary.BigEndian, uint32(types.NFS4_OK))
	// tag (empty)
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))
	// resarray count = 0
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))

	return reply.Bytes()
}

// cbSequenceSeqID extracts the CB_SEQUENCE sequence ID from a callback RPC CALL
// message. The credential and verifier are length-prefixed and their sizes
// depend on the flavor in use, so both are skipped by their declared length
// rather than by a fixed header size.
func cbSequenceSeqID(t *testing.T, body []byte) uint32 {
	t.Helper()

	// XID + msg_type + rpc_vers + prog + vers + proc
	off := 6 * 4

	// Credential and verifier: flavor(4) + length(4) + body(length, padded).
	for i := 0; i < 2; i++ {
		if len(body) < off+8 {
			t.Fatalf("body too short for auth field %d: %d bytes", i, len(body))
		}
		length := int(binary.BigEndian.Uint32(body[off+4 : off+8]))
		off += 8 + (length+3)/4*4
	}

	// CB_COMPOUND: tag_len + minorversion + callback_ident + op_count +
	// first_op_code, then CB_SEQUENCE args: sessionID(16) + seqID(4).
	off += 5*4 + 16
	if len(body) < off+4 {
		t.Fatalf("body too short for CB_SEQUENCE seqID: %d bytes", len(body))
	}
	return binary.BigEndian.Uint32(body[off : off+4])
}

// TestCallbackXIDsAreUniqueAcrossSenders drives two senders down their real
// send path and pins that the XIDs they put on the wire differ. A connection
// carries several sessions at once and its reply demultiplexer is keyed on XID
// alone, so a counter per sender would have both mint the same first XID and
// the second registration would strand the first sender's waiter.
func TestCallbackXIDsAreUniqueAcrossSenders(t *testing.T) {
	firstXID := func() uint32 {
		sender, sm, sessionID := createTestBackchannelSender(t)

		clientConn, serverConn := net.Pipe()
		defer func() { _ = clientConn.Close() }()
		defer func() { _ = serverConn.Close() }()

		connID := uint64(4200)
		sm.RegisterConnWriter(connID, func(data []byte) error {
			_, err := serverConn.Write(data)
			return err
		})
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession: %v", err)
		}

		go func() {
			_ = sender.sendCallback(context.Background(), CallbackRequest{
				OpCode:  types.OP_CB_RECALL,
				Payload: EncodeCBRecallOp(&types.Stateid4{}, false, []byte("fh")),
			})
		}()

		var headerBuf [4]byte
		if _, err := io.ReadFull(clientConn, headerBuf[:]); err != nil {
			t.Fatalf("read header: %v", err)
		}
		fragLen := binary.BigEndian.Uint32(headerBuf[:]) & 0x7FFFFFFF
		body := make([]byte, fragLen)
		if _, err := io.ReadFull(clientConn, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		return binary.BigEndian.Uint32(body[0:4])
	}

	xidA := firstXID()
	xidB := firstXID()
	if xidA == xidB {
		t.Errorf("two senders minted the same XID %d; callback XIDs must be unique per connection", xidA)
	}
}

// TestGetBackchannelSender_SkipsSessionWithNoLiveBackBinding pins the selection.
// A sender now exists per back-bound session, so a client with two sessions has
// two. Returning the first one found keeps choosing a session whose connection
// has since closed: every recall through it fails with "no back-bound
// connection" and revokes the delegation, while a sibling session of the same
// client still has a live path to that client.
func TestGetBackchannelSender_SkipsSessionWithNoLiveBackBinding(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	var verifier [8]byte
	copy(verifier[:], "bcsel001")
	eid, err := sm.ExchangeID([]byte("sender-selection-client"), verifier, 0, nil, "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}

	newSession := func(seq uint32) types.SessionId4 {
		t.Helper()
		res, _, cerr := sm.CreateSession(
			eid.ClientID, seq,
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
		)
		if cerr != nil {
			t.Fatalf("CreateSession: %v", cerr)
		}
		return res.SessionID
	}

	deadSessionID := newSession(eid.SequenceID)
	liveSessionID := newSession(eid.SequenceID + 1)

	// Give both sessions a sender, as StartBackchannelSender does per session.
	for _, id := range []types.SessionId4{deadSessionID, liveSessionID} {
		sess := sm.GetSession(id)
		if sess == nil {
			t.Fatalf("session %x not found", id)
		}
		sess.backchannelSender = NewBackchannelSender(id, eid.ClientID, 0x40000000, nil, sess.BackChannelSlots, sm)
	}

	// Only the live session keeps a back-bound connection. The dead one is
	// given one and then loses it, which is the sequence a closing connection
	// produces, rather than never having had one.
	deadConnID, liveConnID := uint64(2001), uint64(2002)
	sm.RegisterConnWriter(deadConnID, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(deadConnID, deadSessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession(dead): %v", err)
	}
	sm.RegisterConnWriter(liveConnID, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(liveConnID, liveSessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession(live): %v", err)
	}
	sm.UnregisterConnWriter(deadConnID)

	if _, _, _, ok := sm.getBackBoundConnWriter(deadSessionID, 0); ok {
		t.Fatal("fixture is wrong: the dead session still has a back-bound writer, " +
			"so this proves nothing about the selection")
	}
	if _, _, _, ok := sm.getBackBoundConnWriter(liveSessionID, 0); !ok {
		t.Fatal("fixture is wrong: the live session has no back-bound writer")
	}

	got := sm.getBackchannelSender(eid.ClientID)
	if got == nil {
		t.Fatal("no sender selected, though one session has a live back binding")
	}
	if got.sessionID != liveSessionID {
		t.Errorf("selected the session with no back-bound connection; every recall " +
			"through it would fail and revoke a delegation the client can still be reached about")
	}
}

// TestBackchannelParams_ProgramAndCredMoveTogether pins the callback parameters
// to one another. The client negotiates the program number and the credential
// as a pair in BACKCHANNEL_CTL, and every callback carries both. Published
// separately, a callback that loads them while the update is in flight sends the
// new program with the old credential — a pair the client never agreed to, which
// it rejects, and the rejection reads as a dead back channel.
func TestBackchannelParams_ProgramAndCredMoveTogether(t *testing.T) {
	bs := &BackchannelSender{}

	const progA, progB = uint32(0x40000000), uint32(0x40000001)
	parmsA := []types.CallbackSecParms4{{
		CbSecFlavor:  uint32(rpc.AuthUnix),
		AuthSysParms: &types.AuthSysParms{Stamp: 1, MachineName: "host-a", UID: 1000, GID: 1000},
	}}
	parmsB := []types.CallbackSecParms4{{
		CbSecFlavor:  uint32(rpc.AuthUnix),
		AuthSysParms: &types.AuthSysParms{Stamp: 2, MachineName: "host-bravo", UID: 2000, GID: 2000},
	}}
	credA, credB := EncodeCallbackCred(parmsA), EncodeCallbackCred(parmsB)
	if bytes.Equal(credA, credB) {
		t.Fatal("fixture is wrong: the two credentials encode identically, so a mixed pair would be invisible")
	}

	bs.setParams(progA, parmsA)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20000; i++ {
			if i%2 == 0 {
				bs.setParams(progB, parmsB)
			} else {
				bs.setParams(progA, parmsA)
			}
		}
	}()

	for i := 0; i < 20000; i++ {
		got := bs.currentParams()
		switch got.program {
		case progA:
			if !bytes.Equal(got.cred, credA) {
				t.Fatalf("program A carried credential B: the pair was torn")
			}
		case progB:
			if !bytes.Equal(got.cred, credB) {
				t.Fatalf("program B carried credential A: the pair was torn")
			}
		default:
			t.Fatalf("program = %#x, want one of the two published values", got.program)
		}
	}
	<-done
}

// TestProbeV41CallbackPath_StaleVerdictDiscarded covers the probe that finishes
// after the parameters it ran against were replaced. Probes are asynchronous and
// BACKCHANNEL_CTL starts a new one without being able to stop the old, so an
// earlier probe can land last and publish a verdict about a configuration the
// session no longer has — re-enabling delegations on retired parameters, or
// overwriting the verdict of the probe that replaced it.
func TestProbeV41CallbackPath_StaleVerdictDiscarded(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	sender.callbackTimeout = 300 * time.Millisecond

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	connID := uint64(7100)
	sm.RegisterConnWriter(connID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})
	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}
	// Read the probe but never answer it, so it runs until its timeout and the
	// verdict it would publish is "down".
	go func() {
		buf := make([]byte, 4096)
		_, _ = clientConn.Read(buf)
	}()

	// The state a successful earlier probe left behind.
	sm.setCBPathUp(sender.clientID, true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.probeV41CallbackPath(context.Background(), sender)
	}()

	// BACKCHANNEL_CTL replacing the parameters while that probe is in flight.
	time.Sleep(50 * time.Millisecond)
	sender.setParams(0x40000001, []types.CallbackSecParms4{{CbSecFlavor: 0}})

	<-done

	sm.mu.RLock()
	record := sm.clientsByID[sender.clientID]
	sm.mu.RUnlock()
	if record == nil {
		t.Fatal("fixture is wrong: no client record")
	}
	if !record.CBPathUp {
		t.Error("CBPathUp was cleared by a probe whose parameters had already been replaced: " +
			"its verdict describes a configuration the session no longer has")
	}
}

// TestUnbindConnection_ReleasesCallbackWaiters covers what a dying connection
// owes the senders waiting on it. Dropping the demultiplexer makes replies
// unroutable but leaves whoever is already waiting blocked until its own
// timeout, and the recall behind it waits with it — a dead connection turning
// into a revoked delegation on a client another session could still reach.
func TestUnbindConnection_ReleasesCallbackWaiters(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	const connID = uint64(7200)
	pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	replyCh, _ := pending.Register(0xabcd, types.SessionId4{})

	sm.UnbindConnection(connID)

	select {
	case _, open := <-replyCh:
		if open {
			t.Error("waiter received a reply rather than being released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after its connection was torn down")
	}
}

// TestNextUntriedSender_WalksEverySessionOfTheClient covers the search a recall
// makes before giving up. One alternate is not enough: with three sessions the
// selected one can have lost its binding and the first alternate can be
// unreachable while a third still carries the recall. Stopping early revokes a
// delegation from a client that was still listening.
func TestNextUntriedSender_WalksEverySessionOfTheClient(t *testing.T) {
	first, sm, firstSessionID := createTestBackchannelSender(t)
	clientID := first.clientID

	sm.mu.Lock()
	sm.sessionsByID[firstSessionID].backchannelSender = first
	sm.mu.Unlock()

	// A second and third session for the same client, each with its own sender.
	var extra []*BackchannelSender
	sm.mu.Lock()
	for i := 0; i < 2; i++ {
		var sid types.SessionId4
		sid[0] = byte(0xE0 + i)
		sess := &Session{SessionID: sid, ClientID: clientID}
		sender := &BackchannelSender{sessionID: sid, clientID: clientID, sm: sm}
		sess.backchannelSender = sender
		sm.sessionsByID[sid] = sess
		sm.sessionsByClientID[clientID] = append(sm.sessionsByClientID[clientID], sess)
		extra = append(extra, sender)
	}
	sm.mu.Unlock()

	tried := map[*BackchannelSender]bool{first: true}
	seen := map[*BackchannelSender]bool{}
	for s := sm.nextUntriedSender(clientID, tried); s != nil; s = sm.nextUntriedSender(clientID, tried) {
		if seen[s] {
			t.Fatal("nextUntriedSender returned a sender it had already handed out")
		}
		seen[s] = true
		tried[s] = true
	}

	for i, sender := range extra {
		if !seen[sender] {
			t.Errorf("alternate sender %d was never offered: the search stops before "+
				"exhausting the client's sessions", i)
		}
	}
}

// TestSetCBPathUpIfCurrent_RefusesARetiredGeneration pins the publish side of
// the stale-probe guard. Reading the generation and writing the record are one
// step, not two: a probe that checks the generation itself and then calls the
// setter leaves a window in which UpdateBackchannelParams lands between the
// two, and the verdict of a probe against parameters the session no longer has
// is written onto the client record anyway.
func TestSetCBPathUpIfCurrent_RefusesARetiredGeneration(t *testing.T) {
	bs, sm, _ := createTestBackchannelSender(t)

	generation := bs.currentParams().generation
	if !sm.setCBPathUpIfCurrent(bs, generation, true) {
		t.Fatal("a verdict on the current generation was not published")
	}
	if !cbPathUpOf(t, sm, bs.clientID) {
		t.Fatal("CBPathUp was not set by a published verdict")
	}

	// The session renegotiates its callback parameters, as CREATE_SESSION or a
	// BIND_CONN_TO_SESSION would. The in-flight probe's generation is now stale.
	bs.setParams(0x40000001, []types.CallbackSecParms4{{CbSecFlavor: 0}})

	if sm.setCBPathUpIfCurrent(bs, generation, false) {
		t.Error("a verdict from a retired generation was published")
	}
	if !cbPathUpOf(t, sm, bs.clientID) {
		t.Error("a retired verdict overwrote the record it should not have touched")
	}
}

func cbPathUpOf(t *testing.T, sm *StateManager, clientID uint64) bool {
	t.Helper()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	record := sm.clientRecordLocked(clientID)
	if record == nil {
		t.Fatalf("no client record for 0x%x", clientID)
	}
	return record.CBPathUp
}

// TestReapExpiredSessions_ReleasesBackchannelStateOfAnOrphanedConnection covers
// the second way a connection is retired. The socket-close path releases the
// writer and the pending-reply demultiplexer, but the reaper drops orphaned
// bindings directly; when it drops a connection's last one, the connection is
// just as gone, and anything waiting on a callback reply over it waits out its
// own timeout instead of being released.
func TestReapExpiredSessions_ReleasesBackchannelStateOfAnOrphanedConnection(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	const connID = uint64(7311)
	var orphanSession types.SessionId4
	orphanSession[0] = 0xC1

	pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	replyCh, _ := pending.Register(0x5150, types.SessionId4{})

	// A binding to a session that no longer exists — what the reaper collects.
	sm.connMu.Lock()
	binding := &BoundConnection{ConnectionID: connID, SessionID: orphanSession}
	sm.connByID[connID] = append(sm.connByID[connID], binding)
	sm.connBySession[orphanSession] = append(sm.connBySession[orphanSession], binding)
	sm.connMu.Unlock()

	sm.reapExpiredSessions()

	select {
	case _, open := <-replyCh:
		if open {
			t.Error("waiter received a reply rather than being released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after the reaper dropped its connection's last binding")
	}

	sm.connMu.Lock()
	_, writerHeld := sm.connWriters[connID]
	_, repliesHeld := sm.cbRepliesByConn[connID]
	sm.connMu.Unlock()
	if writerHeld {
		t.Error("the callback writer outlived the connection the reaper retired")
	}
	if repliesHeld {
		t.Error("the pending-reply demultiplexer outlived the connection the reaper retired")
	}
}

// TestDestroySession_ReleasesBackchannelStateOfItsLastConnection covers the
// third path that retires a connection. DESTROY_SESSION drops each of the
// session's bindings; when the session holds a connection's only binding, that
// connection is as gone as it is after a socket close, and its writer and
// pending-reply table have to go with it. Left behind, a caller already waiting
// on a callback reply blocks until its own timeout with nothing left to answer.
func TestDestroySession_ReleasesBackchannelStateOfItsLastConnection(t *testing.T) {
	bs, sm, sessionID := createTestBackchannelSender(t)
	_ = bs

	const connID = uint64(7422)
	pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	replyCh, _ := pending.Register(0x9001, types.SessionId4{})

	sm.connMu.Lock()
	binding := &BoundConnection{ConnectionID: connID, SessionID: sessionID}
	sm.connByID[connID] = append(sm.connByID[connID], binding)
	sm.connBySession[sessionID] = append(sm.connBySession[sessionID], binding)
	sm.connMu.Unlock()

	if err := sm.DestroySession(sessionID); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	select {
	case _, open := <-replyCh:
		if open {
			t.Error("waiter received a reply rather than being released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after the session holding its connection's only binding was destroyed")
	}

	sm.connMu.Lock()
	_, writerHeld := sm.connWriters[connID]
	_, repliesHeld := sm.cbRepliesByConn[connID]
	sm.connMu.Unlock()
	if writerHeld {
		t.Error("the callback writer outlived the destroyed session's last connection")
	}
	if repliesHeld {
		t.Error("the pending-reply demultiplexer outlived the destroyed session's last connection")
	}
}

// TestSendCallback_NoBackBoundConnectionIsNotEvidenceAboutTheClient pins the
// classification the three-valued recall outcome exists for. A session with no
// back-bound connection never puts the callback on a wire, so the failure says
// nothing about whether the client is answering — another of its sessions may
// still carry the recall. attemptRecallV41 reads this sentinel to choose
// recallSenderLocal over recallNoPath; without it every failure counted as a
// dead path, cleared CBPathUp and withheld delegations from a live client.
//
// Asserted on sendCallback directly rather than through attemptRecallV41: with
// no sender goroutine running, that path reaches recallSenderLocal via its
// 30-second result timeout instead, and would pass whether or not this sentinel
// existed.
func TestSendCallback_NoBackBoundConnectionIsNotEvidenceAboutTheClient(t *testing.T) {
	sender, _, _ := createTestBackchannelSender(t)

	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: []byte{0x00},
	})
	if err == nil {
		t.Fatal("precondition: the send succeeded, so this test reaches no failure to classify")
	}
	if !errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("error %q does not carry errCallbackNotAttempted: a recall would count it "+
			"against the client's callback path", err)
	}
}

// TestSendCallbackWithRetry_NeverAttemptedIsReportedWithoutBackoff pins two
// things the retry loop got wrong about a send that reached no socket. It must
// not sleep through the backoff — the recall is held there while the caller
// could be trying the client's next session, and no amount of waiting grows a
// back-bound connection onto this one — and it must not mark a backchannel
// fault, which blames the client for a route never attempted.
func TestSendCallbackWithRetry_NeverAttemptedIsReportedWithoutBackoff(t *testing.T) {
	sender, sm, _ := createTestBackchannelSender(t)

	resultCh := make(chan error, 1)
	start := time.Now()
	sender.sendCallbackWithRetry(context.Background(), CallbackRequest{
		OpCode:   types.OP_CB_RECALL,
		Payload:  []byte{0x00},
		ResultCh: resultCh,
	})
	elapsed := time.Since(start)

	err := <-resultCh
	if err == nil {
		t.Fatal("precondition: the send succeeded, so there is no classification to check")
	}
	if !errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("error %q lost errCallbackNotAttempted through the retry loop", err)
	}
	// The first backoff alone is seconds long; anything near it means the loop
	// retried a send that had nothing to retry.
	if elapsed > time.Second {
		t.Errorf("took %s: a never-attempted send was retried through the backoff", elapsed)
	}
	if faulted := backchannelFaultOf(t, sm, sender.clientID); faulted {
		t.Error("a send that reached no socket marked the client's backchannel faulted")
	}
}

func backchannelFaultOf(t *testing.T, sm *StateManager, clientID uint64) bool {
	t.Helper()
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	return sm.backchannelFaults[clientID]
}

// TestPendingCBReplies_RegisterReportsARetiredTable pins the signal the panic
// guard alone did not give. Returning a closed channel keeps a caller from
// blocking forever, but a reply read off a closed channel looks exactly like a
// client that answered with nothing: the sender wrote to a dead socket and then
// scored the silence against the client, clearing CBPathUp and starting
// revocation for a recall that never left the building.
func TestPendingCBReplies_RegisterReportsARetiredTable(t *testing.T) {
	p := NewPendingCBReplies()
	p.FailAll()

	ch, registered := p.Register(0x1234, types.SessionId4{})
	if registered {
		t.Error("Register reported success on a table that had already been failed")
	}
	select {
	case _, open := <-ch:
		if open {
			t.Error("the returned channel carried a value")
		}
	default:
		t.Error("the returned channel is neither registered nor closed, so a caller would block on it")
	}
}

// TestWorstCaseSendDuration_ExceedsEveryAttemptAndBackoff pins the arithmetic
// the recall watchdog depends on. The outer wait must outlast the sender's own
// schedule, or it expires mid-retry and reports a callback that may already
// have reached the client as though nothing was attempted.
func TestWorstCaseSendDuration_ExceedsEveryAttemptAndBackoff(t *testing.T) {
	bs := &BackchannelSender{callbackTimeout: defaultBackchannelTimeout}

	// Two bounded writes (the chosen connection and its alternate) and the
	// reply wait, each on the same budget, for every attempt.
	var want time.Duration
	for i := 0; i < backchannelMaxRetries; i++ {
		want += 3 * defaultBackchannelTimeout
		if i < backchannelMaxRetries-1 {
			want += backchannelRetryDelays[i]
		}
	}
	if got := bs.worstCaseSendDuration(); got != want {
		t.Errorf("worstCaseSendDuration() = %s, want %s", got, want)
	}
	if bs.worstCaseSendDuration()+recallResultGrace <= 30*time.Second {
		t.Errorf("the derived wait (%s) is no longer than the fixed 30s it replaced, "+
			"so it still expires while the sender is retrying",
			bs.worstCaseSendDuration()+recallResultGrace)
	}
}

// TestProbeV41CallbackPath_DeferredProbeRunsAfterTheOneInFlight pins the other
// half of the one-probe-per-session rule. Admitting one at a time stops a client
// renegotiating in a loop from accumulating goroutines, but a probe turned away
// while another runs was asked for against parameters the running one does not
// know about — and that one's verdict is discarded for a stale generation when
// it finishes. Dropping the request outright means nothing ever evaluates the
// new parameters and delegations stay withheld indefinitely.
func TestProbeV41CallbackPath_DeferredProbeRunsAfterTheOneInFlight(t *testing.T) {
	bs, sm, _ := createTestBackchannelSender(t)

	// A probe is already running; the one that arrives now must record that it
	// was turned away rather than simply disappearing.
	bs.probeMu.Lock()
	bs.probeRunning = true
	bs.probeMu.Unlock()

	sm.probeV41CallbackPath(context.Background(), bs)

	bs.probeMu.Lock()
	queued := bs.probeQueued
	bs.probeMu.Unlock()
	if !queued {
		t.Fatal("a probe turned away while another was in flight left no request behind, " +
			"so the new callback parameters would never be evaluated")
	}

	// The in-flight one finishes. Its own run must pick that request up and
	// clear it, so the hand-over happens exactly once.
	bs.probeMu.Lock()
	bs.probeRunning = false
	bs.probeMu.Unlock()

	sm.probeV41CallbackPath(context.Background(), bs)

	bs.probeMu.Lock()
	queued, running := bs.probeQueued, bs.probeRunning
	bs.probeMu.Unlock()
	if queued {
		t.Error("the deferred request was still pending after a probe ran to completion")
	}
	if running {
		t.Error("the probe left itself marked as running, so no later probe can ever start")
	}
}

// TestProbeV41CallbackPath_ARequestArrivingAtTheEndIsNotLost covers the window
// that a lock-free handoff cannot close. A caller that finds a probe running
// queues and returns; if the runner has already decided to stop by then, and
// the two steps are not one, the request is consumed by nobody and the new
// callback parameters go unprobed until something else happens to run one.
func TestProbeV41CallbackPath_ARequestArrivingAtTheEndIsNotLost(t *testing.T) {
	bs, sm, _ := createTestBackchannelSender(t)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sm.probeV41CallbackPath(context.Background(), bs)
		}()
	}
	wg.Wait()

	// Whatever interleaving happened, the guard must come to rest: nothing
	// running, and no request left for a runner that has already gone.
	bs.probeMu.Lock()
	queued, running := bs.probeQueued, bs.probeRunning
	bs.probeMu.Unlock()
	if running {
		t.Error("the guard is still marked running with every caller returned: no probe can start again")
	}
	if queued {
		t.Error("a request outlived every runner, so the parameters that asked for it are never probed")
	}
}

// TestEvictV41Client_ReleasesBackchannelStateOfItsLastConnection covers the
// fourth path that retires a connection, and the one that does not go through
// DESTROY_SESSION at all: a client purge (eviction, DESTROY_CLIENTID, or an
// EXCHANGE_ID that supersedes a live record) drops the client's sessions
// wholesale. A session removed that way still holds connection bindings, and a
// connection whose last binding is one of them is as gone as after a socket
// close — its writer and pending-reply table have to go with it, or a caller
// already waiting on a callback reply blocks with nothing left to answer it.
func TestEvictV41Client_ReleasesBackchannelStateOfItsLastConnection(t *testing.T) {
	bs, sm, sessionID := createTestBackchannelSender(t)

	const connID = uint64(7533)
	pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	replyCh, _ := pending.Register(0x9002, types.SessionId4{})

	sm.connMu.Lock()
	binding := &BoundConnection{ConnectionID: connID, SessionID: sessionID}
	sm.connByID[connID] = append(sm.connByID[connID], binding)
	sm.connBySession[sessionID] = append(sm.connBySession[sessionID], binding)
	sm.connMu.Unlock()

	if err := sm.EvictV41Client(bs.clientID); err != nil {
		t.Fatalf("EvictV41Client: %v", err)
	}

	select {
	case _, open := <-replyCh:
		if open {
			t.Error("waiter received a reply rather than being released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after its connection's only session was purged with the client")
	}

	sm.connMu.Lock()
	_, writerHeld := sm.connWriters[connID]
	_, repliesHeld := sm.cbRepliesByConn[connID]
	bindingsHeld := len(sm.connBySession[sessionID])
	sm.connMu.Unlock()
	if writerHeld {
		t.Error("the callback writer outlived the purged client's last connection")
	}
	if repliesHeld {
		t.Error("the pending-reply demultiplexer outlived the purged client's last connection")
	}
	if bindingsHeld != 0 {
		t.Errorf("connBySession still holds %d binding(s) for the purged client's session", bindingsHeld)
	}
}

// setBindingActivity pins one binding's LastActivity so a test can decide which
// connection the backchannel picks instead of inferring it from bind order.
func setBindingActivity(t *testing.T, sm *StateManager, sessionID types.SessionId4, connID uint64, at time.Time) {
	t.Helper()
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	for _, b := range sm.connBySession[sessionID] {
		if b.ConnectionID == connID {
			b.LastActivity = at
			return
		}
	}
	t.Fatalf("no binding for connection %d on session %s", connID, sessionID.String())
}

// TestSendCallback_RetiredAlternateKeepsTheTransportError covers what the
// verdict is when the first connection's write fails and the alternate is
// retired before the callback can be registered on it.
//
// The send did reach a transport and fail there, which is evidence about the
// client's callback path. Reported as "not attempted", the recall classifies it
// as sender-local, leaves CBPathUp standing, and the next OPEN hands out a
// delegation over a path that has already failed.
func TestSendCallback_RetiredAlternateKeepsTheTransportError(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 200 * time.Millisecond

	// The alternate is still in the tables but its reply router has been
	// retired, which is the race the send loses.
	altConnID := uint64(7101)
	altPending := sm.RegisterConnWriter(altConnID, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(altConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (alternate): %v", err)
	}
	altPending.FailAll()

	failConnID := uint64(7100)
	sm.RegisterConnWriter(failConnID, func([]byte) error {
		return errors.New("broken pipe")
	})
	if _, err := sm.BindConnToSession(failConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (fail): %v", err)
	}

	// Stamp the activity times rather than letting the two binds race the
	// clock. Selection is "most recently active", compared with a strict After,
	// so two binds that land inside one clock tick leave the FIRST one winning
	// — and on a platform with a coarse clock that is the retired alternate,
	// which sends this down the wrong branch and fails for a reason that has
	// nothing to do with what it tests.
	setBindingActivity(t, sm, sessionID, failConnID, time.Now())
	setBindingActivity(t, sm, sessionID, altConnID, time.Now().Add(-time.Minute))

	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01}),
	})
	if err == nil {
		t.Fatal("sendCallback succeeded with a failed write and a retired alternate")
	}
	if errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("a write that failed on a transport was reported as never attempted: %v", err)
	}
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("the transport failure was dropped from the error: %v", err)
	}
}

// TestSendCallback_RetiredSelectionDoesNotStrandALiveConnection covers the race
// between choosing a back-bound connection and registering the reply waiter on
// it. The chosen one can be retired in that window, and that says nothing about
// the client's callback path — only that this socket went away. Reporting the
// send as never attempted would have the recall classify it as sender-local and
// start the short revocation timer, while a second back-bound connection for
// the same session sat there able to carry the callback.
func TestSendCallback_RetiredSelectionDoesNotStrandALiveConnection(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 200 * time.Millisecond

	// The preferred connection: most recently active, and retired.
	deadConnID := uint64(7201)
	deadPending := sm.RegisterConnWriter(deadConnID, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(deadConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (dead): %v", err)
	}
	deadPending.FailAll()

	// The one that is still usable. Its write is recorded rather than answered,
	// so the send ends at its own reply timeout — which is not the point here;
	// what matters is that it was reached at all.
	var wroteOnLive atomic.Bool
	liveConnID := uint64(7202)
	sm.RegisterConnWriter(liveConnID, func([]byte) error {
		wroteOnLive.Store(true)
		return nil
	})
	if _, err := sm.BindConnToSession(liveConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (live): %v", err)
	}

	setBindingActivity(t, sm, sessionID, deadConnID, time.Now())
	setBindingActivity(t, sm, sessionID, liveConnID, time.Now().Add(-time.Minute))

	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01}),
	})
	if err == nil {
		t.Fatal("sendCallback succeeded although no reply was ever delivered")
	}
	if errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("a session with a usable back-bound connection was reported as never attempted: %v", err)
	}
	if !wroteOnLive.Load() {
		t.Error("the live connection was never written to: a retired reply table cost the send " +
			"rather than costing one candidate")
	}
}

// TestProbeCallbackPath_ARetiredSocketIsNotTheClientsVerdict covers what a
// CB_NULL failure is allowed to mean. The probe's answer is published on the
// client record and nothing re-probes until the next parameter update, so
// reporting a socket that went away as "the client does not answer callbacks"
// withholds delegations indefinitely — while a second back-bound connection for
// the same session sits there able to answer.
func TestProbeCallbackPath_ARetiredSocketIsNotTheClientsVerdict(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 200 * time.Millisecond

	// The preferred connection, whose write fails the way a dead socket does.
	deadConnID := uint64(7301)
	sm.RegisterConnWriter(deadConnID, func([]byte) error {
		return errors.New("broken pipe")
	})
	if _, err := sm.BindConnToSession(deadConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (dead): %v", err)
	}

	// The one that answers. It replies to whatever XID it is handed.
	// Takes the bytes and says nothing, which is what "the client is not
	// answering callbacks" actually looks like — and is a verdict about the
	// client rather than about a socket.
	var reachedLive atomic.Bool
	liveConnID := uint64(7302)
	sm.RegisterConnWriter(liveConnID, func([]byte) error {
		reachedLive.Store(true)
		return nil
	})
	if _, err := sm.BindConnToSession(liveConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (live): %v", err)
	}

	setBindingActivity(t, sm, sessionID, deadConnID, time.Now())
	setBindingActivity(t, sm, sessionID, liveConnID, time.Now().Add(-time.Minute))

	err := sender.probeCallbackPath(context.Background())

	if !reachedLive.Load() {
		t.Fatal("the probe never reached the usable connection: a dead socket cost the verdict " +
			"rather than costing one candidate")
	}
	// The live connection never answers, so the probe still ends in an error —
	// but it must be the timeout, which is a statement about the client, not the
	// write failure from the socket it was supposed to move past.
	if err == nil {
		t.Fatal("the probe succeeded although nothing answered CB_NULL")
	}
	if strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("the probe reported the dead socket as its verdict: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("verdict is %q, want the CB_NULL timeout", err)
	}
}

// TestSendCallback_ARetiredWaiterIsALocalOutcome covers a connection retired
// while a callback is already on the wire. FailAll closes the waiter, and a
// receive from a closed channel yields nil — which ValidateCBReply reads as a
// malformed reply and the retry loop classifies as no callback path at all.
// That clears CBPathUp and revokes a delegation because a socket on THIS side
// went away, which is the one thing it is not evidence of.
func TestSendCallback_ARetiredWaiterIsALocalOutcome(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 5 * time.Second

	connID := uint64(7401)
	var pending *PendingCBReplies
	pending = sm.RegisterConnWriter(connID, func([]byte) error {
		// The write lands, and the connection is retired right behind it.
		go pending.FailAll()
		return nil
	})
	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01}),
	})
	if err == nil {
		t.Fatal("sendCallback succeeded although the waiter was closed without a reply")
	}
	if !errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("a connection retired under an in-flight callback was reported as a client "+
			"verdict rather than a local one: %v", err)
	}
}

// TestGetBackBoundConnWriter_SkipsABindingWithNoWriter pins that selection ranks
// candidates rather than testing one. A binding exists before its writer and
// reply table are registered and outlives them once the connection is retired,
// so the most recently active binding is regularly the one that cannot carry a
// callback. Answering "no path" on that basis — with an older live connection on
// the same session sitting usable — is how a recall revokes a delegation and a
// probe publishes a down verdict nothing re-runs.
func TestGetBackBoundConnWriter_SkipsABindingWithNoWriter(t *testing.T) {
	_, sm, sessionID := createTestBackchannelSender(t)

	// The freshest binding, with no writer registered for it yet.
	bare := uint64(7601)
	sm.connMu.Lock()
	b1 := &BoundConnection{ConnectionID: bare, SessionID: sessionID, Direction: ConnDirBoth}
	sm.connByID[bare] = append(sm.connByID[bare], b1)
	sm.connBySession[sessionID] = append(sm.connBySession[sessionID], b1)
	sm.connMu.Unlock()

	// An older one that is fully usable.
	usable := uint64(7602)
	sm.RegisterConnWriter(usable, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(usable, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	setBindingActivity(t, sm, sessionID, bare, time.Now())
	setBindingActivity(t, sm, sessionID, usable, time.Now().Add(-time.Minute))

	connID, writer, pending, ok := sm.getBackBoundConnWriter(sessionID, 0)
	if !ok {
		t.Fatal("no back-bound connection found although one is registered and usable: " +
			"a binding with no writer was treated as the session having no path")
	}
	if connID != usable {
		t.Errorf("selected connection %d, want the usable %d", connID, usable)
	}
	if writer == nil || pending == nil {
		t.Error("selection returned a connection without a writer or reply table")
	}
}

// TestSendCallbackWithRetry_ARejectedReplyIsNotADeadPath covers the difference
// between "we could not reach the client" and "the client answered and said no".
// A reply that fails validation — an RPC or NFS error status, or one that does
// not decode — arrived over a working callback path. Treating it as a transport
// failure retries a request the client already refused, and then classifies the
// path as dead: CBPathUp cleared and a backchannel fault raised against a client
// that demonstrably received the callback and replied to it.
func TestSendCallbackWithRetry_ARejectedReplyIsNotADeadPath(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 2 * time.Second

	var writes atomic.Int32
	connID := uint64(7701)
	var pending *PendingCBReplies
	pending = sm.RegisterConnWriter(connID, func(data []byte) error {
		writes.Add(1)
		// Answer with bytes that are a reply, but not one this server accepts.
		xid := binary.BigEndian.Uint32(data[4:8])
		go pending.Deliver(xid, []byte{0x00, 0x00, 0x00, 0x00})
		return nil
	})
	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	resultCh := make(chan error, 1)
	sender.sendCallbackWithRetry(context.Background(), CallbackRequest{
		OpCode:   types.OP_CB_RECALL,
		Payload:  EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01}),
		ResultCh: resultCh,
	})

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no result reported")
	}
	if err == nil {
		t.Fatal("a rejected reply was reported as a successful callback")
	}
	if !errors.Is(err, errCallbackRejected) {
		t.Errorf("a reply the client sent was classified as a transport failure: %v", err)
	}
	if n := writes.Load(); n != 1 {
		t.Errorf("the callback was written %d times; a request the client already refused "+
			"must not be retried", n)
	}
}

// TestSendCallback_AStoppedSenderDoesNotWaitOutTheTimeout covers a session
// destroyed while one of its callbacks is on the wire. The reply table can
// outlive the session — a connection carrying another session keeps it — so the
// waiter is not failed, and without watching stopCh the send holds its sender
// for the whole callback timeout after the session it belongs to is gone.
func TestSendCallback_AStoppedSenderDoesNotWaitOutTheTimeout(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 30 * time.Second // never reached if stopCh works

	connID := uint64(7801)
	sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- sender.sendCallback(context.Background(), CallbackRequest{
			OpCode:  types.OP_CB_RECALL,
			Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01}),
		})
	}()

	// Let the write land, then retire the session.
	time.Sleep(50 * time.Millisecond)
	close(sender.stopCh)

	select {
	case err := <-done:
		if !errors.Is(err, errCallbackNotAttempted) {
			t.Errorf("a callback abandoned because its session was destroyed was not reported "+
				"as a local outcome: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sendCallback ignored its sender stopping and is waiting out the callback timeout")
	}
}

// TestCancelSession_ReleasesOnlyTheOwnersWaiters pins the demultiplexing the
// session-tagged waiter exists for. The reply table is held per connection and
// several sessions can share one, so a connection-wide release cannot answer
// for a session that goes away while the connection stays live — its sender
// would block until its own timeout with nothing able to route to it. The
// cancellation must be addressable by session, and must leave the other
// sessions on that connection untouched: cancelling their waiters would report
// a callback as unanswered for a client that is still reachable.
func TestCancelSession_ReleasesOnlyTheOwnersWaiters(t *testing.T) {
	p := NewPendingCBReplies()

	var owner, other types.SessionId4
	owner[0], other[0] = 0xAA, 0xBB

	ownedCh, _ := p.Register(0x1001, owner)
	otherCh, _ := p.Register(0x1002, other)

	p.CancelSession(owner)

	select {
	case _, open := <-ownedCh:
		if open {
			t.Error("the cancelled session's waiter received a reply rather than being released")
		}
	case <-time.After(time.Second):
		t.Fatal("the cancelled session's waiter was left armed on a table that cannot route to it")
	}

	// The other session's waiter must still be live and routable.
	select {
	case <-otherCh:
		t.Fatal("another session's waiter was released by this session's cancellation")
	default:
	}
	if !p.Deliver(0x1002, []byte("reply")) {
		t.Fatal("the other session's waiter is no longer routable after a foreign session was cancelled")
	}
	select {
	case data := <-otherCh:
		if !bytes.Equal(data, []byte("reply")) {
			t.Errorf("received %q, want %q", data, "reply")
		}
	default:
		t.Fatal("the delivered reply never reached the other session's waiter")
	}
}

// TestDestroySession_ReleasesItsWaitersOnASharedLiveConnection covers the case
// the per-connection release cannot reach. Session A and session B are both
// bound to connection C; destroying A leaves C with B's binding, so the
// connection-wide FailAll never runs. A's in-flight recall waiter used to stay
// registered in C's shared table and block for the full callback timeout, then
// retry against a session that no longer exists.
func TestDestroySession_ReleasesItsWaitersOnASharedLiveConnection(t *testing.T) {
	_, sm, sessionA := createTestBackchannelSender(t)

	// A second session on the same connection. Its own bindings are what keep
	// the connection alive past A's teardown.
	sessionB := types.SessionId4{0xB2}
	sm.mu.Lock()
	sm.sessionsByID[sessionB] = &Session{SessionID: sessionB}
	sm.mu.Unlock()

	const connID = uint64(7601)
	pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	replyA, _ := pending.Register(0x2001, sessionA)
	replyB, _ := pending.Register(0x2002, sessionB)

	sm.connMu.Lock()
	for _, sid := range []types.SessionId4{sessionA, sessionB} {
		binding := &BoundConnection{ConnectionID: connID, SessionID: sid, Direction: ConnDirBoth}
		sm.connByID[connID] = append(sm.connByID[connID], binding)
		sm.connBySession[sid] = append(sm.connBySession[sid], binding)
	}
	sm.connMu.Unlock()

	if err := sm.DestroySession(sessionA); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	select {
	case _, open := <-replyA:
		if open {
			t.Error("the destroyed session's waiter received a reply rather than being released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the destroyed session's waiter stayed armed on a connection another session keeps alive")
	}

	// The surviving session's waiter and the connection's state must be intact:
	// this connection is still carrying traffic for session B.
	select {
	case <-replyB:
		t.Fatal("the surviving session's waiter was released by the other session's teardown")
	default:
	}
	if !pending.Deliver(0x2002, []byte("reply")) {
		t.Fatal("the surviving session's waiter is no longer routable")
	}
	sm.connMu.Lock()
	_, writerHeld := sm.connWriters[connID]
	_, repliesHeld := sm.cbRepliesByConn[connID]
	sm.connMu.Unlock()
	if !writerHeld || !repliesHeld {
		t.Error("the connection's callback state was released while another session was still bound to it")
	}
}

// TestBindConnToSession_RebindAwayFromBackReleasesItsWaiters covers the second
// unreachable case. A session rebound from a back-capable direction to
// ConnDirFore has no callback route over that connection any more, but the
// binding is replaced directly rather than through dropConnBindingLocked (which
// would release the writer it is about to re-register), so nothing used to
// cancel the waiters it had left there.
func TestBindConnToSession_RebindAwayFromBackReleasesItsWaiters(t *testing.T) {
	_, sm, sessionID := createTestBackchannelSender(t)

	const connID = uint64(7602)
	pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
	replyCh, _ := pending.Register(0x3001, sessionID)

	// Bind as back-capable first, then rebind the same pair to fore-only.
	sm.connMu.Lock()
	binding := &BoundConnection{ConnectionID: connID, SessionID: sessionID, Direction: ConnDirBoth}
	sm.connByID[connID] = append(sm.connByID[connID], binding)
	sm.connBySession[sessionID] = append(sm.connBySession[sessionID], binding)
	sm.connMu.Unlock()

	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	select {
	case _, open := <-replyCh:
		if open {
			t.Error("the rebind's waiter received a reply rather than being released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a session rebound away from the back channel kept a waiter with no callback route to answer it")
	}
}

// TestSendCallback_WalksPastTwoRetiredCandidates covers the walk past retired candidates. Selection and
// registration do not share a hold on connMu, so a connection can be retired in
// the window between them. The walk used to stop after two candidates, which a
// session can exceed: it may hold up to maxConnsPerSession bindings, and the
// freshest ones are exactly the ones most likely to be unusable, because a
// binding exists before its writer is registered and outlives it once the
// connection is retired. With the two freshest both retired, the send was
// reported as never attempted while a usable connection sat there — for a
// recall that revokes a delegation over a reachable path, and for a probe that
// withholds delegations until the next parameter update, since nothing retries
// a probe.
func TestSendCallback_WalksPastTwoRetiredCandidates(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 200 * time.Millisecond

	// Two freshest candidates, both retired before their reply table can be
	// registered on. Ranked ahead of the live one by activity below.
	for i, connID := range []uint64{7401, 7402} {
		pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession (retired %d): %v", i, err)
		}
		pending.FailAll()
	}

	// The third candidate, still usable. Its write is recorded rather than
	// answered, so the send ends at its reply timeout; what matters is that it
	// was reached at all.
	var wroteOnLive atomic.Bool
	const liveConnID = uint64(7403)
	sm.RegisterConnWriter(liveConnID, func([]byte) error {
		wroteOnLive.Store(true)
		return nil
	})
	if _, err := sm.BindConnToSession(liveConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (live): %v", err)
	}

	// The two retired candidates rank first; the live one ranks last. Stamped
	// rather than left to the clock so the order is the one this test means.
	setBindingActivity(t, sm, sessionID, 7401, time.Now())
	setBindingActivity(t, sm, sessionID, 7402, time.Now().Add(-time.Second))
	setBindingActivity(t, sm, sessionID, liveConnID, time.Now().Add(-time.Minute))

	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte{0x01}),
	})
	if err == nil {
		t.Fatal("sendCallback succeeded although no reply was ever delivered")
	}
	if errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("a session with a usable back-bound connection beyond the two freshest was "+
			"reported as never attempted: %v", err)
	}
	if !wroteOnLive.Load() {
		t.Error("the third connection was never written to: the walk stopped before exhausting " +
			"the session's back-capable bindings")
	}
}

// TestSelectBackBoundWaiter_WalksPastTwoRetiredCandidates is the unit-level
// half of the same rule: with the two freshest candidates retired, the selector must
// still reach a usable third rather than giving up. The walk is bounded by the
// session's back-capable bindings, not by a literal attempt count.
func TestSelectBackBoundWaiter_WalksPastTwoRetiredCandidates(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()

	// Two freshest candidates, both retired: registering on either fails.
	for i, connID := range []uint64{7501, 7502} {
		pending := sm.RegisterConnWriter(connID, func([]byte) error { return nil })
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession (retired %d): %v", i, err)
		}
		pending.FailAll()
	}

	// The usable third, ranked last.
	const liveConnID = uint64(7503)
	sm.RegisterConnWriter(liveConnID, func([]byte) error { return nil })
	if _, err := sm.BindConnToSession(liveConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession (live): %v", err)
	}

	setBindingActivity(t, sm, sessionID, 7501, time.Now())
	setBindingActivity(t, sm, sessionID, 7502, time.Now().Add(-time.Second))
	setBindingActivity(t, sm, sessionID, liveConnID, time.Now().Add(-time.Minute))

	connID, _, _, _, ok := sender.selectBackBoundWaiter(1, map[uint64]bool{})
	if !ok {
		t.Fatal("selector gave up although a usable back-capable connection was bound")
	}
	if connID != liveConnID {
		t.Errorf("selector chose connection %d, want the usable %d", connID, liveConnID)
	}
}
