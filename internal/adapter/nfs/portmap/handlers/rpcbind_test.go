package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
)

// fakeRegistry is a minimal PortmapRegistry for handler tests. It avoids
// importing the parent portmap package (which imports this one).
type fakeRegistry struct {
	mappings []*xdr.Mapping
}

func (f *fakeRegistry) Set(m *xdr.Mapping) bool {
	f.mappings = append(f.mappings, m)
	return true
}
func (f *fakeRegistry) Unset(_, _, _ uint32) bool { return false }
func (f *fakeRegistry) Getport(prog, vers, prot uint32) uint32 {
	for _, m := range f.mappings {
		if m.Prog == prog && m.Vers == vers && m.Prot == prot {
			return m.Port
		}
	}
	return 0
}
func (f *fakeRegistry) Dump() []*xdr.Mapping { return f.mappings }

// encodeRPCB builds an RPCBIND rpcb argument for GETADDR.
func encodeRPCB(prog, vers uint32, netid, addr, owner string) []byte {
	var buf []byte
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], prog)
	buf = append(buf, u[:]...)
	binary.BigEndian.PutUint32(u[:], vers)
	buf = append(buf, u[:]...)
	for _, s := range []string{netid, addr, owner} {
		binary.BigEndian.PutUint32(u[:], uint32(len(s)))
		buf = append(buf, u[:]...)
		buf = append(buf, s...)
		for len(buf)%4 != 0 {
			buf = append(buf, 0)
		}
	}
	return buf
}

// decodeXDRString reads the single XDR string a GETADDR reply carries.
func decodeXDRString(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 4 {
		t.Fatalf("reply too short: %d bytes", len(b))
	}
	n := int(binary.BigEndian.Uint32(b[:4]))
	if 4+n > len(b) {
		t.Fatalf("reply string length %d exceeds %d bytes", n, len(b))
	}
	return string(b[4 : 4+n])
}

func newRpcbHandler() *Handler {
	reg := &fakeRegistry{}
	// nlockmgr (NLM) v3 over UDP on the NFS data port — the macOS lock path.
	reg.Set(&xdr.Mapping{Prog: 100021, Vers: 3, Prot: types.ProtoUDP, Port: 12049})
	reg.Set(&xdr.Mapping{Prog: 100021, Vers: 4, Prot: types.ProtoTCP, Port: 12049})
	return NewHandler(reg)
}

// TestGetaddr_ResolvesUniversalAddress verifies GETADDR returns the correct
// uaddr for a registered service over both IPv4 and IPv6 netids (12049 -> .47.17),
// and an empty string for unknown services or netids.
func TestGetaddr_ResolvesUniversalAddress(t *testing.T) {
	h := newRpcbHandler()

	cases := []struct {
		name       string
		prog, vers uint32
		netid      string
		client     string
		wantUaddr  string
	}{
		{"udp6 IPv6 client (macOS NLM)", 100021, 3, "udp6", "[::1]:611", "::1.47.17"},
		{"udp IPv4 client", 100021, 3, "udp", "127.0.0.1:702", "127.0.0.1.47.17"},
		{"tcp6 IPv6 client", 100021, 4, "tcp6", "[fd00::5]:900", "fd00::5.47.17"},
		{"unregistered program -> empty", 100099, 1, "udp6", "[::1]:611", ""},
		{"unknown netid -> empty", 100021, 3, "rdma", "[::1]:611", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arg := encodeRPCB(tc.prog, tc.vers, tc.netid, "", "")
			reply, err := h.Getaddr(arg, tc.client)
			if err != nil {
				t.Fatalf("Getaddr: %v", err)
			}
			if got := decodeXDRString(t, reply); got != tc.wantUaddr {
				t.Errorf("uaddr = %q, want %q", got, tc.wantUaddr)
			}
		})
	}
}

// TestRpcbDump_EmitsEntries verifies DUMP renders each registration as an rpcb
// entry terminated by a FALSE list marker.
func TestRpcbDump_EmitsEntries(t *testing.T) {
	h := newRpcbHandler()
	reply, err := h.RpcbDump("127.0.0.1:702")
	if err != nil {
		t.Fatalf("RpcbDump: %v", err)
	}
	if len(reply) < 4 {
		t.Fatalf("dump reply too short: %d bytes", len(reply))
	}
	// Must be non-empty (at least one TRUE marker) and end with a FALSE marker.
	if binary.BigEndian.Uint32(reply[:4]) != 1 {
		t.Errorf("expected first list marker TRUE (1), got %d", binary.BigEndian.Uint32(reply[:4]))
	}
	if tail := binary.BigEndian.Uint32(reply[len(reply)-4:]); tail != 0 {
		t.Errorf("expected terminating list marker FALSE (0), got %d", tail)
	}
}

// TestDecodeRPCB_RoundTrip exercises the rpcb decoder against a hand-built arg.
func TestDecodeRPCB_RoundTrip(t *testing.T) {
	arg := encodeRPCB(100021, 4, "udp6", "::1.47.17", "owner")
	r, err := xdr.DecodeRPCB(arg)
	if err != nil {
		t.Fatalf("DecodeRPCB: %v", err)
	}
	if r.Prog != 100021 || r.Vers != 4 || r.Netid != "udp6" || r.Addr != "::1.47.17" || r.Owner != "owner" {
		t.Errorf("decoded %+v, mismatch", r)
	}
}
