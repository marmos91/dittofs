package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
)

// ============================================================================
// Read Request Validation
// ============================================================================

// readValidationError represents a READ request validation error.
type readValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *readValidationError) Error() string {
	return e.message
}

// validateReadRequest validates READ request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count is not zero (RFC 1813 allows it, but it's unusual)
//   - Count doesn't exceed reasonable limits
//
// Returns:
//   - nil if valid
//   - *readValidationError with NFS status if invalid
func validateReadRequest(req *ReadRequest) *readValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &readValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate count - zero is technically valid but unusual
	if req.Count == 0 {
		logger.Debug("READ request with count=0 (unusual but valid)")
	}

	// Validate count doesn't exceed reasonable limits (1GB)
	// While RFC 1813 doesn't specify a maximum, extremely large reads should be rejected
	const maxReadSize = 1024 * 1024 * 1024 // 1GB
	if req.Count > maxReadSize {
		return &readValidationError{
			message:   fmt.Sprintf("read count too large: %d bytes (max %d)", req.Count, maxReadSize),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}
