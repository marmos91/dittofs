package callback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
)

// fakePortmapper accepts one TCP connection, reads the record-marked GETPORT
// call, and replies with a successful RPC reply carrying replyPort. verfLen is
// the verifier opaque length to advertise: the verifier body is XDR-padded to a
// 4-byte boundary, exercising the reader's padding handling.
func fakePortmapper(t *testing.T, ln net.Listener, replyPort uint32, verfLen int) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return // listener closed
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Read and discard the call (record mark + body).
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return
	}
	fragLen := binary.BigEndian.Uint32(hdr[:]) & 0x7FFFFFFF
	if _, err := io.CopyN(io.Discard, conn, int64(fragLen)); err != nil {
		return
	}

	var body bytes.Buffer
	write := func(v uint32) { _ = binary.Write(&body, binary.BigEndian, v) }
	write(0)               // xid
	write(1)               // msg_type = REPLY
	write(0)               // reply_stat = MSG_ACCEPTED
	write(0)               // verifier flavor = AUTH_NULL
	write(uint32(verfLen)) // verifier length
	if padded := (verfLen + 3) &^ 3; padded > 0 {
		body.Write(make([]byte, padded)) // verifier body + XDR padding
	}
	write(0)         // accept_stat = SUCCESS
	write(replyPort) // port

	out := make([]byte, 4+body.Len())
	binary.BigEndian.PutUint32(out[:4], uint32(body.Len())|0x80000000)
	copy(out[4:], body.Bytes())
	_, _ = conn.Write(out)
}

// TestResolveNLMCallbackPort_UsesPortmapNotHardcoded is the negative control for
// the hardcoded-12049 finding. The resolver must return the port the client's
// portmapper advertises (here 54045), proving it no longer dials a fixed port.
func TestResolveNLMCallbackPort_UsesPortmapNotHardcoded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	const advertised = 54045
	go fakePortmapper(t, ln, advertised, 0)

	got, err := resolveListener(t, ln)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != advertised {
		t.Fatalf("resolved port = %d, want advertised %d (must not be hardcoded)", got, advertised)
	}
	if got == 12049 {
		t.Fatalf("resolved port is the old hardcoded 12049; portmap lookup not used")
	}
}

// TestResolveNLMCallbackPort_NonAlignedVerifier exercises the XDR padding of the
// reply verifier: a verifier length that is not a multiple of 4 must still leave
// the reader aligned so accept_stat/port are read from the right offset. Without
// rounding the skip up to a 4-byte boundary the port resolves incorrectly.
func TestResolveNLMCallbackPort_NonAlignedVerifier(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	const advertised = 4045
	go fakePortmapper(t, ln, advertised, 5) // 5-byte verifier -> 3 bytes padding

	got, err := resolveListener(t, ln)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != advertised {
		t.Fatalf("resolved port = %d, want %d (verifier padding mishandled)", got, advertised)
	}
}

// resolveListener runs the GETPORT query against the fake portmapper bound to ln.
func resolveListener(t *testing.T, ln net.Listener) (uint16, error) {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	return resolveViaPortmapAddr(context.Background(), host, port,
		types.ProgramNLM, types.NLMVersion4)
}
