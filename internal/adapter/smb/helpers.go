// Package smb provides SMB2 protocol dispatch, result types, and helper utilities.
package smb

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Generic Request/Response Handling
// ============================================================================

// smbRequest defines the type constraint for all SMB2 request types.
//
// This interface enables generic handling of SMB2 requests through the
// handleRequest helper function, providing consistent decode → handle → encode
// flow across all SMB2 commands.
type smbRequest interface {
	*handlers.FlushRequest |
		*handlers.CloseRequest |
		*handlers.ReadRequest |
		*handlers.WriteRequest |
		*handlers.CreateRequest |
		*handlers.QueryInfoRequest |
		*handlers.SetInfoRequest |
		*handlers.QueryDirectoryRequest |
		*handlers.LogoffRequest |
		*handlers.EchoRequest
}

// smbResponse defines the interface that all SMB2 response types must implement.
//
// Response types must provide:
//   - Encode(): Serialize the response to wire format (little-endian binary)
//   - GetStatus(): Return the NT_STATUS code for metrics and error handling
//
// The SMBResponseBase type provides a default GetStatus() implementation
// when embedded in response structs.
type smbResponse interface {
	// Encode serializes the response to SMB2 wire format.
	// Returns the response body bytes (excluding the 64-byte SMB2 header).
	Encode() ([]byte, error)

	// GetStatus returns the NT_STATUS code for this response.
	// Used by the dispatcher for metrics tracking and error handling.
	GetStatus() types.Status
}

// handleRequest provides a generic decode → handle → encode pipeline for SMB2 commands.
//
// This helper mirrors the NFS handleRequest pattern, providing consistent:
//   - Request decoding with error handling
//   - Handler invocation with proper context
//   - Response encoding with error handling
//   - Error response generation on failures
//
// Error responses (decode/handle/encode failures, or any handler-returned error
// status) return a nil body alongside the error status. The outer
// buildResponseHeaderAndBody layer substitutes MakeErrorBody() per
// [MS-SMB2] 2.2.2 before the response hits the wire, so we avoid encoding a
// command-specific body that would then be discarded.
//
// **Type Parameters:**
//   - Req: The request type (must satisfy smbRequest constraint)
//   - Resp: The response type (must satisfy smbResponse interface)
//
// **Parameters:**
//   - body: Raw request body bytes (after SMB2 header)
//   - decode: Function to parse body into typed request
//   - handle: Function to process request and return response
//   - errorStatus: NT_STATUS code to use when decode/handle fails
//   - makeErrorResp: retained for signature compatibility with existing
//     callers; unused since the outer layer substitutes the ERROR body.
//
// **Returns:**
//   - *HandlerResult: Contains encoded response data and status code
//   - error: Non-nil only for system-level failures; protocol errors use status codes
func handleRequest[Req smbRequest, Resp smbResponse](
	body []byte,
	decode func([]byte) (Req, error),
	handle func(Req) (Resp, error),
	errorStatus types.Status,
	_ func(types.Status) Resp,
) (*HandlerResult, error) {
	req, err := decode(body)
	if err != nil {
		logger.Debug("SMB: error decoding request", "error", err)
		return &HandlerResult{Status: errorStatus}, nil
	}

	resp, err := handle(req)
	if err != nil {
		logger.Debug("SMB: handler error", "error", err)
		return &HandlerResult{Status: errorStatus}, nil
	}

	status := resp.GetStatus()

	// Skip encoding when the handler reported an error status: the outer
	// buildResponseHeaderAndBody replaces the body with MakeErrorBody() per
	// [MS-SMB2] 2.2.2, so command-specific encoding would be thrown away.
	// Informational/warning statuses (e.g., STATUS_NO_MORE_FILES,
	// STATUS_BUFFER_OVERFLOW) still need the command body — they're treated
	// as partial-success by clients.
	if status.IsError() {
		return &HandlerResult{Status: status}, nil
	}

	encoded, err := resp.Encode()
	if err != nil {
		logger.Debug("SMB: error encoding response", "error", err)
		return &HandlerResult{Status: types.StatusInternalError}, nil
	}

	return &HandlerResult{Data: encoded, Status: status}, nil
}
