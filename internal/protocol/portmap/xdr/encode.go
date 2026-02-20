// Package xdr provides XDR encoding and decoding for portmapper protocol messages.
//
// This package implements the wire format serialization for the portmapper (portmap v2)
// protocol as specified in RFC 1057 Section A. The portmap mapping struct is 4 fixed-size
// uint32 fields (prog, vers, prot, port), making XDR encoding straightforward with
// encoding/binary BigEndian.
//
// The DUMP response uses XDR optional-data linked list encoding:
// each entry is preceded by a uint32(1) discriminant, and the list
// is terminated by uint32(0).
//
// References:
//   - RFC 1057 Section A (Port Mapper Program Protocol)
//   - RFC 4506 (XDR: External Data Representation Standard)
package xdr

import (
	"encoding/binary"
)

// Mapping represents a portmap mapping entry.
//
// Wire format (RFC 1057):
//
//	prog: uint32 - RPC program number
//	vers: uint32 - RPC program version
//	prot: uint32 - Protocol (6=TCP, 17=UDP)
//	port: uint32 - Port number
type Mapping struct {
	Prog uint32
	Vers uint32
	Prot uint32
	Port uint32
}

// MappingSize is the XDR-encoded size of a single mapping (4 x uint32 = 16 bytes).
const MappingSize = 16

// EncodeMapping encodes a single portmap mapping to 16 bytes XDR.
//
// Wire format: [prog:uint32][vers:uint32][prot:uint32][port:uint32]
func EncodeMapping(m *Mapping) []byte {
	buf := make([]byte, MappingSize)
	binary.BigEndian.PutUint32(buf[0:4], m.Prog)
	binary.BigEndian.PutUint32(buf[4:8], m.Vers)
	binary.BigEndian.PutUint32(buf[8:12], m.Prot)
	binary.BigEndian.PutUint32(buf[12:16], m.Port)
	return buf
}

// EncodeDumpResponse encodes a DUMP response as an XDR optional-data linked list.
//
// Wire format per RFC 1057:
//
//	For each mapping:
//	  value_follows: uint32(1)
//	  mapping: [prog:uint32][vers:uint32][prot:uint32][port:uint32]
//	After last mapping:
//	  value_follows: uint32(0)
//
// An empty list produces just uint32(0) (4 bytes).
func EncodeDumpResponse(mappings []*Mapping) []byte {
	// Each entry: 4 bytes discriminant + 16 bytes mapping
	// Plus 4 bytes terminator
	entrySize := 4 + MappingSize
	buf := make([]byte, len(mappings)*entrySize+4)

	offset := 0
	for _, m := range mappings {
		// value_follows = true
		binary.BigEndian.PutUint32(buf[offset:offset+4], 1)
		offset += 4

		// mapping data
		binary.BigEndian.PutUint32(buf[offset:offset+4], m.Prog)
		binary.BigEndian.PutUint32(buf[offset+4:offset+8], m.Vers)
		binary.BigEndian.PutUint32(buf[offset+8:offset+12], m.Prot)
		binary.BigEndian.PutUint32(buf[offset+12:offset+16], m.Port)
		offset += MappingSize
	}

	// Terminator: value_follows = false
	binary.BigEndian.PutUint32(buf[offset:offset+4], 0)

	return buf
}

// EncodeGetportResponse encodes a GETPORT response as a single uint32.
//
// Wire format: [port:uint32]
// Returns 0 if the program is not registered.
func EncodeGetportResponse(port uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, port)
	return buf
}

// EncodeBoolResponse encodes an XDR boolean response.
//
// Wire format: uint32(1) for true, uint32(0) for false.
// Used for SET and UNSET response values.
func EncodeBoolResponse(val bool) []byte {
	buf := make([]byte, 4)
	if val {
		binary.BigEndian.PutUint32(buf, 1)
	}
	// buf is zero-initialized, so false case needs no explicit write
	return buf
}
