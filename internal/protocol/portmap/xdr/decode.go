package xdr

import (
	"encoding/binary"
	"fmt"
)

// DecodeMapping decodes a portmap mapping struct from XDR bytes.
//
// Wire format: [prog:uint32][vers:uint32][prot:uint32][port:uint32]
//
// The input must be at least 16 bytes (trailing bytes are ignored). Used for SET, UNSET, and GETPORT
// request arguments, which all send a mapping struct as their argument.
func DecodeMapping(data []byte) (*Mapping, error) {
	if len(data) < MappingSize {
		return nil, fmt.Errorf("portmap mapping too short: got %d bytes, need %d", len(data), MappingSize)
	}

	m := &Mapping{
		Prog: binary.BigEndian.Uint32(data[0:4]),
		Vers: binary.BigEndian.Uint32(data[4:8]),
		Prot: binary.BigEndian.Uint32(data[8:12]),
		Port: binary.BigEndian.Uint32(data[12:16]),
	}

	return m, nil
}
