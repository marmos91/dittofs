package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// Write Request Validation

// validateWriteRequest validates WRITE request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count matches actual data length
//   - Offset + Count doesn't overflow uint64
//   - Stability level is valid
//
// An over-large request (data exceeding the advertised wtmax) is NOT rejected
// here. Per RFC 1813 Section 3.3.7 the server may write fewer bytes than
// requested; the WRITE handler short-writes by capping the data to the value
// advertised in FSINFO (GetFilesystemCapabilities().MaxWriteSize), mirroring
// Linux nfsd which clamps to svc_max_payload rather than returning NFS3ERR_FBIG.
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateWriteRequest(req *WriteRequest) *validationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &validationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate data length matches count
	// Some tolerance is acceptable, but large mismatches indicate corruption
	dataLen := uint32(len(req.Data))
	if dataLen != req.Count {
		logger.Warn("WRITE: count mismatch (proceeding with actual data length)", "count", req.Count, "data_len", dataLen)
		// Not fatal - we'll use actual data length
	}

	// Validate offset + count doesn't overflow
	// This prevents integer overflow attacks
	// CRITICAL: This must be checked BEFORE any calculations use offset + count
	if req.Offset > ^uint64(0)-uint64(dataLen) {
		return &validationError{
			message:   fmt.Sprintf("offset + count would overflow: offset=%d count=%d", req.Offset, dataLen),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate stability level
	if req.Stable > FileSyncWrite {
		return &validationError{
			message:   fmt.Sprintf("invalid stability level: %d (max %d)", req.Stable, FileSyncWrite),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}
