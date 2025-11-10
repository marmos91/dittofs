package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// Helper to convert metadata.FileAttr to NFS FileAttr
func MetadataToNFSAttr(mdAttr *metadata.FileAttr, fileid uint64) *FileAttr {
	return &FileAttr{
		Type:  uint32(mdAttr.Type),
		Mode:  mdAttr.Mode,
		Nlink: 1,
		UID:   mdAttr.UID,
		GID:   mdAttr.GID,
		Size:  mdAttr.Size,
		Used:  mdAttr.Size,
		Rdev: SpecData{
			Major: 0,
			Minor: 0,
		},
		Fsid:   0,
		Fileid: fileid,
		Atime: TimeVal{
			Seconds:  uint32(mdAttr.Atime.Unix()),
			Nseconds: uint32(mdAttr.Atime.Nanosecond()),
		},
		Mtime: TimeVal{
			Seconds:  uint32(mdAttr.Mtime.Unix()),
			Nseconds: uint32(mdAttr.Mtime.Nanosecond()),
		},
		Ctime: TimeVal{
			Seconds:  uint32(mdAttr.Ctime.Unix()),
			Nseconds: uint32(mdAttr.Ctime.Nanosecond()),
		},
	}
}

// captureWccAttr captures weak cache consistency attributes from file metadata.
// This is used to provide before/after snapshots for NFS clients to detect changes.
func captureWccAttr(attr *metadata.FileAttr) *WccAttr {
	if attr == nil {
		return nil
	}

	return &WccAttr{
		Size: attr.Size,
		Mtime: TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}
}

// extractFileID extracts a file ID from a file handle.
// Uses the first 8 bytes of the handle as the file ID.
func extractFileID(handle metadata.FileHandle) uint64 {
	if len(handle) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(handle[:8])
}

// applySetAttrs applies set attributes to file metadata.
// Only applies attributes that are explicitly set in the request.
func applySetAttrs(fileAttr *metadata.FileAttr, setAttrs *metadata.SetAttrs) {
	if setAttrs == nil {
		return
	}

	if setAttrs.SetMode {
		fileAttr.Mode = setAttrs.Mode
	}

	if setAttrs.SetUID {
		fileAttr.UID = setAttrs.UID
	}

	if setAttrs.SetGID {
		fileAttr.GID = setAttrs.GID
	}

	if setAttrs.SetSize {
		fileAttr.Size = setAttrs.Size
	}

	if setAttrs.SetAtime {
		fileAttr.Atime = setAttrs.Atime
	}

	if setAttrs.SetMtime {
		fileAttr.Mtime = setAttrs.Mtime
	}
}

// ============================================================================
// XDR Decoding Helpers
// ============================================================================

// decodeOpaque decodes XDR opaque data (variable-length byte array).
// Format: [length:uint32][data:bytes][padding:0-3 bytes]
func decodeOpaque(reader io.Reader) ([]byte, error) {
	// Read length
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}

	// Read data
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}

	// Skip padding (align to 4-byte boundary)
	padding := (4 - (length % 4)) % 4
	if padding > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(padding)); err != nil {
			return nil, fmt.Errorf("skip padding: %w", err)
		}
	}

	return data, nil
}

// decodeString decodes XDR string (same as opaque but returns string).
// Format: [length:uint32][data:bytes][padding:0-3 bytes]
func decodeString(reader io.Reader) (string, error) {
	data, err := decodeOpaque(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// decodeSetAttrs decodes NFS sattr3 (set attributes) structure.
// Format per RFC 1813 Section 2.5.3:
//
//	struct sattr3 {
//	    set_mode3   mode;
//	    set_uid3    uid;
//	    set_gid3    gid;
//	    set_size3   size;
//	    set_atime   atime;
//	    set_mtime   mtime;
//	};
func decodeSetAttrs(reader io.Reader) (*metadata.SetAttrs, error) {
	attr := &metadata.SetAttrs{}

	// Decode mode
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

	// Decode UID
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

	// Decode GID
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

	// Decode Size
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

	// Decode Atime
	var setAtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setAtime); err != nil {
		return nil, fmt.Errorf("read set_atime: %w", err)
	}
	switch setAtime {
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
	}

	// Decode Mtime
	var setMtime uint32
	if err := binary.Read(reader, binary.BigEndian, &setMtime); err != nil {
		return nil, fmt.Errorf("read set_mtime: %w", err)
	}
	switch setMtime {
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
	}

	return attr, nil
}

// ============================================================================
// XDR Encoding Helpers
// ============================================================================

// encodeOptionalOpaque encodes optional XDR opaque data.
// Format: [present:uint32][length:uint32][data:bytes][padding:0-3 bytes]
// If data is nil or empty, only writes present=0.
func encodeOptionalOpaque(buf *bytes.Buffer, data []byte) error {
	if len(data) == 0 {
		// Not present
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

	// Padding
	padding := (4 - (length % 4)) % 4
	for range padding {
		if err := buf.WriteByte(0); err != nil {
			return fmt.Errorf("write padding: %w", err)
		}
	}

	return nil
}

// encodeOptionalFileAttr encodes optional NFS file attributes.
// Format: [present:uint32][fattr3]
func encodeOptionalFileAttr(buf *bytes.Buffer, attr *FileAttr) error {
	if attr == nil {
		// Not present
		return binary.Write(buf, binary.BigEndian, uint32(0))
	}

	// Present flag
	if err := binary.Write(buf, binary.BigEndian, uint32(1)); err != nil {
		return fmt.Errorf("write present flag: %w", err)
	}

	// Encode file attributes
	return encodeFileAttr(buf, attr)
}

// encodeWccData encodes weak cache consistency data.
// Format per RFC 1813 Section 2.6:
//
//	struct wcc_data {
//	    pre_op_attr   before;
//	    post_op_attr  after;
//	};
func encodeWccData(buf *bytes.Buffer, before *WccAttr, after *FileAttr) error {
	// Encode pre-op attributes
	if before != nil {
		if err := binary.Write(buf, binary.BigEndian, uint32(1)); err != nil {
			return fmt.Errorf("write before present: %w", err)
		}
		if err := encodeWccAttr(buf, before); err != nil {
			return fmt.Errorf("encode before attributes: %w", err)
		}
	} else {
		if err := binary.Write(buf, binary.BigEndian, uint32(0)); err != nil {
			return fmt.Errorf("write before not present: %w", err)
		}
	}

	// Encode post-op attributes
	if err := encodeOptionalFileAttr(buf, after); err != nil {
		return fmt.Errorf("encode after attributes: %w", err)
	}

	return nil
}

// encodeWccAttr encodes pre-operation WCC attributes.
// Format: [size:uint64][mtime:nfstime3][ctime:nfstime3]
func encodeWccAttr(buf *bytes.Buffer, attr *WccAttr) error {
	if err := binary.Write(buf, binary.BigEndian, attr.Size); err != nil {
		return fmt.Errorf("write size: %w", err)
	}

	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Seconds); err != nil {
		return fmt.Errorf("write mtime seconds: %w", err)
	}

	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Nseconds); err != nil {
		return fmt.Errorf("write mtime nseconds: %w", err)
	}

	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Seconds); err != nil {
		return fmt.Errorf("write ctime seconds: %w", err)
	}

	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Nseconds); err != nil {
		return fmt.Errorf("write ctime nseconds: %w", err)
	}

	return nil
}

// ============================================================================
// Time Conversion Helpers
// ============================================================================

// timeValToTime converts NFS time to Go time.Time.
func timeValToTime(seconds, nseconds uint32) time.Time {
	return time.Unix(int64(seconds), int64(nseconds))
}

// getCurrentTime returns the current time.
// Separated into a function for easier testing/mocking.
func getCurrentTime() time.Time {
	return time.Now()
}

func timeToTimeVal(t time.Time) TimeVal {
	return TimeVal{
		Seconds:  uint32(t.Unix()),
		Nseconds: uint32(t.Nanosecond()),
	}
}

func encodeFileAttr(buf *bytes.Buffer, attr *FileAttr) error {
	if err := binary.Write(buf, binary.BigEndian, attr.Type); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mode); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Nlink); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.UID); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.GID); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Size); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Used); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Rdev); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Fsid); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Fileid); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Atime.Seconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Atime.Nseconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Seconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Nseconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Seconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Nseconds); err != nil {
		return err
	}
	return nil
}

// ============================================================================
// Attribute Conversion Helpers
// ============================================================================

// convertSetAttrsToMetadata converts SetAttrs to metadata.FileAttr format
// for the repository, applying authentication context defaults.
//
// This helper prepares the attributes structure that the repository expects,
// ensuring all required fields are populated with sensible defaults.
//
// Parameters:
//   - fileType: The type of file being created (Directory, Regular, Char, Block, etc.)
//   - setAttrs: Client-requested attributes (may be partial)
//   - authCtx: Authentication context for default ownership
//
// Returns:
//   - *metadata.FileAttr: Partial attributes for repository (type, mode, uid, gid)
//     The repository will complete the attributes with timestamps and other fields.
func convertSetAttrsToMetadata(fileType metadata.FileType, setAttrs *metadata.SetAttrs, authCtx *metadata.AuthContext) *metadata.FileAttr {
	attr := &metadata.FileAttr{
		Type: fileType,
	}

	// Apply mode (with sensible defaults based on file type)
	if setAttrs.SetMode {
		attr.Mode = setAttrs.Mode
	} else {
		// Set default mode based on file type
		switch fileType {
		case metadata.FileTypeDirectory:
			attr.Mode = 0755 // Default: rwxr-xr-x
		case metadata.FileTypeRegular:
			attr.Mode = 0644 // Default: rw-r--r--
		case metadata.FileTypeChar, metadata.FileTypeBlock:
			attr.Mode = 0644 // Default: rw-r--r--
		case metadata.FileTypeSocket, metadata.FileTypeFifo:
			attr.Mode = 0644 // Default: rw-r--r--
		case metadata.FileTypeSymlink:
			attr.Mode = 0777 // Default: rwxrwxrwx (symlinks typically have 777)
		default:
			attr.Mode = 0644 // Safe default
		}
	}

	// Apply UID (default: authenticated user or 0)
	if setAttrs.SetUID {
		attr.UID = setAttrs.UID
	} else if authCtx.UID != nil {
		attr.UID = *authCtx.UID
	} else {
		attr.UID = 0 // Default: root
	}

	// Apply GID (default: authenticated group or 0)
	if setAttrs.SetGID {
		attr.GID = setAttrs.GID
	} else if authCtx.GID != nil {
		attr.GID = *authCtx.GID
	} else {
		attr.GID = 0 // Default: root
	}

	// Note: Size, timestamps, and ContentID are set by the repository
	// based on implementation requirements

	return attr
}

// extractClientIP extracts the IP address from a client address string.
// Format: "IP:port" → "IP"
func extractClientIP(clientAddr string) string {
	ip, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		// If parsing fails, return the original address
		return clientAddr
	}
	return ip
}

func mapRepositoryErrorToNFSStatus(err error, clientIP string, operation string) uint32 {
	if err == nil {
		return NFS3OK
	}

	// Check if it's an ExportError
	if exportErr, ok := err.(*metadata.ExportError); ok {
		switch exportErr.Code {
		case metadata.ExportErrNotFound:
			logger.Warn("%s failed: %s client=%s", operation, exportErr.Message, clientIP)
			return NFS3ErrNoEnt

		case metadata.ExportErrAccessDenied:
			logger.Warn("%s failed: %s client=%s", operation, exportErr.Message, clientIP)
			return NFS3ErrAcces

		case metadata.ExportErrServerFault:
			// Check specific messages for more precise status codes
			if exportErr.Message == "parent is not a directory" {
				logger.Warn("%s failed: parent not a directory client=%s", operation, clientIP)
				return NFS3ErrNotDir
			}
			if exportErr.Message == "cannot remove directory with REMOVE (use RMDIR)" {
				logger.Warn("%s failed: attempted to remove directory client=%s", operation, clientIP)
				return NFS3ErrIsDir
			}
			logger.Error("%s failed: %s client=%s", operation, exportErr.Message, clientIP)
			return NFS3ErrIO

		default:
			logger.Error("%s failed: unknown export error: %s client=%s", operation, exportErr.Message, clientIP)
			return NFS3ErrIO
		}
	}

	// Generic error
	logger.Error("%s failed: %v client=%s", operation, err, clientIP)
	return NFS3ErrIO
}
