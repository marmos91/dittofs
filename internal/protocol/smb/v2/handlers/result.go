package handlers

import "github.com/marmos91/dittofs/internal/protocol/smb/types"

// HandlerResult contains the response data and status.
//
// Every SMB2 handler returns a HandlerResult indicating the outcome
// of the operation and any response data to send to the client.
type HandlerResult struct {
	// Data contains the response body (excluding the 64-byte header).
	// For error responses, this may be nil.
	Data []byte

	// Status is the NT_STATUS code indicating the operation result.
	// Common values:
	//   - types.StatusSuccess: Operation completed successfully
	//   - types.StatusMoreProcessingRequired: Multi-round authentication in progress
	//   - types.StatusAccessDenied: Permission denied
	//   - types.StatusLogonFailure: Authentication failed
	Status types.Status
}

// NewResult creates a new handler result with the given status and data.
//
// Example:
//
//	return NewResult(types.StatusSuccess, responseBody)
func NewResult(status types.Status, data []byte) *HandlerResult {
	return &HandlerResult{
		Status: status,
		Data:   data,
	}
}

// NewErrorResult creates an error result with the given status and no data.
//
// Example:
//
//	return NewErrorResult(types.StatusAccessDenied)
func NewErrorResult(status types.Status) *HandlerResult {
	return &HandlerResult{
		Status: status,
		Data:   nil,
	}
}
