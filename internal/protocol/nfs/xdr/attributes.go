package xdr

import (
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
)

// MetadataToNFS converts internal file metadata to NFS fattr3 format.
//
// Per RFC 1813 Section 2.3.1 (fattr3):
// The fattr3 structure contains the attributes of a file; "attributes"
// refers to the metadata associated with the file as opposed to file data.
//
// This conversion:
// - Maps internal file types to NFS type constants (NF3REG, NF3DIR, etc.)
// - Converts Unix-style permissions and ownership
// - Translates timestamps to NFS nfstime3 format (seconds + nanoseconds)
// - Sets Nlink to 1 (simplified; production may track actual hard links)
// - Uses provided fileid as the unique file identifier
//
// Parameters:
//   - mdAttr: Internal file attributes from metadata layer
//   - fileid: Unique file identifier (extracted from file handle)
//
// Returns:
//   - *types.NFSFileAttr: NFS protocol file attributes ready for encoding
//
// Note: Fsid is set to 0 (single filesystem); Rdev is zeroed (not used for
// regular files/directories). Production implementations may extend these.
func MetadataToNFS(mdAttr *metadata.FileAttr, fileid uint64) *types.NFSFileAttr {
	if mdAttr == nil {
		// Defensive: should not happen, but return safe defaults
		return &types.NFSFileAttr{
			Type:   uint32(metadata.FileTypeRegular),
			Mode:   0644,
			Nlink:  1,
			UID:    0,
			GID:    0,
			Size:   0,
			Used:   0,
			Rdev:   types.SpecData{Major: 0, Minor: 0},
			Fsid:   0,
			Fileid: fileid,
			Atime:  types.TimeVal{Seconds: 0, Nseconds: 0},
			Mtime:  types.TimeVal{Seconds: 0, Nseconds: 0},
			Ctime:  types.TimeVal{Seconds: 0, Nseconds: 0},
		}
	}

	return &types.NFSFileAttr{
		Type:  uint32(mdAttr.Type),
		Mode:  mdAttr.Mode,
		Nlink: 1, // Simplified: real implementation should track hard links
		UID:   mdAttr.UID,
		GID:   mdAttr.GID,
		Size:  mdAttr.Size,
		Used:  mdAttr.Size, // Used space equals size (no sparse files)
		Rdev: types.SpecData{
			Major: 0, // Device major number (0 for regular files)
			Minor: 0, // Device minor number (0 for regular files)
		},
		Fsid:   0,      // Filesystem ID (0 = single filesystem)
		Fileid: fileid, // Unique file identifier
		Atime: types.TimeVal{
			Seconds:  uint32(mdAttr.Atime.Unix()),
			Nseconds: uint32(mdAttr.Atime.Nanosecond()),
		},
		Mtime: types.TimeVal{
			Seconds:  uint32(mdAttr.Mtime.Unix()),
			Nseconds: uint32(mdAttr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(mdAttr.Ctime.Unix()),
			Nseconds: uint32(mdAttr.Ctime.Nanosecond()),
		},
	}
}

// convertSetAttrsToMetadata converts client-requested attributes to internal format.
//
// Per RFC 1813 Section 2.5.3 (sattr3):
// The sattr3 structure contains the file attributes which can be set
// from the client. Fields are marked as set by a boolean flag for each.
//
// This conversion:
// - Applies client-requested mode, UID, GID if provided
// - Falls back to authentication context for ownership defaults
// - Sets type-appropriate default permissions if not specified
// - Prepares partial attributes for repository completion
//
// Default permissions by file type:
//   - Directories: 0755 (rwxr-xr-x) - owner can read/write/execute, others can read/execute
//   - Regular files: 0644 (rw-r--r--) - owner can read/write, others can read
//   - Symlinks: 0777 (rwxrwxrwx) - permissions don't apply to symlinks
//   - Special files: 0644 (rw-r--r--) - conservative default
//
// Parameters:
//   - fileType: Type of file being created (Directory, Regular, Symlink, etc.)
//   - setAttrs: Client-requested attributes (may be nil or partial)
//   - authCtx: Authentication context for default ownership
//
// Returns:
//   - *metadata.FileAttr: Partial attributes with type, mode, uid, gid set
//     Repository will complete with timestamps, size, and ContentID
func ConvertSetAttrsToMetadata(fileType metadata.FileType, setAttrs *metadata.SetAttrs, authCtx *metadata.AuthContext) *metadata.FileAttr {
	if authCtx == nil {
		// Defensive: should not happen in production
		authCtx = &metadata.AuthContext{
			UID: ptrUint32(0),
			GID: ptrUint32(0),
		}
	}

	attr := &metadata.FileAttr{
		Type: fileType,
	}

	// Apply mode with type-appropriate defaults
	if setAttrs != nil && setAttrs.SetMode {
		attr.Mode = setAttrs.Mode
	} else {
		// Set default mode based on file type per Unix conventions
		switch fileType {
		case metadata.FileTypeDirectory:
			attr.Mode = 0755 // rwxr-xr-x
		case metadata.FileTypeRegular:
			attr.Mode = 0644 // rw-r--r--
		case metadata.FileTypeSymlink:
			attr.Mode = 0777 // rwxrwxrwx (symlink perms are typically ignored)
		case metadata.FileTypeChar, metadata.FileTypeBlock:
			attr.Mode = 0644 // rw-r--r-- (device files)
		case metadata.FileTypeSocket, metadata.FileTypeFifo:
			attr.Mode = 0644 // rw-r--r-- (IPC files)
		default:
			attr.Mode = 0644 // Safe default
		}
	}

	// Apply UID with fallback to authenticated user
	if setAttrs != nil && setAttrs.SetUID {
		attr.UID = setAttrs.UID
	} else if authCtx.UID != nil {
		attr.UID = *authCtx.UID
	} else {
		attr.UID = 0 // Fallback: root
	}

	// Apply GID with fallback to authenticated group
	if setAttrs != nil && setAttrs.SetGID {
		attr.GID = setAttrs.GID
	} else if authCtx.GID != nil {
		attr.GID = *authCtx.GID
	} else {
		attr.GID = 0 // Fallback: root
	}

	// Note: Size, timestamps, and ContentID are set by the repository
	// The repository will populate:
	// - Size: based on actual content
	// - Atime, Mtime, Ctime: current time or from setAttrs
	// - ContentID: generated based on implementation

	return attr
}

// applySetAttrs applies client-requested attribute changes to existing metadata.
//
// Per RFC 1813 Section 3.3.2 (SETATTR):
// The sattr3 structure contains attributes which can be set from the client.
// The fields are discriminated unions allowing the client to specify which
// attributes to set and which to leave unchanged.
//
// This function only modifies attributes that are explicitly marked as "set"
// in the request, leaving all other attributes unchanged. This is critical
// for preserving file metadata integrity.
//
// Parameters:
//   - fileAttr: Current file attributes to modify in-place
//   - setAttrs: Client-requested changes (nil-safe, only set fields are applied)
//
// Modified in place: fileAttr is updated directly
func ApplySetAttrs(fileAttr *metadata.FileAttr, setAttrs *metadata.SetAttrs) {
	if fileAttr == nil || setAttrs == nil {
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
// Weak Cache Consistency (WCC) Support
// ============================================================================

// CaptureWccAttr captures pre-operation attributes for WCC data.
//
// Per RFC 1813 Section 1.4.7 (Weak Cache Consistency):
// WCC provides clients with before and after attributes for operations that
// modify file data or metadata. Clients use this to detect if their cached
// data is still valid.
//
// The pre_op_attr (wcc_attr) contains a subset of attributes (size, mtime, ctime)
// that are sufficient for cache validation without the overhead of full fattr3.
//
// This should be captured BEFORE the operation that modifies the file.
//
// Parameters:
//   - attr: Current file attributes (before modification)
//
// Returns:
//   - *types.WccAttr: Pre-operation attributes or nil if attr is nil
func CaptureWccAttr(attr *metadata.FileAttr) *types.WccAttr {
	if attr == nil {
		return nil
	}

	return &types.WccAttr{
		Size: attr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}
}
