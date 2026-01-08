package xdr

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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
//   - ErrPermissionDenied → types.NFS3ErrAcces (EACCES: Permission denied)
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

	// Check if it's a typed StoreError
	storeErr, ok := err.(*metadata.StoreError)
	if !ok {
		// Generic error: log and return I/O error
		logger.Error("Operation failed", "operation", operation, "error", err, "client", clientIP)
		return types.NFS3ErrIO
	}

	// Map StoreError codes to NFS status codes
	switch storeErr.Code {
	case metadata.ErrNotFound:
		// File or directory not found
		logger.Warn("Operation failed", "operation", operation, "message", storeErr.Message, "client", clientIP)
		return types.NFS3ErrNoEnt

	case metadata.ErrAccessDenied, metadata.ErrPermissionDenied:
		// Permission denied (share-level or file-level)
		logger.Warn("Operation failed", "operation", operation, "message", storeErr.Message, "client", clientIP)
		return types.NFS3ErrAccess

	case metadata.ErrAuthRequired:
		// Authentication required (map to access denied for NFS)
		logger.Warn("Operation failed: authentication required", "operation", operation, "client", clientIP)
		return types.NFS3ErrAccess

	case metadata.ErrNotDirectory:
		// Attempting to create/lookup within a non-directory
		logger.Warn("Operation failed: not a directory", "operation", operation, "client", clientIP)
		return types.NFS3ErrNotDir

	case metadata.ErrIsDirectory:
		// Attempting to remove a directory with REMOVE instead of RMDIR
		logger.Warn("Operation failed: is a directory", "operation", operation, "client", clientIP)
		return types.NFS3ErrIsDir

	case metadata.ErrAlreadyExists:
		// File or directory already exists
		logger.Warn("Operation failed: already exists", "operation", operation, "client", clientIP)
		return types.NFS3ErrExist

	case metadata.ErrNotEmpty:
		// Directory not empty (cannot remove)
		logger.Warn("Operation failed: directory not empty", "operation", operation, "client", clientIP)
		return types.NFS3ErrNotEmpty

	case metadata.ErrNoSpace:
		// No space left on device
		logger.Error("Operation failed: no space left", "operation", operation, "client", clientIP)
		return types.NFS3ErrNoSpc

	case metadata.ErrReadOnly:
		// Read-only filesystem
		logger.Warn("Operation failed: read-only filesystem", "operation", operation, "client", clientIP)
		return types.NFS3ErrRofs

	case metadata.ErrStaleHandle:
		// Stale file handle
		logger.Warn("Operation failed: stale handle", "operation", operation, "client", clientIP)
		return types.NFS3ErrStale

	case metadata.ErrInvalidHandle:
		// Invalid file handle
		logger.Warn("Operation failed: invalid handle", "operation", operation, "client", clientIP)
		return types.NFS3ErrBadHandle

	case metadata.ErrNotSupported:
		// Operation not supported
		logger.Warn("Operation failed: not supported", "operation", operation, "client", clientIP)
		return types.NFS3ErrNotSupp

	case metadata.ErrInvalidArgument:
		// Invalid argument (map to I/O error or INVAL depending on NFS version)
		logger.Warn("Operation failed: invalid argument", "operation", operation, "client", clientIP)
		return types.NFS3ErrIO

	case metadata.ErrIOError:
		// Generic I/O error
		logger.Error("Operation failed: I/O error", "operation", operation, "message", storeErr.Message, "client", clientIP)
		return types.NFS3ErrIO

	case metadata.ErrLocked:
		// File is locked/busy - map to JUKEBOX (retry later) per RFC 8881 Section 18.9.4
		// This is used for transient errors like lock conflicts or delegation conflicts.
		// Clients receiving JUKEBOX should retry the operation after a short delay.
		// This matches Linux kernel behavior for -EBUSY on non-directories.
		logger.Warn("Operation failed: file locked", "operation", operation, "message", storeErr.Message, "client", clientIP)
		return types.NFS3ErrJukebox

	case metadata.ErrPrivilegeRequired:
		// Operation requires elevated privileges (e.g., setting arbitrary timestamps as non-owner)
		// Maps to EPERM per POSIX - "Operation not permitted"
		logger.Warn("Operation failed: privilege required", "operation", operation, "message", storeErr.Message, "client", clientIP)
		return types.NFS3ErrPerm

	case metadata.ErrNameTooLong:
		// Path or filename exceeds limits
		logger.Warn("Operation failed: name too long", "operation", operation, "path", storeErr.Path, "client", clientIP)
		return types.NFS3ErrNameTooLong

	default:
		// Unknown error code
		logger.Error("Operation failed: unknown error code", "operation", operation, "code", storeErr.Code, "message", storeErr.Message, "client", clientIP)
		return types.NFS3ErrIO
	}
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

	// Check if it's a typed StoreError first
	_, ok := err.(*metadata.StoreError)
	if ok {
		// Use the more specific error mapping
		return MapStoreErrorToNFSStatus(err, "", "content operation")
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

	default:
		// Generic I/O error for unrecognized errors
		return types.NFS3ErrIO
	}
}
