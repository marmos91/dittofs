package xdr

import (
	"encoding/binary"
	"fmt"
)

// RPCB is the RPCBIND v3/v4 address mapping (RFC 1833, struct rpcb).
//
//	struct rpcb {
//	    unsigned long r_prog;   // program number
//	    unsigned long r_vers;   // version number
//	    string        r_netid;  // transport: "tcp", "udp", "tcp6", "udp6"
//	    string        r_addr;   // universal address ("host.p1.p2")
//	    string        r_owner;  // owner of the registration
//	};
type RPCB struct {
	Prog  uint32
	Vers  uint32
	Netid string
	Addr  string
	Owner string
}

// readXDRString reads an XDR variable-length string (uint32 length + bytes +
// padding to a 4-byte boundary) starting at off, returning the next offset.
func readXDRString(data []byte, off int) (string, int, error) {
	if off+4 > len(data) {
		return "", 0, fmt.Errorf("rpcb: truncated string length at offset %d", off)
	}
	n := int(binary.BigEndian.Uint32(data[off:]))
	off += 4
	if n < 0 || off+n > len(data) {
		return "", 0, fmt.Errorf("rpcb: string length %d exceeds remaining buffer", n)
	}
	s := string(data[off : off+n])
	off += n + (4-n%4)%4 // skip the value and its XDR padding
	if off > len(data) {
		off = len(data)
	}
	return s, off, nil
}

// writeXDRString appends an XDR string (length + bytes + padding) to buf.
func writeXDRString(buf []byte, s string) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(s)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, s...)
	if pad := (4 - len(s)%4) % 4; pad > 0 {
		buf = append(buf, make([]byte, pad)...)
	}
	return buf
}

// DecodeRPCB decodes an RPCBIND rpcb argument (used by GETADDR/GETVERSADDR).
func DecodeRPCB(data []byte) (*RPCB, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("rpcb: argument too short (%d bytes)", len(data))
	}
	r := &RPCB{
		Prog: binary.BigEndian.Uint32(data[0:4]),
		Vers: binary.BigEndian.Uint32(data[4:8]),
	}
	off := 8
	var err error
	if r.Netid, off, err = readXDRString(data, off); err != nil {
		return nil, err
	}
	if r.Addr, off, err = readXDRString(data, off); err != nil {
		return nil, err
	}
	if r.Owner, _, err = readXDRString(data, off); err != nil {
		return nil, err
	}
	return r, nil
}

// EncodeGetaddrResponse encodes the GETADDR result: a single XDR string holding
// the universal address ("" when the service is not registered).
func EncodeGetaddrResponse(uaddr string) []byte {
	return writeXDRString(nil, uaddr)
}

// EncodeRpcbDumpResponse encodes the RPCBIND DUMP result, an rpcblist. The XDR
// pointer/list form prefixes each element with a TRUE marker and terminates the
// list with FALSE.
func EncodeRpcbDumpResponse(entries []RPCB) []byte {
	var buf []byte
	var marker [4]byte
	for i := range entries {
		binary.BigEndian.PutUint32(marker[:], 1) // value_follows = TRUE
		buf = append(buf, marker[:]...)
		var pv [8]byte
		binary.BigEndian.PutUint32(pv[0:4], entries[i].Prog)
		binary.BigEndian.PutUint32(pv[4:8], entries[i].Vers)
		buf = append(buf, pv[:]...)
		buf = writeXDRString(buf, entries[i].Netid)
		buf = writeXDRString(buf, entries[i].Addr)
		buf = writeXDRString(buf, entries[i].Owner)
	}
	binary.BigEndian.PutUint32(marker[:], 0) // value_follows = FALSE (end of list)
	return append(buf, marker[:]...)
}

// BuildUaddr renders a universal address (RFC 1833 §2.2): the textual host
// followed by ".p1.p2", where the port equals p1*256 + p2.
func BuildUaddr(host string, port uint32) string {
	return fmt.Sprintf("%s.%d.%d", host, (port>>8)&0xff, port&0xff)
}
