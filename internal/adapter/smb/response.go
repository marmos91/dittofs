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
//   - rawMessage: complete raw SMB2 message bytes (header + body) for hooks
//   - connInfo: connection context for dispatch
//   - isEncrypted: whether the request was received inside an SMB3 Transform Header
//   - asyncNotifyCallback: optional callback for CHANGE_NOTIFY async responses (nil = no async)
func ProcessSingleRequest(
	ctx context.Context,
	reqHeader *header.SMB2Header,
	body []byte,
	rawMessage []byte,
	connInfo *ConnInfo,
	isEncrypted bool,
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

	// Run before-hooks (e.g., preauth hash update for NEGOTIATE request)
	RunBeforeHooks(connInfo, reqHeader.Command, rawMessage)

	cmd, handlerCtx, errStatus := prepareDispatch(ctx, reqHeader, connInfo)
	if errStatus != 0 {
		return SendErrorResponse(reqHeader, errStatus, connInfo)
	}

	handlerCtx.RequestEncrypted = isEncrypted

	// Per MS-SMB2 3.3.5.2.1: enforce encryption requirements.
	if errStatus := checkEncryptionRequired(reqHeader, handlerCtx, connInfo, isEncrypted); errStatus != 0 {
		return SendErrorResponse(reqHeader, errStatus, connInfo)
	}

	// For CHANGE_NOTIFY, set up async callback so notifications can be sent
	if reqHeader.Command == types.SMB2ChangeNotify && asyncNotifyCallback != nil {
		handlerCtx.AsyncNotifyCallback = asyncNotifyCallback
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageID", reqHeader.MessageID,
		"client", handlerCtx.ClientAddr)

	// Execute handler
	result, err := cmd.Handler(handlerCtx, connInfo.Handler, connInfo.Handler.Registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return SendErrorResponse(reqHeader, types.StatusInternalError, connInfo)
	}

	// Per [MS-SMB2] 3.3.5.16: CANCEL must not send a response.
	// Handlers return nil result to indicate "no response should be sent".
	// Only Cancel is expected to return nil; other handlers must always return a result.
	if result == nil {
		if reqHeader.Command != types.SMB2Cancel {
			logger.Warn("Handler returned nil result for non-CANCEL command",
				"command", cmd.Name, "client", handlerCtx.ClientAddr)
		}
		return nil
	}

	// DropConnection: close TCP without sending a response.
	// Used for fatal protocol violations (e.g., VALIDATE_NEGOTIATE failure).
	if result.DropConnection {
		logger.Debug("Handler requested connection drop",
			"command", cmd.Name, "client", handlerCtx.ClientAddr)
		return connInfo.Conn.Close()
	}

	// Track session lifecycle for connection cleanup
	TrackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status, connInfo.SessionTracker)

	// Send response and run after-hooks with the response bytes
	if err := SendResponseWithHooks(reqHeader, handlerCtx, result, connInfo); err != nil {
		return err
	}

	// Deferred session delete: LOGOFF sets this so the session stays alive for
	// response signing/encryption, then we delete it here after sending.
	if handlerCtx.DeferredSessionDelete != 0 {
		connInfo.Handler.DeleteSession(handlerCtx.DeferredSessionDelete)
	}

	return nil
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

	// Populate CryptoState so handlers (e.g., NEGOTIATE) can store
	// negotiation parameters on the connection.
	handlerCtx.ConnCryptoState = connInfo.CryptoState

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
func ProcessRequestWithFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, connInfo *ConnInfo, isEncrypted bool) (*HandlerResult, [16]byte, *handlers.SMBHandlerContext) {
	var fileID [16]byte

	cmd, handlerCtx, errStatus := prepareDispatch(ctx, reqHeader, connInfo)
	if errStatus != 0 {
		return &HandlerResult{Status: errStatus, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	handlerCtx.RequestEncrypted = isEncrypted

	// Per MS-SMB2 3.3.5.2.1: enforce encryption requirements.
	if errStatus := checkEncryptionRequired(reqHeader, handlerCtx, connInfo, isEncrypted); errStatus != 0 {
		return &HandlerResult{Status: errStatus, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageID", reqHeader.MessageID,
		"client", handlerCtx.ClientAddr)

	result, err := cmd.Handler(handlerCtx, connInfo.Handler, connInfo.Handler.Registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return &HandlerResult{Status: types.StatusInternalError, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	// Per [MS-SMB2] 3.3.5.16: CANCEL must not send a response.
	// Return a nil result to signal "no response" to the caller.
	if result == nil {
		return nil, fileID, handlerCtx
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
func ProcessRequestWithInheritedFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, inheritedFileID [16]byte, connInfo *ConnInfo, isEncrypted bool) (*HandlerResult, *handlers.SMBHandlerContext) {
	body = InjectFileID(reqHeader.Command, body, inheritedFileID)
	result, _, handlerCtx := ProcessRequestWithFileID(ctx, reqHeader, body, connInfo, isEncrypted)
	return result, handlerCtx
}

// checkEncryptionRequired enforces global and per-share encryption requirements.
// Per MS-SMB2 3.3.5.2.1: if the server requires encryption (globally or for the
// tree's share) and the request was not encrypted, return STATUS_ACCESS_DENIED.
// NEGOTIATE and SESSION_SETUP are exempt because encryption keys are not yet available.
func checkEncryptionRequired(reqHeader *header.SMB2Header, handlerCtx *handlers.SMBHandlerContext, connInfo *ConnInfo, isEncrypted bool) types.Status {
	if isEncrypted {
		return 0
	}

	// NEGOTIATE and SESSION_SETUP are always allowed unencrypted
	if reqHeader.Command == types.SMB2Negotiate || reqHeader.Command == types.SMB2SessionSetup {
		return 0
	}

	// Global encryption enforcement: when mode is "required", all post-session-setup
	// messages must be encrypted.
	if connInfo.Handler.EncryptionConfig.Mode == "required" && reqHeader.SessionID != 0 {
		logger.Debug("Rejecting unencrypted request: global encryption required",
			"command", reqHeader.Command.String(),
			"sessionID", reqHeader.SessionID)
		return types.StatusAccessDenied
	}

	// Per-share encryption enforcement: if the tree was connected to a share
	// with EncryptData=true, all requests on that tree must be encrypted.
	if reqHeader.TreeID != 0 {
		if tree, ok := connInfo.Handler.GetTree(reqHeader.TreeID); ok && tree.EncryptData {
			logger.Debug("Rejecting unencrypted request: share requires encryption",
				"command", reqHeader.Command.String(),
				"treeID", reqHeader.TreeID,
				"shareName", tree.ShareName)
			return types.StatusAccessDenied
		}
	}

	return 0
}

// SendResponseWithHooks sends an SMB2 response and runs after-hooks with the
// full response bytes. This is used by ProcessSingleRequest where hooks need
// the raw response bytes (e.g., preauth integrity hash chain).
func SendResponseWithHooks(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) error {
	respHeader, body := buildResponseHeaderAndBody(reqHeader, ctx, result, connInfo)

	if err := SendMessage(respHeader, body, connInfo); err != nil {
		return err
	}

	// Build raw response bytes for after-hooks (e.g., preauth integrity hash).
	rawResponse := append(respHeader.Encode(), body...)
	RunAfterHooks(connInfo, reqHeader.Command, rawResponse)
	return nil
}

// SendResponse sends an SMB2 response with credit management and signing.
func SendResponse(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) error {
	respHeader, body := buildResponseHeaderAndBody(reqHeader, ctx, result, connInfo)
	return SendMessage(respHeader, body, connInfo)
}

// buildResponseHeaderAndBody constructs the response header and body from a
// handler result. This consolidates credit granting, session/tree ID
// propagation, and error body replacement that SendResponse and
// SendResponseWithHooks both need.
func buildResponseHeaderAndBody(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) (*header.SMB2Header, []byte) {
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

	// Per MS-SMB2 2.2.2: If the Status field indicates an error or warning,
	// the response MUST use the ERROR Response format (9 bytes), not the
	// command-specific response body. This applies to both error statuses
	// (severity 11, 0xC0xxxxxx) and warning statuses (severity 10,
	// 0x80xxxxxx) like STATUS_NO_MORE_FILES.
	//
	// Exceptions where non-success statuses carry meaningful data:
	//   - StatusMoreProcessingRequired: SPNEGO challenge token in SESSION_SETUP
	//   - StatusBufferOverflow: truncated data in QUERY_INFO
	body := result.Data
	if (result.Status.IsError() || result.Status.IsWarning()) &&
		result.Status != types.StatusMoreProcessingRequired &&
		result.Status != types.StatusBufferOverflow {
		body = MakeErrorBody()
	}

	return respHeader, body
}

// SendErrorResponse sends an SMB2 error response.
// Per user decision: all responses on encrypted sessions are encrypted,
// including error responses.
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

// SendMessage sends an SMB2 message with NetBIOS framing, optional encryption,
// and optional signing.
//
// Per MS-SMB2 3.3.4.1.1: encrypted sessions use AEAD instead of signing.
func SendMessage(hdr *header.SMB2Header, body []byte, connInfo *ConnInfo) error {
	smbPayload := append(hdr.Encode(), body...)

	if hdr.SessionID != 0 {
		if sess, ok := connInfo.Handler.GetSession(hdr.SessionID); ok {
			// Per MS-SMB2 3.3.5.5.3: the initial SESSION_SETUP response that
			// establishes a NEW session MUST NOT be encrypted. The client has not
			// yet derived encryption keys at this point — it needs the unencrypted
			// response to complete key derivation. Only sign the response instead.
			//
			// For re-authentication (SESSION_SETUP on an existing session), the
			// client already has encryption keys, so the response MUST be encrypted.
			// We distinguish the two cases via sess.NewlyCreated, which is true only
			// for sessions just created during this SESSION_SETUP exchange.
			isNewSessionSetup := hdr.Command == types.SMB2SessionSetup && hdr.Status == types.StatusSuccess && sess.NewlyCreated
			if isNewSessionSetup {
				sess.NewlyCreated = false // Clear so subsequent messages get encrypted
			}
			if sess.ShouldEncrypt() && connInfo.EncryptionMiddleware != nil && !isNewSessionSetup {
				encrypted, err := connInfo.EncryptionMiddleware.EncryptResponse(hdr.SessionID, smbPayload)
				if err != nil {
					return fmt.Errorf("encrypt response: %w", err)
				}
				logger.Debug("Encrypted outgoing SMB2 message",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
				return WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, encrypted)
			}
			if sess.ShouldSign() {
				sess.SignMessage(smbPayload)
				// Sync signature back so callers (e.g. SendResponseWithHooks)
				// that re-encode the header get the real signature for preauth
				// integrity hash computation per MS-SMB2 3.3.5.5.
				copy(hdr.Signature[:], smbPayload[48:64])
				logger.Debug("Signed outgoing SMB2 message",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
			}
		}
	}

	logger.Debug("Sent SMB2 response",
		"command", hdr.Command.String(),
		"status", hdr.Status.String(),
		"messageID", hdr.MessageID,
		"bytes", len(smbPayload))

	return WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, smbPayload)
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
//
// Per MS-SMB2 §3.3.5.3:
//   - If the client offered "SMB 2.???" → respond with DialectRevision 0x02FF (§3.3.5.3.2)
//   - If the client offered only "SMB 2.002" → respond with DialectRevision 0x0202 (§3.3.5.3.1)
func HandleSMB1Negotiate(connInfo *ConnInfo, message []byte) error {
	logger.Debug("Received SMB1 NEGOTIATE, responding with SMB2 upgrade",
		"address", connInfo.Conn.RemoteAddr().String())

	// Determine response dialect by parsing SMB1 NEGOTIATE dialect strings.
	// SMB1 header is 32 bytes, then: WordCount (1) + ByteCount (2) + dialects.
	// Each dialect: BufferFormat (0x02) + null-terminated ASCII string.
	responseDialect := types.SMB2Dialect0202 // default: "SMB 2.002" only
	if len(message) > 35 {
		dialects := message[35:]
		for len(dialects) > 1 {
			if dialects[0] != 0x02 {
				break
			}
			dialects = dialects[1:]
			// Find null terminator; if absent the dialect string is malformed so skip it.
			end := 0
			for end < len(dialects) && dialects[end] != 0 {
				end++
			}
			if end >= len(dialects) {
				// Unterminated dialect string — stop parsing.
				break
			}
			name := string(dialects[:end])
			if name == "SMB 2.???" {
				responseDialect = types.SMB2DialectWild
			}
			dialects = dialects[end+1:] // skip past null terminator
		}
	}

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

	binary.LittleEndian.PutUint16(respBody[4:6], uint16(responseDialect))
	copy(respBody[8:24], connInfo.Handler.ServerGUID[:]) // ServerGUID
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
