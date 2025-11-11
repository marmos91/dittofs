package xdr

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Error Mapping - Repository Errors → NFS Status Codes
// ============================================================================

// MapRepositoryErrorToNFSStatus maps repository errors to NFS status codes.
//
// Per RFC 1813 Section 2.2 (nfsstat3):
// NFS procedures return status codes indicating success or specific failure
// conditions. This function translates internal repository errors into the
// appropriate NFS status codes for client consumption.
//
// Error Mapping:
//   - ExportErrNotFound → types.NFS3ErrNoEnt (ENOENT: No such file or directory)
//   - ExportErrAccessDenied → NFS3ErrAcces (EACCES: Permission denied)
//   - ExportErrServerFault → types.NFS3ErrIO or more specific (depends on message)
//   - "parent is not a directory" → types.NFS3ErrNotDir (ENOTDIR)
//   - "cannot remove directory with REMOVE" → types.NFS3ErrIsDir (EISDIR)
//   - other → types.NFS3ErrIO (EIO: I/O error)
//   - Unknown errors → types.NFS3ErrIO (generic I/O error)
//
// This function also handles audit logging at appropriate levels:
//   - Client errors (NotFound, AccessDenied): logged as warnings
//   - Server errors: logged as errors
//
// Parameters:
//   - err: Repository error to map (nil = success)
//   - clientIP: Client IP address for audit logging
//   - operation: Operation name for audit logging (e.g., "LOOKUP", "CREATE")
//
// Returns:
//   - uint32: NFS status code (NFS3OK on success, error code on failure)
func MapRepositoryErrorToNFSStatus(err error, clientIP string, operation string) uint32 {
	if err == nil {
		return types.NFS3OK
	}

	// Check if it's a typed ExportError
	exportErr, ok := err.(*metadata.ExportError)
	if !ok {
		// Generic error: log and return I/O error
		logger.Error("%s failed: %v client=%s", operation, err, clientIP)
		return types.NFS3ErrIO
	}

	// Map ExportError codes to NFS status codes
	switch exportErr.Code {
	case metadata.ExportErrNotFound:
		// File or directory not found
		logger.Warn("%s failed: %s client=%s", operation, exportErr.Message, clientIP)
		return types.NFS3ErrNoEnt

	case metadata.ExportErrAccessDenied:
		// Permission denied
		logger.Warn("%s failed: %s client=%s", operation, exportErr.Message, clientIP)
		return types.NFS3ErrAcces

	case metadata.ExportErrServerFault:
		// Server error: check message for more specific status
		switch exportErr.Message {
		case "parent is not a directory":
			// Attempting to create/lookup within a non-directory
			logger.Warn("%s failed: parent not a directory client=%s", operation, clientIP)
			return types.NFS3ErrNotDir

		case "cannot remove directory with REMOVE (use RMDIR)":
			// Attempting to remove a directory with REMOVE instead of RMDIR
			logger.Warn("%s failed: attempted to remove directory client=%s", operation, clientIP)
			return types.NFS3ErrIsDir

		default:
			// Generic server error
			logger.Error("%s failed: %s client=%s", operation, exportErr.Message, clientIP)
			return types.NFS3ErrIO
		}

	default:
		// Unknown error code
		logger.Error("%s failed: unknown export error: %s client=%s", operation, exportErr.Message, clientIP)
		return types.NFS3ErrIO
	}
}

// mapContentErrorToNFSStatus maps content repository errors to appropriate
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
		return types.NFS3ErrAcces

	case containsIgnoreCase(errMsg, "stale") || containsIgnoreCase(errMsg, "invalid handle"):
		return types.NFS3ErrStale

	default:
		// Generic I/O error for unrecognized errors
		return types.NFS3ErrIO
	}
}
