package handlers

import "github.com/marmos91/dittofs/internal/protocol/smb/types"

// ============================================================================
// Response Base Type
// ============================================================================

// SMBResponseBase provides common status tracking for all SMB2 response types.
//
// This type should be embedded in all SMB2 response structs to enable:
//   - Consistent status code handling across all handlers
//   - Interface satisfaction for the generic handleRequest helper
//   - Type-safe status code access without manual field access
//
// **Usage:**
//
// Embed SMBResponseBase in response structs:
//
//	type ReadResponse struct {
//	    SMBResponseBase      // Embeds Status field and GetStatus() method
//	    DataOffset    uint8
//	    Data          []byte
//	    DataRemaining uint32
//	}
//
// The embedded GetStatus() method satisfies the smbResponse interface,
// enabling use with the generic handleRequest dispatcher.
type SMBResponseBase struct {
	// Status is the NT_STATUS code for this response.
	// Handlers set this to indicate success or failure.
	//
	// Common values:
	//   - types.StatusSuccess: Operation completed successfully
	//   - types.StatusInvalidParameter: Malformed request
	//   - types.StatusAccessDenied: Permission denied
	//   - types.StatusInvalidHandle: Invalid file handle
	Status types.Status
}

// GetStatus returns the NT_STATUS code for this response.
//
// This method satisfies the smbResponse interface, enabling
// responses to be used with the generic handleRequest helper.
func (b SMBResponseBase) GetStatus() types.Status {
	return b.Status
}

// ============================================================================
// Handler Result Type
// ============================================================================

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
