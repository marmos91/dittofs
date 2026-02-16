package attrs

import (
	"bytes"
	"context"
	"fmt"

	v4types "github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FATTR4 Attribute Bit Numbers
// ============================================================================
//
// Per RFC 7530 Section 5, attributes are identified by bit numbers within
// the bitmap4 mask. Mandatory attributes (Section 5.1) must be supported.
// Recommended attributes (Section 5.2) should be supported where possible.

// Mandatory attributes (REQUIRED by the protocol)
const (
	FATTR4_SUPPORTED_ATTRS = 0  // bitmap4: attributes supported by server
	FATTR4_TYPE            = 1  // nfs_ftype4: file type (REG, DIR, etc.)
	FATTR4_FH_EXPIRE_TYPE  = 2  // uint32: file handle volatility
	FATTR4_CHANGE          = 3  // changeid4 (uint64): change attribute
	FATTR4_SIZE            = 4  // uint64: file size in bytes
	FATTR4_LINK_SUPPORT    = 5  // bool: hard links supported
	FATTR4_SYMLINK_SUPPORT = 6  // bool: symbolic links supported
	FATTR4_NAMED_ATTR      = 7  // bool: named attributes supported
	FATTR4_FSID            = 8  // fsid4: filesystem identifier
	FATTR4_UNIQUE_HANDLES  = 9  // bool: handles are unique
	FATTR4_LEASE_TIME      = 10 // uint32: lease duration in seconds
	FATTR4_RDATTR_ERROR    = 11 // nfsstat4: per-entry READDIR error
)

// ACL-related attributes (word 0, bits 12-13)
const (
	FATTR4_ACL        = 12 // nfsace4<>: Access Control List
	FATTR4_ACLSUPPORT = 13 // uint32: ACL support flags
)

// Recommended attributes (used for pseudo-fs and real files)
const (
	FATTR4_FILEHANDLE        = 19 // nfs_fh4: the file handle itself
	FATTR4_FILEID            = 20 // uint64: unique file identifier
	FATTR4_MODE              = 33 // uint32: POSIX mode bits
	FATTR4_NUMLINKS          = 35 // uint32: number of hard links
	FATTR4_OWNER             = 36 // utf8str_mixed: owner name
	FATTR4_OWNER_GROUP       = 37 // utf8str_mixed: group owner name
	FATTR4_SPACE_USED        = 45 // uint64: disk space used
	FATTR4_TIME_ACCESS       = 47 // nfstime4: last access time
	FATTR4_TIME_ACCESS_SET   = 48 // settime4: set atime (writable)
	FATTR4_TIME_MODIFY       = 53 // nfstime4: last modify time
	FATTR4_TIME_MODIFY_SET   = 54 // settime4: set mtime (writable)
	FATTR4_MOUNTED_ON_FILEID = 55 // uint64: fileid of mounted-on dir
)

// time_how4 constants for SETATTR timestamp setting (RFC 7530 Section 5.7)
const (
	SET_TO_SERVER_TIME4 = 0 // Server sets the time
	SET_TO_CLIENT_TIME4 = 1 // Client provides the time
)

// DefaultLeaseTime is the default lease duration in seconds.
// 90 seconds is a common default (Linux nfsd uses 90s).
const DefaultLeaseTime = 90

// leaseTimeSeconds holds the configured lease time for FATTR4_LEASE_TIME.
// Updated by SetLeaseTime() from the handler when StateManager is available.
var leaseTimeSeconds uint32 = DefaultLeaseTime

// SetLeaseTime configures the lease time returned by FATTR4_LEASE_TIME.
// Called by the handler layer when StateManager provides the lease duration.
func SetLeaseTime(seconds uint32) {
	leaseTimeSeconds = seconds
}

// GetLeaseTime returns the currently configured lease time in seconds.
func GetLeaseTime() uint32 {
	return leaseTimeSeconds
}

// identityMapper holds the configured identity mapper for FATTR4_OWNER/OWNER_GROUP encoding.
// When nil, the numeric UID/GID format is used as fallback.
var identityMapper identity.IdentityMapper

// SetIdentityMapper configures the identity mapper used for FATTR4_OWNER
// and FATTR4_OWNER_GROUP encoding. When set, UIDs/GIDs are reverse-resolved
// to user@domain/group@domain format. When nil, numeric format is used.
func SetIdentityMapper(mapper identity.IdentityMapper) {
	identityMapper = mapper
}

// GetIdentityMapper returns the currently configured identity mapper, or nil.
func GetIdentityMapper() identity.IdentityMapper {
	return identityMapper
}

// ============================================================================
// PseudoFSAttrSource Interface
// ============================================================================

// PseudoFSAttrSource defines the interface that pseudo-filesystem nodes
// must implement to provide attribute values for encoding.
type PseudoFSAttrSource interface {
	// GetHandle returns the file handle for this node.
	GetHandle() []byte

	// GetFSID returns the filesystem ID (major, minor) for this node.
	GetFSID() (uint64, uint64)

	// GetFileID returns the unique file identifier for this node.
	GetFileID() uint64

	// GetChangeID returns the change attribute value for this node.
	GetChangeID() uint64

	// GetType returns the NFS file type (NF4DIR, NF4REG, etc.).
	GetType() uint32
}

// ============================================================================
// Supported Attributes
// ============================================================================

// SupportedAttrs returns the bitmap of all attributes this server supports.
//
// This includes all mandatory attributes plus the recommended attributes
// needed for pseudo-fs browsing and file operations.
func SupportedAttrs() []uint32 {
	var bitmap []uint32

	// Mandatory attributes (word 0, bits 0-11, 19-20)
	SetBit(&bitmap, FATTR4_SUPPORTED_ATTRS)
	SetBit(&bitmap, FATTR4_TYPE)
	SetBit(&bitmap, FATTR4_FH_EXPIRE_TYPE)
	SetBit(&bitmap, FATTR4_CHANGE)
	SetBit(&bitmap, FATTR4_SIZE)
	SetBit(&bitmap, FATTR4_LINK_SUPPORT)
	SetBit(&bitmap, FATTR4_SYMLINK_SUPPORT)
	SetBit(&bitmap, FATTR4_NAMED_ATTR)
	SetBit(&bitmap, FATTR4_FSID)
	SetBit(&bitmap, FATTR4_UNIQUE_HANDLES)
	SetBit(&bitmap, FATTR4_LEASE_TIME)
	SetBit(&bitmap, FATTR4_RDATTR_ERROR)
	SetBit(&bitmap, FATTR4_ACL)
	SetBit(&bitmap, FATTR4_ACLSUPPORT)
	SetBit(&bitmap, FATTR4_FILEHANDLE)
	SetBit(&bitmap, FATTR4_FILEID)

	// Recommended attributes (word 1, bits 33-55)
	SetBit(&bitmap, FATTR4_MODE)
	SetBit(&bitmap, FATTR4_NUMLINKS)
	SetBit(&bitmap, FATTR4_OWNER)
	SetBit(&bitmap, FATTR4_OWNER_GROUP)
	SetBit(&bitmap, FATTR4_SPACE_USED)
	SetBit(&bitmap, FATTR4_TIME_ACCESS)
	SetBit(&bitmap, FATTR4_TIME_MODIFY)
	SetBit(&bitmap, FATTR4_MOUNTED_ON_FILEID)

	return bitmap
}

// ============================================================================
// Attribute Encoding
// ============================================================================

// EncodePseudoFSAttrs encodes the requested attributes for a pseudo-fs node.
//
// Per RFC 7530 Section 3.3.10 (Attribute Encoding):
// The response contains:
//  1. A bitmap of attributes actually returned (intersection of requested and supported)
//  2. An opaque data block containing the attribute values in bit-number order
//
// Only attributes that are both requested and supported are encoded.
// Attribute values are written in ascending bit-number order within the
// opaque data block.
func EncodePseudoFSAttrs(buf *bytes.Buffer, requested []uint32, node PseudoFSAttrSource) error {
	supported := SupportedAttrs()
	responseBitmap := Intersect(requested, supported)

	// Encode the response bitmap
	if err := EncodeBitmap4(buf, responseBitmap); err != nil {
		return fmt.Errorf("encode response bitmap: %w", err)
	}

	// Build the attribute value data
	var attrData bytes.Buffer

	// Attributes must be encoded in ascending bit-number order.
	// We iterate through all possible bits in the response bitmap.
	maxBits := uint32(len(responseBitmap) * 32)
	for bit := uint32(0); bit < maxBits; bit++ {
		if !IsBitSet(responseBitmap, bit) {
			continue
		}

		if err := encodeSingleAttr(&attrData, bit, node); err != nil {
			return fmt.Errorf("encode attr bit %d: %w", bit, err)
		}
	}

	// Write the attribute data as an opaque block (length-prefixed)
	if err := xdr.WriteXDROpaque(buf, attrData.Bytes()); err != nil {
		return fmt.Errorf("encode attr data: %w", err)
	}

	return nil
}

// encodeSingleAttr encodes a single attribute value into the buffer.
func encodeSingleAttr(buf *bytes.Buffer, bit uint32, node PseudoFSAttrSource) error {
	switch bit {
	case FATTR4_SUPPORTED_ATTRS:
		// Encode the supported attributes bitmap
		return EncodeBitmap4(buf, SupportedAttrs())

	case FATTR4_TYPE:
		// nfs_ftype4 (uint32)
		return xdr.WriteUint32(buf, node.GetType())

	case FATTR4_FH_EXPIRE_TYPE:
		// uint32: FH4_PERSISTENT (handles don't expire)
		return xdr.WriteUint32(buf, v4types.FH4_PERSISTENT)

	case FATTR4_CHANGE:
		// changeid4 (uint64): monotonic change counter
		return xdr.WriteUint64(buf, node.GetChangeID())

	case FATTR4_SIZE:
		// uint64: 0 for directories
		return xdr.WriteUint64(buf, 0)

	case FATTR4_LINK_SUPPORT:
		// bool: true (hard links supported)
		return xdr.WriteUint32(buf, 1)

	case FATTR4_SYMLINK_SUPPORT:
		// bool: true (symbolic links supported)
		return xdr.WriteUint32(buf, 1)

	case FATTR4_NAMED_ATTR:
		// bool: false (named attributes not supported)
		return xdr.WriteUint32(buf, 0)

	case FATTR4_FSID:
		// fsid4: two uint64s (major, minor)
		major, minor := node.GetFSID()
		if err := xdr.WriteUint64(buf, major); err != nil {
			return err
		}
		return xdr.WriteUint64(buf, minor)

	case FATTR4_UNIQUE_HANDLES:
		// bool: true (handles are unique)
		return xdr.WriteUint32(buf, 1)

	case FATTR4_LEASE_TIME:
		// uint32: lease duration in seconds (configured via SetLeaseTime)
		return xdr.WriteUint32(buf, leaseTimeSeconds)

	case FATTR4_RDATTR_ERROR:
		// nfsstat4: NFS4_OK (no error)
		return xdr.WriteUint32(buf, v4types.NFS4_OK)

	case FATTR4_ACL:
		// Pseudo-fs nodes have no ACL: encode 0 ACEs
		return EncodeACLAttr(buf, nil)

	case FATTR4_ACLSUPPORT:
		// Report support for all four ACE types
		return EncodeACLSupportAttr(buf)

	case FATTR4_FILEHANDLE:
		// nfs_fh4: opaque file handle
		return xdr.WriteXDROpaque(buf, node.GetHandle())

	case FATTR4_FILEID:
		// uint64: unique file identifier
		return xdr.WriteUint64(buf, node.GetFileID())

	case FATTR4_MODE:
		// uint32: 0755 for directories
		return xdr.WriteUint32(buf, 0755)

	case FATTR4_NUMLINKS:
		// uint32: 2 for directories (. and ..)
		return xdr.WriteUint32(buf, 2)

	case FATTR4_OWNER:
		// utf8str_mixed: NFSv4 identity format "user@domain"
		return xdr.WriteXDRString(buf, "root@localdomain")

	case FATTR4_OWNER_GROUP:
		// utf8str_mixed: NFSv4 identity format "group@domain"
		return xdr.WriteXDRString(buf, "wheel@localdomain")

	case FATTR4_SPACE_USED:
		// uint64: 0 for pseudo-fs directories
		return xdr.WriteUint64(buf, 0)

	case FATTR4_TIME_ACCESS:
		// nfstime4: {seconds: 0, nseconds: 0} for pseudo-fs
		if err := xdr.WriteUint64(buf, 0); err != nil { // seconds (int64 on wire but uint64 encoding)
			return err
		}
		return xdr.WriteUint32(buf, 0) // nseconds

	case FATTR4_TIME_MODIFY:
		// nfstime4: {seconds: 0, nseconds: 0} for pseudo-fs
		if err := xdr.WriteUint64(buf, 0); err != nil {
			return err
		}
		return xdr.WriteUint32(buf, 0)

	case FATTR4_MOUNTED_ON_FILEID:
		// uint64: same as fileid for pseudo-fs nodes
		return xdr.WriteUint64(buf, node.GetFileID())

	default:
		// Unknown attribute -- should not reach here if Intersect is correct
		return fmt.Errorf("unsupported attribute bit %d", bit)
	}
}

// ============================================================================
// Real File Attribute Encoding
// ============================================================================

// SupportedRealAttrs returns the bitmap of all attributes supported for real files.
//
// This is the same as SupportedAttrs() -- the pseudo-fs and real-FS share the
// same supported attribute set. The function exists for clarity in handler code
// that distinguishes pseudo-fs from real-FS attribute encoding.
func SupportedRealAttrs() []uint32 {
	return SupportedAttrs()
}

// EncodeRealFileAttrs encodes the requested attributes for a real file.
//
// This mirrors EncodePseudoFSAttrs but sources values from a metadata.File
// rather than a PseudoFSAttrSource. Real file attributes include actual
// size, mode, ownership, timestamps, and link count.
//
// Parameters:
//   - buf: Output buffer to write the encoded fattr4 (bitmap + opaque data)
//   - requested: Client-requested attribute bitmap
//   - file: The real file metadata
//   - handle: The file handle (used for FILEHANDLE and FILEID attributes)
func EncodeRealFileAttrs(buf *bytes.Buffer, requested []uint32, file *metadata.File, handle metadata.FileHandle) error {
	supported := SupportedRealAttrs()
	responseBitmap := Intersect(requested, supported)

	// Encode the response bitmap
	if err := EncodeBitmap4(buf, responseBitmap); err != nil {
		return fmt.Errorf("encode response bitmap: %w", err)
	}

	// Build the attribute value data
	var attrData bytes.Buffer

	// Attributes must be encoded in ascending bit-number order.
	maxBits := uint32(len(responseBitmap) * 32)
	for bit := uint32(0); bit < maxBits; bit++ {
		if !IsBitSet(responseBitmap, bit) {
			continue
		}

		if err := encodeRealFileAttr(&attrData, bit, file, handle); err != nil {
			return fmt.Errorf("encode attr bit %d: %w", bit, err)
		}
	}

	// Write the attribute data as an opaque block (length-prefixed)
	if err := xdr.WriteXDROpaque(buf, attrData.Bytes()); err != nil {
		return fmt.Errorf("encode attr data: %w", err)
	}

	return nil
}

// encodeRealFileAttr encodes a single attribute value for a real file.
func encodeRealFileAttr(buf *bytes.Buffer, bit uint32, file *metadata.File, handle metadata.FileHandle) error {
	switch bit {
	case FATTR4_SUPPORTED_ATTRS:
		return EncodeBitmap4(buf, SupportedRealAttrs())

	case FATTR4_TYPE:
		return xdr.WriteUint32(buf, MapFileTypeToNFS4(file.Type))

	case FATTR4_FH_EXPIRE_TYPE:
		return xdr.WriteUint32(buf, v4types.FH4_PERSISTENT)

	case FATTR4_CHANGE:
		// Use ctime as the change attribute (nanoseconds since epoch)
		return xdr.WriteUint64(buf, uint64(file.Ctime.UnixNano()))

	case FATTR4_SIZE:
		return xdr.WriteUint64(buf, file.Size)

	case FATTR4_LINK_SUPPORT:
		return xdr.WriteUint32(buf, 1) // true

	case FATTR4_SYMLINK_SUPPORT:
		return xdr.WriteUint32(buf, 1) // true

	case FATTR4_NAMED_ATTR:
		return xdr.WriteUint32(buf, 0) // false

	case FATTR4_FSID:
		// Use same FSID as pseudo-FS (0, 1) to prevent macOS triggered mounts.
		// macOS creates a separate triggered mount when it detects an FSID change
		// at a junction boundary, which fails for our implementation.
		// Using the same FSID tells the client this is all one filesystem.
		if err := xdr.WriteUint64(buf, 0); err != nil {
			return err
		}
		return xdr.WriteUint64(buf, 1)

	case FATTR4_UNIQUE_HANDLES:
		return xdr.WriteUint32(buf, 1) // true

	case FATTR4_LEASE_TIME:
		// uint32: lease duration in seconds (configured via SetLeaseTime)
		return xdr.WriteUint32(buf, leaseTimeSeconds)

	case FATTR4_RDATTR_ERROR:
		return xdr.WriteUint32(buf, v4types.NFS4_OK)

	case FATTR4_ACL:
		// Encode the file's ACL. If nil, encodes 0 ACEs.
		return EncodeACLAttr(buf, file.ACL)

	case FATTR4_ACLSUPPORT:
		// Report support for all four ACE types
		return EncodeACLSupportAttr(buf)

	case FATTR4_FILEHANDLE:
		return xdr.WriteXDROpaque(buf, []byte(handle))

	case FATTR4_FILEID:
		return xdr.WriteUint64(buf, metadata.HandleToINode(handle))

	case FATTR4_MODE:
		return xdr.WriteUint32(buf, file.Mode&0o7777)

	case FATTR4_NUMLINKS:
		return xdr.WriteUint32(buf, file.Nlink)

	case FATTR4_OWNER:
		// NFSv4 identity format: "user@domain" per RFC 7530 Section 5.9
		// Use identity mapper for reverse resolution if available.
		owner := resolveOwnerString(file.UID)
		return xdr.WriteXDRString(buf, owner)

	case FATTR4_OWNER_GROUP:
		// NFSv4 identity format: "group@domain" per RFC 7530 Section 5.9
		// Use identity mapper for reverse resolution if available.
		group := resolveGroupString(file.GID)
		return xdr.WriteXDRString(buf, group)

	case FATTR4_SPACE_USED:
		return xdr.WriteUint64(buf, file.Size)

	case FATTR4_TIME_ACCESS:
		// nfstime4: seconds (int64) + nseconds (uint32)
		if err := xdr.WriteUint64(buf, uint64(file.Atime.Unix())); err != nil {
			return err
		}
		return xdr.WriteUint32(buf, uint32(file.Atime.Nanosecond()))

	case FATTR4_TIME_MODIFY:
		if err := xdr.WriteUint64(buf, uint64(file.Mtime.Unix())); err != nil {
			return err
		}
		return xdr.WriteUint32(buf, uint32(file.Mtime.Nanosecond()))

	case FATTR4_MOUNTED_ON_FILEID:
		return xdr.WriteUint64(buf, metadata.HandleToINode(handle))

	default:
		return fmt.Errorf("unsupported attribute bit %d", bit)
	}
}

// MapFileTypeToNFS4 maps internal metadata file types to NFSv4 nfs_ftype4 constants.
func MapFileTypeToNFS4(fileType metadata.FileType) uint32 {
	switch fileType {
	case metadata.FileTypeRegular:
		return v4types.NF4REG
	case metadata.FileTypeDirectory:
		return v4types.NF4DIR
	case metadata.FileTypeSymlink:
		return v4types.NF4LNK
	case metadata.FileTypeSocket:
		return v4types.NF4SOCK
	case metadata.FileTypeFIFO:
		return v4types.NF4FIFO
	case metadata.FileTypeBlockDevice:
		return v4types.NF4BLK
	case metadata.FileTypeCharDevice:
		return v4types.NF4CHR
	default:
		return v4types.NF4REG // Default to regular file
	}
}

// ============================================================================
// Identity Mapper Helpers for OWNER/OWNER_GROUP
// ============================================================================

// resolveOwnerString converts a UID to an NFSv4 "user@domain" string.
//
// When an identity mapper is configured, it attempts to reverse-resolve the UID.
// Falls back to numeric format: "uid@localdomain" with well-known overrides
// (UID 0 = "root@localdomain").
func resolveOwnerString(uid uint32) string {
	if identityMapper != nil {
		// Try numeric UID format as the principal for reverse lookup
		principal := fmt.Sprintf("%d@localdomain", uid)
		resolved, err := identityMapper.Resolve(context.Background(), principal)
		if err == nil && resolved != nil && resolved.Found && resolved.Username != "" {
			domain := resolved.Domain
			if domain == "" {
				domain = "localdomain"
			}
			return resolved.Username + "@" + domain
		}
	}

	// Fallback: well-known UIDs and numeric format
	if uid == 0 {
		return "root@localdomain"
	}
	return fmt.Sprintf("%d@localdomain", uid)
}

// resolveGroupString converts a GID to an NFSv4 "group@domain" string.
//
// Falls back to numeric format: "gid@localdomain" with well-known overrides
// (GID 0 = "wheel@localdomain").
func resolveGroupString(gid uint32) string {
	// Note: Identity mapper does not currently support group reverse resolution.
	// This can be extended when GroupResolver implements reverse lookup.
	if gid == 0 {
		return "wheel@localdomain"
	}
	return fmt.Sprintf("%d@localdomain", gid)
}

// shareNameToFSIDMinor generates a consistent minor FSID number from a share name.
// Uses SHA-256 hash of the share name, taking the first 8 bytes as a uint64.
