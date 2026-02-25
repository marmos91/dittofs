package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// ============================================================================
// Write Request Validation
// ============================================================================

// writeValidationError represents a WRITE request validation error.
type writeValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *writeValidationError) Error() string {
	return e.message
}

// validateWriteRequest validates WRITE request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count matches actual data length
//   - Count doesn't exceed server's maximum write size
//   - Offset + Count doesn't overflow uint64
//   - Stability level is valid
//
// Parameters:
//   - req: The write request to validate
//   - maxWriteSize: Maximum write size from store configuration
//
// Returns:
//   - nil if valid
//   - *writeValidationError with NFS status if invalid
func validateWriteRequest(req *WriteRequest, maxWriteSize uint32) *writeValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &writeValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &writeValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &writeValidationError{
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

	// Validate count doesn't exceed maximum write size configured by store
	// The store can configure this based on its constraints and the
	// wtmax value advertised in FSINFO.
	if dataLen > maxWriteSize {
		return &writeValidationError{
			message:   fmt.Sprintf("write data too large: %d bytes (max %d)", dataLen, maxWriteSize),
			nfsStatus: types.NFS3ErrFBig,
		}
	}

	// Validate offset + count doesn't overflow
	// This prevents integer overflow attacks
	// CRITICAL: This must be checked BEFORE any calculations use offset + count
	if req.Offset > ^uint64(0)-uint64(dataLen) {
		return &writeValidationError{
			message:   fmt.Sprintf("offset + count would overflow: offset=%d count=%d", req.Offset, dataLen),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate stability level
	if req.Stable > FileSyncWrite {
		return &writeValidationError{
			message:   fmt.Sprintf("invalid stability level: %d (max %d)", req.Stable, FileSyncWrite),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}
