// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 LOGOFF command handler [MS-SMB2] 2.2.7, 2.2.8.
// LOGOFF terminates a session and frees all associated resources.
package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// LogoffRequest represents an SMB2 LOGOFF request from a client [MS-SMB2] 2.2.7.
//
// LOGOFF requests that the server close the session identified by SessionID
// in the SMB2 header. All tree connections and open files associated with
// the session are closed.
//
// **Wire format (4 bytes):**
//
//	Offset  Size  Field          Description
//	0       2     StructureSize  Always 4
//	2       2     Reserved       Must be 0
//
// **Example:**
//
//	req, err := DecodeLogoffRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
//	resp, err := handler.Logoff(ctx, req)
type LogoffRequest struct {
	// StructureSize is always 4 for LOGOFF requests.
	// Validated during decoding but not used by handler logic.
	StructureSize uint16

	// Reserved is for future use and should be 0.
	Reserved uint16
}

// LogoffResponse represents an SMB2 LOGOFF response to a client [MS-SMB2] 2.2.8.
//
// A successful response indicates the session has been terminated.
//
// **Wire format (4 bytes):**
//
//	Offset  Size  Field          Description
//	0       2     StructureSize  Always 4
//	2       2     Reserved       Must be 0
type LogoffResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeLogoffRequest parses an SMB2 LOGOFF request body [MS-SMB2] 2.2.7.
//
// **Parameters:**
//   - body: Request body starting after the SMB2 header (64 bytes)
//
// **Returns:**
//   - *LogoffRequest: Parsed request structure
//   - error: Decoding error if body is malformed
//
// **Example:**
//
//	req, err := DecodeLogoffRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
func DecodeLogoffRequest(body []byte) (*LogoffRequest, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("LOGOFF request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	req := &LogoffRequest{
		StructureSize: r.ReadUint16(),
		Reserved:      r.ReadUint16(),
	}
	if r.Err() != nil {
		return nil, fmt.Errorf("LOGOFF decode error: %w", r.Err())
	}
	return req, nil
}

// Encode serializes the LogoffResponse into SMB2 wire format [MS-SMB2] 2.2.8.
//
// **Returns:**
//   - []byte: 4-byte response body
//   - error: Always nil (included for interface consistency)
func (resp *LogoffResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(4)
	w.WriteUint16(4) // StructureSize
	w.WriteUint16(0) // Reserved
	if w.Err() != nil {
		return nil, w.Err()
	}
	return w.Bytes(), nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Logoff handles SMB2 LOGOFF command [MS-SMB2] 2.2.7, 2.2.8.
//
// LOGOFF terminates the session identified by the SessionID in the SMB2 header.
// All tree connections and open files associated with the session are closed
// and their resources freed.
//
// **Purpose:**
//
// The LOGOFF command allows clients to:
//   - Gracefully terminate a session
//   - Release server resources
//   - Close all tree connections and open files
//
// **Process:**
//
//  1. Validate the request
//  2. Verify the session exists
//  3. Perform full session cleanup:
//     - Close all open files (releases locks, flushes caches)
//     - Delete all tree connections
//     - Clean up pending auth state
//     - Delete the session
//  4. Return success response
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request
//   - StatusUserSessionDeleted: Session not found
//
// **Parameters:**
//   - ctx: SMB handler context with session information
//   - req: Parsed LOGOFF request
//
// **Returns:**
//   - *LogoffResponse: Response (typically success)
//   - error: Internal error (rare)
func (h *Handler) Logoff(ctx *SMBHandlerContext, req *LogoffRequest) (*LogoffResponse, error) {
	// ========================================================================
	// Step 1: Verify session exists
	// ========================================================================

	sess, ok := h.GetSession(ctx.SessionID)
	if !ok {
		return &LogoffResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUserSessionDeleted}}, nil
	}

	// Mark the session as logged off immediately. This prevents a race where
	// the next request arrives before the deferred session delete executes:
	// the signing verifier and dispatch layer check this flag to return
	// STATUS_USER_SESSION_DELETED instead of STATUS_ACCESS_DENIED.
	sess.LoggedOff.Store(true)

	// ========================================================================
	// Step 2: Partial cleanup — close files, trees, pending auth
	// ========================================================================
	//
	// Per MS-SMB2 3.3.5.6: the LOGOFF response MUST be signed (or encrypted)
	// using the session's key. The session remains alive in the SessionManager
	// with LoggedOff=true so that:
	//   1. The LOGOFF response can be signed/encrypted.
	//   2. In-flight goroutines (dispatched before LOGOFF was read) can still
	//      sign their responses via SendMessage -> GetSession.
	//   3. The verifier skips signature checks for logged-off sessions.
	//   4. prepareDispatch returns STATUS_USER_SESSION_DELETED.
	//
	// The session is NOT deleted here. It is cleaned up on connection close
	// via cleanupSessions -> CleanupSession -> DeleteSession.

	logger.Debug("Logoff: partial cleanup (session kept for response signing)",
		"sessionID", ctx.SessionID)

	filesClosed := h.CloseAllFilesForSession(ctx.Context, ctx.SessionID, false)

	// Release leases and notify watchers that may not have been cleaned up
	// by per-file CLOSE (e.g. client skipped CLOSE before LOGOFF).
	h.releaseSessionLeasesAndNotifies(ctx.Context, ctx.SessionID)

	treesDeleted := h.DeleteAllTreesForSession(ctx.SessionID)
	h.DeletePendingAuth(ctx.SessionID)

	logger.Debug("Logoff: partial cleanup done",
		"sessionID", ctx.SessionID,
		"filesClosed", filesClosed,
		"treesDeleted", treesDeleted)

	// ========================================================================
	// Step 3: Return success response
	// ========================================================================

	return &LogoffResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
}
