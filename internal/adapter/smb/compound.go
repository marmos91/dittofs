package smb

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// compoundResponse holds a single command's response for compound batching.
type compoundResponse struct {
	respHeader *header.SMB2Header
	body       []byte
}

// ProcessCompoundRequest processes all commands in a compound request sequentially.
// Related operations share FileID from the previous response.
// compoundData contains the remaining commands after the first one.
//
// Per MS-SMB2 3.3.5.2.7, responses are batched into a single compound response
// frame with NextCommand offsets and 8-byte alignment padding.
//
// Parameters:
//   - ctx: context for cancellation
//   - firstHeader: parsed header of the first command
//   - firstBody: body bytes of the first command
//   - compoundData: remaining compound bytes after the first command
//   - connInfo: connection metadata for handler dispatch
//   - isEncrypted: whether the compound request was received inside an SMB3 Transform Header
func ProcessCompoundRequest(ctx context.Context, firstHeader *header.SMB2Header, firstBody []byte, compoundData []byte, connInfo *ConnInfo, isEncrypted bool) {
	// Per MS-SMB2 3.3.5.2.7.2: the first command in a compound MUST NOT have
	// the SMB2_FLAGS_RELATED_OPERATIONS flag set. Fail the entire compound.
	if firstHeader.IsRelated() {
		logger.Debug("Compound first command has related flag - failing entire compound",
			"command", firstHeader.Command.String(),
			"messageID", firstHeader.MessageID)
		var errResponses []compoundResponse
		// Error for the first command
		rh, rb := buildErrorResponseHeaderAndBody(firstHeader, types.StatusInvalidParameter, connInfo)
		errResponses = append(errResponses, compoundResponse{respHeader: rh, body: rb})
		// Remaining related commands also get INVALID_PARAMETER (they inherit from
		// the invalid first command). Non-related commands get FILE_CLOSED (they
		// attempt to use their own sentinel handle which is not valid).
		rem := compoundData
		for len(rem) >= header.HeaderSize {
			hdr, _, nextRem, err := ParseCompoundCommand(rem)
			if err != nil {
				break
			}
			rem = nextRem
			status := types.StatusFileClosed
			if hdr.IsRelated() {
				status = types.StatusInvalidParameter
			}
			erh, erb := buildErrorResponseHeaderAndBody(hdr, status, connInfo)
			errResponses = append(errResponses, compoundResponse{respHeader: erh, body: erb})
		}
		if err := sendCompoundResponses(errResponses, connInfo); err != nil {
			logger.Debug("Error sending compound error responses", "error", err)
		}
		return
	}

	// Track the last FileID for related operations
	var lastFileID [16]byte
	lastSessionID := firstHeader.SessionID
	lastTreeID := firstHeader.TreeID

	// Track whether the last command failed at session/tree validation level.
	// Per MS-SMB2 3.3.5.2.7.2, when a related command follows a predecessor that
	// failed at the session/tree level (USER_SESSION_DELETED, NETWORK_NAME_DELETED),
	// the server returns INVALID_PARAMETER because there's no valid session/tree to inherit.
	lastCmdSessionFailed := false

	// Collect all responses for compound batching
	var responses []compoundResponse

	// Process first command
	logger.Debug("Processing compound request - first command",
		"command", firstHeader.Command.String(),
		"messageID", firstHeader.MessageID)

	result, fileID, handlerCtx := ProcessRequestWithFileID(ctx, firstHeader, firstBody, connInfo, isEncrypted)
	if fileID != [16]byte{} {
		lastFileID = fileID
	}
	// Use handler context for response so handler-assigned SessionID/TreeID
	// (e.g. from SESSION_SETUP or TREE_CONNECT) propagate to the response.
	if handlerCtx != nil {
		if handlerCtx.SessionID != 0 {
			lastSessionID = handlerCtx.SessionID
		}
		if handlerCtx.TreeID != 0 {
			lastTreeID = handlerCtx.TreeID
		}
	}

	respHeader, body := buildResponseHeaderAndBody(firstHeader, handlerCtx, result, connInfo)
	responses = append(responses, compoundResponse{respHeader: respHeader, body: body})
	if result != nil {
		lastCmdSessionFailed = isSessionLevelError(result.Status)
	}

	// Process remaining commands from compound data
	remaining := compoundData
	for len(remaining) >= header.HeaderSize {
		// Keep a reference to the current command's start for signature verification.
		// Per MS-SMB2 3.2.4.1.4, each compound command is signed over its own bytes.
		currentCommandData := remaining

		hdr, cmdBody, nextRemaining, err := ParseCompoundCommand(remaining)
		if err != nil {
			logger.Debug("Error parsing compound command", "error", err)
			break
		}
		remaining = nextRemaining

		// Verify signature for this compound sub-command
		if err := VerifyCompoundCommandSignature(currentCommandData, hdr, connInfo); err != nil {
			logger.Warn("Compound command signature verification failed", "error", err)
			errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, types.StatusAccessDenied, connInfo)
			responses = append(responses, compoundResponse{respHeader: errHeader, body: errBody})
			break
		}

		// Per MS-SMB2 3.3.5.2.7.2: if a related command follows a predecessor
		// that failed at the session/tree validation level, return INVALID_PARAMETER
		// because there is no valid session/tree context to inherit.
		if hdr.IsRelated() && lastCmdSessionFailed {
			errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, types.StatusInvalidParameter, connInfo)
			errHeader.Flags |= types.FlagRelated
			responses = append(responses, compoundResponse{respHeader: errHeader, body: errBody})
			// This command also failed at the session level for the next command
			lastCmdSessionFailed = true
			continue
		}

		// Handle related operations - inherit IDs from previous command.
		// Per MS-SMB2 2.2.3.1, related operations use 0xFFFFFFFFFFFFFFFF for
		// SessionID and 0xFFFFFFFF for TreeID to indicate "use previous value".
		if hdr.IsRelated() {
			if hdr.SessionID == 0 || hdr.SessionID == 0xFFFFFFFFFFFFFFFF {
				hdr.SessionID = lastSessionID
			}
			if hdr.TreeID == 0 || hdr.TreeID == 0xFFFFFFFF {
				hdr.TreeID = lastTreeID
			}
		}

		logger.Debug("Processing compound request - command",
			"command", hdr.Command.String(),
			"messageID", hdr.MessageID,
			"isRelated", hdr.IsRelated(),
			"usingFileID", lastFileID != [16]byte{})

		// Process with the inherited FileID for related operations
		var cmdResult *HandlerResult
		var cmdCtx *handlers.SMBHandlerContext
		if hdr.IsRelated() && lastFileID != [16]byte{} {
			cmdResult, cmdCtx = ProcessRequestWithInheritedFileID(ctx, hdr, cmdBody, lastFileID, connInfo, isEncrypted)
		} else {
			var fid [16]byte
			cmdResult, fid, cmdCtx = ProcessRequestWithFileID(ctx, hdr, cmdBody, connInfo, isEncrypted)
			if fid != [16]byte{} {
				// CREATE returns the new FileID explicitly
				lastFileID = fid
			} else if !hdr.IsRelated() {
				// For non-related commands (CLOSE, READ, etc.), extract the FileID
				// from the request body so subsequent related commands inherit it.
				// This is critical: related commands inherit from the immediately
				// preceding command's context, not from the last CREATE.
				if extracted := ExtractFileID(hdr.Command, cmdBody); extracted != [16]byte{} {
					lastFileID = extracted
				}
			}
		}

		// Update tracking from handler context (preserves handler-assigned IDs)
		if cmdCtx != nil {
			if cmdCtx.SessionID != 0 {
				lastSessionID = cmdCtx.SessionID
			}
			if cmdCtx.TreeID != 0 {
				lastTreeID = cmdCtx.TreeID
			}
		}

		// Track session-level failures for related command error propagation
		if cmdResult != nil {
			lastCmdSessionFailed = isSessionLevelError(cmdResult.Status)
		} else {
			lastCmdSessionFailed = false
		}

		rh, rb := buildResponseHeaderAndBody(hdr, cmdCtx, cmdResult, connInfo)
		// Per MS-SMB2 3.3.5.2.7: if the request had FLAGS_RELATED_OPERATIONS,
		// the response MUST also have FLAGS_RELATED_OPERATIONS set.
		if hdr.IsRelated() {
			rh.Flags |= types.FlagRelated
		}
		responses = append(responses, compoundResponse{respHeader: rh, body: rb})
	}

	// Send all responses as a single compound response frame
	if err := sendCompoundResponses(responses, connInfo); err != nil {
		logger.Debug("Error sending compound responses", "error", err)
	}
}

// sendCompoundResponses sends all compound responses in a single NetBIOS frame.
// Per MS-SMB2 3.3.5.2.7:
//   - Each non-last response is padded to 8-byte alignment
//   - NextCommand in the header points to the next command's offset
//   - Each command is signed individually (if signing is active)
//   - The entire compound may be encrypted as one message
func sendCompoundResponses(responses []compoundResponse, connInfo *ConnInfo) error {
	if len(responses) == 0 {
		return nil
	}

	// Single response - no compound framing needed
	if len(responses) == 1 {
		return SendMessage(responses[0].respHeader, responses[0].body, connInfo)
	}

	// Build compound payload: sign each command individually, then concatenate.
	// Per Windows Server behavior (validated by smbtorture compound-padding test),
	// ALL responses in a compound frame are padded to 8-byte alignment, including
	// the last one. Only standalone (non-compound) responses are unpadded.
	var segments [][]byte
	for i := range responses {
		body := responses[i].body

		// Pad body to 8-byte boundary for all compound responses
		totalLen := header.HeaderSize + len(body)
		padding := (8 - totalLen%8) % 8
		if padding > 0 {
			body = append(body, make([]byte, padding)...)
		}

		// Set NextCommand offset for non-last responses
		if i < len(responses)-1 {
			responses[i].respHeader.NextCommand = uint32(header.HeaderSize + len(body))
		}

		// Encode header (after setting NextCommand)
		encoded := responses[i].respHeader.Encode()

		// Build full command bytes (header + body)
		cmdBytes := make([]byte, len(encoded)+len(body))
		copy(cmdBytes, encoded)
		copy(cmdBytes[len(encoded):], body)

		// Sign this command individually
		sessionID := responses[i].respHeader.SessionID
		if sessionID != 0 {
			if sess, ok := connInfo.Handler.GetSession(sessionID); ok {
				// Per MS-SMB2 3.3.4.1.1: encrypted sessions use AEAD, not signing
				if sess.ShouldSign() && !sess.ShouldEncrypt() {
					sess.SignMessage(cmdBytes)
				}
			}
		}

		segments = append(segments, cmdBytes)
	}

	// Concatenate all signed command segments
	totalLen := 0
	for _, seg := range segments {
		totalLen += len(seg)
	}
	payload := make([]byte, 0, totalLen)
	for _, seg := range segments {
		payload = append(payload, seg...)
	}

	// Handle encryption for the whole compound
	sessionID := responses[0].respHeader.SessionID
	if sessionID != 0 {
		if sess, ok := connInfo.Handler.GetSession(sessionID); ok {
			isSessionSetupSuccess := responses[0].respHeader.Command == types.SMB2SessionSetup &&
				responses[0].respHeader.Status == types.StatusSuccess
			if sess.ShouldEncrypt() && connInfo.EncryptionMiddleware != nil && !isSessionSetupSuccess {
				encrypted, err := connInfo.EncryptionMiddleware.EncryptResponse(sessionID, payload)
				if err != nil {
					return fmt.Errorf("encrypt compound response: %w", err)
				}
				logger.Debug("Encrypted compound response",
					"sessionID", sessionID,
					"commands", len(responses))
				return WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, encrypted)
			}
		}
	}

	logger.Debug("Sending compound response",
		"commands", len(responses),
		"totalBytes", len(payload))

	return WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, payload)
}

// isSessionLevelError returns true if the status indicates a session or tree
// validation failure. When such an error occurs in a compound, subsequent related
// commands cannot inherit a valid session/tree context and must get INVALID_PARAMETER.
func isSessionLevelError(status types.Status) bool {
	return status == types.StatusUserSessionDeleted ||
		status == types.StatusNetworkNameDeleted
}

// buildErrorResponseHeaderAndBody creates a response header and error body for
// compound error responses (e.g., signature verification failures).
func buildErrorResponseHeaderAndBody(reqHeader *header.SMB2Header, status types.Status, connInfo *ConnInfo) (*header.SMB2Header, []byte) {
	credits := connInfo.SessionManager.GrantCredits(
		reqHeader.SessionID,
		reqHeader.Credits,
		reqHeader.CreditCharge,
	)
	respHeader := header.NewResponseHeaderWithCredits(reqHeader, status, credits)
	return respHeader, MakeErrorBody()
}

// ParseCompoundCommand parses the next command from compound data.
// Returns header, body, remaining data, and error.
func ParseCompoundCommand(data []byte) (*header.SMB2Header, []byte, []byte, error) {
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
		"messageID", hdr.MessageID,
		"sessionID", fmt.Sprintf("0x%x", hdr.SessionID),
		"treeID", hdr.TreeID,
		"nextCommand", hdr.NextCommand,
		"flags", fmt.Sprintf("0x%x", hdr.Flags),
		"isRelated", hdr.IsRelated())

	return hdr, body, remaining, nil
}

// VerifyCompoundCommandSignature verifies the signature of a compound sub-command.
// Per MS-SMB2 3.2.4.1.4, each command in a compound is signed individually.
// The signature covers only this command's bytes (from its header to NextCommand or end).
func VerifyCompoundCommandSignature(data []byte, hdr *header.SMB2Header, connInfo *ConnInfo) error {
	if hdr.SessionID == 0 || hdr.Command == types.SMB2Negotiate || hdr.Command == types.SMB2SessionSetup {
		return nil
	}

	sess, ok := connInfo.Handler.GetSession(hdr.SessionID)
	if !ok {
		return nil
	}

	isSigned := hdr.Flags.IsSigned()
	if sess.CryptoState != nil && sess.CryptoState.SigningRequired && !isSigned {
		return fmt.Errorf("STATUS_ACCESS_DENIED: compound message not signed")
	}

	// Per MS-SMB2 3.3.5.2.4: For dialect 3.1.1, unsigned unencrypted requests
	// from authenticated sessions require disconnect.
	if !isSigned && connInfo.CryptoState != nil && connInfo.CryptoState.GetDialect() == types.Dialect0311 &&
		!sess.IsGuest && !sess.IsNull &&
		sess.CryptoState != nil && sess.CryptoState.ShouldVerify() {
		return fmt.Errorf("SMB 3.1.1: unsigned unencrypted compound request requires disconnect")
	}

	if isSigned && sess.ShouldVerify() {
		// Determine the bytes this command's signature covers
		verifyBytes := data
		if hdr.NextCommand > 0 && int(hdr.NextCommand) <= len(data) {
			verifyBytes = data[:hdr.NextCommand]
		}

		if !sess.VerifyMessage(verifyBytes) {
			logger.Warn("SMB2 compound command signature verification failed",
				"command", hdr.Command.String(),
				"sessionID", hdr.SessionID,
				"verifyLen", len(verifyBytes))
			return fmt.Errorf("STATUS_ACCESS_DENIED: compound signature verification failed")
		}
		logger.Debug("Verified compound command signature",
			"command", hdr.Command.String(),
			"sessionID", hdr.SessionID)
	}
	return nil
}

// ExtractFileID reads the FileID from a request body at the command-specific offset.
// Used in compound processing to track the FileID used by non-related commands
// so subsequent related commands can inherit it.
func ExtractFileID(command types.Command, body []byte) [16]byte {
	var offset int
	switch command {
	case types.SMB2Close, types.SMB2QueryDirectory, types.SMB2Ioctl,
		types.SMB2Flush, types.SMB2Lock, types.SMB2OplockBreak,
		types.SMB2ChangeNotify:
		offset = 8
	case types.SMB2Read, types.SMB2Write, types.SMB2SetInfo:
		offset = 16
	case types.SMB2QueryInfo:
		offset = 24
	default:
		return [16]byte{}
	}
	if len(body) < offset+16 {
		return [16]byte{}
	}
	var fid [16]byte
	copy(fid[:], body[offset:offset+16])
	return fid
}

// InjectFileID injects a FileID into the appropriate position in the request body.
// Offsets are per [MS-SMB2] specification for each command.
func InjectFileID(command types.Command, body []byte, fileID [16]byte) []byte {
	// FileID offset within the request body, per [MS-SMB2] spec for each command.
	var offset int
	switch command {
	case types.SMB2Close, types.SMB2QueryDirectory, types.SMB2Ioctl,
		types.SMB2Flush, types.SMB2Lock, types.SMB2OplockBreak,
		types.SMB2ChangeNotify:
		offset = 8 // [MS-SMB2] 2.2.15, 2.2.33, 2.2.31, 2.2.17, 2.2.26, 2.2.23, 2.2.35
	case types.SMB2Read, types.SMB2Write, types.SMB2SetInfo:
		offset = 16 // [MS-SMB2] 2.2.19, 2.2.21, 2.2.39
	case types.SMB2QueryInfo:
		offset = 24 // [MS-SMB2] 2.2.37
	default:
		return body
	}

	requiredLen := offset + 16
	if len(body) < requiredLen {
		logger.Debug("Body too small for FileID injection",
			"command", command.String(),
			"need", requiredLen,
			"have", len(body))
		return body
	}

	// Make a copy to avoid modifying the original
	newBody := make([]byte, len(body))
	copy(newBody, body)
	copy(newBody[offset:offset+16], fileID[:])

	logger.Debug("Injected FileID",
		"command", command.String(),
		"offset", offset)

	return newBody
}
