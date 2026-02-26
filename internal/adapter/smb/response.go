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

	cmd, handlerCtx, errStatus := prepareDispatch(ctx, reqHeader, connInfo)
	if errStatus != 0 {
		return SendErrorResponse(reqHeader, errStatus, connInfo)
	}

	// For CHANGE_NOTIFY, set up async callback so notifications can be sent
	if reqHeader.Command == types.SMB2ChangeNotify && asyncNotifyCallback != nil {
		handlerCtx.AsyncNotifyCallback = asyncNotifyCallback
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageId", reqHeader.MessageID,
		"client", handlerCtx.ClientAddr)

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

// prepareDispatch looks up the command in the dispatch table, builds the handler context,
// and validates session/tree requirements. Returns the command, context, and an error
// status (0 on success). This consolidates the shared setup logic used by both
// ProcessSingleRequest and ProcessRequestWithFileID.
func prepareDispatch(ctx context.Context, reqHeader *header.SMB2Header, connInfo *ConnInfo) (*Command, *handlers.SMBHandlerContext, types.Status) {
	cmd, ok := DispatchTable[reqHeader.Command]
	if !ok {
		logger.Debug("Unknown SMB2 command", "command", reqHeader.Command)
		return nil, nil, types.StatusNotSupported
	}

	handlerCtx := handlers.NewSMBHandlerContext(
		ctx,
		connInfo.Conn.RemoteAddr().String(),
		reqHeader.SessionID,
		reqHeader.TreeID,
		reqHeader.MessageID,
	)

	if cmd.NeedsSession && reqHeader.SessionID != 0 {
		sess, ok := connInfo.Handler.GetSession(reqHeader.SessionID)
		if !ok {
			return nil, nil, types.StatusUserSessionDeleted
		}
		handlerCtx.IsGuest = sess.IsGuest
		handlerCtx.Username = sess.Username
	}

	if cmd.NeedsTree && reqHeader.TreeID != 0 {
		tree, ok := connInfo.Handler.GetTree(reqHeader.TreeID)
		if !ok {
			return nil, nil, types.StatusNetworkNameDeleted
		}
		handlerCtx.ShareName = tree.ShareName
	}

	return cmd, handlerCtx, 0
}

// ProcessRequestWithFileID processes a request and returns the FileID if applicable (for CREATE).
// Used in compound request processing where FileID propagation is needed.
// Also returns the handler context so callers (compound processing) can pass
// handler-populated fields (e.g. SessionID from SESSION_SETUP, TreeID from
// TREE_CONNECT) through to SendResponse.
func ProcessRequestWithFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, connInfo *ConnInfo) (*HandlerResult, [16]byte, *handlers.SMBHandlerContext) {
	var fileID [16]byte

	cmd, handlerCtx, errStatus := prepareDispatch(ctx, reqHeader, connInfo)
	if errStatus != 0 {
		return &HandlerResult{Status: errStatus, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageId", reqHeader.MessageID,
		"client", handlerCtx.ClientAddr)

	result, err := cmd.Handler(handlerCtx, connInfo.Handler, connInfo.Handler.Registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return &HandlerResult{Status: types.StatusInternalError, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	// Track session lifecycle for connection cleanup
	TrackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status, connInfo.SessionTracker)

	// Extract FileID from CREATE response (bytes 64-80)
	if reqHeader.Command == types.SMB2Create && result.Status == types.StatusSuccess && len(result.Data) >= 80 {
		copy(fileID[:], result.Data[64:80])
	}

	return result, fileID, handlerCtx
}

// ProcessRequestWithInheritedFileID processes a request using an inherited FileID.
// InjectFileID is a no-op for commands that do not use a FileID, so no pre-filtering is needed.
func ProcessRequestWithInheritedFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, inheritedFileID [16]byte, connInfo *ConnInfo) (*HandlerResult, *handlers.SMBHandlerContext) {
	body = InjectFileID(reqHeader.Command, body, inheritedFileID)
	result, _, handlerCtx := ProcessRequestWithFileID(ctx, reqHeader, body, connInfo)
	return result, handlerCtx
}

// SendResponse sends an SMB2 response with credit management and signing.
func SendResponse(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) error {
	// Use session manager for adaptive credit grants
	sessionID := reqHeader.SessionID
	if ctx != nil && ctx.SessionID != 0 {
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
	if ctx != nil && ctx.SessionID != 0 && reqHeader.SessionID == 0 {
		respHeader.SessionID = ctx.SessionID
	}

	// Update TreeID in response if it was set by handler (TREE_CONNECT)
	if ctx != nil && ctx.TreeID != 0 && reqHeader.TreeID == 0 {
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

	// Build SMB2 NEGOTIATE response header.
	// When responding to SMB1 NEGOTIATE, we use a special SMB2 header format.
	// Fields not explicitly set below are zero (CreditCharge, NextCommand,
	// MessageID, Reserved, TreeID, SessionID, Signature).
	respHeader := make([]byte, header.HeaderSize)
	binary.LittleEndian.PutUint32(respHeader[0:4], types.SMB2ProtocolID)           // Protocol ID
	binary.LittleEndian.PutUint16(respHeader[4:6], header.HeaderSize)              // Structure Size: 64
	binary.LittleEndian.PutUint32(respHeader[8:12], uint32(types.StatusSuccess))   // Status
	binary.LittleEndian.PutUint16(respHeader[12:14], uint16(types.SMB2Negotiate))  // Command
	binary.LittleEndian.PutUint16(respHeader[14:16], 1)                            // Credits: 1
	binary.LittleEndian.PutUint32(respHeader[16:20], types.SMB2FlagsServerToRedir) // Flags: response

	// Build NEGOTIATE response body (65 bytes structure).
	// Fields not explicitly set are zero (Reserved, NegotiateContextCount,
	// Capabilities, SecurityBufferLength, NegotiateContextOffset).
	respBody := make([]byte, 65)
	binary.LittleEndian.PutUint16(respBody[0:2], 65) // StructureSize

	// SecurityMode: set based on signing configuration [MS-SMB2 2.2.4]
	if connInfo.Handler.SigningConfig.Enabled {
		respBody[2] |= 0x01 // SMB2_NEGOTIATE_SIGNING_ENABLED
	}
	if connInfo.Handler.SigningConfig.Required {
		respBody[2] |= 0x02 // SMB2_NEGOTIATE_SIGNING_REQUIRED
	}

	binary.LittleEndian.PutUint16(respBody[4:6], uint16(types.SMB2Dialect0202)) // DialectRevision: SMB 2.0.2
	copy(respBody[8:24], connInfo.Handler.ServerGUID[:])                        // ServerGUID
	binary.LittleEndian.PutUint32(respBody[28:32], connInfo.Handler.MaxTransactSize)
	binary.LittleEndian.PutUint32(respBody[32:36], connInfo.Handler.MaxReadSize)
	binary.LittleEndian.PutUint32(respBody[36:40], connInfo.Handler.MaxWriteSize)
	binary.LittleEndian.PutUint64(respBody[40:48], types.TimeToFiletime(time.Now()))                 // SystemTime
	binary.LittleEndian.PutUint64(respBody[48:56], types.TimeToFiletime(connInfo.Handler.StartTime)) // ServerStartTime
	binary.LittleEndian.PutUint16(respBody[56:58], 128)                                              // SecurityBufferOffset: 64 (header) + 64 (fixed body)

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

// MakeErrorBody creates a minimal error response body per MS-SMB2 2.2.2.
// Layout (9 bytes): StructureSize (2) + ErrorContextCount (1) + Reserved (1) + ByteCount (4) + ErrorData (1 padding).
func MakeErrorBody() []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint16(body[0:2], 9) // StructureSize
	return body
}
