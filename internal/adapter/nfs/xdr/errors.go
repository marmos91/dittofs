package xdr

import (
	"errors"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Error Mapping - store Errors → NFS Status Codes
// ============================================================================

// MapStoreErrorToNFSStatus maps store errors to NFS status codes.
//
// Per RFC 1813 Section 2.2 (nfsstat3):
// NFS procedures return status codes indicating success or specific failure
// conditions. Translates internal store errors into the
// appropriate NFS status codes for client consumption.
//
// Error Mapping:
//   - ErrNotFound → types.NFS3ErrNoEnt (ENOENT: No such file or directory)
//   - ErrAccessDenied → types.NFS3ErrAcces (EACCES: Permission denied)
//   - ErrPermissionDenied → types.NFS3ErrPerm (EPERM: Operation not permitted)
//   - ErrNotDirectory → types.NFS3ErrNotDir (ENOTDIR: Not a directory)
//   - ErrIsDirectory → types.NFS3ErrIsDir (EISDIR: Is a directory)
//   - ErrAlreadyExists → types.NFS3ErrExist (EEXIST: File exists)
//   - ErrNotEmpty → types.NFS3ErrNotEmpty (ENOTEMPTY: Directory not empty)
//   - ErrNoSpace → types.NFS3ErrNoSpc (ENOSPC: No space left on device)
//   - ErrReadOnly → types.NFS3ErrRofs (EROFS: Read-only filesystem)
//   - ErrStaleHandle → types.NFS3ErrStale (ESTALE: Stale file handle)
//   - ErrInvalidHandle → types.NFS3ErrBadHandle (EBADHANDLE: Illegal NFS file handle)
//   - ErrNotSupported → types.NFS3ErrNotSupp (ENOTSUP: Operation not supported)
//   - ErrIOError → types.NFS3ErrIO (EIO: I/O error)
//   - Other errors → types.NFS3ErrIO (EIO: Generic I/O error)
//
// Also handles audit logging at appropriate levels:
//   - Client errors (NotFound, AccessDenied): logged as warnings
//   - Server errors: logged as errors
//
// Parameters:
//   - err: store error to map (nil = success)
//   - clientIP: Client IP address for audit logging
//   - operation: Operation name for audit logging (e.g., "LOOKUP", "CREATE")
//
// Returns:
//   - uint32: NFS status code (NFS3OK on success, error code on failure)
func MapStoreErrorToNFSStatus(err error, clientIP string, operation string) uint32 {
	if err == nil {
		return types.NFS3OK
	}

	// Delegate the code translation to common.MapToNFS3 (the single
	// source of truth across NFSv3/NFSv4/SMB). This wrapper only adds
	// audit logging at appropriate levels.
	nfsCode := common.MapToNFS3(err)

	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) {
		// Generic error — log as server-side failure.
		logger.Error("Operation failed", "operation", operation, "error", err, "client", clientIP)
		return nfsCode
	}

	// Client-visible errors are warnings; server-side failures are errors.
	switch storeErr.Code {
	case metadata.ErrNoSpace, metadata.ErrIOError:
		logger.Error("Operation failed", "operation", operation, "code", storeErr.Code, "message", storeErr.Message, "client", clientIP)
	default:
		logger.Warn("Operation failed", "operation", operation, "code", storeErr.Code, "message", storeErr.Message, "path", storeErr.Path, "client", clientIP)
	}
	return nfsCode
}
