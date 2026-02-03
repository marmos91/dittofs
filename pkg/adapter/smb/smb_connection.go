package smb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/bufpool"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb"
	"github.com/marmos91/dittofs/internal/protocol/smb/header"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/internal/protocol/smb/v2/handlers"
)

// SMBConnection handles a single SMB2 client connection.
type SMBConnection struct {
	server *SMBAdapter
	conn   net.Conn

	// Concurrent request handling
	requestSem chan struct{}  // Semaphore to limit concurrent requests
	wg         sync.WaitGroup // Track active requests for graceful shutdown
	writeMu    sync.Mutex     // Protects connection writes (replies must be serialized)

	// Session tracking for cleanup on disconnect
	sessionsMu sync.Mutex          // Protects sessions map
	sessions   map[uint64]struct{} // Sessions created on this connection
}

// NewSMBConnection creates a new SMB connection handler.
func NewSMBConnection(server *SMBAdapter, conn net.Conn) *SMBConnection {
	return &SMBConnection{
		server:     server,
		conn:       conn,
		requestSem: make(chan struct{}, server.config.MaxRequestsPerConnection),
		sessions:   make(map[uint64]struct{}),
	}
}

// TrackSession records a session as belonging to this connection.
// Called when SESSION_SETUP completes successfully.
func (c *SMBConnection) TrackSession(sessionID uint64) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	c.sessions[sessionID] = struct{}{}
	logger.Debug("Tracking session on connection",
		"sessionID", sessionID,
		"address", c.conn.RemoteAddr().String())
}

// UntrackSession removes a session from this connection's tracking.
// Called when LOGOFF is processed.
func (c *SMBConnection) UntrackSession(sessionID uint64) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	delete(c.sessions, sessionID)
	logger.Debug("Untracking session from connection",
		"sessionID", sessionID,
		"address", c.conn.RemoteAddr().String())
}

// Serve handles all SMB2 requests for this connection.
//
// It implements panic recovery to prevent a single misbehaving connection
// from crashing the entire server.
//
// The connection is automatically closed when:
// - The context is cancelled (server shutdown)
// - An idle timeout occurs
// - A read or write timeout occurs
// - An unrecoverable error occurs
// - The client closes the connection
func (c *SMBConnection) Serve(ctx context.Context) {
	defer c.handleConnectionClose()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New SMB connection", "address", clientAddr)

	// Set initial idle timeout
	if c.server.config.Timeouts.Idle > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
			logger.Warn("Failed to set deadline", "address", clientAddr, "error", err)
		}
	}

	for {
		// Check for context cancellation before processing next request
		select {
		case <-ctx.Done():
			logger.Debug("SMB connection closed due to context cancellation", "address", clientAddr)
			return
		case <-c.server.shutdown:
			logger.Debug("SMB connection closed due to server shutdown", "address", clientAddr)
			return
		default:
		}

		// Read and process the request
		hdr, body, remainingCompound, err := c.readRequest(ctx)
		if err != nil {
			if err == io.EOF {
				logger.Debug("SMB connection closed by client", "address", clientAddr)
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("SMB connection timed out", "address", clientAddr, "error", err)
			} else if err == context.Canceled || err == context.DeadlineExceeded {
				logger.Debug("SMB connection cancelled", "address", clientAddr, "error", err)
			} else {
				logger.Debug("Error reading SMB request", "address", clientAddr, "error", err)
			}
			return
		}

		// Check if this is the start of a compound request
		isCompoundStart := len(remainingCompound) > 0

		if isCompoundStart {
			// Process compound requests sequentially (required for related operations)
			c.requestSem <- struct{}{}
			c.wg.Add(1)

			// Copy compound data to avoid races (goroutine owns this copy)
			compoundData := make([]byte, len(remainingCompound))
			copy(compoundData, remainingCompound)

			go func() {
				defer c.handleRequestPanic(clientAddr, hdr.MessageID)
				c.processCompoundRequest(ctx, hdr, body, compoundData)
			}()
		} else {
			// Process single request
			c.requestSem <- struct{}{}
			c.wg.Add(1)

			go func(reqHeader *header.SMB2Header, reqBody []byte) {
				defer c.handleRequestPanic(clientAddr, reqHeader.MessageID)

				if err := c.processRequest(ctx, reqHeader, reqBody); err != nil {
					logger.Debug("Error processing SMB request", "address", clientAddr, "messageId", reqHeader.MessageID, "error", err)
				}
			}(hdr, body)
		}

		// Reset idle timeout after reading request
		if c.server.config.Timeouts.Idle > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
				logger.Warn("Failed to reset deadline", "address", clientAddr, "error", err)
			}
		}
	}
}

// readRequest reads a complete SMB2 message from the connection.
//
// SMB2 messages are framed with a 4-byte NetBIOS session header containing
// the message length, followed by the SMB2 header (64 bytes) and body.
// For compound requests, remainingCompound contains the bytes after the first command.
func (c *SMBConnection) readRequest(ctx context.Context) (*header.SMB2Header, []byte, []byte, error) {
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	default:
	}

	// Apply read timeout if configured
	if c.server.config.Timeouts.Read > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Read)
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, nil, nil, fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read NetBIOS session header (4 bytes)
	// Format: 1 byte type (0x00 for session message) + 3 bytes length (big-endian)
	var nbHeader [4]byte
	if _, err := io.ReadFull(c.conn, nbHeader[:]); err != nil {
		return nil, nil, nil, err
	}

	// Parse NetBIOS length (24-bit big-endian)
	msgLen := uint32(nbHeader[1])<<16 | uint32(nbHeader[2])<<8 | uint32(nbHeader[3])

	// Validate message size (configurable via SMBConfig.MaxMessageSize)
	if msgLen > uint32(c.server.config.MaxMessageSize) {
		return nil, nil, nil, fmt.Errorf("SMB message too large: %d bytes (max %d)", msgLen, c.server.config.MaxMessageSize)
	}

	// SMB messages must be at least 4 bytes to read the protocol ID.
	// SMB1 header is 32 bytes, SMB2 header is 64 bytes.
	// We defer the full size check until after we know the protocol version.
	const minProtocolIDSize = 4
	if msgLen < minProtocolIDSize {
		return nil, nil, nil, fmt.Errorf("SMB message too small: %d bytes", msgLen)
	}

	// Check context before reading potentially large message
	select {
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	default:
	}

	// Read the entire SMB message
	message := make([]byte, msgLen)
	if _, err := io.ReadFull(c.conn, message); err != nil {
		return nil, nil, nil, fmt.Errorf("read SMB message: %w", err)
	}

	// Check if this is SMB1 (legacy negotiate) - needs upgrade to SMB2
	// SMB1 messages can be smaller than 64 bytes (SMB1 header is 32 bytes)
	protocolID := binary.LittleEndian.Uint32(message[0:4])
	if protocolID == types.SMB1ProtocolID {
		// Handle SMB1 NEGOTIATE by responding with SMB2 NEGOTIATE response
		if err := c.handleSMB1Negotiate(ctx, message); err != nil {
			return nil, nil, nil, fmt.Errorf("handle SMB1 negotiate: %w", err)
		}
		// Read the next message which should be SMB2
		return c.readRequest(ctx)
	}

	// For SMB2, validate that we have at least a full header (64 bytes)
	if msgLen < header.HeaderSize {
		return nil, nil, nil, fmt.Errorf("SMB2 message too small: %d bytes (need %d)", msgLen, header.HeaderSize)
	}

	// Parse SMB2 header
	hdr, err := header.Parse(message[:header.HeaderSize])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse SMB2 header: %w", err)
	}

	// Verify message signature if required
	// Skip verification for messages without a session (SessionID == 0)
	// and for NEGOTIATE/SESSION_SETUP which may not have signing set up yet.
	//
	// Per MS-SMB2 3.3.5.2.4: Only verify if:
	// - Session requires signing (SigningRequired), OR
	// - The message has SMB2_FLAGS_SIGNED set
	//
	// If signing is enabled but not required, and the client doesn't sign,
	// we accept the unsigned message (signing is optional).
	if hdr.SessionID != 0 && hdr.Command != types.SMB2Negotiate && hdr.Command != types.SMB2SessionSetup {
		if sess, ok := c.server.handler.GetSession(hdr.SessionID); ok {
			// Check if message is signed (SMB2_FLAGS_SIGNED = 0x00000008)
			isSigned := hdr.Flags.IsSigned()

			if sess.Signing != nil && sess.Signing.SigningRequired && !isSigned {
				// Signing required but message not signed - reject
				logger.Warn("SMB2 message not signed but signing required",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID,
					"client", c.conn.RemoteAddr().String())
				return nil, nil, nil, fmt.Errorf("STATUS_ACCESS_DENIED: message not signed")
			}

			if isSigned && sess.ShouldVerify() {
				// Message is signed - verify it
				logger.Debug("Verifying incoming SMB2 message signature",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID,
					"messageLen", len(message))
				if !sess.VerifyMessage(message) {
					logger.Warn("SMB2 message signature verification failed",
						"command", hdr.Command.String(),
						"sessionID", hdr.SessionID,
						"client", c.conn.RemoteAddr().String())
					return nil, nil, nil, fmt.Errorf("STATUS_ACCESS_DENIED: signature verification failed")
				}
				logger.Debug("Verified incoming SMB2 message signature",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
			} else if !isSigned {
				// Message not signed and signing not required - accept
				logger.Debug("Accepting unsigned message (signing not required)",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
			}
		}
	}

	// For compound requests, extract only the body for this command
	var body []byte
	var remainingCompound []byte
	if hdr.NextCommand > 0 {
		// Body is from after header to next command offset
		bodyEnd := int(hdr.NextCommand)
		if bodyEnd > len(message) {
			bodyEnd = len(message)
		}
		body = message[header.HeaderSize:bodyEnd]
		// Return remaining compound bytes
		if int(hdr.NextCommand) < len(message) {
			remainingCompound = message[hdr.NextCommand:]
			logger.Debug("Compound request detected",
				"remainingBytes", len(remainingCompound))
		}
	} else {
		// Last or only command - body is everything after header
		body = message[header.HeaderSize:]
	}

	logger.Debug("SMB2 request",
		"command", hdr.Command.String(),
		"messageId", hdr.MessageID,
		"sessionId", fmt.Sprintf("0x%x", hdr.SessionID),
		"treeId", hdr.TreeID,
		"nextCommand", hdr.NextCommand,
		"flags", fmt.Sprintf("0x%x", hdr.Flags))

	return hdr, body, remainingCompound, nil
}

// parseCompoundCommand parses the next command from compound data.
// Returns header, body, remaining data, and error.
func parseCompoundCommand(data []byte) (*header.SMB2Header, []byte, []byte, error) {
	if len(data) < header.HeaderSize {
		return nil, nil, nil, fmt.Errorf("compound data too small: %d bytes", len(data))
	}

	// Parse SMB2 header
	hdr, err := header.Parse(data[:header.HeaderSize])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse compound SMB2 header: %w", err)
	}

	// Extract body for this command
	var body []byte
	var remaining []byte
	if hdr.NextCommand > 0 {
		bodyEnd := int(hdr.NextCommand)
		if bodyEnd > len(data) {
			bodyEnd = len(data)
		}
		body = data[header.HeaderSize:bodyEnd]
		// Return remaining data
		if int(hdr.NextCommand) < len(data) {
			remaining = data[hdr.NextCommand:]
		}
	} else {
		// Last command in compound
		body = data[header.HeaderSize:]
	}

	logger.Debug("SMB2 compound request",
		"command", hdr.Command.String(),
		"messageId", hdr.MessageID,
		"sessionId", fmt.Sprintf("0x%x", hdr.SessionID),
		"treeId", hdr.TreeID,
		"nextCommand", hdr.NextCommand,
		"flags", fmt.Sprintf("0x%x", hdr.Flags),
		"isRelated", hdr.IsRelated())

	return hdr, body, remaining, nil
}

// processCompoundRequest processes all commands in a compound request sequentially.
// Related operations share FileID from the previous response.
// compoundData contains the remaining commands after the first one.
func (c *SMBConnection) processCompoundRequest(ctx context.Context, firstHeader *header.SMB2Header, firstBody []byte, compoundData []byte) {
	// Track the last FileID for related operations
	var lastFileID [16]byte
	lastSessionID := firstHeader.SessionID
	lastTreeID := firstHeader.TreeID

	// Process first command
	logger.Debug("Processing compound request - first command",
		"command", firstHeader.Command.String(),
		"messageId", firstHeader.MessageID)

	result, fileID := c.processRequestWithFileID(ctx, firstHeader, firstBody)
	if fileID != [16]byte{} {
		lastFileID = fileID
	}
	if err := c.sendResponse(firstHeader, &handlers.SMBHandlerContext{SessionID: lastSessionID, TreeID: lastTreeID}, result); err != nil {
		logger.Debug("Error sending compound response", "error", err)
	}

	// Process remaining commands from compound data
	remaining := compoundData
	for len(remaining) >= header.HeaderSize {
		hdr, body, nextRemaining, err := parseCompoundCommand(remaining)
		if err != nil {
			logger.Debug("Error parsing compound command", "error", err)
			break
		}
		remaining = nextRemaining

		// Handle related operations - inherit IDs from previous command
		if hdr.IsRelated() {
			if hdr.SessionID == 0 {
				hdr.SessionID = lastSessionID
			}
			if hdr.TreeID == 0 {
				hdr.TreeID = lastTreeID
			}
		}

		logger.Debug("Processing compound request - command",
			"command", hdr.Command.String(),
			"messageId", hdr.MessageID,
			"isRelated", hdr.IsRelated(),
			"usingFileID", lastFileID != [16]byte{})

		// Process with the inherited FileID for related operations
		var result *smb.HandlerResult
		if hdr.IsRelated() && lastFileID != [16]byte{} {
			result = c.processRequestWithInheritedFileID(ctx, hdr, body, lastFileID)
		} else {
			var fileID [16]byte
			result, fileID = c.processRequestWithFileID(ctx, hdr, body)
			if fileID != [16]byte{} {
				lastFileID = fileID
			}
		}

		// Update tracking
		lastSessionID = hdr.SessionID
		lastTreeID = hdr.TreeID

		if err := c.sendResponse(hdr, &handlers.SMBHandlerContext{SessionID: lastSessionID, TreeID: lastTreeID}, result); err != nil {
			logger.Debug("Error sending compound response", "error", err)
		}
	}
}

// processRequestWithFileID processes a request and returns the FileID if applicable (for CREATE).
func (c *SMBConnection) processRequestWithFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte) (*smb.HandlerResult, [16]byte) {
	var fileID [16]byte

	clientAddr := c.conn.RemoteAddr().String()

	cmd, ok := smb.DispatchTable[reqHeader.Command]
	if !ok {
		logger.Debug("Unknown SMB2 command", "command", reqHeader.Command)
		return &smb.HandlerResult{Status: types.StatusNotSupported, Data: makeErrorBody()}, fileID
	}

	handlerCtx := &handlers.SMBHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		SessionID:  reqHeader.SessionID,
		TreeID:     reqHeader.TreeID,
		MessageID:  reqHeader.MessageID,
	}

	if cmd.NeedsSession && reqHeader.SessionID != 0 {
		session, ok := c.server.handler.GetSession(reqHeader.SessionID)
		if !ok {
			return &smb.HandlerResult{Status: types.StatusUserSessionDeleted, Data: makeErrorBody()}, fileID
		}
		handlerCtx.IsGuest = session.IsGuest
		handlerCtx.Username = session.Username
	}

	if cmd.NeedsTree && reqHeader.TreeID != 0 {
		tree, ok := c.server.handler.GetTree(reqHeader.TreeID)
		if !ok {
			return &smb.HandlerResult{Status: types.StatusNetworkNameDeleted, Data: makeErrorBody()}, fileID
		}
		handlerCtx.ShareName = tree.ShareName
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageId", reqHeader.MessageID,
		"client", clientAddr)

	result, err := cmd.Handler(handlerCtx, c.server.handler, c.server.registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return &smb.HandlerResult{Status: types.StatusInternalError, Data: makeErrorBody()}, fileID
	}

	// Track session lifecycle for connection cleanup
	c.trackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status)

	// Extract FileID from CREATE response (bytes 64-80)
	if reqHeader.Command == types.SMB2Create && result.Status == types.StatusSuccess && len(result.Data) >= 80 {
		copy(fileID[:], result.Data[64:80])
	}

	return result, fileID
}

// processRequestWithInheritedFileID processes a request using an inherited FileID.
func (c *SMBConnection) processRequestWithInheritedFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, inheritedFileID [16]byte) *smb.HandlerResult {
	// For commands that use FileID, inject the inherited FileID into the request body
	if reqHeader.Command == types.SMB2QueryInfo || reqHeader.Command == types.SMB2Close ||
		reqHeader.Command == types.SMB2Read || reqHeader.Command == types.SMB2Write ||
		reqHeader.Command == types.SMB2QueryDirectory || reqHeader.Command == types.SMB2SetInfo {
		body = c.injectFileID(reqHeader.Command, body, inheritedFileID)
	}

	result, _ := c.processRequestWithFileID(ctx, reqHeader, body)
	return result
}

// injectFileID injects a FileID into the appropriate position in the request body.
// Offsets are per [MS-SMB2] specification for each command.
func (c *SMBConnection) injectFileID(command types.Command, body []byte, fileID [16]byte) []byte {
	// Make a copy to avoid modifying the original
	newBody := make([]byte, len(body))
	copy(newBody, body)

	logger.Debug("Injecting FileID",
		"command", command.String(),
		"bodyLen", len(body),
		"fileID", fmt.Sprintf("%x", fileID))

	switch command {
	case types.SMB2QueryInfo:
		// FileId is at offset 24-40 in QUERY_INFO request [MS-SMB2] 2.2.37
		if len(newBody) >= 40 {
			copy(newBody[24:40], fileID[:])
			logger.Debug("Injected FileID into QUERY_INFO at offset 24")
		} else {
			logger.Debug("Body too small for QUERY_INFO FileID injection", "need", 40, "have", len(newBody))
		}
	case types.SMB2Close:
		// FileId is at offset 8-24 in CLOSE request [MS-SMB2] 2.2.15
		if len(newBody) >= 24 {
			copy(newBody[8:24], fileID[:])
			logger.Debug("Injected FileID into CLOSE at offset 8")
		} else {
			logger.Debug("Body too small for CLOSE FileID injection", "need", 24, "have", len(newBody))
		}
	case types.SMB2Read:
		// FileId is at offset 16-32 in READ request [MS-SMB2] 2.2.19
		if len(newBody) >= 32 {
			copy(newBody[16:32], fileID[:])
		}
	case types.SMB2Write:
		// FileId is at offset 16-32 in WRITE request [MS-SMB2] 2.2.21
		if len(newBody) >= 32 {
			copy(newBody[16:32], fileID[:])
		}
	case types.SMB2QueryDirectory:
		// FileId is at offset 8-24 in QUERY_DIRECTORY request [MS-SMB2] 2.2.33
		if len(newBody) >= 24 {
			copy(newBody[8:24], fileID[:])
			logger.Debug("Injected FileID into QUERY_DIRECTORY at offset 8")
		} else {
			logger.Debug("Body too small for QUERY_DIRECTORY FileID injection", "need", 24, "have", len(newBody))
		}
	case types.SMB2SetInfo:
		// FileId is at offset 16-32 in SET_INFO request [MS-SMB2] 2.2.39
		if len(newBody) >= 32 {
			copy(newBody[16:32], fileID[:])
			logger.Debug("Injected FileID into SET_INFO at offset 16")
		} else {
			logger.Debug("Body too small for SET_INFO FileID injection", "need", 32, "have", len(newBody))
		}
	}

	return newBody
}

// makeErrorBody creates a minimal error response body per MS-SMB2 spec.
// Error response body (8 bytes minimum):
// StructureSize (2) + ErrorContextCount (1) + Reserved (1) + ByteCount (4)
func makeErrorBody() []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint16(body[0:2], 9) // StructureSize
	body[2] = 0                                 // ErrorContextCount
	body[3] = 0                                 // Reserved
	binary.LittleEndian.PutUint32(body[4:8], 0) // ByteCount
	return body
}

// processRequest dispatches an SMB2 request to the appropriate handler.
func (c *SMBConnection) processRequest(ctx context.Context, reqHeader *header.SMB2Header, body []byte) error {
	// Check context before processing
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Track request for adaptive credit management
	c.server.sessionManager.RequestStarted(reqHeader.SessionID)
	defer c.server.sessionManager.RequestCompleted(reqHeader.SessionID)

	clientAddr := c.conn.RemoteAddr().String()

	// Look up command in dispatch table
	cmd, ok := smb.DispatchTable[reqHeader.Command]
	if !ok {
		logger.Debug("Unknown SMB2 command", "command", reqHeader.Command)
		return c.sendErrorResponse(reqHeader, types.StatusNotSupported)
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
	if reqHeader.Command == types.SMB2ChangeNotify {
		handlerCtx.AsyncNotifyCallback = func(sessionID, messageID uint64, response *handlers.ChangeNotifyResponse) error {
			return c.SendAsyncChangeNotifyResponse(sessionID, messageID, response)
		}
	}

	// Validate session if required
	if cmd.NeedsSession && reqHeader.SessionID != 0 {
		session, ok := c.server.handler.GetSession(reqHeader.SessionID)
		if !ok {
			return c.sendErrorResponse(reqHeader, types.StatusUserSessionDeleted)
		}
		handlerCtx.IsGuest = session.IsGuest
		handlerCtx.Username = session.Username
	}

	// Validate tree connection if required
	if cmd.NeedsTree && reqHeader.TreeID != 0 {
		tree, ok := c.server.handler.GetTree(reqHeader.TreeID)
		if !ok {
			return c.sendErrorResponse(reqHeader, types.StatusNetworkNameDeleted)
		}
		handlerCtx.ShareName = tree.ShareName
	}

	logger.Debug("Dispatching SMB2 command",
		"command", cmd.Name,
		"messageId", reqHeader.MessageID,
		"client", clientAddr)

	// Execute handler
	result, err := cmd.Handler(handlerCtx, c.server.handler, c.server.registry, body)
	if err != nil {
		logger.Debug("Handler error", "command", cmd.Name, "error", err)
		return c.sendErrorResponse(reqHeader, types.StatusInternalError)
	}

	// Track session lifecycle for connection cleanup
	c.trackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status)

	// Send response
	return c.sendResponse(reqHeader, handlerCtx, result)
}

// trackSessionLifecycle tracks session creation/deletion for connection cleanup.
// This ensures proper cleanup when connections close ungracefully.
func (c *SMBConnection) trackSessionLifecycle(command types.Command, reqSessionID, ctxSessionID uint64, status types.Status) {
	switch command {
	case types.SMB2SessionSetup:
		// Track newly created sessions on successful SESSION_SETUP completion.
		// Note: StatusMoreProcessingRequired indicates NTLM handshake in progress -
		// at that point only PendingAuth exists, not a real session. We only track
		// on StatusSuccess when the session is fully established.
		if status == types.StatusSuccess {
			// Prefer the session ID explicitly set by the handler (ctxSessionID),
			// but fall back to the request's SessionID when ctxSessionID is not set.
			// This handles NTLM Type 3 (AUTHENTICATE) completion where the session
			// is created with the pending auth's sessionID but ctx.SessionID may
			// not be explicitly set by all code paths.
			sessionIDToTrack := ctxSessionID
			if sessionIDToTrack == 0 {
				sessionIDToTrack = reqSessionID
			}
			if sessionIDToTrack != 0 {
				c.TrackSession(sessionIDToTrack)
			}
		}
	case types.SMB2Logoff:
		// Untrack sessions on LOGOFF (they are already cleaned up by the handler)
		if status == types.StatusSuccess && reqSessionID != 0 {
			c.UntrackSession(reqSessionID)
		}
	}
}

// sendResponse sends an SMB2 response.
// If the result indicates an error status and has no data, a proper error body is added.
func (c *SMBConnection) sendResponse(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *smb.HandlerResult) error {
	// Use session manager for adaptive credit grants
	sessionID := reqHeader.SessionID
	if ctx.SessionID != 0 {
		sessionID = ctx.SessionID
	}

	credits := c.server.sessionManager.GrantCredits(
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
		body = makeErrorBody()
	}

	return c.sendMessage(respHeader, body)
}

// sendErrorResponse sends an SMB2 error response.
func (c *SMBConnection) sendErrorResponse(reqHeader *header.SMB2Header, status types.Status) error {
	// Use session manager for adaptive credit grants
	credits := c.server.sessionManager.GrantCredits(
		reqHeader.SessionID,
		reqHeader.Credits,
		reqHeader.CreditCharge,
	)

	respHeader := header.NewResponseHeaderWithCredits(reqHeader, status, credits)

	// Error response body (8 bytes minimum)
	// StructureSize (2) + ErrorContextCount (1) + Reserved (1) + ByteCount (4)
	errorBody := make([]byte, 9)
	binary.LittleEndian.PutUint16(errorBody[0:2], 9) // StructureSize
	errorBody[2] = 0                                 // ErrorContextCount
	errorBody[3] = 0                                 // Reserved
	binary.LittleEndian.PutUint32(errorBody[4:8], 0) // ByteCount

	return c.sendMessage(respHeader, errorBody)
}

// sendMessage sends an SMB2 message with NetBIOS framing.
// If the session has signing enabled, the message is signed before sending.
func (c *SMBConnection) sendMessage(hdr *header.SMB2Header, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// Apply write timeout if configured
	if c.server.config.Timeouts.Write > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Write)
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	// Encode SMB2 header
	headerBytes := hdr.Encode()

	// Calculate total message length
	msgLen := len(headerBytes) + len(body)

	// Get a pooled buffer for the complete message (NetBIOS header + SMB2 message)
	// Buffer layout: [4-byte NetBIOS header][SMB2 header + body]
	totalLen := 4 + msgLen
	message := bufpool.Get(totalLen)
	defer bufpool.Put(message)

	// Build NetBIOS session header at the start
	// Type (1 byte) = 0x00 for session message
	// Length (3 bytes) = message length in big-endian
	message[0] = 0x00 // Session message type
	message[1] = byte(msgLen >> 16)
	message[2] = byte(msgLen >> 8)
	message[3] = byte(msgLen)

	// Copy SMB2 header and body after NetBIOS header
	smbMessage := message[4 : 4+msgLen]
	copy(smbMessage[0:len(headerBytes)], headerBytes)
	copy(smbMessage[len(headerBytes):], body)

	// Sign the message if session requires signing
	// Skip signing for messages without a session (SessionID == 0)
	//
	// Per MS-SMB2: Sign if SigningRequired is TRUE.
	// If signing is enabled but not required, we only sign if the client is signing
	// (which we track by whether the session requires it).
	if hdr.SessionID != 0 {
		if sess, ok := c.server.handler.GetSession(hdr.SessionID); ok {
			shouldSign := sess.Signing != nil && sess.Signing.SigningRequired && sess.ShouldSign()
			if shouldSign {
				sess.SignMessage(smbMessage)
				logger.Debug("Signed outgoing SMB2 message",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
			}
		}
	}

	_, err := c.conn.Write(message)
	if err != nil {
		return fmt.Errorf("write SMB message: %w", err)
	}

	logger.Debug("Sent SMB2 response",
		"command", hdr.Command.String(),
		"status", hdr.Status.String(),
		"messageId", hdr.MessageID,
		"bytes", msgLen)

	return nil
}

// handleConnectionClose handles cleanup and panic recovery for the connection.
//
// This performs full cleanup of all resources associated with the connection:
// 1. Wait for all in-flight requests to complete
// 2. Clean up all sessions created on this connection (files, trees, locks)
// 3. Close the TCP connection
//
// This ensures proper resource cleanup even when clients disconnect ungracefully
// (network failure, client crash, etc.) without sending LOGOFF.
func (c *SMBConnection) handleConnectionClose() {
	clientAddr := c.conn.RemoteAddr().String()

	// Panic recovery
	if r := recover(); r != nil {
		logger.Error("Panic in SMB connection handler", "address", clientAddr, "error", r)
	}

	// Wait for all in-flight requests to complete before cleanup
	c.wg.Wait()

	// Clean up all sessions created on this connection
	c.cleanupSessions()

	// Close the TCP connection
	_ = c.conn.Close()
	logger.Debug("SMB connection closed", "address", clientAddr)
}

// cleanupSessions cleans up all sessions that were created on this connection.
// This is called when the connection closes (gracefully or ungracefully) to ensure
// all resources (open files, locks, tree connections) are properly released.
func (c *SMBConnection) cleanupSessions() {
	// Capture client address early since connection may be closed
	clientAddr := c.conn.RemoteAddr().String()

	c.sessionsMu.Lock()
	sessions := make([]uint64, 0, len(c.sessions))
	for sessionID := range c.sessions {
		sessions = append(sessions, sessionID)
	}
	c.sessions = make(map[uint64]struct{}) // Clear immediately to avoid duplicate cleanup
	c.sessionsMu.Unlock()

	if len(sessions) == 0 {
		return
	}

	logger.Debug("Cleaning up sessions on connection close",
		"address", clientAddr,
		"sessionCount", len(sessions))

	// Use a context with timeout to prevent cleanup from blocking indefinitely
	// if storage operations hang (e.g., slow S3, network issues)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, sessionID := range sessions {
		// CleanupSession handles all resource cleanup:
		// - Close all open files (releases locks, flushes caches)
		// - Delete all tree connections
		// - Clean up pending auth state
		// - Delete the session itself
		c.server.handler.CleanupSession(ctx, sessionID)
	}

	logger.Debug("Session cleanup complete",
		"address", clientAddr,
		"sessionCount", len(sessions))
}

// handleRequestPanic handles cleanup and panic recovery for individual requests.
func (c *SMBConnection) handleRequestPanic(clientAddr string, messageID uint64) {
	<-c.requestSem // Release semaphore slot
	c.wg.Done()

	if r := recover(); r != nil {
		stack := string(debug.Stack())
		logger.Error("Panic in SMB request handler",
			"address", clientAddr,
			"messageId", messageID,
			"error", r,
			"stack", stack)
	}
}

// handleSMB1Negotiate handles legacy SMB1 NEGOTIATE requests by responding with
// an SMB2 NEGOTIATE response, which tells the client to upgrade to SMB2.
//
// This is required because many clients (including macOS Finder) start with
// SMB1 NEGOTIATE and expect the server to respond with SMB2 if it supports it.
func (c *SMBConnection) handleSMB1Negotiate(ctx context.Context, message []byte) error {
	logger.Debug("Received SMB1 NEGOTIATE, responding with SMB2 upgrade",
		"address", c.conn.RemoteAddr().String())

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
	// This should match what we send in the proper SMB2 NEGOTIATE response
	var securityMode byte
	if c.server.handler.SigningConfig.Enabled {
		securityMode |= 0x01 // SMB2_NEGOTIATE_SIGNING_ENABLED
	}
	if c.server.handler.SigningConfig.Required {
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
	copy(respBody[8:24], c.server.handler.ServerGUID[:])
	// Capabilities: 0
	binary.LittleEndian.PutUint32(respBody[24:28], 0)
	// MaxTransactSize
	binary.LittleEndian.PutUint32(respBody[28:32], c.server.handler.MaxTransactSize)
	// MaxReadSize
	binary.LittleEndian.PutUint32(respBody[32:36], c.server.handler.MaxReadSize)
	// MaxWriteSize
	binary.LittleEndian.PutUint32(respBody[36:40], c.server.handler.MaxWriteSize)
	// SystemTime
	binary.LittleEndian.PutUint64(respBody[40:48], types.TimeToFiletime(time.Now()))
	// ServerStartTime
	binary.LittleEndian.PutUint64(respBody[48:56], types.TimeToFiletime(c.server.handler.StartTime))
	// SecurityBufferOffset: offset from start of SMB2 header = 64 (header) + 64 (fixed body) = 128
	binary.LittleEndian.PutUint16(respBody[56:58], 128)
	// SecurityBufferLength: 0
	binary.LittleEndian.PutUint16(respBody[58:60], 0)
	// NegotiateContextOffset: 0
	binary.LittleEndian.PutUint32(respBody[60:64], 0)

	// Send the response
	return c.sendRawMessage(respHeader, respBody)
}

// SendAsyncChangeNotifyResponse sends an asynchronous CHANGE_NOTIFY response.
// This is called when a filesystem change matches a pending watch.
//
// Parameters:
//   - sessionID: The session that registered the watch
//   - messageID: The original CHANGE_NOTIFY request's message ID
//   - response: The change notification data
//
// Returns an error if the response could not be sent (e.g., connection closed).
func (c *SMBConnection) SendAsyncChangeNotifyResponse(sessionID, messageID uint64, response *handlers.ChangeNotifyResponse) error {
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

	return c.sendMessage(respHeader, body)
}

// sendRawMessage sends an SMB2 message with NetBIOS framing (without building from SMB2Header struct).
func (c *SMBConnection) sendRawMessage(headerBytes, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// Apply write timeout if configured
	if c.server.config.Timeouts.Write > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Write)
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	// Calculate total message length
	msgLen := len(headerBytes) + len(body)

	// Get a pooled buffer for the complete message (NetBIOS header + SMB2 message)
	totalLen := 4 + msgLen
	message := bufpool.Get(totalLen)
	defer bufpool.Put(message)

	// Build NetBIOS session header at the start
	message[0] = 0x00 // Session message type
	message[1] = byte(msgLen >> 16)
	message[2] = byte(msgLen >> 8)
	message[3] = byte(msgLen)

	// Copy header and body after NetBIOS header
	copy(message[4:4+len(headerBytes)], headerBytes)
	copy(message[4+len(headerBytes):], body)

	_, err := c.conn.Write(message)
	if err != nil {
		return fmt.Errorf("write SMB message: %w", err)
	}

	logger.Debug("Sent SMB2 NEGOTIATE response (upgrade from SMB1)",
		"bytes", msgLen)

	return nil
}
