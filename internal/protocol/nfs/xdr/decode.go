package xdr

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/metadata"
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

	// Skip padding to 4-byte boundary
	// Example: length=5 → padding=3, length=8 → padding=0
	padding := (4 - (length % 4)) % 4
	if padding > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(padding)); err != nil {
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

// xdr.DecodeSetAttrs decodes NFS sattr3 (set attributes) structure from XDR.
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
//   - set=0 (DONT_CHANGE): field not set, skip value
//   - set=1 (SET_TO_CLIENT_TIME): field is set, read value (for time: read nfstime3)
//   - set=2 (SET_TO_SERVER_TIME): field is set to server time (for time fields only)
//
// Parameters:
//   - reader: Input stream positioned at start of sattr3
//
// Returns:
//   - *metadata.SetAttrs: Decoded attributes with flags indicating which fields are set
//   - error: Decoding error
func DecodeSetAttrs(reader io.Reader) (*metadata.SetAttrs, error) {
	attr := &metadata.SetAttrs{}

	// Decode mode (set_mode3)
	var setMode uint32
	if err := binary.Read(reader, binary.BigEndian, &setMode); err != nil {
		return nil, fmt.Errorf("read set_mode: %w", err)
	}
	attr.SetMode = (setMode == 1)
	if attr.SetMode {
		if err := binary.Read(reader, binary.BigEndian, &attr.Mode); err != nil {
			return nil, fmt.Errorf("read mode: %w", err)
		}
	}

	// Decode UID (set_uid3)
	var setUID uint32
	if err := binary.Read(reader, binary.BigEndian, &setUID); err != nil {
		return nil, fmt.Errorf("read set_uid: %w", err)
	}
	attr.SetUID = (setUID == 1)
	if attr.SetUID {
		if err := binary.Read(reader, binary.BigEndian, &attr.UID); err != nil {
			return nil, fmt.Errorf("read uid: %w", err)
		}
	}

	// Decode GID (set_gid3)
	var setGID uint32
	if err := binary.Read(reader, binary.BigEndian, &setGID); err != nil {
		return nil, fmt.Errorf("read set_gid: %w", err)
	}
	attr.SetGID = (setGID == 1)
	if attr.SetGID {
		if err := binary.Read(reader, binary.BigEndian, &attr.GID); err != nil {
			return nil, fmt.Errorf("read gid: %w", err)
		}
	}

	// Decode Size (set_size3)
	var setSize uint32
	if err := binary.Read(reader, binary.BigEndian, &setSize); err != nil {
		return nil, fmt.Errorf("read set_size: %w", err)
	}
	attr.SetSize = (setSize == 1)
	if attr.SetSize {
		if err := binary.Read(reader, binary.BigEndian, &attr.Size); err != nil {
			return nil, fmt.Errorf("read size: %w", err)
		}
	}

	// Decode Atime (set_atime)
	var setAtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setAtime); err != nil {
		return nil, fmt.Errorf("read set_atime: %w", err)
	}
	switch setAtime {
	case 0: // DONT_CHANGE
		attr.SetAtime = false
	case 1: // SET_TO_CLIENT_TIME
		attr.SetAtime = true
		var seconds, nseconds uint32
		if err := binary.Read(reader, binary.BigEndian, &seconds); err != nil {
			return nil, fmt.Errorf("read atime seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &nseconds); err != nil {
			return nil, fmt.Errorf("read atime nseconds: %w", err)
		}
		attr.Atime = timeValToTime(seconds, nseconds)
	case 2: // SET_TO_SERVER_TIME
		attr.SetAtime = true
		attr.Atime = getCurrentTime()
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
		attr.SetMtime = false
	case 1: // SET_TO_CLIENT_TIME
		attr.SetMtime = true
		var seconds, nseconds uint32
		if err := binary.Read(reader, binary.BigEndian, &seconds); err != nil {
			return nil, fmt.Errorf("read mtime seconds: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &nseconds); err != nil {
			return nil, fmt.Errorf("read mtime nseconds: %w", err)
		}
		attr.Mtime = timeValToTime(seconds, nseconds)
	case 2: // SET_TO_SERVER_TIME
		attr.SetMtime = true
		attr.Mtime = getCurrentTime()
	default:
		return nil, fmt.Errorf("invalid set_mtime value: %d", setMtime)
	}

	return attr, nil
}
