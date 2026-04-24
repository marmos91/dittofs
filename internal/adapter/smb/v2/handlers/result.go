package handlers

import "github.com/marmos91/dittofs/internal/adapter/smb/types"

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

	// ReleaseData, when non-nil, is invoked by the response encoder AFTER the
	// wire write completes (plain or encrypted path, success or failure). It is
	// the hook by which pooled response buffers are returned to
	// internal/adapter/pool.
	//
	// Per D-09 (phase 09 context): SMB regular-file READ hands
	// common.BlockReadResult.Release here instead of deferring in the handler,
	// so the buffer lifetime extends through compound response assembly +
	// encryption and the release fires exactly once AFTER the bytes reach the
	// socket.
	//
	// Non-pooled responses — which is every non-READ command AND the
	// pipe/symlink READ variants whose buffer sources (mfsymlink.Encode,
	// pipe.ProcessRead) are already heap-allocated or owned by other
	// subsystems — MUST leave this nil. The encoder null-checks.
	ReleaseData func()
}

// GetStatus returns the NT_STATUS code for this response.
//
// This method satisfies the smbResponse interface, enabling
// responses to be used with the generic handleRequest helper.
func (b SMBResponseBase) GetStatus() types.Status {
	return b.Status
}

// GetReleaseData returns the release closure (if any) that the response
// encoder must invoke after the wire write completes. Returns nil for
// non-pooled responses — callers MUST null-check before invoking.
//
// This is the optional-interface hook consumed by the generic handleRequest
// helper (internal/adapter/smb/helpers.go) to propagate ReleaseData from a
// typed response envelope (e.g. *ReadResponse) onto the wire-level
// HandlerResult. The encoder then fires it in SendResponse /
// SendResponseWithHooks / compound paths.
func (b SMBResponseBase) GetReleaseData() func() {
	return b.ReleaseData
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

	// DropConnection signals the dispatch layer to close the TCP connection
	// without sending a response. Used for fatal protocol errors where
	// continuing is unsafe (e.g., VALIDATE_NEGOTIATE failure per MS-SMB2 3.3.5.15.12).
	DropConnection bool

	// AsyncId is set when the response should use the async header format.
	// When non-zero, the response header will have FlagAsync set and AsyncId
	// will replace the Reserved/TreeID fields. Used for CHANGE_NOTIFY interim
	// responses (STATUS_PENDING) and async completion responses.
	// [MS-SMB2] Section 2.2.1.2
	AsyncId uint64

	// IsBinding is true when this result completes an SMB2 session bind
	// (MS-SMB2 §3.3.5.5.2). The response dispatch layer must NOT treat a
	// successful bind response as session-creation on the bound connection:
	// the session lives on a different connection, and tracking it here
	// would cause this connection's close to delete the original session.
	IsBinding bool

	// ReleaseData, when non-nil, is invoked by the response encoder AFTER the
	// wire write completes. Mirrors SMBResponseBase.ReleaseData — the generic
	// handleRequest helper copies the typed response envelope's ReleaseData
	// here so the wire-level encoder (response.go sendMessage and compound.go
	// sendCompoundResponses) can fire it in one canonical place.
	//
	// Non-pooled commands leave this nil; the encoder null-checks. Firing
	// order: AFTER WriteNetBIOSFrame returns (plain, encrypted, compound),
	// regardless of write success. This ensures the pooled buffer stays valid
	// through the full response-assembly pipeline and is never double-freed.
	ReleaseData func()
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
