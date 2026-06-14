package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// TestHandleRPCCall_UnknownProgram_ReturnsProgUnavail verifies that an RPC
// call for an unrecognised program number receives accept_stat=1 (PROG_UNAVAIL,
// RFC 5531 §9) and NOT accept_stat=3 (PROC_UNAVAIL).
//
// Fails before the fix because RPCProcUnavail (3) is returned instead.
// Passes after the fix because RPCProgUnavail (1) is returned.
func TestHandleRPCCall_UnknownProgram_ReturnsProgUnavail(t *testing.T) {
	const testXID = uint32(0xDEADBEEF)
	const unknownProg = uint32(999999)

	// Build a minimal NFSAdapter (no runtime wired — the default branch fires
	// before any handler is reached).
	adapter := New(NFSConfig{
		Enabled: true,
		Port:    12049,
	})

	// net.Pipe gives a synchronous, in-process full-duplex connection.
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	conn := NewNFSConnection(adapter, server, 1)

	// Build an RPC CALL for an unknown program with AUTH_NULL credentials.
	call := &rpc.RPCCallMessage{
		XID:       testXID,
		Program:   unknownProg,
		Version:   1,
		Procedure: 0,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}

	// Drive handleRPCCall in a goroutine; it writes to server end of the pipe.
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.handleRPCCall(context.Background(), call, nil, nil)
	}()

	// Read the reply from the client end.
	// RPC-over-TCP: 4-byte fragment header then the XDR body.
	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("reading fragment header: %v", err)
	}
	// Top bit marks last fragment; lower 31 bits = body length.
	bodyLen := binary.BigEndian.Uint32(header) &^ 0x80000000
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(client, body); err != nil {
		t.Fatalf("reading reply body: %v", err)
	}

	// Verify the goroutine completed without error.
	if err := <-errCh; err != nil {
		t.Fatalf("handleRPCCall returned error: %v", err)
	}

	// Parse the XDR reply:
	//   body[0:4]   = XID
	//   body[4:8]   = MsgType (1 = REPLY)
	//   body[8:12]  = ReplyState (0 = MSG_ACCEPTED)
	//   body[12:16] = Verf Flavor
	//   body[16:20] = Verf Body Length
	//   body[20:24] = AcceptStat  <-- what we check
	const minLen = 24
	if len(body) < minLen {
		t.Fatalf("reply body too short: got %d bytes, want >= %d", len(body), minLen)
	}

	xid := binary.BigEndian.Uint32(body[0:4])
	if xid != testXID {
		t.Errorf("XID echo: got 0x%x, want 0x%x", xid, testXID)
	}

	acceptStat := binary.BigEndian.Uint32(body[20:24])
	if acceptStat != rpc.RPCProgUnavail {
		t.Errorf("accept_stat: got %d, want %d (PROG_UNAVAIL); got PROC_UNAVAIL(3)=%v",
			acceptStat, rpc.RPCProgUnavail, acceptStat == rpc.RPCProcUnavail)
	}
}
