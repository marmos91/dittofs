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
// call, and replies with a successful RPC reply carrying replyPort.
func fakePortmapper(t *testing.T, ln net.Listener, replyPort uint32) {
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

	// Extract the XID from the call we just read is not needed; a real client
	// validates only the port here, so we reply with a fixed XID of 0.
	var body bytes.Buffer
	write := func(v uint32) { _ = binary.Write(&body, binary.BigEndian, v) }
	write(0)         // xid
	write(1)         // msg_type = REPLY
	write(0)         // reply_stat = MSG_ACCEPTED
	write(0)         // verifier flavor = AUTH_NULL
	write(0)         // verifier length = 0
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
	go fakePortmapper(t, ln, advertised)

	// Point the resolver at our fake portmapper's actual address by overriding
	// the host:port through a direct dial: ResolveNLMCallbackPort always dials
	// host:111, so we run the fake listener on an ephemeral port and verify the
	// wire logic via a thin wrapper that targets the listener address.
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	got, err := resolveViaPortmapAddr(context.Background(), host, port,
		types.ProgramNLM, types.NLMVersion4)
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
