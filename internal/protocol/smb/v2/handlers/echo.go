// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 ECHO command handler [MS-SMB2] 2.2.28, 2.2.29.
// ECHO is a keep-alive/ping command used to verify server responsiveness.
package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// EchoRequest represents an SMB2 ECHO request from a client [MS-SMB2] 2.2.28.
//
// ECHO is a simple keep-alive command used to verify the server is responsive.
// The request has no meaningful fields beyond the structure size.
//
// **Wire format (4 bytes):**
//
//	Offset  Size  Field          Description
//	0       2     StructureSize  Always 4
//	2       2     Reserved       Must be 0
//
// **Example:**
//
//	req, err := DecodeEchoRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
//	resp, err := handler.Echo(ctx, req)
type EchoRequest struct {
	// StructureSize is always 4 for ECHO requests.
	// Validated during decoding but not used by handler logic.
	StructureSize uint16

	// Reserved is for future use and should be 0.
	Reserved uint16
}

// EchoResponse represents an SMB2 ECHO response to a client [MS-SMB2] 2.2.29.
//
// A successful response indicates the server is responsive.
//
// **Wire format (4 bytes):**
//
//	Offset  Size  Field          Description
//	0       2     StructureSize  Always 4
//	2       2     Reserved       Must be 0
type EchoResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeEchoRequest parses an SMB2 ECHO request body [MS-SMB2] 2.2.28.
//
// **Parameters:**
//   - body: Request body starting after the SMB2 header (64 bytes)
//
// **Returns:**
//   - *EchoRequest: Parsed request structure
//   - error: Decoding error if body is malformed
//
// **Example:**
//
//	req, err := DecodeEchoRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
func DecodeEchoRequest(body []byte) (*EchoRequest, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("ECHO request too short: %d bytes", len(body))
	}

	return &EchoRequest{
		StructureSize: binary.LittleEndian.Uint16(body[0:2]),
		Reserved:      binary.LittleEndian.Uint16(body[2:4]),
	}, nil
}

// Encode serializes the EchoResponse into SMB2 wire format [MS-SMB2] 2.2.29.
//
// **Returns:**
//   - []byte: 4-byte response body
//   - error: Always nil (included for interface consistency)
func (resp *EchoResponse) Encode() ([]byte, error) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint16(buf[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], 0) // Reserved

	return buf, nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Echo handles SMB2 ECHO command [MS-SMB2] 2.2.28, 2.2.29.
//
// ECHO is a simple keep-alive command that allows clients to verify
// the server is responsive. It does not modify any state.
//
// **Purpose:**
//
// The ECHO command allows clients to:
//   - Verify the server is responsive
//   - Keep connections alive
//   - Measure round-trip latency
//
// **Process:**
//
//  1. Receive and decode the request
//  2. Return immediate success response
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request (too short)
//
// **Parameters:**
//   - ctx: SMB handler context (unused for ECHO)
//   - req: Parsed ECHO request
//
// **Returns:**
//   - *EchoResponse: Success response
//   - error: Internal error (rare)
func (h *Handler) Echo(ctx *SMBHandlerContext, req *EchoRequest) (*EchoResponse, error) {
	// ECHO is stateless - just return success
	return &EchoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
}
