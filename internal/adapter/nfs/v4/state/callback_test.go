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
// ParseUniversalAddr Tests
// ============================================================================

func TestParseUniversalAddr_IPv4(t *testing.T) {
	host, port, err := ParseUniversalAddr("tcp", "10.1.3.7.2.15")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "10.1.3.7" {
		t.Errorf("host = %q, want %q", host, "10.1.3.7")
	}
	// port = 2*256 + 15 = 527
	if port != 527 {
		t.Errorf("port = %d, want %d", port, 527)
	}
}

func TestParseUniversalAddr_IPv4_HighPort(t *testing.T) {
	host, port, err := ParseUniversalAddr("tcp", "192.168.1.1.31.144")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "192.168.1.1" {
		t.Errorf("host = %q, want %q", host, "192.168.1.1")
	}
	// port = 31*256 + 144 = 8080
	if port != 8080 {
		t.Errorf("port = %d, want %d", port, 8080)
	}
}

func TestParseUniversalAddr_IPv4_LowPort(t *testing.T) {
	host, port, err := ParseUniversalAddr("tcp", "127.0.0.1.0.111")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want %q", host, "127.0.0.1")
	}
	// port = 0*256 + 111 = 111
	if port != 111 {
		t.Errorf("port = %d, want %d", port, 111)
	}
}

func TestParseUniversalAddr_IPv6(t *testing.T) {
	host, port, err := ParseUniversalAddr("tcp6", "fe80::1.2.15")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "fe80::1" {
		t.Errorf("host = %q, want %q", host, "fe80::1")
	}
	// port = 2*256 + 15 = 527
	if port != 527 {
		t.Errorf("port = %d, want %d", port, 527)
	}
}

func TestParseUniversalAddr_IPv6_Loopback(t *testing.T) {
	host, port, err := ParseUniversalAddr("tcp6", "::1.0.111")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "::1" {
		t.Errorf("host = %q, want %q", host, "::1")
	}
	// port = 0*256 + 111 = 111
	if port != 111 {
		t.Errorf("port = %d, want %d", port, 111)
	}
}

func TestParseUniversalAddr_NoDots(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", "invalid")
	if err == nil {
		t.Fatal("expected error for address with no dots")
	}
}

func TestParseUniversalAddr_OneDot(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", "invalid.1")
	if err == nil {
		t.Fatal("expected error for address with only one dot")
	}
}

func TestParseUniversalAddr_BadPortP1(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", "10.1.3.7.abc.15")
	if err == nil {
		t.Fatal("expected error for non-numeric p1")
	}
}

func TestParseUniversalAddr_BadPortP2(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", "10.1.3.7.2.xyz")
	if err == nil {
		t.Fatal("expected error for non-numeric p2")
	}
}

func TestParseUniversalAddr_PortOutOfRange(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", "10.1.3.7.256.1")
	if err == nil {
		t.Fatal("expected error for p1 > 255")
	}
}

func TestParseUniversalAddr_PortP2OutOfRange(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", "10.1.3.7.1.256")
	if err == nil {
		t.Fatal("expected error for p2 > 255")
	}
}

func TestParseUniversalAddr_EmptyHost(t *testing.T) {
	_, _, err := ParseUniversalAddr("tcp", ".1.2")
	if err == nil {
		t.Fatal("expected error for empty host")
	}
}

// ============================================================================
// CB_COMPOUND Encoding Tests
// ============================================================================

func TestEncodeCBRecallOp(t *testing.T) {
	stateid := &types.Stateid4{
		Seqid: 42,
	}
	// Set a recognizable other field
	for i := 0; i < len(stateid.Other); i++ {
		stateid.Other[i] = byte(i + 1)
	}

	fh := []byte{0xAA, 0xBB, 0xCC}
	truncate := true

	encoded := EncodeCBRecallOp(stateid, truncate, fh)

	reader := bytes.NewReader(encoded)

	// 1. Verify OP_CB_RECALL (4)
	var opNum uint32
	if err := binary.Read(reader, binary.BigEndian, &opNum); err != nil {
		t.Fatalf("read opnum: %v", err)
	}
	if opNum != types.OP_CB_RECALL {
		t.Errorf("opnum = %d, want %d (OP_CB_RECALL)", opNum, types.OP_CB_RECALL)
	}

	// 2. Verify stateid4: seqid + other
	var seqid uint32
	if err := binary.Read(reader, binary.BigEndian, &seqid); err != nil {
		t.Fatalf("read seqid: %v", err)
	}
	if seqid != 42 {
		t.Errorf("seqid = %d, want 42", seqid)
	}

	var other [types.NFS4_OTHER_SIZE]byte
	if _, err := io.ReadFull(reader, other[:]); err != nil {
		t.Fatalf("read other: %v", err)
	}
	for i := 0; i < types.NFS4_OTHER_SIZE; i++ {
		if other[i] != byte(i+1) {
			t.Errorf("other[%d] = %d, want %d", i, other[i], i+1)
		}
	}

	// 3. Verify truncate bool (uint32: 1)
	var truncateVal uint32
	if err := binary.Read(reader, binary.BigEndian, &truncateVal); err != nil {
		t.Fatalf("read truncate: %v", err)
	}
	if truncateVal != 1 {
		t.Errorf("truncate = %d, want 1", truncateVal)
	}

	// 4. Verify file handle as XDR opaque (length + data + padding)
	var fhLen uint32
	if err := binary.Read(reader, binary.BigEndian, &fhLen); err != nil {
		t.Fatalf("read fh len: %v", err)
	}
	if fhLen != 3 {
		t.Errorf("fh len = %d, want 3", fhLen)
	}

	fhData := make([]byte, fhLen)
	if _, err := io.ReadFull(reader, fhData); err != nil {
		t.Fatalf("read fh data: %v", err)
	}
	if !bytes.Equal(fhData, []byte{0xAA, 0xBB, 0xCC}) {
		t.Errorf("fh data = %v, want [AA BB CC]", fhData)
	}

	// Read padding (1 byte for 3 bytes of data -> 4-byte alignment)
	padding := make([]byte, 1)
	if _, err := io.ReadFull(reader, padding); err != nil {
		t.Fatalf("read padding: %v", err)
	}

	// Should have consumed all data
	if reader.Len() != 0 {
		t.Errorf("remaining bytes: %d, want 0", reader.Len())
	}
}

func TestEncodeCBCompound(t *testing.T) {
	// Some fake operation data
	ops := []byte{0x01, 0x02, 0x03, 0x04}
	callbackIdent := uint32(12345)

	encoded := encodeCBCompound(callbackIdent, ops)

	reader := bytes.NewReader(encoded)

	// 1. tag: empty XDR opaque (length=0)
	var tagLen uint32
	if err := binary.Read(reader, binary.BigEndian, &tagLen); err != nil {
		t.Fatalf("read tag len: %v", err)
	}
	if tagLen != 0 {
		t.Errorf("tag len = %d, want 0", tagLen)
	}

	// 2. minorversion: 0
	var minorVersion uint32
	if err := binary.Read(reader, binary.BigEndian, &minorVersion); err != nil {
		t.Fatalf("read minorversion: %v", err)
	}
	if minorVersion != 0 {
		t.Errorf("minorversion = %d, want 0", minorVersion)
	}

	// 3. callback_ident
	var cbIdent uint32
	if err := binary.Read(reader, binary.BigEndian, &cbIdent); err != nil {
		t.Fatalf("read callback_ident: %v", err)
	}
	if cbIdent != 12345 {
		t.Errorf("callback_ident = %d, want 12345", cbIdent)
	}

	// 4. array count: 1
	var arrayCount uint32
	if err := binary.Read(reader, binary.BigEndian, &arrayCount); err != nil {
		t.Fatalf("read array count: %v", err)
	}
	if arrayCount != 1 {
		t.Errorf("array count = %d, want 1", arrayCount)
	}

	// 5. operations data
	remaining := make([]byte, reader.Len())
	if _, err := io.ReadFull(reader, remaining); err != nil {
		t.Fatalf("read remaining: %v", err)
	}
	if !bytes.Equal(remaining, ops) {
		t.Errorf("ops = %v, want %v", remaining, ops)
	}
}

// ============================================================================
// RPC Message Building Tests
// ============================================================================

func TestBuildCBRPCCallMessage(t *testing.T) {
	xid := uint32(0xDEADBEEF)
	prog := uint32(0x40000000)
	vers := uint32(1)
	proc := uint32(1)
	args := []byte{0xCA, 0xFE}

	msg := BuildCBRPCCallMessage(xid, prog, vers, proc, args)

	reader := bytes.NewReader(msg)

	// XID
	var gotXID uint32
	if err := binary.Read(reader, binary.BigEndian, &gotXID); err != nil {
		t.Fatalf("read xid: %v", err)
	}
	if gotXID != xid {
		t.Errorf("xid = 0x%08X, want 0x%08X", gotXID, xid)
	}

	// MsgType = CALL (0)
	var msgType uint32
	if err := binary.Read(reader, binary.BigEndian, &msgType); err != nil {
		t.Fatalf("read msg type: %v", err)
	}
	if msgType != rpc.RPCCall {
		t.Errorf("msg type = %d, want %d (CALL)", msgType, rpc.RPCCall)
	}

	// RPC version = 2
	var rpcVers uint32
	if err := binary.Read(reader, binary.BigEndian, &rpcVers); err != nil {
		t.Fatalf("read rpc version: %v", err)
	}
	if rpcVers != 2 {
		t.Errorf("rpc version = %d, want 2", rpcVers)
	}

	// Program
	var gotProg uint32
	if err := binary.Read(reader, binary.BigEndian, &gotProg); err != nil {
		t.Fatalf("read program: %v", err)
	}
	if gotProg != prog {
		t.Errorf("program = 0x%08X, want 0x%08X", gotProg, prog)
	}

	// Version
	var gotVers uint32
	if err := binary.Read(reader, binary.BigEndian, &gotVers); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if gotVers != vers {
		t.Errorf("version = %d, want %d", gotVers, vers)
	}

	// Procedure
	var gotProc uint32
	if err := binary.Read(reader, binary.BigEndian, &gotProc); err != nil {
		t.Fatalf("read procedure: %v", err)
	}
	if gotProc != proc {
		t.Errorf("procedure = %d, want %d", gotProc, proc)
	}

	// AUTH_NULL credentials: flavor=0, length=0
	var credFlavor, credLen uint32
	_ = binary.Read(reader, binary.BigEndian, &credFlavor)
	_ = binary.Read(reader, binary.BigEndian, &credLen)
	if credFlavor != rpc.AuthNull || credLen != 0 {
		t.Errorf("cred = (flavor=%d, len=%d), want (0, 0)", credFlavor, credLen)
	}

	// AUTH_NULL verifier: flavor=0, length=0
	var verfFlavor, verfLen uint32
	_ = binary.Read(reader, binary.BigEndian, &verfFlavor)
	_ = binary.Read(reader, binary.BigEndian, &verfLen)
	if verfFlavor != rpc.AuthNull || verfLen != 0 {
		t.Errorf("verf = (flavor=%d, len=%d), want (0, 0)", verfFlavor, verfLen)
	}

	// Args
	remaining := make([]byte, reader.Len())
	_, _ = io.ReadFull(reader, remaining)
	if !bytes.Equal(remaining, args) {
		t.Errorf("args = %v, want %v", remaining, args)
	}
}

func TestAddCBRecordMark(t *testing.T) {
	msg := []byte{0x01, 0x02, 0x03, 0x04, 0x05}

	framed := AddCBRecordMark(msg, true)

	if len(framed) != 4+5 {
		t.Fatalf("framed length = %d, want 9", len(framed))
	}

	// Header: last-fragment bit set + length 5
	header := binary.BigEndian.Uint32(framed[0:4])
	expectedHeader := uint32(0x80000000) | uint32(5)
	if header != expectedHeader {
		t.Errorf("header = 0x%08X, want 0x%08X", header, expectedHeader)
	}

	// Body matches
	if !bytes.Equal(framed[4:], msg) {
		t.Errorf("body = %v, want %v", framed[4:], msg)
	}
}

func TestAddCBRecordMark_NotLastFragment(t *testing.T) {
	msg := []byte{0xAA, 0xBB}

	framed := AddCBRecordMark(msg, false)

	header := binary.BigEndian.Uint32(framed[0:4])
	// No last fragment bit, just length 2
	if header != 2 {
		t.Errorf("header = 0x%08X, want 0x00000002", header)
	}
}

// ============================================================================
// Integration-style tests with mock TCP servers
// ============================================================================

// buildMockCBCompoundReply builds a valid RPC reply with CB_COMPOUND4res containing NFS4_OK.
func buildMockCBCompoundReply(xid uint32) []byte {
	var reply bytes.Buffer

	// XID (echo back)
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
	// resarray count = 0 (we don't need per-op results for this test)
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))

	body := reply.Bytes()

	// Add record mark
	framed := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(framed[0:4], uint32(len(body))|0x80000000)
	copy(framed[4:], body)

	return framed
}

// buildMockNullReply builds a minimal valid RPC reply for CB_NULL.
func buildMockNullReply(xid uint32) []byte {
	var reply bytes.Buffer

	// XID
	_ = binary.Write(&reply, binary.BigEndian, xid)
	// MsgType = REPLY
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
	// reply_stat = MSG_ACCEPTED
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCMsgAccepted))
	// Verifier: AUTH_NULL
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.AuthNull))
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))
	// accept_stat = SUCCESS
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCSuccess))

	body := reply.Bytes()

	// Add record mark
	framed := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(framed[0:4], uint32(len(body))|0x80000000)
	copy(framed[4:], body)

	return framed
}

// extractXIDFromRequest reads an RPC request and extracts the XID.
func extractXIDFromRequest(conn net.Conn) (uint32, error) {
	// Read fragment header
	var headerBuf [4]byte
	if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
		return 0, err
	}
	header := binary.BigEndian.Uint32(headerBuf[:])
	fragLen := header & 0x7FFFFFFF

	// Read body
	body := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, err
	}

	// Extract XID (first 4 bytes)
	xid := binary.BigEndian.Uint32(body[0:4])
	return xid, nil
}

func makeCallbackInfoFromListener(l net.Listener) CallbackInfo {
	addr := l.Addr().(*net.TCPAddr)
	ip := addr.IP.To4()
	if ip == nil {
		ip = net.IPv4(127, 0, 0, 1)
	}
	p1 := addr.Port / 256
	p2 := addr.Port % 256

	uaddr := fmt.Sprintf("%d.%d.%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3], p1, p2)

	return CallbackInfo{
		Program: 0x40000000, // arbitrary callback program number
		NetID:   "tcp",
		Addr:    uaddr,
	}
}

func TestSendCBRecall_Success(t *testing.T) {
	// Start a mock TCP server
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	callback := makeCallbackInfoFromListener(l)

	// Accept and respond in a goroutine
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()

		// Read the RPC request and extract XID
		xid, err := extractXIDFromRequest(conn)
		if err != nil {
			done <- err
			return
		}

		// Send valid CB_COMPOUND reply with NFS4_OK
		reply := buildMockCBCompoundReply(xid)
		_, err = conn.Write(reply)
		done <- err
	}()

	// Send CB_RECALL
	stateid := &types.Stateid4{Seqid: 1}
	fh := []byte{0x01, 0x02, 0x03, 0x04}

	err = SendCBRecall(context.Background(), callback, stateid, false, fh)
	if err != nil {
		t.Fatalf("SendCBRecall error: %v", err)
	}

	// Wait for server goroutine
	if serverErr := <-done; serverErr != nil {
		t.Fatalf("server error: %v", serverErr)
	}
}

func TestSendCBRecall_ConnectionRefused(t *testing.T) {
	// Use a callback address with a port that's not listening
	callback := CallbackInfo{
		Program: 0x40000000,
		NetID:   "tcp",
		Addr:    "127.0.0.1.0.1", // port 1 -- very unlikely to be listening
	}

	stateid := &types.Stateid4{Seqid: 1}
	fh := []byte{0x01}

	err := SendCBRecall(context.Background(), callback, stateid, false, fh)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestSendCBRecall_Timeout(t *testing.T) {
	// Start a listener that accepts but never responds
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	callback := makeCallbackInfoFromListener(l)

	// Accept but don't respond
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		// Hold connection open without responding
		// Read the request to consume it (prevent RST)
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		// Sleep longer than timeout
		time.Sleep(10 * time.Second)
		_ = conn.Close()
	}()

	// Use a very short timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	stateid := &types.Stateid4{Seqid: 1}
	fh := []byte{0x01}

	err = SendCBRecall(ctx, callback, stateid, false, fh)
	if err == nil {
		t.Fatal("expected error for timeout")
	}
}

func TestSendCBNull_Success(t *testing.T) {
	// Start a mock TCP server
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	callback := makeCallbackInfoFromListener(l)

	// Accept and respond in a goroutine
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()

		// Read the RPC request and extract XID
		xid, err := extractXIDFromRequest(conn)
		if err != nil {
			done <- err
			return
		}

		// Send valid NULL reply
		reply := buildMockNullReply(xid)
		_, err = conn.Write(reply)
		done <- err
	}()

	// Send CB_NULL
	err = SendCBNull(context.Background(), callback)
	if err != nil {
		t.Fatalf("SendCBNull error: %v", err)
	}

	// Wait for server goroutine
	if serverErr := <-done; serverErr != nil {
		t.Fatalf("server error: %v", serverErr)
	}
}

func TestSendCBNull_ConnectionRefused(t *testing.T) {
	callback := CallbackInfo{
		Program: 0x40000000,
		NetID:   "tcp",
		Addr:    "127.0.0.1.0.1", // port 1
	}

	err := SendCBNull(context.Background(), callback)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestSendCBNull_Timeout(t *testing.T) {
	// Start a listener that accepts but never responds
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	callback := makeCallbackInfoFromListener(l)

	// Accept but don't respond
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		time.Sleep(10 * time.Second)
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = SendCBNull(ctx, callback)
	if err == nil {
		t.Fatal("expected error for timeout")
	}
}

// ============================================================================
// ReadAndValidateCBReply Tests
// ============================================================================

func TestReadAndValidateCBReply_ValidCompound(t *testing.T) {
	// Build a valid CB_COMPOUND reply
	var reply bytes.Buffer
	xid := uint32(1234)

	_ = binary.Write(&reply, binary.BigEndian, xid)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCMsgAccepted))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.AuthNull)) // verf flavor
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))            // verf body len
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCSuccess))
	_ = binary.Write(&reply, binary.BigEndian, uint32(types.NFS4_OK))

	body := reply.Bytes()
	framed := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(framed[0:4], uint32(len(body))|0x80000000)
	copy(framed[4:], body)

	// Create a pipe to simulate a connection
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = server.Write(framed)
	}()

	err := readAndValidateCBReply(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadAndValidateCBReply_DeniedReply(t *testing.T) {
	var reply bytes.Buffer
	xid := uint32(1234)

	_ = binary.Write(&reply, binary.BigEndian, xid)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCMsgDenied)) // DENIED

	body := reply.Bytes()
	framed := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(framed[0:4], uint32(len(body))|0x80000000)
	copy(framed[4:], body)

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = server.Write(framed)
	}()

	err := readAndValidateCBReply(client)
	if err == nil {
		t.Fatal("expected error for denied reply")
	}
}

func TestReadAndValidateCBReply_NFS4Error(t *testing.T) {
	var reply bytes.Buffer
	xid := uint32(1234)

	_ = binary.Write(&reply, binary.BigEndian, xid)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCMsgAccepted))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.AuthNull))
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCSuccess))
	_ = binary.Write(&reply, binary.BigEndian, uint32(types.NFS4ERR_BADHANDLE)) // NFS error

	body := reply.Bytes()
	framed := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(framed[0:4], uint32(len(body))|0x80000000)
	copy(framed[4:], body)

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = server.Write(framed)
	}()

	err := readAndValidateCBReply(client)
	if err == nil {
		t.Fatal("expected error for NFS4ERR_BADHANDLE")
	}
}

// ============================================================================
// SendCBRecall with verification of wire format
// ============================================================================

func TestSendCBRecall_VerifyWireFormat(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	callback := makeCallbackInfoFromListener(l)

	done := make(chan error, 1)
	var receivedProc uint32
	var receivedProg uint32

	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()

		// Read fragment header
		var headerBuf [4]byte
		if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
			done <- err
			return
		}
		header := binary.BigEndian.Uint32(headerBuf[:])
		fragLen := header & 0x7FFFFFFF

		// Read body
		body := make([]byte, fragLen)
		if _, err := io.ReadFull(conn, body); err != nil {
			done <- err
			return
		}

		reader := bytes.NewReader(body)

		// Parse: XID, MsgType, RPCVersion, Program, Version, Procedure
		var xid, msgType, rpcVer uint32
		_ = binary.Read(reader, binary.BigEndian, &xid)
		_ = binary.Read(reader, binary.BigEndian, &msgType)
		_ = binary.Read(reader, binary.BigEndian, &rpcVer)
		_ = binary.Read(reader, binary.BigEndian, &receivedProg)
		var vers uint32
		_ = binary.Read(reader, binary.BigEndian, &vers)
		_ = binary.Read(reader, binary.BigEndian, &receivedProc)

		// Send reply
		reply := buildMockCBCompoundReply(xid)
		_, err = conn.Write(reply)
		done <- err
	}()

	stateid := &types.Stateid4{Seqid: 5}
	fh := []byte{0xDE, 0xAD}

	err = SendCBRecall(context.Background(), callback, stateid, true, fh)
	if err != nil {
		t.Fatalf("SendCBRecall error: %v", err)
	}

	if serverErr := <-done; serverErr != nil {
		t.Fatalf("server error: %v", serverErr)
	}

	// Verify the procedure was CB_PROC_COMPOUND
	if receivedProc != types.CB_PROC_COMPOUND {
		t.Errorf("procedure = %d, want %d (CB_PROC_COMPOUND)", receivedProc, types.CB_PROC_COMPOUND)
	}

	// Verify the program was the callback program
	if receivedProg != callback.Program {
		t.Errorf("program = 0x%08X, want 0x%08X", receivedProg, callback.Program)
	}
}

func TestSendCBNull_VerifyProcedure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	callback := makeCallbackInfoFromListener(l)

	done := make(chan error, 1)
	var receivedProc uint32

	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()

		// Read fragment header
		var headerBuf [4]byte
		if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
			done <- err
			return
		}
		header := binary.BigEndian.Uint32(headerBuf[:])
		fragLen := header & 0x7FFFFFFF

		body := make([]byte, fragLen)
		if _, err := io.ReadFull(conn, body); err != nil {
			done <- err
			return
		}

		reader := bytes.NewReader(body)

		// Parse header fields
		var xid, msgType, rpcVer, prog, vers uint32
		_ = binary.Read(reader, binary.BigEndian, &xid)
		_ = binary.Read(reader, binary.BigEndian, &msgType)
		_ = binary.Read(reader, binary.BigEndian, &rpcVer)
		_ = binary.Read(reader, binary.BigEndian, &prog)
		_ = binary.Read(reader, binary.BigEndian, &vers)
		_ = binary.Read(reader, binary.BigEndian, &receivedProc)

		// Send reply
		reply := buildMockNullReply(xid)
		_, err = conn.Write(reply)
		done <- err
	}()

	err = SendCBNull(context.Background(), callback)
	if err != nil {
		t.Fatalf("SendCBNull error: %v", err)
	}

	if serverErr := <-done; serverErr != nil {
		t.Fatalf("server error: %v", serverErr)
	}

	// Verify the procedure was CB_PROC_NULL
	if receivedProc != types.CB_PROC_NULL {
		t.Errorf("procedure = %d, want %d (CB_PROC_NULL)", receivedProc, types.CB_PROC_NULL)
	}
}
