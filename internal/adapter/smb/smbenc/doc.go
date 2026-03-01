// Package smbenc provides binary encoding and decoding utilities for the SMB2/3
// wire protocol.
//
// The package uses an error-accumulation pattern inspired by bufio.Scanner:
// callers perform multiple read/write operations and check for errors once at
// the end, rather than after every individual operation.
//
// Reader wraps a byte slice with a position cursor and accumulates the first
// error. Once an error occurs, all subsequent reads become no-ops returning
// zero values. This eliminates repetitive error checking:
//
//	r := smbenc.NewReader(data)
//	dialect := r.ReadUint16()
//	flags := r.ReadUint32()
//	guid := r.ReadBytes(16)
//	if r.Err() != nil {
//	    return r.Err()  // handles any short read in the sequence
//	}
//
// Writer appends to a byte buffer with pre-allocated capacity. It provides
// padding and backpatching support needed for SMB2/3 negotiate contexts:
//
//	w := smbenc.NewWriter(256)
//	w.WriteUint16(dialect)
//	w.WriteUint32(flags)
//	w.Pad(8) // pad to 8-byte alignment
//	return w.Bytes()
//
// All integer operations use little-endian byte order as required by the
// SMB2/3 protocol specification ([MS-SMB2]).
package smbenc
