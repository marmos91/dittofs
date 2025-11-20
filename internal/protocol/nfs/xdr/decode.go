package xdr

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// XDR Decoding Helpers - Wire Format → Go Structures
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

// DecodeSetAttrs decodes NFS sattr3 (set attributes) structure from XDR.
//
// Per RFC 1813 Section 2.5.3 (sattr3):
//
//	struct sattr3 {
//	    set_mode3   mode;    // [set:uint32][mode:uint32]
//	    set_uid3    uid;     // [set:uint32][uid:uint32]
//	    set_gid3    gid;     // [set:uint32][gid:uint32]
//	    set_size3   size;    // [set:uint32][size:uint64]
//	    set_atime   atime;   // [set:uint32][time:nfstime3]
//	    set_mtime   mtime;   // [set:uint32][time:nfstime3]
//	};
//
// Each field is a discriminated union:
//   - set=0 (DONT_CHANGE): field not set, pointer remains nil
//   - set=1 (SET_TO_CLIENT_TIME): field is set, pointer to value (for time: read nfstime3)
//   - set=2 (SET_TO_SERVER_TIME): field is set to server time (for time fields only)
//
// Parameters:
//   - reader: Input stream positioned at start of sattr3
//
// Returns:
//   - *metadata.SetAttrs: Decoded attributes with nil pointers for unchanged fields
//   - error: Decoding error
func DecodeSetAttrs(reader io.Reader) (*metadata.SetAttrs, error) {
	attr := &metadata.SetAttrs{}

	// Decode mode (set_mode3)
	var setMode uint32
	if err := binary.Read(reader, binary.BigEndian, &setMode); err != nil {
		return nil, fmt.Errorf("read set_mode: %w", err)
	}
	if setMode == 1 {
		var mode uint32
		if err := binary.Read(reader, binary.BigEndian, &mode); err != nil {
			return nil, fmt.Errorf("read mode: %w", err)
		}
		attr.Mode = &mode
	} else if setMode != 0 {
		return nil, fmt.Errorf("invalid set_mode discriminator: %d (expected 0 or 1)", setMode)
	}

	// Decode UID (set_uid3)
	var setUID uint32
	if err := binary.Read(reader, binary.BigEndian, &setUID); err != nil {
		return nil, fmt.Errorf("read set_uid: %w", err)
	}
	if setUID == 1 {
		var uid uint32
		if err := binary.Read(reader, binary.BigEndian, &uid); err != nil {
			return nil, fmt.Errorf("read uid: %w", err)
		}
		attr.UID = &uid
	} else if setUID != 0 {
		return nil, fmt.Errorf("invalid set_uid discriminator: %d (expected 0 or 1)", setUID)
	}

	// Decode GID (set_gid3)
	var setGID uint32
	if err := binary.Read(reader, binary.BigEndian, &setGID); err != nil {
		return nil, fmt.Errorf("read set_gid: %w", err)
	}
	if setGID == 1 {
		var gid uint32
		if err := binary.Read(reader, binary.BigEndian, &gid); err != nil {
			return nil, fmt.Errorf("read gid: %w", err)
		}
		attr.GID = &gid
	} else if setGID != 0 {
		return nil, fmt.Errorf("invalid set_gid discriminator: %d (expected 0 or 1)", setGID)
	}

	// Decode Size (set_size3)
	var setSize uint32
	if err := binary.Read(reader, binary.BigEndian, &setSize); err != nil {
		return nil, fmt.Errorf("read set_size: %w", err)
	}
	if setSize == 1 {
		var size uint64
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
			return nil, fmt.Errorf("read size: %w", err)
		}
		attr.Size = &size
	} else if setSize != 0 {
		return nil, fmt.Errorf("invalid set_size discriminator: %d (expected 0 or 1)", setSize)
	}

	// Decode Atime (set_atime)
	var setAtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setAtime); err != nil {
		return nil, fmt.Errorf("read set_atime: %w", err)
	}
	switch setAtime {
	case 0: // DONT_CHANGE
		// attr.Atime remains nil
	case 1: // SET_TO_SERVER_TIME
		t := time.Now()
		attr.Atime = &t
	case 2: // SET_TO_CLIENT_TIME
		var seconds, nseconds uint32
		if err := binary.Read(reader, binary.BigEndian, &seconds); err != nil {
			return nil, fmt.Errorf("read atime seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &nseconds); err != nil {
			return nil, fmt.Errorf("read atime nseconds: %w", err)
		}
		t := timeValToTime(seconds, nseconds)
		attr.Atime = &t
	default:
		return nil, fmt.Errorf("invalid set_atime value: %d", setAtime)
	}

	// Decode Mtime (set_mtime)
	var setMtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setMtime); err != nil {
		return nil, fmt.Errorf("read set_mtime: %w", err)
	}
	switch setMtime {
	case 0: // DONT_CHANGE
		// attr.Mtime remains nil
	case 1: // SET_TO_SERVER_TIME
		t := time.Now()
		attr.Mtime = &t
	case 2: // SET_TO_CLIENT_TIME
		var seconds, nseconds uint32
		if err := binary.Read(reader, binary.BigEndian, &seconds); err != nil {
			return nil, fmt.Errorf("read mtime seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &nseconds); err != nil {
			return nil, fmt.Errorf("read mtime nseconds: %w", err)
		}
		t := timeValToTime(seconds, nseconds)
		attr.Mtime = &t
	default:
		return nil, fmt.Errorf("invalid set_mtime value: %d", setMtime)
	}

	return attr, nil
}
