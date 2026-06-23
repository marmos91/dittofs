package nfs

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// TestUDPReplyFraming verifies the UDP transport's core framing decision: the
// shared reply builders prepend a 4-byte RFC 5531 record-marking header for the
// stream transport, and the UDP path strips exactly that prefix so the datagram
// carries a bare RPC reply beginning with the XID.
func TestUDPReplyFraming(t *testing.T) {
	const xid = uint32(0xDEADBEEF)
	body := []byte{0x00, 0x00, 0x00, 0x00} // minimal NLM-style result body

	framed, err := rpc.MakeSuccessReply(xid, body)
	if err != nil {
		t.Fatalf("MakeSuccessReply: %v", err)
	}
	if len(framed) < rpcRecordMarkLen+4 {
		t.Fatalf("framed reply too short: %d bytes", len(framed))
	}

	// The record mark is (0x80000000 | payload length) over the first 4 bytes.
	mark := binary.BigEndian.Uint32(framed[:rpcRecordMarkLen])
	wantMark := uint32(0x80000000) | uint32(len(framed)-rpcRecordMarkLen)
	if mark != wantMark {
		t.Fatalf("record mark = 0x%08x, want 0x%08x", mark, wantMark)
	}

	// After stripping the record mark the datagram payload must start with the
	// XID (first field of the RPC reply message).
	payload := framed[rpcRecordMarkLen:]
	if got := binary.BigEndian.Uint32(payload[:4]); got != xid {
		t.Fatalf("stripped payload XID = 0x%08x, want 0x%08x", got, xid)
	}
}

// TestIsUDPEnabled covers the *bool tri-state: unset defaults to disabled.
func TestIsUDPEnabled(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		name string
		val  *bool
		want bool
	}{
		{"unset defaults off", nil, false},
		{"explicit true", &tr, true},
		{"explicit false", &fa, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &NFSAdapter{config: NFSConfig{UDP: NFSUDPConfig{Enabled: tc.val}}}
			if got := s.isUDPEnabled(); got != tc.want {
				t.Fatalf("isUDPEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
