// Package keys is the store's key encoding. A key is the concatenation of its
// components' encodings with no separator; every encoding preserves order and
// delimits itself, so keys compare byte for byte as their component tuples do
// and the keys under a prefix are exactly those whose leading components are
// the prefix's.
//
// The codes below are part of the store format: changing one is a format bump,
// and a code once assigned is never given to another kind. Golden vectors in
// keys_test.go pin them.
package keys

import (
	"encoding/binary"
	"errors"
)

// Kind is a key's first byte. No kind is a prefix of another.
const (
	KindStore byte = 0x00 // followed by "format"; nothing else is under 0x00

	KindF byte = 0x01 // per file: F‖ShareID‖FileID
	KindS byte = 0x02 // per share: S‖ShareID

	KindC  byte = 0x10 // per namespace, content-addressed
	KindCR byte = 0x11
	KindB  byte = 0x12
	KindBR byte = 0x13
	KindBD byte = 0x14
	KindBC byte = 0x15
	KindI  byte = 0x16
	KindNS byte = 0x17

	KindU      byte = 0x20 // server-wide
	KindG      byte = 0x21
	KindM      byte = 0x22
	KindMR     byte = 0x23
	KindGR     byte = 0x24
	KindNX     byte = 0x25
	KindNP     byte = 0x26
	KindNG     byte = 0x27
	KindSL     byte = 0x28
	KindIN     byte = 0x29
	KindNSM    byte = 0x2A
	KindPX     byte = 0x2B
	KindN      byte = 0x2C
	KindSH     byte = 0x2D
	KindJ      byte = 0x2E
	KindRS     byte = 0x2F
	KindMV     byte = 0x30
	KindSLOT   byte = 0x31
	KindCFG    byte = 0x32
	KindCFGGEN byte = 0x33
	KindSEC    byte = 0x34
	KindCL     byte = 0x35
	KindCLO    byte = 0x36
)

// Sub-kinds under F‖id. The File itself has none.
const (
	FAcl byte = 0x01
	FX   byte = 0x02
	FS   byte = 0x03
	FE   byte = 0x04
	FT   byte = 0x05
	FH   byte = 0x06 // hole, h
	FRm  byte = 0x07
	FOv  byte = 0x08
	FRef byte = 0x09
	FHis byte = 0x0A // history, H
	FFx  byte = 0x0B
	FFo  byte = 0x0C
	FRel byte = 0x0D
	FOp  byte = 0x0E
	FDp  byte = 0x0F

	OpL byte = 0x01 // under F‖id‖op‖openID
)

// Sub-kinds under S‖id.
const (
	SInfo byte = 0x01
	SSt   byte = 0x02
	SNsg  byte = 0x03
	SG    byte = 0x04
	SXp   byte = 0x05
	SSnap byte = 0x06
	SCut  byte = 0x07
	SLive byte = 0x08
	SSc   byte = 0x09
	SHd   byte = 0x0A
	SHr   byte = 0x0B
	SUse  byte = 0x0C
	SRh   byte = 0x0D
	SOr   byte = 0x0E
	SUt   byte = 0x0F
	SPu   byte = 0x10
	SPj   byte = 0x11
	SUd   byte = 0x12
	SQt   byte = 0x13
	SQx   byte = 0x14
	SVf   byte = 0x15
	SFnc  byte = 0x16
)

// Sub-kinds under NS‖ns, and under NS‖ns‖gc.
const (
	NSGc    byte = 0x01
	NSClaim byte = 0x02
	NSPk    byte = 0x03
	NSKey   byte = 0x04
	NSBk    byte = 0x05

	GcLease   byte = 0x01
	GcRecheck byte = 0x02
	GcHold    byte = 0x03
	GcSuspect byte = 0x04
	GcForward byte = 0x05
	GcPause   byte = 0x06
	GcCursor  byte = 0x07
)

// Sub-kinds under N‖node, SH‖shard and CL‖clientID.
const (
	NExp byte = 0x01
	NClk byte = 0x02

	SHRq byte = 0x01
	SHXh byte = 0x02
	SHHw byte = 0x03

	CLSh byte = 0x01
)

// Enumerated components.
const (
	NXUser  byte = 0x01
	NXGroup byte = 0x02
	NXShare byte = 0x03

	PXUid byte = 0x01
	PXGid byte = 0x02
	PXSid byte = 0x03
	PXKrb byte = 0x04

	KeyChunkID  byte = 0x01
	KeyChunking byte = 0x02
	KeyHeader   byte = 0x03
	KeyExport   byte = 0x04
	KeyData     byte = 0x05
)

// IDLen is the length of every minted identifier.
const IDLen = 16

// Format is the store format record's key.
var Format = []byte("\x00format")

// ErrMalformed is a key that does not decode as the component asked for.
var ErrMalformed = errors.New("keys: malformed key")

// Uint64 appends an integer: 8 bytes, big-endian. Times are store time as
// unsigned nanoseconds since the Unix epoch, encoded the same way.
func Uint64(dst []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(dst, v) }

// ID appends a fixed-length identifier raw. Its length is the caller's to
// hold fixed, IDLen for every minted ID.
func ID(dst, id []byte) []byte { return append(dst, id...) }

// String appends a byte string with each 0x00 written as 0x00 0xFF, then the
// terminator 0x00 0x01. The terminator cannot occur inside a string, so no
// string's encoding is a prefix of another's, and a string sorts before every
// longer string it begins.
func String(dst, s []byte) []byte {
	for _, c := range s {
		dst = append(dst, c)
		if c == 0x00 {
			dst = append(dst, 0xFF)
		}
	}
	return append(dst, 0x00, 0x01)
}

// ReadUint64 decodes an integer from the front of b.
func ReadUint64(b []byte) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, ErrMalformed
	}
	return binary.BigEndian.Uint64(b), b[8:], nil
}

// ReadID decodes an n-byte identifier from the front of b, sharing b's bytes.
func ReadID(b []byte, n int) ([]byte, []byte, error) {
	if len(b) < n {
		return nil, nil, ErrMalformed
	}
	return b[:n:n], b[n:], nil
}

// ReadString decodes a byte string from the front of b into a new slice.
func ReadString(b []byte) ([]byte, []byte, error) {
	var s []byte
	for i := 0; i < len(b); i++ {
		if b[i] != 0x00 {
			s = append(s, b[i])
			continue
		}
		if i+1 == len(b) {
			break
		}
		switch b[i+1] {
		case 0x01:
			return s, b[i+2:], nil
		case 0xFF:
			s = append(s, 0x00)
			i++
		default:
			return nil, nil, ErrMalformed
		}
	}
	return nil, nil, ErrMalformed
}
