package nfs

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// NFSStatusToString converts an NFS v3 status code to a human-readable string
// suitable for use as a metric label.
//
// This function maps all NFS v3 error codes defined in RFC 1813 to their
// canonical string representations. Unknown status codes are returned as
// "UNKNOWN_<code>".
//
// Parameters:
//   - status: The NFS v3 status code (uint32)
//
// Returns:
//   - String representation of the status code (e.g., "NFS3_OK", "NFS3ERR_NOENT")
//
// Example:
//
//	status := NFSStatusToString(types.NFS3OK)        // Returns "NFS3_OK"
//	status := NFSStatusToString(types.NFS3ErrNoEnt)  // Returns "NFS3ERR_NOENT"
//	status := NFSStatusToString(999)                 // Returns "UNKNOWN_999"
func NFSStatusToString(status uint32) string {
	switch status {
	case types.NFS3OK:
		return "NFS3_OK"
	case types.NFS3ErrPerm:
		return "NFS3ERR_PERM"
	case types.NFS3ErrNoEnt:
		return "NFS3ERR_NOENT"
	case types.NFS3ErrIO:
		return "NFS3ERR_IO"
	case types.NFS3ErrAccess:
		return "NFS3ERR_ACCES"
	case types.NFS3ErrExist:
		return "NFS3ERR_EXIST"
	case types.NFS3ErrNotDir:
		return "NFS3ERR_NOTDIR"
	case types.NFS3ErrIsDir:
		return "NFS3ERR_ISDIR"
	case types.NFS3ErrInval:
		return "NFS3ERR_INVAL"
	case types.NFS3ErrFBig:
		return "NFS3ERR_FBIG"
	case types.NFS3ErrNoSpc:
		return "NFS3ERR_NOSPC"
	case types.NFS3ErrRofs:
		return "NFS3ERR_ROFS"
	case types.NFS3ErrNameTooLong:
		return "NFS3ERR_NAMETOOLONG"
	case types.NFS3ErrNotEmpty:
		return "NFS3ERR_NOTEMPTY"
	case types.NFS3ErrStale:
		return "NFS3ERR_STALE"
	case types.NFS3ErrBadHandle:
		return "NFS3ERR_BADHANDLE"
	case types.NFS3ErrNotSync:
		return "NFS3ERR_NOT_SYNC"
	case types.NFS3ErrNotSupp:
		return "NFS3ERR_NOTSUPP"
	default:
		return fmt.Sprintf("UNKNOWN_%d", status)
	}
}

// MountStatusToString converts a Mount protocol status code to a human-readable
// string suitable for use as a metric label.
//
// This function maps Mount protocol status codes defined in RFC 1813 Appendix I
// to their canonical string representations. Unknown status codes are returned
// as "MOUNT_UNKNOWN_<code>".
//
// Parameters:
//   - status: The Mount protocol status code (uint32)
//
// Returns:
//   - String representation of the status code (e.g., "MOUNT_OK", "MOUNT_ERR_NOENT")
//
// Example:
//
//	status := MountStatusToString(handlers.MountOK)           // Returns "MOUNT_OK"
//	status := MountStatusToString(handlers.MountErrNoEnt)     // Returns "MOUNT_ERR_NOENT"
//	status := MountStatusToString(999)                        // Returns "MOUNT_UNKNOWN_999"
func MountStatusToString(status uint32) string {
	switch status {
	case handlers.MountOK:
		return "MOUNT_OK"
	case handlers.MountErrPerm:
		return "MOUNT_ERR_PERM"
	case handlers.MountErrNoEnt:
		return "MOUNT_ERR_NOENT"
	case handlers.MountErrIO:
		return "MOUNT_ERR_IO"
	case handlers.MountErrAccess:
		return "MOUNT_ERR_ACCESS"
	case handlers.MountErrNotDir:
		return "MOUNT_ERR_NOTDIR"
	case handlers.MountErrInval:
		return "MOUNT_ERR_INVAL"
	case handlers.MountErrNameTooLong:
		return "MOUNT_ERR_NAMETOOLONG"
	case handlers.MountErrNotSupp:
		return "MOUNT_ERR_NOTSUPP"
	case handlers.MountErrServerFault:
		return "MOUNT_ERR_SERVERFAULT"
	default:
		return fmt.Sprintf("MOUNT_UNKNOWN_%d", status)
	}
}
