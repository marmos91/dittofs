package attrs

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	v4types "github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// Writable Attributes
// ============================================================================

// WritableAttrs returns the bitmap of attributes valid for SETATTR.
//
// Per RFC 7530 Section 5.5, these are the attributes that can be set by the
// client via the SETATTR operation.
func WritableAttrs() []uint32 {
	var bitmap []uint32
	SetBit(&bitmap, FATTR4_SIZE)
	SetBit(&bitmap, FATTR4_ACL) // bit 12: ACL is writable via SETATTR
	SetBit(&bitmap, FATTR4_MODE)
	SetBit(&bitmap, FATTR4_OWNER)
	SetBit(&bitmap, FATTR4_OWNER_GROUP)
	SetBit(&bitmap, FATTR4_TIME_ACCESS_SET)
	SetBit(&bitmap, FATTR4_TIME_MODIFY_SET)
	return bitmap
}

// isWritableAttr checks if a given bit is in the set of writable attributes.
func isWritableAttr(bit uint32) bool {
	return IsBitSet(WritableAttrs(), bit)
}

// ============================================================================
// Owner/Group String Parsing
// ============================================================================

// ParseOwnerString parses an NFSv4 "user@domain" owner string to a numeric UID.
//
// Supported formats:
//   - "N@domain" where N is numeric (e.g., "1000@localdomain" -> 1000)
//   - "N" bare numeric (e.g., "0" -> 0)
//   - Well-known names: "root@localdomain" -> 0, "nobody@localdomain" -> 65534
//
// Returns NFS4ERR_BADOWNER-style error for unrecognized names.
func ParseOwnerString(owner string) (uint32, error) {
	// Try "N@domain" format
	if idx := strings.Index(owner, "@"); idx >= 0 {
		name := owner[:idx]

		// Try numeric@domain
		if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
			return uint32(uid), nil
		}

		// Try well-known names
		switch strings.ToLower(name) {
		case "root":
			return 0, nil
		case "nobody":
			return 65534, nil
		}

		return 0, fmt.Errorf("invalid owner string: %s", owner)
	}

	// Try bare numeric
	if uid, err := strconv.ParseUint(owner, 10, 32); err == nil {
		return uint32(uid), nil
	}

	// Try well-known names without domain
	switch strings.ToLower(owner) {
	case "root":
		return 0, nil
	case "nobody":
		return 65534, nil
	}

	return 0, fmt.Errorf("invalid owner string: %s", owner)
}

// ParseGroupString parses an NFSv4 "group@domain" group string to a numeric GID.
//
// Supported formats:
//   - "N@domain" where N is numeric (e.g., "1000@localdomain" -> 1000)
//   - "N" bare numeric (e.g., "0" -> 0)
//   - Well-known names: "root@localdomain" -> 0, "nogroup@localdomain" -> 65534,
//     "nobody@localdomain" -> 65534
//
// Returns NFS4ERR_BADOWNER-style error for unrecognized names.
func ParseGroupString(group string) (uint32, error) {
	// Try "N@domain" format
	if idx := strings.Index(group, "@"); idx >= 0 {
		name := group[:idx]

		// Try numeric@domain
		if gid, err := strconv.ParseUint(name, 10, 32); err == nil {
			return uint32(gid), nil
		}

		// Try well-known names
		switch strings.ToLower(name) {
		case "root", "wheel":
			return 0, nil
		case "nogroup", "nobody":
			return 65534, nil
		}

		return 0, fmt.Errorf("invalid group string: %s", group)
	}

	// Try bare numeric
	if gid, err := strconv.ParseUint(group, 10, 32); err == nil {
		return uint32(gid), nil
	}

	// Try well-known names without domain
	switch strings.ToLower(group) {
	case "root", "wheel":
		return 0, nil
	case "nogroup", "nobody":
		return 65534, nil
	}

	return 0, fmt.Errorf("invalid group string: %s", group)
}

// ============================================================================
// fattr4 Decode for SETATTR
// ============================================================================

// DecodeFattr4ToSetAttrs decodes an fattr4 structure from an io.Reader into
// a metadata.SetAttrs and the bitmap of requested attributes.
//
// Per RFC 7530, fattr4 is:
//
//	struct fattr4 {
//	    bitmap4     attrmask;
//	    opaque      attr_vals<>;
//	};
//
// The attr_vals opaque contains the attribute values in ascending bit-number
// order within the attrmask bitmap.
//
// Returns:
//   - *metadata.SetAttrs: The decoded attribute values
//   - []uint32: The bitmap of requested attributes (for attrsset response)
//   - error: On decode failure or unsupported attribute
func DecodeFattr4ToSetAttrs(reader io.Reader) (*metadata.SetAttrs, []uint32, error) {
	// 1. Decode bitmap4
	bitmap, err := DecodeBitmap4(reader)
	if err != nil {
		return nil, nil, fmt.Errorf("decode fattr4 bitmap: %w", err)
	}

	// 2. Read attr_vals as opaque data
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, nil, fmt.Errorf("decode fattr4 attr_vals: %w", err)
	}

	// 3. Create a reader for the attr_vals
	attrReader := bytes.NewReader(attrData)

	// 4. Iterate through bits in ascending order
	setAttrs := &metadata.SetAttrs{}
	maxBits := uint32(len(bitmap) * 32)

	for bit := uint32(0); bit < maxBits; bit++ {
		if !IsBitSet(bitmap, bit) {
			continue
		}

		// Check if this attribute is writable
		if !isWritableAttr(bit) {
			return nil, nil, &attrNotSuppError{bit: bit}
		}

		if err := decodeSingleSetAttr(attrReader, bit, setAttrs); err != nil {
			return nil, nil, err
		}
	}

	return setAttrs, bitmap, nil
}

// decodeSingleSetAttr decodes a single attribute value from the reader into setAttrs.
func decodeSingleSetAttr(reader io.Reader, bit uint32, setAttrs *metadata.SetAttrs) error {
	switch bit {
	case FATTR4_SIZE:
		// uint64: file size
		size, err := xdr.DecodeUint64(reader)
		if err != nil {
			return fmt.Errorf("decode FATTR4_SIZE: %w", err)
		}
		setAttrs.Size = &size

	case FATTR4_ACL:
		// nfsace4<>: ACL
		decodedACL, err := DecodeACLAttr(reader)
		if err != nil {
			return &badXDRError{msg: fmt.Sprintf("invalid ACL encoding: %v", err)}
		}
		// Validate the decoded ACL (canonical ordering, max ACEs, etc.)
		if err := acl.ValidateACL(decodedACL); err != nil {
			return &invalidACLError{msg: err.Error()}
		}
		setAttrs.ACL = decodedACL

	case FATTR4_MODE:
		// uint32: POSIX mode bits
		mode, err := xdr.DecodeUint32(reader)
		if err != nil {
			return fmt.Errorf("decode FATTR4_MODE: %w", err)
		}
		if mode > 0o7777 {
			return &invalidModeError{mode: mode}
		}
		setAttrs.Mode = &mode

	case FATTR4_OWNER:
		// utf8str_mixed: owner name
		ownerStr, err := xdr.DecodeString(reader)
		if err != nil {
			return fmt.Errorf("decode FATTR4_OWNER: %w", err)
		}
		uid, err := ParseOwnerString(ownerStr)
		if err != nil {
			return &badOwnerError{owner: ownerStr}
		}
		setAttrs.UID = &uid

	case FATTR4_OWNER_GROUP:
		// utf8str_mixed: group name
		groupStr, err := xdr.DecodeString(reader)
		if err != nil {
			return fmt.Errorf("decode FATTR4_OWNER_GROUP: %w", err)
		}
		gid, err := ParseGroupString(groupStr)
		if err != nil {
			return &badOwnerError{owner: groupStr}
		}
		setAttrs.GID = &gid

	case FATTR4_TIME_ACCESS_SET:
		// settime4: time_how4 + optional nfstime4
		how, err := xdr.DecodeUint32(reader)
		if err != nil {
			return fmt.Errorf("decode FATTR4_TIME_ACCESS_SET how: %w", err)
		}
		switch how {
		case SET_TO_SERVER_TIME4:
			t := time.Now()
			setAttrs.Atime = &t
			setAttrs.AtimeNow = true
		case SET_TO_CLIENT_TIME4:
			t, err := decodeNFSTime4(reader)
			if err != nil {
				return fmt.Errorf("decode FATTR4_TIME_ACCESS_SET time: %w", err)
			}
			setAttrs.Atime = &t
		default:
			return fmt.Errorf("invalid time_how4 for atime: %d", how)
		}

	case FATTR4_TIME_MODIFY_SET:
		// settime4: time_how4 + optional nfstime4
		how, err := xdr.DecodeUint32(reader)
		if err != nil {
			return fmt.Errorf("decode FATTR4_TIME_MODIFY_SET how: %w", err)
		}
		switch how {
		case SET_TO_SERVER_TIME4:
			t := time.Now()
			setAttrs.Mtime = &t
			setAttrs.MtimeNow = true
		case SET_TO_CLIENT_TIME4:
			t, err := decodeNFSTime4(reader)
			if err != nil {
				return fmt.Errorf("decode FATTR4_TIME_MODIFY_SET time: %w", err)
			}
			setAttrs.Mtime = &t
		default:
			return fmt.Errorf("invalid time_how4 for mtime: %d", how)
		}

	default:
		return &attrNotSuppError{bit: bit}
	}

	return nil
}

// decodeNFSTime4 reads an nfstime4 structure (int64 seconds + uint32 nseconds).
func decodeNFSTime4(reader io.Reader) (time.Time, error) {
	seconds, err := xdr.DecodeUint64(reader)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode nfstime4 seconds: %w", err)
	}
	nseconds, err := xdr.DecodeUint32(reader)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode nfstime4 nseconds: %w", err)
	}
	return time.Unix(int64(seconds), int64(nseconds)), nil
}

// ============================================================================
// Decode Error Types
// ============================================================================

// attrNotSuppError represents an NFS4ERR_ATTRNOTSUPP condition.
type attrNotSuppError struct {
	bit uint32
}

func (e *attrNotSuppError) Error() string {
	return fmt.Sprintf("attribute bit %d not supported for SETATTR", e.bit)
}

// NFS4Status returns the NFS4 error code for this error.
func (e *attrNotSuppError) NFS4Status() uint32 {
	return v4types.NFS4ERR_ATTRNOTSUPP
}

// invalidModeError represents an NFS4ERR_INVAL condition for mode values.
type invalidModeError struct {
	mode uint32
}

func (e *invalidModeError) Error() string {
	return fmt.Sprintf("invalid mode 0%o (must be <= 07777)", e.mode)
}

// NFS4Status returns the NFS4 error code for this error.
func (e *invalidModeError) NFS4Status() uint32 {
	return v4types.NFS4ERR_INVAL
}

// badOwnerError represents an NFS4ERR_BADOWNER condition.
type badOwnerError struct {
	owner string
}

func (e *badOwnerError) Error() string {
	return fmt.Sprintf("bad owner/group string: %s", e.owner)
}

// NFS4Status returns the NFS4 error code for this error.
func (e *badOwnerError) NFS4Status() uint32 {
	return v4types.NFS4ERR_BADOWNER
}

// badXDRError represents an NFS4ERR_BADXDR condition.
type badXDRError struct {
	msg string
}

func (e *badXDRError) Error() string {
	return e.msg
}

// NFS4Status returns the NFS4 error code for this error.
func (e *badXDRError) NFS4Status() uint32 {
	return v4types.NFS4ERR_BADXDR
}

// invalidACLError represents an NFS4ERR_INVAL condition for invalid ACLs.
type invalidACLError struct {
	msg string
}

func (e *invalidACLError) Error() string {
	return fmt.Sprintf("invalid ACL: %s", e.msg)
}

// NFS4Status returns the NFS4 error code for this error.
func (e *invalidACLError) NFS4Status() uint32 {
	return v4types.NFS4ERR_INVAL
}

// NFS4StatusError is an interface for errors that carry an NFS4 status code.
type NFS4StatusError interface {
	error
	NFS4Status() uint32
}
