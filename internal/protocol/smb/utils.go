// Package smb provides SMB2 protocol dispatch, result types, and helper utilities.
package smb

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/internal/protocol/smb/v2/handlers"
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
// **Flow:**
//
//  1. Decode the request body using the provided decode function
//  2. If decode fails: return error response with errorStatus
//  3. Call the handler with the decoded request
//  4. If handler returns error: return error response with errorStatus
//  5. Encode the response using the Encode() method
//  6. If encode fails: return error response with errorStatus
//  7. Return successful HandlerResult with encoded data and status
//
// **Type Parameters:**
//   - Req: The request type (must satisfy smbRequest constraint)
//   - Resp: The response type (must satisfy smbResponse interface)
//
// **Parameters:**
//   - body: Raw request body bytes (after SMB2 header)
//   - decode: Function to parse body into typed request
//   - handle: Function to process request and return response
//   - errorStatus: NT_STATUS code to use for error responses
//   - makeErrorResp: Function to create an error response with given status
//
// **Returns:**
//   - *HandlerResult: Contains encoded response data and status code
//   - error: Non-nil only for system-level failures; protocol errors use status codes
//
// **Example:**
//
//	return handleRequest(
//	    body,
//	    handlers.DecodeReadRequest,
//	    func(req *handlers.ReadRequest) (*handlers.ReadResponse, error) {
//	        return h.Read(ctx, req)
//	    },
//	    types.StatusInvalidParameter,
//	    func(status types.Status) *handlers.ReadResponse {
//	        return &handlers.ReadResponse{SMBResponseBase: handlers.SMBResponseBase{Status: status}}
//	    },
//	)
func handleRequest[Req smbRequest, Resp smbResponse](
	body []byte,
	decode func([]byte) (Req, error),
	handle func(Req) (Resp, error),
	errorStatus types.Status,
	makeErrorResp func(types.Status) Resp,
) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := decode(body)
	if err != nil {
		logger.Debug("SMB: error decoding request", "error", err)
		errorResp := makeErrorResp(errorStatus)
		encoded, encErr := errorResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, Status: errorStatus}, encErr
		}
		return &HandlerResult{Data: encoded, Status: errorStatus}, err
	}

	// ========================================================================
	// Step 2: Call handler
	// ========================================================================

	resp, err := handle(req)
	if err != nil {
		logger.Debug("SMB: handler error", "error", err)
		errorResp := makeErrorResp(errorStatus)
		encoded, encErr := errorResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, Status: errorStatus}, encErr
		}
		return &HandlerResult{Data: encoded, Status: errorStatus}, err
	}

	// ========================================================================
	// Step 3: Extract status before encoding
	// ========================================================================

	status := resp.GetStatus()

	// ========================================================================
	// Step 4: Encode response
	// ========================================================================

	encoded, err := resp.Encode()
	if err != nil {
		logger.Debug("SMB: error encoding response", "error", err)
		errorResp := makeErrorResp(errorStatus)
		encodedErr, encErr := errorResp.Encode()
		if encErr != nil {
			return &HandlerResult{Data: nil, Status: errorStatus}, encErr
		}
		return &HandlerResult{Data: encodedErr, Status: errorStatus}, err
	}

	return &HandlerResult{Data: encoded, Status: status}, nil
}
