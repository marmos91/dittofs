package xdr

import (
	"bytes"
	"io"
)

// ============================================================================
// XDR Codec Interfaces
// ============================================================================

// XdrEncoder is implemented by types that can encode themselves to XDR format.
// This interface is used by NFSv4.1 types to enable generic codec helpers.
type XdrEncoder interface {
	Encode(buf *bytes.Buffer) error
}

// XdrDecoder is implemented by types that can decode themselves from XDR format.
// This interface is used by NFSv4.1 types to enable generic codec helpers.
type XdrDecoder interface {
	Decode(r io.Reader) error
}

// ============================================================================
// XDR Discriminated Union Helpers
// ============================================================================

// EncodeUnionDiscriminant writes the uint32 discriminant of an XDR union.
// This is an alias for WriteUint32 that makes union encode code self-documenting.
//
// Per RFC 4506 Section 4.15 (Discriminated Unions):
// The discriminant is always encoded as a uint32 before the union arm data.
func EncodeUnionDiscriminant(buf *bytes.Buffer, disc uint32) error {
	return WriteUint32(buf, disc)
}

// DecodeUnionDiscriminant reads the uint32 discriminant of an XDR union.
// This is an alias for DecodeUint32 that makes union decode code self-documenting.
//
// Per RFC 4506 Section 4.15 (Discriminated Unions):
// The discriminant is always decoded as a uint32 before the union arm data.
func DecodeUnionDiscriminant(r io.Reader) (uint32, error) {
	return DecodeUint32(r)
}
