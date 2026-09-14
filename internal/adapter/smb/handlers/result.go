package handlers

import "github.com/marmos91/dittofs/internal/adapter/smb/types"

// SMBResponseBase is the common status/release envelope embedded in every SMB2
// response struct. It lives in the types package so response types declared
// outside this package can embed it; the alias keeps the unqualified spelling
// working at the embedding and field-access sites here.
type SMBResponseBase = types.SMBResponseBase

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
