package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	mount_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// authSysBody encodes an AUTH_SYS credential body (RFC 5531 Appendix A).
func authSysBody(machine string, uid, gid uint32, gids []uint32) []byte {
	pad := (4 - len(machine)%4) % 4
	b := make([]byte, 0, 20+len(machine)+pad+4*len(gids))
	b = binary.BigEndian.AppendUint32(b, 0) // stamp
	b = binary.BigEndian.AppendUint32(b, uint32(len(machine)))
	b = append(b, machine...)
	b = append(b, make([]byte, pad)...)
	b = binary.BigEndian.AppendUint32(b, uid)
	b = binary.BigEndian.AppendUint32(b, gid)
	b = binary.BigEndian.AppendUint32(b, uint32(len(gids)))
	for _, g := range gids {
		b = binary.BigEndian.AppendUint32(b, g)
	}
	return b
}

// badCredentials are credentials RFC 5531 gives the server no identity for: an
// AUTH_SYS body that does not decode to exactly one authsys_parms, and flavors
// this server does not implement.
func badCredentials() map[string]rpc.OpaqueAuth {
	valid := authSysBody("client", 1000, 1000, nil)
	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}
	return map[string]rpc.OpaqueAuth{
		"auth_sys empty body":     {Flavor: rpc.AuthUnix, Body: []byte{}},
		"auth_sys truncated":      {Flavor: rpc.AuthUnix, Body: valid[:len(valid)-2]},
		"auth_sys trailing bytes": {Flavor: rpc.AuthUnix, Body: append(append([]byte{}, valid...), 0, 0, 0, 0)},
		"auth_sys 17 gids":        {Flavor: rpc.AuthUnix, Body: authSysBody("client", 1000, 1000, make([]uint32, 17))},
		"auth_sys 256-byte name":  {Flavor: rpc.AuthUnix, Body: authSysBody(string(longName), 1000, 1000, nil)},
		"auth_sys name past body": {Flavor: rpc.AuthUnix, Body: []byte{0, 0, 0, 0, 0, 0, 0, 200, 'x'}},
		"auth_short":              {Flavor: rpc.AuthShort, Body: []byte{0, 0, 0, 1}},
		"auth_dh":                 {Flavor: rpc.AuthDES, Body: []byte{0, 0, 0, 1}},
		"unknown flavor 9":        {Flavor: 9, Body: []byte{}},
		"auth_tls on a data call": {Flavor: rpc.AuthTLS, Body: []byte{}},
	}
}

// readRPCReply reads one record-marked reply off the pipe.
func readRPCReply(t *testing.T, client net.Conn) []byte {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("reading fragment header: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(header)&^0x80000000)
	if _, err := io.ReadFull(client, body); err != nil {
		t.Fatalf("reading reply body: %v", err)
	}
	return body
}

// assertAuthError checks a reply is MSG_DENIED / AUTH_ERROR with want.
func assertAuthError(t *testing.T, reply []byte, want uint32) {
	t.Helper()
	if len(reply) < 12 {
		t.Fatalf("reply too short: %d bytes", len(reply))
	}
	if state := binary.BigEndian.Uint32(reply[8:12]); state != rpc.RPCMsgDenied {
		t.Fatalf("reply_state = %d, want MSG_DENIED: the call was dispatched instead of refused", state)
	}
	if len(reply) < 20 {
		t.Fatalf("MSG_DENIED reply too short: %d bytes", len(reply))
	}
	if rs := binary.BigEndian.Uint32(reply[12:16]); rs != 1 {
		t.Fatalf("reject_stat = %d, want 1 (AUTH_ERROR)", rs)
	}
	if as := binary.BigEndian.Uint32(reply[16:20]); as != want {
		t.Fatalf("auth_stat = %d, want %d", as, want)
	}
}

// TestHandleRPCCall_BadCredentialIsRefused drives a GETATTR on a real share's
// root with each bad credential. The share's root is world-readable, so a
// credential that degrades to anonymous instead of being refused gets an
// NFS3_OK — the share's anonymous access handed to a credential that decoded
// to nothing.
func TestHandleRPCCall_BadCredentialIsRefused(t *testing.T) {
	fixture, _, rootHandle := newV3PolicyFixture(t)

	for name, cred := range badCredentials() {
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			conn := NewNFSConnection(fixture.server, server, 2)

			call, data := getattrCall(rootHandle)
			call.Cred = cred

			errCh := make(chan error, 1)
			go func() { errCh <- conn.handleRPCCall(context.Background(), call, nil, data) }()
			reply := readRPCReply(t, client)
			<-errCh
			assertAuthError(t, reply, rpc.AuthBadCred)
		})
	}

	// Control: the same GETATTR with a well-formed AUTH_SYS credential is served.
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := NewNFSConnection(fixture.server, server, 3)
	call, data := getattrCall(rootHandle)
	go func() { _ = conn.handleRPCCall(context.Background(), call, nil, data) }()
	reply := readRPCReply(t, client)
	if state := binary.BigEndian.Uint32(reply[8:12]); state != rpc.RPCMsgAccepted {
		t.Fatalf("well-formed AUTH_SYS: reply_state = %d, want MSG_ACCEPTED", state)
	}
}

// TestHandleUDPDatagram_BadCredentialIsRefused covers the UDP listener, which
// routes MOUNT, NLM and NSM without passing through handleRPCCall; MNT stands
// for all three, since the refusal runs before the program switch. An
// RPCSEC_GSS credential is refused there too: UDP has no GSS processing, so a
// flavor-6 call would reach a handler unverified.
func TestHandleUDPDatagram_BadCredentialIsRefused(t *testing.T) {
	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	conn := NewNFSConnection(adapter, nil, 1)

	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	cli, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	creds := badCredentials()
	creds["rpcsec_gss over udp"] = rpc.OpaqueAuth{Flavor: rpc.AuthRPCSECGSS, Body: []byte{0, 0, 0, 1}}
	for name, cred := range creds {
		t.Run(name, func(t *testing.T) {
			want := rpc.AuthBadCred
			if cred.Flavor == rpc.AuthRPCSECGSS {
				want = rpc.AuthTooWeak
			}
			msg := encodeCredCall(&rpc.RPCCallMessage{
				XID: 0xBAD, Program: rpc.ProgramMount, Version: rpc.MountVersion3,
				Procedure: mount_handlers.MountProcMnt,
				Cred:      cred, Verf: rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
			})
			conn.handleUDPDatagram(context.Background(), srv, cli.LocalAddr().(*net.UDPAddr), msg)

			buf := make([]byte, 512)
			_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := cli.Read(buf)
			if err != nil {
				t.Fatalf("no reply: %v", err)
			}
			assertAuthError(t, buf[:n], want)
		})
	}
}

// encodeCredCall marshals an RPC call header with no procedure arguments.
func encodeCredCall(call *rpc.RPCCallMessage) []byte {
	b := binary.BigEndian.AppendUint32(nil, call.XID)
	b = binary.BigEndian.AppendUint32(b, 0) // CALL
	b = binary.BigEndian.AppendUint32(b, 2) // RPC version
	b = binary.BigEndian.AppendUint32(b, call.Program)
	b = binary.BigEndian.AppendUint32(b, call.Version)
	b = binary.BigEndian.AppendUint32(b, call.Procedure)
	for _, a := range []rpc.OpaqueAuth{call.Cred, call.Verf} {
		b = binary.BigEndian.AppendUint32(b, a.Flavor)
		b = binary.BigEndian.AppendUint32(b, uint32(len(a.Body)))
		b = append(b, a.Body...)
		b = append(b, make([]byte, (4-len(a.Body)%4)%4)...)
	}
	// A few zero words so procedures that decode arguments do not short-read.
	return append(b, make([]byte, 64)...)
}
