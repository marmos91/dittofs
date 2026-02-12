package xdr

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ============================================================================
// XDR Decoding Helpers - Wire Format → Go Types
// ============================================================================

// DecodeOpaque decodes XDR variable-length opaque data.
//
// Per RFC 4506 Section 4.10 (Variable-Length Opaque Data):
// Format: [length:uint32][data:length bytes][padding:0-3 bytes]
// Padding aligns the next item to a 4-byte boundary.
//
// Parameters:
//   - reader: Input stream positioned at start of opaque data
//
// Returns:
//   - []byte: Decoded data
//   - error: Decoding error (EOF, short read, etc.)
//
// XDR Alignment Rule:
// All XDR data types are aligned to 4-byte boundaries. Variable-length data
// is padded with 0-3 zero bytes to achieve this alignment.
func DecodeOpaque(reader io.Reader) ([]byte, error) {
	// Read length (4 bytes)
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}

	// Validate reasonable length (protect against malicious input)
	// NFS typically doesn't have data > 1MB in single opaque fields
	const maxOpaqueLength = 1024 * 1024 // 1 MB
	if length > maxOpaqueLength {
		return nil, fmt.Errorf("opaque length %d exceeds maximum %d", length, maxOpaqueLength)
	}

	// Read data
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}

	// PERFORMANCE OPTIMIZATION: Skip padding using stack-allocated buffer
	// XDR padding is max 3 bytes, so we use a tiny stack buffer instead of io.CopyN
	// This avoids the overhead of io.CopyN for tiny reads
	// Example: length=5 → padding=3, length=8 → padding=0
	padding := (4 - (length % 4)) % 4
	if padding > 0 {
		var padBuf [3]byte
		if _, err := io.ReadFull(reader, padBuf[:padding]); err != nil {
			return nil, fmt.Errorf("skip padding: %w", err)
		}
	}

	return data, nil
}

// DecodeString decodes XDR variable-length string.
//
// Per RFC 4506 Section 4.11 (String):
// Strings use the same encoding as opaque data but are interpreted as UTF-8.
// Format: [length:uint32][data:length bytes][padding:0-3 bytes]
//
// Parameters:
//   - reader: Input stream positioned at start of string
//
// Returns:
//   - string: Decoded string (UTF-8)
//   - error: Decoding error
func DecodeString(reader io.Reader) (string, error) {
	data, err := DecodeOpaque(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DecodeUint32 decodes a 32-bit unsigned integer from XDR format.
//
// Per RFC 4506 Section 4.1 (Integer):
// Unsigned 32-bit integers are encoded in big-endian byte order.
//
// Parameters:
//   - reader: Input stream positioned at start of uint32
//
// Returns:
//   - uint32: Decoded value
//   - error: Decoding error
func DecodeUint32(reader io.Reader) (uint32, error) {
	var v uint32
	if err := binary.Read(reader, binary.BigEndian, &v); err != nil {
		return 0, fmt.Errorf("read uint32: %w", err)
	}
	return v, nil
}

// DecodeUint64 decodes a 64-bit unsigned integer from XDR format.
//
// Per RFC 4506 Section 4.5 (Hyper Integer):
// Unsigned 64-bit integers are encoded in big-endian byte order.
//
// Parameters:
//   - reader: Input stream positioned at start of uint64
//
// Returns:
//   - uint64: Decoded value
//   - error: Decoding error
func DecodeUint64(reader io.Reader) (uint64, error) {
	var v uint64
	if err := binary.Read(reader, binary.BigEndian, &v); err != nil {
		return 0, fmt.Errorf("read uint64: %w", err)
	}
	return v, nil
}

// DecodeInt32 decodes a 32-bit signed integer from XDR format.
//
// Per RFC 4506 Section 4.1 (Integer):
// Signed 32-bit integers are encoded in big-endian byte order using
// two's complement representation.
//
// Parameters:
//   - reader: Input stream positioned at start of int32
//
// Returns:
//   - int32: Decoded value
//   - error: Decoding error
func DecodeInt32(reader io.Reader) (int32, error) {
	var v int32
	if err := binary.Read(reader, binary.BigEndian, &v); err != nil {
		return 0, fmt.Errorf("read int32: %w", err)
	}
	return v, nil
}

// DecodeBool decodes an XDR boolean value.
//
// Per RFC 4506 Section 4.4 (Boolean):
// Booleans are encoded as uint32 where 0 = false, any non-zero = true.
// Typically only 0 and 1 are used.
//
// Parameters:
//   - reader: Input stream positioned at start of boolean
//
// Returns:
//   - bool: Decoded value (false if 0, true otherwise)
//   - error: Decoding error
func DecodeBool(reader io.Reader) (bool, error) {
	v, err := DecodeUint32(reader)
	if err != nil {
		return false, err
	}
	return v != 0, nil
}
