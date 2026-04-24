package xdr

import (
	"errors"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Error Mapping - store Errors → NFS Status Codes
// ============================================================================

// MapStoreErrorToNFSStatus maps store errors to NFS status codes.
//
// Per RFC 1813 Section 2.2 (nfsstat3):
// NFS procedures return status codes indicating success or specific failure
// conditions. This function translates internal store errors into the
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
// This function also handles audit logging at appropriate levels:
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

	// Delegate the code translation to common.MapToNFS3 (ADAPT-03, D-06 —
	// single source of truth across NFSv3/NFSv4/SMB). This wrapper only adds
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

// MapContentErrorToNFSStatus maps content repository errors to appropriate
// NFS status codes.
//
// This function analyzes error messages and types to determine the most
// appropriate NFS error code. In the future, the content repository should
// return typed errors for more precise mapping.
//
// Common mappings:
//   - "no space" / "disk full" → NFS3ErrNoSpc
//   - "read-only" / "permission denied" → NFS3ErrRofs
//   - "not found" / "does not exist" → NFS3ErrNoEnt
//   - Other errors → NFS3ErrIO (generic I/O error)
//
// Parameters:
//   - err: Error returned from content repository
//
// Returns:
//   - uint32: Appropriate NFS status code
func MapContentErrorToNFSStatus(err error) uint32 {
	if err == nil {
		return types.NFS3OK
	}

	// Check if it's a typed StoreError first (using errors.As to handle wrapped errors)
	var storeErr *metadata.StoreError
	if errors.As(err, &storeErr) {
		// Use the more specific error mapping
		return MapStoreErrorToNFSStatus(err, "", "content operation")
	}

	// Check for blockstore sentinel errors
	if errors.Is(err, blockstore.ErrRemoteUnavailable) {
		return types.NFS3ErrIO
	}

	// Analyze error message for common patterns
	// This is a best-effort approach until content repository returns typed errors
	errMsg := err.Error()

	// Check for specific error patterns (case-insensitive substring matching)
	switch {
	case containsIgnoreCase(errMsg, "no space") || containsIgnoreCase(errMsg, "disk full"):
		return types.NFS3ErrNoSpc

	case containsIgnoreCase(errMsg, "read-only") || containsIgnoreCase(errMsg, "read only"):
		return types.NFS3ErrRofs

	case containsIgnoreCase(errMsg, "not found") || containsIgnoreCase(errMsg, "does not exist"):
		return types.NFS3ErrNoEnt

	case containsIgnoreCase(errMsg, "permission denied") || containsIgnoreCase(errMsg, "access denied"):
		return types.NFS3ErrAccess

	case containsIgnoreCase(errMsg, "stale") || containsIgnoreCase(errMsg, "invalid handle"):
		return types.NFS3ErrStale

	case containsIgnoreCase(errMsg, "cache full"):
		// Cache full - return JUKEBOX to tell client to retry after flushing
		// This provides backpressure to prevent OOM conditions
		return types.NFS3ErrJukebox

	default:
		// Generic I/O error for unrecognized errors
		return types.NFS3ErrIO
	}
}
