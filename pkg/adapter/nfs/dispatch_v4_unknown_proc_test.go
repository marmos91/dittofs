package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// TestHandleRPCCall_NFSv4UnknownProcedure_SingleReply verifies that an NFSv4
// RPC call for an unknown procedure number (not NULL=0 or COMPOUND=1) produces
// EXACTLY ONE reply (PROC_UNAVAIL) on the connection.
//
// Before the fix the v4 default branch returned (nil, nil) after writing its
// PROC_UNAVAIL reply, so handleRPCCall fell through to sendReply and wrote a
// SECOND (empty MSG_ACCEPTED) reply on the same XID — corrupting the TCP stream
// for every subsequent request on the connection.
//
// net.Pipe is synchronous and unbuffered: a second write would block until a
// second read consumed it. The test reads exactly one reply, then asserts the
// handler goroutine has already returned (it would still be blocked on the
// second write if the bug were present) and returned errDropReply.
func TestHandleRPCCall_NFSv4UnknownProcedure_SingleReply(t *testing.T) {
	const testXID = uint32(0xCAFEF00D)
	const unknownProc = uint32(7) // NFSv4 only defines NULL(0) and COMPOUND(1)

	adapter := New(NFSConfig{Enabled: true, Port: 12049})

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	conn := NewNFSConnection(adapter, server, 1)

	call := &rpc.RPCCallMessage{
		XID:       testXID,
		Program:   rpc.ProgramNFS,
		Version:   rpc.NFSVersion4,
		Procedure: unknownProc,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.handleRPCCall(context.Background(), call, nil, nil)
	}()

	// Read exactly one RPC-over-TCP reply (4-byte fragment header + body).
	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("reading fragment header: %v", err)
	}
	bodyLen := binary.BigEndian.Uint32(header) &^ 0x80000000
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(client, body); err != nil {
		t.Fatalf("reading reply body: %v", err)
	}

	// First reply must be PROC_UNAVAIL on the request XID.
	const minLen = 24
	if len(body) < minLen {
		t.Fatalf("reply body too short: got %d bytes, want >= %d", len(body), minLen)
	}
	if xid := binary.BigEndian.Uint32(body[0:4]); xid != testXID {
		t.Errorf("XID echo: got 0x%x, want 0x%x", xid, testXID)
	}
	if acceptStat := binary.BigEndian.Uint32(body[20:24]); acceptStat != rpc.RPCProcUnavail {
		t.Errorf("accept_stat: got %d, want %d (PROC_UNAVAIL)", acceptStat, rpc.RPCProcUnavail)
	}

	// The handler must have returned already. With the bug it would still be
	// blocked writing a SECOND reply to the synchronous pipe (no second reader),
	// so this select would time out. handleRPCCall recognises the internal
	// errDropReply signal and returns nil (no further reply, no error).
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("handleRPCCall returned %v, want nil (single reply already sent)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleRPCCall did not return: it is blocked writing a SECOND reply (double-reply bug)")
	}

	// Belt-and-braces: confirm no second frame is queued on the pipe.
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	extra := make([]byte, 4)
	if n, err := io.ReadFull(client, extra); err == nil {
		t.Fatalf("unexpected second reply frame on the connection: read %d bytes", n)
	}
}
