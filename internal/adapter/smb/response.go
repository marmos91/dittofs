package smb

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// ProcessSingleRequest dispatches an SMB2 request to the appropriate handler.
// This is used for non-compound (single) requests with full credit tracking.
//
// Parameters:
//   - ctx: context for cancellation
//   - reqHeader: parsed SMB2 request header
//   - body: request body bytes
//   - connInfo: connection context for dispatch
//   - asyncNotifyCallback: optional callback for CHANGE_NOTIFY async responses (nil = no async)
func ProcessSingleRequest(
	ctx context.Context,
	reqHeader *header.SMB2Header,
	body []byte,
	connInfo *ConnInfo,
	asyncNotifyCallback handlers.AsyncResponseCallback,
) error {
	// Check context before processing
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Track request for adaptive credit management
	connInfo.SessionManager.RequestStarted(reqHeader.SessionID)
	defer connInfo.SessionManager.RequestCompleted(reqHeader.SessionID)

	clientAddr := connInfo.Conn.RemoteAddr().String()

	// Look up command in dispatch table
	cmd, ok := DispatchTable[reqHeader.Command]
	if !ok {
		logger.Debug("Unknown SMB2 command", "command", reqHeader.Command)
		return SendErrorResponse(reqHeader, types.StatusNotSupported, connInfo)
	}

	// Create handler context
	handlerCtx := &handlers.SMBHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		SessionID:  reqHeader.SessionID,
		TreeID:     reqHeader.TreeID,
		MessageID:  reqHeader.MessageID,
	}

	// For CHANGE_NOTIFY, set up async callback so notifications can be sent
	if reqHeader.Command == types.SMB2ChangeNotify && asyncNotifyCallback != nil {
		handlerCtx.AsyncNotifyCallback = asyncNotifyCallback
	}

	// Validate session if required
	if cmd.NeedsSession && reqHeader.SessionID != 0 {
		session, ok := connInfo.Handler.GetSession(reqHeader.SessionID)
		if !ok {
			return SendErrorResponse(reqHeader, types.StatusUserSessionDeleted, connInfo)
		}
		handlerCtx.IsGuest = session.IsGuest
		handlerCtx.Username = session.Username
	}

	// Validate tree connection if required
	if cmd.NeedsTree && reqHeader.TreeID != 0 {
		tree, ok := connInfo.Handler.GetTree(reqHeader.TreeID)
		if !ok {
			return SendErrorResponse(reqHeader, types.StatusNetworkNameDeleted, connInfo)
		}
		handlerCtx.ShareName = tree.ShareName
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageId", reqHeader.MessageID,
		"client", clientAddr)

	// Execute handler
	result, err := cmd.Handler(handlerCtx, connInfo.Handler, connInfo.Handler.Registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return SendErrorResponse(reqHeader, types.StatusInternalError, connInfo)
	}

	// Track session lifecycle for connection cleanup
	TrackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status, connInfo.SessionTracker)

	// Send response
	return SendResponse(reqHeader, handlerCtx, result, connInfo)
}

// ProcessRequestWithFileID processes a request and returns the FileID if applicable (for CREATE).
// Used in compound request processing where FileID propagation is needed.
func ProcessRequestWithFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, connInfo *ConnInfo) (*HandlerResult, [16]byte) {
	var fileID [16]byte

	clientAddr := connInfo.Conn.RemoteAddr().String()

	cmd, ok := DispatchTable[reqHeader.Command]
	if !ok {
		logger.Debug("Unknown SMB2 command", "command", reqHeader.Command)
		return &HandlerResult{Status: types.StatusNotSupported, Data: MakeErrorBody()}, fileID
	}

	handlerCtx := &handlers.SMBHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		SessionID:  reqHeader.SessionID,
		TreeID:     reqHeader.TreeID,
		MessageID:  reqHeader.MessageID,
	}

	if cmd.NeedsSession && reqHeader.SessionID != 0 {
		session, ok := connInfo.Handler.GetSession(reqHeader.SessionID)
		if !ok {
			return &HandlerResult{Status: types.StatusUserSessionDeleted, Data: MakeErrorBody()}, fileID
		}
		handlerCtx.IsGuest = session.IsGuest
		handlerCtx.Username = session.Username
	}

	if cmd.NeedsTree && reqHeader.TreeID != 0 {
		tree, ok := connInfo.Handler.GetTree(reqHeader.TreeID)
		if !ok {
			return &HandlerResult{Status: types.StatusNetworkNameDeleted, Data: MakeErrorBody()}, fileID
		}
		handlerCtx.ShareName = tree.ShareName
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageId", reqHeader.MessageID,
		"client", clientAddr)

	result, err := cmd.Handler(handlerCtx, connInfo.Handler, connInfo.Handler.Registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return &HandlerResult{Status: types.StatusInternalError, Data: MakeErrorBody()}, fileID
	}

	// Track session lifecycle for connection cleanup
	TrackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status, connInfo.SessionTracker)

	// Extract FileID from CREATE response (bytes 64-80)
	if reqHeader.Command == types.SMB2Create && result.Status == types.StatusSuccess && len(result.Data) >= 80 {
		copy(fileID[:], result.Data[64:80])
	}

	return result, fileID
}

// ProcessRequestWithInheritedFileID processes a request using an inherited FileID.
func ProcessRequestWithInheritedFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, inheritedFileID [16]byte, connInfo *ConnInfo) *HandlerResult {
	// For commands that use FileID, inject the inherited FileID into the request body
	if reqHeader.Command == types.SMB2QueryInfo || reqHeader.Command == types.SMB2Close ||
		reqHeader.Command == types.SMB2Read || reqHeader.Command == types.SMB2Write ||
		reqHeader.Command == types.SMB2QueryDirectory || reqHeader.Command == types.SMB2SetInfo {
		body = InjectFileID(reqHeader.Command, body, inheritedFileID)
	}

	result, _ := ProcessRequestWithFileID(ctx, reqHeader, body, connInfo)
	return result
}

// SendResponse sends an SMB2 response with credit management and signing.
func SendResponse(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) error {
	// Use session manager for adaptive credit grants
	sessionID := reqHeader.SessionID
	if ctx.SessionID != 0 {
		sessionID = ctx.SessionID
	}

	credits := connInfo.SessionManager.GrantCredits(
		sessionID,
		reqHeader.Credits,
		reqHeader.CreditCharge,
	)

	// Build response header with calculated credits
	respHeader := header.NewResponseHeaderWithCredits(reqHeader, result.Status, credits)

	// Update SessionID in response if it was set by handler (SESSION_SETUP)
	if ctx.SessionID != 0 && reqHeader.SessionID == 0 {
		respHeader.SessionID = ctx.SessionID
	}

	// Update TreeID in response if it was set by handler (TREE_CONNECT)
	if ctx.TreeID != 0 && reqHeader.TreeID == 0 {
		respHeader.TreeID = ctx.TreeID
	}

	// If result has error status but no data, add proper error body
	// Error responses must include a valid error body per MS-SMB2 spec
	body := result.Data
	if body == nil && result.Status.IsError() {
		body = MakeErrorBody()
	}

	return SendMessage(respHeader, body, connInfo)
}

// SendErrorResponse sends an SMB2 error response.
func SendErrorResponse(reqHeader *header.SMB2Header, status types.Status, connInfo *ConnInfo) error {
	// Use session manager for adaptive credit grants
	credits := connInfo.SessionManager.GrantCredits(
		reqHeader.SessionID,
		reqHeader.Credits,
		reqHeader.CreditCharge,
	)

	respHeader := header.NewResponseHeaderWithCredits(reqHeader, status, credits)

	return SendMessage(respHeader, MakeErrorBody(), connInfo)
}

// SendMessage sends an SMB2 message with NetBIOS framing and optional signing.
func SendMessage(hdr *header.SMB2Header, body []byte, connInfo *ConnInfo) error {
	headerBytes := hdr.Encode()

	// Sign the SMB2 payload if session has signing enabled.
	// We build the SMB payload (header + body) first, sign in place, then frame it.
	//
	// Per MS-SMB2 3.3.5.5.3: Once a session is established with signing negotiated,
	// the server MUST sign responses. ShouldSign() checks both that signing is
	// enabled and that a valid signing key exists.
	smbPayload := make([]byte, len(headerBytes)+len(body))
	copy(smbPayload, headerBytes)
	copy(smbPayload[len(headerBytes):], body)

	if hdr.SessionID != 0 {
		if sess, ok := connInfo.Handler.GetSession(hdr.SessionID); ok {
			if sess.ShouldSign() {
				sess.SignMessage(smbPayload)
				logger.Debug("Signed outgoing SMB2 message",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
			}
		}
	}

	if err := WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, smbPayload); err != nil {
		return err
	}

	logger.Debug("Sent SMB2 response",
		"command", hdr.Command.String(),
		"status", hdr.Status.String(),
		"messageId", hdr.MessageID,
		"bytes", len(smbPayload))

	return nil
}

// SendAsyncChangeNotifyResponse sends an asynchronous CHANGE_NOTIFY response.
// This is called when a filesystem change matches a pending watch.
func SendAsyncChangeNotifyResponse(sessionID, messageID uint64, response *handlers.ChangeNotifyResponse, connInfo *ConnInfo) error {
	// Encode the response body
	body, err := response.Encode()
	if err != nil {
		return fmt.Errorf("encode change notify response: %w", err)
	}

	// Build async response header
	respHeader := &header.SMB2Header{
		Command:   types.SMB2ChangeNotify,
		Status:    types.StatusSuccess,
		Flags:     types.FlagResponse | types.FlagAsync,
		MessageID: messageID,
		SessionID: sessionID,
		TreeID:    0, // Not used in async responses
		Credits:   1, // Grant 1 credit with async response
	}

	logger.Debug("Sending async CHANGE_NOTIFY response",
		"sessionID", sessionID,
		"messageID", messageID,
		"bufferLen", len(response.Buffer))

	return SendMessage(respHeader, body, connInfo)
}

// HandleSMB1Negotiate handles legacy SMB1 NEGOTIATE requests by responding with
// an SMB2 NEGOTIATE response, which tells the client to upgrade to SMB2.
//
// This is required because many clients (including macOS Finder) start with
// SMB1 NEGOTIATE and expect the server to respond with SMB2 if it supports it.
func HandleSMB1Negotiate(connInfo *ConnInfo) error {
	logger.Debug("Received SMB1 NEGOTIATE, responding with SMB2 upgrade",
		"address", connInfo.Conn.RemoteAddr().String())

	// Build SMB2 NEGOTIATE response header
	// When responding to SMB1 NEGOTIATE, we use a special SMB2 header format
	respHeader := make([]byte, header.HeaderSize)

	// Protocol ID: SMB2
	binary.LittleEndian.PutUint32(respHeader[0:4], types.SMB2ProtocolID)
	// Structure Size: 64
	binary.LittleEndian.PutUint16(respHeader[4:6], header.HeaderSize)
	// Credit Charge: 0
	binary.LittleEndian.PutUint16(respHeader[6:8], 0)
	// Status: STATUS_SUCCESS
	binary.LittleEndian.PutUint32(respHeader[8:12], uint32(types.StatusSuccess))
	// Command: NEGOTIATE
	binary.LittleEndian.PutUint16(respHeader[12:14], uint16(types.SMB2Negotiate))
	// Credits: 1
	binary.LittleEndian.PutUint16(respHeader[14:16], 1)
	// Flags: SERVER_TO_REDIR (response)
	binary.LittleEndian.PutUint32(respHeader[16:20], types.SMB2FlagsServerToRedir)
	// NextCommand: 0
	binary.LittleEndian.PutUint32(respHeader[20:24], 0)
	// MessageID: 0
	binary.LittleEndian.PutUint64(respHeader[24:32], 0)
	// Reserved: 0
	binary.LittleEndian.PutUint32(respHeader[32:36], 0)
	// TreeID: 0
	binary.LittleEndian.PutUint32(respHeader[36:40], 0)
	// SessionID: 0
	binary.LittleEndian.PutUint64(respHeader[40:48], 0)
	// Signature: zeros (no signing)

	// Build NEGOTIATE response body (65 bytes structure)
	respBody := make([]byte, 65)

	// StructureSize: 65
	binary.LittleEndian.PutUint16(respBody[0:2], 65)
	// SecurityMode: Set based on signing configuration [MS-SMB2 2.2.4]
	var securityMode byte
	if connInfo.Handler.SigningConfig.Enabled {
		securityMode |= 0x01 // SMB2_NEGOTIATE_SIGNING_ENABLED
	}
	if connInfo.Handler.SigningConfig.Required {
		securityMode |= 0x02 // SMB2_NEGOTIATE_SIGNING_REQUIRED
	}
	respBody[2] = securityMode
	// Reserved
	respBody[3] = 0
	// DialectRevision: SMB 2.0.2
	binary.LittleEndian.PutUint16(respBody[4:6], uint16(types.SMB2Dialect0202))
	// NegotiateContextCount: 0 (SMB 3.1.1 only)
	binary.LittleEndian.PutUint16(respBody[6:8], 0)
	// ServerGUID (16 bytes)
	copy(respBody[8:24], connInfo.Handler.ServerGUID[:])
	// Capabilities: 0
	binary.LittleEndian.PutUint32(respBody[24:28], 0)
	// MaxTransactSize
	binary.LittleEndian.PutUint32(respBody[28:32], connInfo.Handler.MaxTransactSize)
	// MaxReadSize
	binary.LittleEndian.PutUint32(respBody[32:36], connInfo.Handler.MaxReadSize)
	// MaxWriteSize
	binary.LittleEndian.PutUint32(respBody[36:40], connInfo.Handler.MaxWriteSize)
	// SystemTime
	binary.LittleEndian.PutUint64(respBody[40:48], types.TimeToFiletime(time.Now()))
	// ServerStartTime
	binary.LittleEndian.PutUint64(respBody[48:56], types.TimeToFiletime(connInfo.Handler.StartTime))
	// SecurityBufferOffset: offset from start of SMB2 header = 64 (header) + 64 (fixed body) = 128
	binary.LittleEndian.PutUint16(respBody[56:58], 128)
	// SecurityBufferLength: 0
	binary.LittleEndian.PutUint16(respBody[58:60], 0)
	// NegotiateContextOffset: 0
	binary.LittleEndian.PutUint32(respBody[60:64], 0)

	// Send the response
	return SendRawMessage(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, respHeader, respBody)
}

// TrackSessionLifecycle tracks session creation/deletion for connection cleanup.
// This ensures proper cleanup when connections close ungracefully.
func TrackSessionLifecycle(command types.Command, reqSessionID, ctxSessionID uint64, status types.Status, tracker SessionTracker) {
	if tracker == nil {
		return
	}

	switch command {
	case types.SMB2SessionSetup:
		// Track newly created sessions on successful SESSION_SETUP completion.
		if status == types.StatusSuccess {
			sessionIDToTrack := ctxSessionID
			if sessionIDToTrack == 0 {
				sessionIDToTrack = reqSessionID
			}
			if sessionIDToTrack != 0 {
				tracker.TrackSession(sessionIDToTrack)
			}
		}
	case types.SMB2Logoff:
		// Untrack sessions on LOGOFF (they are already cleaned up by the handler)
		if status == types.StatusSuccess && reqSessionID != 0 {
			tracker.UntrackSession(reqSessionID)
		}
	}
}

// MakeErrorBody creates a minimal error response body per MS-SMB2 spec.
// Error response body (8 bytes minimum):
// StructureSize (2) + ErrorContextCount (1) + Reserved (1) + ByteCount (4)
func MakeErrorBody() []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint16(body[0:2], 9) // StructureSize
	body[2] = 0                                 // ErrorContextCount
	body[3] = 0                                 // Reserved
	binary.LittleEndian.PutUint32(body[4:8], 0) // ByteCount
	return body
}
