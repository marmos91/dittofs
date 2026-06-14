package xdr

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// XDR Encoding Helpers - Go Structures → Wire Format
// ============================================================================

// EncodeOptionalOpaque encodes optional XDR opaque data.
//
// Per RFC 1813 Section 2.4 (Optional Data):
// Format: [present:uint32] if present=1: [length:uint32][data][padding]
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - data: Opaque data to encode (nil or empty = not present)
//
// Returns:
//   - error: Encoding error
//
// XDR Optional Rule:
// Optional data is preceded by a boolean (uint32: 0=absent, 1=present).
// If absent, only the boolean is written. If present, the data follows.
func EncodeOptionalOpaque(buf *bytes.Buffer, data []byte) error {
	if len(data) == 0 {
		// Not present: write 0 and return
		return binary.Write(buf, binary.BigEndian, uint32(0))
	}

	// Present flag
	if err := binary.Write(buf, binary.BigEndian, uint32(1)); err != nil {
		return fmt.Errorf("write present flag: %w", err)
	}

	// Length
	length := uint32(len(data))
	if err := binary.Write(buf, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write length: %w", err)
	}

	// Data
	if _, err := buf.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	// Padding to 4-byte boundary
	padding := (4 - (length % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if err := buf.WriteByte(0); err != nil {
			return fmt.Errorf("write padding: %w", err)
		}
	}

	return nil
}

// EncodeOptionalFileAttr encodes optional NFS file attributes.
//
// Per RFC 1813 Section 2.4 (post_op_attr):
// Format: [present:uint32] if present=1: [fattr3]
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - attr: File attributes to encode (nil = not present)
//
// Returns:
//   - error: Encoding error
func EncodeOptionalFileAttr(buf *bytes.Buffer, attr *types.NFSFileAttr) error {
	if attr == nil {
		// Not present: write 0
		return binary.Write(buf, binary.BigEndian, uint32(0))
	}

	// Present flag: write 1
	if err := binary.Write(buf, binary.BigEndian, uint32(1)); err != nil {
		return fmt.Errorf("write present flag: %w", err)
	}

	// Encode file attributes
	return EncodeFileAttr(buf, attr)
}

// EncodeWccData encodes weak cache consistency data.
//
// Per RFC 1813 Section 2.6 (wcc_data):
//
//	struct wcc_data {
//	    pre_op_attr   before;  // Optional: attributes before operation
//	    post_op_attr  after;   // Optional: attributes after operation
//	};
//
// WCC data allows clients to validate their cache:
// - before: captured before the modifying operation
// - after: captured after the modifying operation
//
// If before.mtime matches client's cached mtime, and after.mtime changed,
// client knows their cache is stale and must refresh.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - before: Pre-operation attributes (wcc_attr, may be nil)
//   - after: Post-operation attributes (fattr3, may be nil)
//
// Returns:
//   - error: Encoding error
func EncodeWccData(buf *bytes.Buffer, before *types.WccAttr, after *types.NFSFileAttr) error {
	// Encode pre-op attributes (wcc_attr)
	if before != nil {
		// Present
		if err := binary.Write(buf, binary.BigEndian, uint32(1)); err != nil {
			return fmt.Errorf("write before present: %w", err)
		}
		if err := encodeWccAttr(buf, before); err != nil {
			return fmt.Errorf("encode before attributes: %w", err)
		}
	} else {
		// Not present
		if err := binary.Write(buf, binary.BigEndian, uint32(0)); err != nil {
			return fmt.Errorf("write before not present: %w", err)
		}
	}

	// Encode post-op attributes (fattr3)
	if err := EncodeOptionalFileAttr(buf, after); err != nil {
		return fmt.Errorf("encode after attributes: %w", err)
	}

	return nil
}

// encodeWccAttr encodes pre-operation WCC attributes.
//
// Per RFC 1813 Section 2.6 (wcc_attr):
//
//	struct wcc_attr {
//	    size3   size;   // File size (uint64)
//	    nfstime3 mtime; // Modification time
//	    nfstime3 ctime; // Change time
//	};
//
// This is a subset of fattr3 containing only the fields clients need
// for cache validation, reducing wire overhead.
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - attr: WCC attributes to encode
//
// Returns:
//   - error: Encoding error
func encodeWccAttr(buf *bytes.Buffer, attr *types.WccAttr) error {
	if attr == nil {
		// Defensive: caller should have checked, but handle gracefully
		return fmt.Errorf("wcc_attr is nil")
	}

	// Write size (uint64)
	if err := binary.Write(buf, binary.BigEndian, attr.Size); err != nil {
		return fmt.Errorf("write size: %w", err)
	}

	// Write mtime (nfstime3: seconds + nseconds)
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Seconds); err != nil {
		return fmt.Errorf("write mtime seconds: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Nseconds); err != nil {
		return fmt.Errorf("write mtime nseconds: %w", err)
	}

	// Write ctime (nfstime3: seconds + nseconds)
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Seconds); err != nil {
		return fmt.Errorf("write ctime seconds: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Nseconds); err != nil {
		return fmt.Errorf("write ctime nseconds: %w", err)
	}

	return nil
}

// EncodeFileAttr encodes NFS file attributes (fattr3).
//
// Per RFC 1813 Section 2.3.1 (fattr3):
// The fattr3 structure contains all attributes of a file.
//
// Wire format (all fields required, no optional):
//   - type (uint32): file type (NF3REG, NF3DIR, etc.)
//   - mode (uint32): Unix permission bits
//   - nlink (uint32): number of hard links
//   - uid (uint32): owner user ID
//   - gid (uint32): owner group ID
//   - size (uint64): file size in bytes
//   - used (uint64): disk space used in bytes
//   - rdev (specdata3): device number for special files
//   - fsid (uint64): filesystem identifier
//   - fileid (uint64): file identifier (inode number)
//   - atime (nfstime3): last access time
//   - mtime (nfstime3): last modification time
//   - ctime (nfstime3): last metadata change time
//
// Parameters:
//   - buf: Output buffer for encoded data
//   - attr: File attributes to encode
//
// Returns:
//   - error: Encoding error
func EncodeFileAttr(buf *bytes.Buffer, attr *types.NFSFileAttr) error {
	if attr == nil {
		return fmt.Errorf("file attributes are nil")
	}

	// fattr3 is a fixed-size structure: 5 uint32 + 2 uint64 + specdata3 (2
	// uint32) + 2 uint64 + 3 nfstime3 (each 2 uint32) = 84 bytes. Encode all
	// fields directly into a stack-allocated array via binary.BigEndian to
	// avoid the per-field reflection overhead of binary.Write.
	var b [84]byte
	binary.BigEndian.PutUint32(b[0:4], attr.Type)
	binary.BigEndian.PutUint32(b[4:8], attr.Mode)
	binary.BigEndian.PutUint32(b[8:12], attr.Nlink)
	binary.BigEndian.PutUint32(b[12:16], attr.UID)
	binary.BigEndian.PutUint32(b[16:20], attr.GID)
	binary.BigEndian.PutUint64(b[20:28], attr.Size)
	binary.BigEndian.PutUint64(b[28:36], attr.Used)
	// rdev (specdata3): major then minor
	binary.BigEndian.PutUint32(b[36:40], attr.Rdev.Major)
	binary.BigEndian.PutUint32(b[40:44], attr.Rdev.Minor)
	binary.BigEndian.PutUint64(b[44:52], attr.Fsid)
	binary.BigEndian.PutUint64(b[52:60], attr.Fileid)
	// atime (nfstime3): seconds then nseconds
	binary.BigEndian.PutUint32(b[60:64], attr.Atime.Seconds)
	binary.BigEndian.PutUint32(b[64:68], attr.Atime.Nseconds)
	// mtime (nfstime3)
	binary.BigEndian.PutUint32(b[68:72], attr.Mtime.Seconds)
	binary.BigEndian.PutUint32(b[72:76], attr.Mtime.Nseconds)
	// ctime (nfstime3)
	binary.BigEndian.PutUint32(b[76:80], attr.Ctime.Seconds)
	binary.BigEndian.PutUint32(b[80:84], attr.Ctime.Nseconds)

	if _, err := buf.Write(b[:]); err != nil {
		return fmt.Errorf("write fattr3: %w", err)
	}

	return nil
}

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
	return xdr.WriteXDROpaque(buf, data)
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
	return xdr.WriteXDRString(buf, s)
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
	return xdr.WriteXDRPadding(buf, dataLen)
}
