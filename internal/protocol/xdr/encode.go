package xdr

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// ============================================================================
// XDR Encoding Helpers - Go Types → Wire Format
// ============================================================================

// WriteXDROpaque encodes opaque data (byte array) in XDR format: length + data + padding.
//
// Per RFC 4506 Section 4.9 (Variable-Length Opaque Data):
// Format: [length:uint32][data:bytes][padding:bytes]
//
// XDR opaque data is encoded as:
// 1. Length (uint32): Number of bytes in the data
// 2. Data: The actual bytes
// 3. Padding: Zero bytes to align to 4-byte boundary
//
// This is identical to string encoding but takes []byte instead of string.
// Used for binary data like file handles, authentication tokens, etc.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - data: Byte array to encode
//
// Returns:
//   - error: Encoding error
//
// Example:
//
//	[]byte{0x01, 0x02, 0x03} → [00 00 00 03][01 02 03][00] (8 bytes total)
func WriteXDROpaque(buf *bytes.Buffer, data []byte) error {
	// Write length
	length := uint32(len(data))
	if err := binary.Write(buf, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write opaque length: %w", err)
	}

	// Write data
	if _, err := buf.Write(data); err != nil {
		return fmt.Errorf("write opaque data: %w", err)
	}

	// Write padding to 4-byte boundary
	return WriteXDRPadding(buf, length)
}

// WriteXDRString encodes a string in XDR format: length + data + padding.
//
// Per RFC 4506 Section 4.11 (String):
// Format: [length:uint32][data:bytes][padding:bytes]
//
// XDR strings are encoded as:
// 1. Length (uint32): Number of bytes in the string
// 2. Data: The actual string bytes
// 3. Padding: Zero bytes to align to 4-byte boundary
//
// Padding calculation: (4 - (length % 4)) % 4
// - Ensures total encoded size is multiple of 4 bytes
// - 0-3 padding bytes depending on string length
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - s: String to encode
//
// Returns:
//   - error: Encoding error
//
// Example:
//
//	"abc" (3 bytes) → [00 00 00 03][61 62 63][00] (8 bytes total)
//	"test" (4 bytes) → [00 00 00 04][74 65 73 74] (8 bytes total)
func WriteXDRString(buf *bytes.Buffer, s string) error {
	// Write length
	length := uint32(len(s))
	if err := binary.Write(buf, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write string length: %w", err)
	}

	// Write data
	if _, err := buf.Write([]byte(s)); err != nil {
		return fmt.Errorf("write string data: %w", err)
	}

	// Write padding to 4-byte boundary
	return WriteXDRPadding(buf, length)
}

// WriteXDRPadding writes padding bytes to align to 4-byte boundary.
//
// Per RFC 4506 Section 4.11:
// All XDR data must be aligned to 4-byte boundaries. After writing
// variable-length data, padding bytes (always zero) are added.
//
// Padding calculation: (4 - (dataLen % 4)) % 4
// - 0 bytes padding if dataLen is multiple of 4
// - 1-3 bytes padding otherwise
//
// Parameters:
//   - buf: Output buffer for padding bytes
//   - dataLen: Length of data that was just written
//
// Returns:
//   - error: Write error
//
// Example:
//
//	dataLen=3 → writes 1 padding byte
//	dataLen=4 → writes 0 padding bytes
//	dataLen=5 → writes 3 padding bytes
func WriteXDRPadding(buf *bytes.Buffer, dataLen uint32) error {
	padding := (4 - (dataLen % 4)) % 4
	if padding > 0 {
		paddingBytes := make([]byte, padding)
		if _, err := buf.Write(paddingBytes); err != nil {
			return fmt.Errorf("write padding: %w", err)
		}
	}
	return nil
}

// WriteUint32 encodes a 32-bit unsigned integer in XDR format.
//
// Per RFC 4506 Section 4.1 (Integer):
// Unsigned 32-bit integers are encoded in big-endian byte order.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - v: Value to encode
//
// Returns:
//   - error: Encoding error
func WriteUint32(buf *bytes.Buffer, v uint32) error {
	if err := binary.Write(buf, binary.BigEndian, v); err != nil {
		return fmt.Errorf("write uint32: %w", err)
	}
	return nil
}

// WriteUint64 encodes a 64-bit unsigned integer in XDR format.
//
// Per RFC 4506 Section 4.5 (Hyper Integer):
// Unsigned 64-bit integers are encoded in big-endian byte order.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - v: Value to encode
//
// Returns:
//   - error: Encoding error
func WriteUint64(buf *bytes.Buffer, v uint64) error {
	if err := binary.Write(buf, binary.BigEndian, v); err != nil {
		return fmt.Errorf("write uint64: %w", err)
	}
	return nil
}

// WriteInt32 encodes a 32-bit signed integer in XDR format.
//
// Per RFC 4506 Section 4.1 (Integer):
// Signed 32-bit integers are encoded in big-endian byte order using
// two's complement representation.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - v: Value to encode
//
// Returns:
//   - error: Encoding error
func WriteInt32(buf *bytes.Buffer, v int32) error {
	if err := binary.Write(buf, binary.BigEndian, v); err != nil {
		return fmt.Errorf("write int32: %w", err)
	}
	return nil
}

// WriteInt64 encodes a 64-bit signed integer in XDR format.
//
// Per RFC 4506 Section 4.5 (Hyper Integer):
// Signed 64-bit integers are encoded in big-endian byte order using
// two's complement representation.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - v: Value to encode
//
// Returns:
//   - error: Encoding error
func WriteInt64(buf *bytes.Buffer, v int64) error {
	if err := binary.Write(buf, binary.BigEndian, v); err != nil {
		return fmt.Errorf("write int64: %w", err)
	}
	return nil
}

// WriteBool encodes a boolean value in XDR format.
//
// Per RFC 4506 Section 4.4 (Boolean):
// Booleans are encoded as uint32 where 0 = false, 1 = true.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - v: Value to encode
//
// Returns:
//   - error: Encoding error
func WriteBool(buf *bytes.Buffer, v bool) error {
	var val uint32
	if v {
		val = 1
	}
	return WriteUint32(buf, val)
}
