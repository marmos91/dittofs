package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// TestHandleRPCCall_GSSWithoutProcessor_RejectsAuthTooWeak covers a call that
// claims RPCSEC_GSS on a server with no GSS processor configured.
//
// The GSS interception below only runs when a processor exists, so such a call
// used to fall through to program dispatch carrying a credential nothing had
// verified. At a handler it then passed for Kerberos: a share's RequireKerberos
// is satisfied by the flavor alone, and AllowAuthSys does not apply because the
// flavor is not AUTH_UNIX. Refusing here covers every program at once — MOUNT,
// NFS v3 and v4, NLM and NSM — rather than each handler guarding itself.
func TestHandleRPCCall_GSSWithoutProcessor_RejectsAuthTooWeak(t *testing.T) {
	const testXID = uint32(0xFEEDFACE)

	// No Kerberos configured, so the adapter has no GSS processor.
	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	if adapter.gssProcessor != nil {
		t.Fatal("fixture has a GSS processor; this test must run without one")
	}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := NewNFSConnection(adapter, server, 1)

	// An NFS GETATTR-shaped call whose credential merely claims flavor 6. The
	// body is arbitrary: nothing is in a position to verify it.
	call := &rpc.RPCCallMessage{
		XID:       testXID,
		Program:   rpc.ProgramNFS,
		Version:   rpc.NFSVersion3,
		Procedure: 1,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthRPCSECGSS, Body: []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- conn.handleRPCCall(context.Background(), call, nil, nil) }()

	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("reading fragment header: %v", err)
	}
	bodyLen := binary.BigEndian.Uint32(header) &^ 0x80000000
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(client, body); err != nil {
		t.Fatalf("reading reply body: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("handleRPCCall returned error: %v", err)
	}

	// MSG_DENIED reply (RFC 5531):
	//   [0:4]   XID
	//   [4:8]   MsgType    (1 = REPLY)
	//   [8:12]  ReplyState (1 = MSG_DENIED)
	//   [12:16] reject_stat (1 = AUTH_ERROR)
	//   [16:20] auth_stat
	if len(body) < 20 {
		t.Fatalf("reply body too short: got %d bytes, want >= 20", len(body))
	}
	if xid := binary.BigEndian.Uint32(body[0:4]); xid != testXID {
		t.Errorf("XID echo: got 0x%x, want 0x%x", xid, testXID)
	}
	if state := binary.BigEndian.Uint32(body[8:12]); state != uint32(rpc.RPCMsgDenied) {
		t.Fatalf("reply_state: got %d, want %d (MSG_DENIED) — the call was dispatched instead of refused",
			state, rpc.RPCMsgDenied)
	}
	if rejectStat := binary.BigEndian.Uint32(body[12:16]); rejectStat != 1 {
		t.Errorf("reject_stat: got %d, want 1 (AUTH_ERROR)", rejectStat)
	}
	if authStat := binary.BigEndian.Uint32(body[16:20]); authStat != rpc.AuthTooWeak {
		t.Errorf("auth_stat: got %d, want %d (AUTH_TOOWEAK)", authStat, rpc.AuthTooWeak)
	}
}
