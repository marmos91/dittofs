package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// BackchannelSender Integration Tests (Phase 22)
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
		session.BackChannelSlots,
		sm,
		nil, // no metrics for basic test
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

		// Parse past RPC header (10 uint32s = 40 bytes) to get to CB_COMPOUND args
		// XID(4) + msg_type(4) + rpc_vers(4) + prog(4) + vers(4) + proc(4) +
		// cred_flavor(4) + cred_len(4) + verf_flavor(4) + verf_len(4) = 40 bytes
		// Then CB_COMPOUND: tag_len(4) + minorversion(4) + callback_ident(4) +
		// op_count(4) + first_op_code(4) = 20 bytes
		// CB_SEQUENCE args start at offset 60
		// CB_SEQUENCE args: sessionID(16 bytes) + seqID(4 bytes)
		if len(body) < 60+16+4 {
			t.Fatalf("body too short: %d bytes", len(body))
		}
		seqID := binary.BigEndian.Uint32(body[60+16 : 60+16+4])

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

	// The seqID should be different and increasing between calls
	if seqID1 == seqID2 {
		t.Errorf("seqID should increment between callbacks: seqID1=%d, seqID2=%d", seqID1, seqID2)
	}
	if seqID2 <= seqID1 {
		t.Errorf("seqID should increase: seqID1=%d, seqID2=%d", seqID1, seqID2)
	}
}

// TestPendingCBReplies_RegisterDeliverCancel tests XID routing:
// register, deliver, verify channel receives data; also test Cancel cleanup.
func TestPendingCBReplies_RegisterDeliverCancel(t *testing.T) {
	p := NewPendingCBReplies()

	// Register and deliver
	ch := p.Register(42)
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
	ch2 := p.Register(100)
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

// TestBackchannelMetrics_NilSafe verifies all metric methods can be called on nil receiver.
func TestBackchannelMetrics_NilSafe(t *testing.T) {
	var m *BackchannelMetrics

	// None of these should panic
	m.RecordCallback()
	m.RecordFailure()
	m.RecordRetry()
	m.ObserveDuration(100 * time.Millisecond)
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
