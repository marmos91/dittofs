package nfs

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"

	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// compoundPrefix encodes the leading fields of COMPOUND4args: the echoed tag
// and the minorversion.
func compoundPrefix(tag string, minorVersion uint32) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, tag)
	_ = xdr.WriteUint32(&buf, minorVersion)
	_ = xdr.WriteUint32(&buf, 0) // numops
	return buf.Bytes()
}

// TestIsV40Compound covers the gate that keeps the duplicate request cache off
// the v4.1 and v4.2 paths, which get exactly-once semantics from the session
// slot table and must not have a retransmission answered from anywhere else.
//
// The tag is a variable-length opaque, so the minorversion does not sit at a
// fixed offset: a non-empty tag has to be stepped over, padding included.
func TestIsV40Compound(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"empty tag, v4.0", compoundPrefix("", 0), true},
		{"tag, v4.0", compoundPrefix("dittofs", 0), true},
		{"unpadded-length tag, v4.0", compoundPrefix("ab", 0), true},
		{"empty tag, v4.1", compoundPrefix("", 1), false},
		{"tag, v4.1", compoundPrefix("dittofs", 1), false},
		{"tag, v4.2", compoundPrefix("dittofs", 2), false},
		{"empty body", nil, false},
		{"truncated before minorversion", compoundPrefix("", 0)[:4], false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isV40Compound(tc.body); got != tc.want {
				t.Fatalf("isV40Compound(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestV40Compound_ReleasesDRCSlotOnPanic checks that the in-progress slot a
// v4.0 COMPOUND reserves is released even when the handler panics.
//
// This matters more than the usual panic-safety argument. lookup matches an
// in-progress entry before it considers the entry's age, and the TTL sweep only
// runs when a shard is at capacity, so a slot left behind is not reclaimed by
// time: every later retransmission of that exact request is answered with a
// silent drop. Request panics are recovered per request and leave the
// connection open, so the client keeps its source port, keeps producing the
// same cache key, and the request stays wedged for the life of the connection.
//
// The adapter here has no runtime, so v4Handler is nil and ProcessCompound
// panics -- which is exactly the situation being pinned.
func TestV40Compound_ReleasesDRCSlotOnPanic(t *testing.T) {
	const (
		xid        = uint32(0xBEEF)
		clientAddr = "10.1.2.3:4321"
	)

	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := NewNFSConnection(adapter, server, 1)

	body := compoundPrefix("", 0)
	call := &rpc.RPCCallMessage{
		XID:       xid,
		Program:   rpc.ProgramNFS,
		Version:   rpc.NFSVersion4,
		Procedure: v4types.NFSPROC4_COMPOUND,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}

	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = conn.handleNFSv4Procedure(context.Background(), call, body, clientAddr)
	}()
	if !panicked {
		t.Fatal("handler did not panic; this test no longer exercises the release path")
	}

	// A slot left in-progress answers the retransmission with a drop.
	if res, _ := adapter.drc.lookup(clientAddr, xid, body); res != drcMiss {
		t.Fatalf("after a panicking COMPOUND, lookup = %v, want drcMiss (the reserved slot leaked)", res)
	}
}
