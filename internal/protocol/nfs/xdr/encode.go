package xdr

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
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
	for range padding {
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

	// Write all fields in order per RFC 1813
	if err := binary.Write(buf, binary.BigEndian, attr.Type); err != nil {
		return fmt.Errorf("write type: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mode); err != nil {
		return fmt.Errorf("write mode: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Nlink); err != nil {
		return fmt.Errorf("write nlink: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.UID); err != nil {
		return fmt.Errorf("write uid: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.GID); err != nil {
		return fmt.Errorf("write gid: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Size); err != nil {
		return fmt.Errorf("write size: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Used); err != nil {
		return fmt.Errorf("write used: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Rdev); err != nil {
		return fmt.Errorf("write rdev: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Fsid); err != nil {
		return fmt.Errorf("write fsid: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Fileid); err != nil {
		return fmt.Errorf("write fileid: %w", err)
	}

	// Write atime (nfstime3)
	if err := binary.Write(buf, binary.BigEndian, attr.Atime.Seconds); err != nil {
		return fmt.Errorf("write atime seconds: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Atime.Nseconds); err != nil {
		return fmt.Errorf("write atime nseconds: %w", err)
	}

	// Write mtime (nfstime3)
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Seconds); err != nil {
		return fmt.Errorf("write mtime seconds: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Nseconds); err != nil {
		return fmt.Errorf("write mtime nseconds: %w", err)
	}

	// Write ctime (nfstime3)
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Seconds); err != nil {
		return fmt.Errorf("write ctime seconds: %w", err)
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Nseconds); err != nil {
		return fmt.Errorf("write ctime nseconds: %w", err)
	}

	return nil
}
