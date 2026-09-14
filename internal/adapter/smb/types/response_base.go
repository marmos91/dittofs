package types

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
//
// It lives beside the Status codes it carries rather than with the handlers
// that populate it, so response types defined outside the handler package can
// embed it without importing that package.
type SMBResponseBase struct {
	// Status is the NT_STATUS code for this response.
	// Handlers set this to indicate success or failure.
	//
	// Common values:
	//   - StatusSuccess: Operation completed successfully
	//   - StatusInvalidParameter: Malformed request
	//   - StatusAccessDenied: Permission denied
	//   - StatusInvalidHandle: Invalid file handle
	Status Status

	// ReleaseData, when non-nil, is invoked by the response encoder AFTER the
	// wire write completes (plain or encrypted path, success or failure). It is
	// the hook by which pooled response buffers are returned to
	// internal/adapter/pool.
	//
	// SMB regular-file READ hands common.BlockReadResult.Release here
	// instead of deferring in the handler, so the buffer lifetime
	// extends through compound response assembly + encryption and the
	// release fires exactly once AFTER the bytes reach the socket.
	//
	// Non-pooled responses — which is every non-READ command AND the
	// pipe/symlink READ variants whose buffer sources (mfsymlink.Encode,
	// pipe.ProcessRead) are already heap-allocated or owned by other
	// subsystems — MUST leave this nil. The encoder null-checks.
	ReleaseData func()
}

// GetStatus returns the NT_STATUS code for this response.
//
// Satisfies the smbResponse interface, enabling
// responses to be used with the generic handleRequest helper.
func (b SMBResponseBase) GetStatus() Status {
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
