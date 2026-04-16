package smb

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
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
// When a command returns STATUS_PENDING with an AsyncId (e.g., CHANGE_NOTIFY),
// the compound includes an interim response at that position and continues
// processing subsequent commands. The actual async completion is sent separately.
//
// Parameters:
//   - ctx: context for cancellation
//   - firstHeader: parsed header of the first command
//   - firstBody: body bytes of the first command
//   - compoundData: remaining compound bytes after the first command
//   - connInfo: connection metadata for handler dispatch
//   - isEncrypted: whether the compound request was received inside an SMB3 Transform Header
//   - asyncNotifyCallback: optional callback for CHANGE_NOTIFY async responses (nil = no async)
func ProcessCompoundRequest(ctx context.Context, firstHeader *header.SMB2Header, firstBody []byte, firstRaw []byte, compoundData []byte, connInfo *ConnInfo, isEncrypted bool, asyncNotifyCallback handlers.AsyncResponseCallback) {
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

	// Per MS-SMB2 3.2.4.1.4: compound-level credit accounting.
	// The first command's CreditCharge covers the entire compound.
	// CreditCharge size validation is skipped for exempt commands; sequence
	// window Consume still runs for NEGOTIATE and first SESSION_SETUP but is
	// skipped for CANCEL — see response.go for rationale (#378).
	exempt := session.IsCreditExempt(firstHeader.Command, firstHeader.SessionID)
	if !exempt && connInfo.SupportsMultiCredit {
		if err := session.ValidateCreditCharge(firstHeader.Command, firstHeader.CreditCharge, firstBody); err != nil {
			logger.Debug("Compound credit charge validation failed",
				"command", firstHeader.Command.String(),
				"creditCharge", firstHeader.CreditCharge,
				"error", err)
			failEntireCompound(firstHeader, compoundData, types.StatusInvalidParameter, connInfo)
			return
		}
	}
	if connInfo.SequenceWindow != nil && firstHeader.Command != types.CommandCancel {
		charge := session.EffectiveCreditCharge(firstHeader.CreditCharge)
		if !connInfo.SequenceWindow.Consume(firstHeader.MessageID, charge) {
			logger.Debug("Compound sequence window validation failed",
				"command", firstHeader.Command.String(),
				"messageID", firstHeader.MessageID,
				"creditCharge", charge,
				"exempt", exempt)
			failEntireCompound(firstHeader, compoundData, types.StatusInvalidParameter, connInfo)
			return
		}
	}

	// Track the last FileID for related operations
	var lastFileID [16]byte
	lastSessionID := firstHeader.SessionID
	lastTreeID := firstHeader.TreeID

	// Track whether the last command failed (any error, not just session-level).
	// Per MS-SMB2 3.3.5.2.7.2, when a related command follows a predecessor that
	// failed at the session/tree level (USER_SESSION_DELETED, NETWORK_NAME_DELETED),
	// the server returns INVALID_PARAMETER because there's no valid session/tree to inherit.
	// When a non-session-level command fails and the inherited FileID is invalid,
	// subsequent related commands get the predecessor's error status propagated.
	lastCmdSessionFailed := false
	lastCmdFailed := false
	var lastCmdStatus types.Status

	// Collect all responses for compound batching
	var responses []compoundResponse

	// Collect deferred post-send hooks (e.g. STATUS_NOTIFY_CLEANUP after CLOSE)
	// that must fire strictly after the entire compound response is written.
	var postSendHooks []func()

	// Process first command
	logger.Debug("Processing compound request - first command",
		"command", firstHeader.Command.String(),
		"messageID", firstHeader.MessageID)

	result, fileID, handlerCtx := ProcessRequestWithFileIDAndCallback(ctx, firstHeader, firstBody, firstRaw, connInfo, isEncrypted, asyncNotifyCallback)
	if fileID != [16]byte{} {
		lastFileID = fileID
	} else {
		// For non-CREATE first commands, extract the FileID from the request
		// body so subsequent related commands can inherit it. This is important
		// when the first command fails (e.g., IOCTL with invalid handle): the
		// related command should inherit the handle and also get FILE_CLOSED,
		// not INVALID_PARAMETER.
		if extracted := ExtractFileID(firstHeader.Command, firstBody); extracted != [16]byte{} {
			lastFileID = extracted
		}
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
	if handlerCtx != nil && handlerCtx.PostSend != nil {
		postSendHooks = append(postSendHooks, handlerCtx.PostSend)
	}
	if result != nil {
		lastCmdSessionFailed = isSessionLevelError(result.Status)
		lastCmdFailed = result.Status.IsError()
		if lastCmdFailed {
			lastCmdStatus = result.Status
		}
	}

	// Process remaining commands from compound data
	remaining := compoundData
	for len(remaining) >= header.HeaderSize {
		// Keep a reference to the current command's start for signature verification.
		// Per MS-SMB2 3.2.4.1.4, each compound command is signed over its own bytes.
		currentCommandData := remaining

		hdr, cmdBody, nextRemaining, err := ParseCompoundCommand(remaining)
		if err != nil {
			// Per MS-SMB2 3.3.5.2.7: if a compound command has an invalid header
			// (bad magic, bad structure size, bad NextCommand alignment), return
			// STATUS_INVALID_PARAMETER for this command and stop processing.
			logger.Debug("Error parsing compound command", "error", err)
			// Build a minimal error response using what we can extract from the data.
			// Since parsing failed, we create a synthetic response header.
			errRh, errRb := buildCompoundParseErrorResponse(remaining, connInfo)
			responses = append(responses, compoundResponse{respHeader: errRh, body: errRb})
			break
		}
		remaining = nextRemaining

		// Verify signature for this compound sub-command.
		// Per MS-SMB2 3.2.5.1.1: skip signing verification when the message was
		// received inside an encrypted (TRANSFORM_HEADER) envelope — encryption
		// already provides integrity protection.
		if !isEncrypted {
			if err := VerifyCompoundCommandSignature(currentCommandData, hdr, connInfo); err != nil {
				logger.Warn("Compound command signature verification failed", "error", err)
				errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, types.StatusAccessDenied, connInfo)
				responses = append(responses, compoundResponse{respHeader: errHeader, body: errBody})
				break
			}
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
			lastCmdFailed = true
			continue
		}

		// Per MS-SMB2 3.3.5.2.7.2: if a related command follows a predecessor
		// that failed and the inherited FileID is invalid (all zeros), propagate
		// the predecessor's error status. Windows Server propagates the original
		// error (e.g., OBJECT_NAME_NOT_FOUND from a failed CREATE) rather than
		// always returning INVALID_PARAMETER.
		if hdr.IsRelated() && lastCmdFailed && lastFileID == [16]byte{} {
			propagatedStatus := lastCmdStatus
			if propagatedStatus == 0 {
				propagatedStatus = types.StatusInvalidParameter
			}
			errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, propagatedStatus, connInfo)
			errHeader.Flags |= types.FlagRelated
			responses = append(responses, compoundResponse{respHeader: errHeader, body: errBody})
			lastCmdFailed = true
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

		// Per Windows Server behavior: CHANGE_NOTIFY can only go async as the
		// last command in a compound. When it appears in a non-last position,
		// the server cannot split the compound response around an async operation.
		// Windows returns STATUS_INTERNAL_ERROR in this case (validated by
		// smbtorture compound.interim2).
		isLastCommand := len(remaining) < header.HeaderSize
		if hdr.Command == types.SMB2ChangeNotify && !isLastCommand {
			logger.Debug("CHANGE_NOTIFY in non-last compound position - returning INTERNAL_ERROR",
				"messageID", hdr.MessageID)
			errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, types.StatusInternalError, connInfo)
			if hdr.IsRelated() {
				errHeader.Flags |= types.FlagRelated
			}
			responses = append(responses, compoundResponse{respHeader: errHeader, body: errBody})
			lastCmdFailed = true
			lastCmdStatus = types.StatusInternalError
			lastCmdSessionFailed = false
			continue
		}

		logger.Debug("Processing compound request - command",
			"command", hdr.Command.String(),
			"messageID", hdr.MessageID,
			"isRelated", hdr.IsRelated(),
			"usingFileID", lastFileID != [16]byte{})

		// Raw wire bytes for this subcommand (header + body, up to NextCommand
		// offset if compounded further). Passed to dispatch so handlers that
		// hash the request (SESSION_SETUP preauth chain per [MS-SMB2] 3.3.5.5)
		// see the exact bytes the client hashed.
		subRaw := currentCommandData[:header.HeaderSize+len(cmdBody)]

		// Process with the inherited FileID for related operations
		var cmdResult *HandlerResult
		var cmdCtx *handlers.SMBHandlerContext
		if hdr.IsRelated() && lastFileID != [16]byte{} {
			var fid [16]byte
			cmdResult, fid, cmdCtx = ProcessRequestWithInheritedFileID(ctx, hdr, cmdBody, subRaw, lastFileID, connInfo, isEncrypted, asyncNotifyCallback)
			// Update lastFileID if a related CREATE returned a new FileID.
			// This is critical for compound sequences like CREATE+CLOSE+CREATE+NOTIFY
			// where the second CREATE produces a new handle that NOTIFY must inherit.
			if fid != [16]byte{} {
				lastFileID = fid
			}
		} else {
			var fid [16]byte
			cmdResult, fid, cmdCtx = ProcessRequestWithFileIDAndCallback(ctx, hdr, cmdBody, subRaw, connInfo, isEncrypted, asyncNotifyCallback)
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

		// Track session-level failures and general failures for related command error propagation
		if cmdResult != nil {
			lastCmdSessionFailed = isSessionLevelError(cmdResult.Status)
			lastCmdFailed = cmdResult.Status.IsError()
			if lastCmdFailed {
				lastCmdStatus = cmdResult.Status
			}
		} else {
			lastCmdSessionFailed = false
			lastCmdFailed = false
			lastCmdStatus = 0
		}

		rh, rb := buildResponseHeaderAndBody(hdr, cmdCtx, cmdResult, connInfo)
		// Per MS-SMB2 3.3.5.2.7: if the request had FLAGS_RELATED_OPERATIONS,
		// the response MUST also have FLAGS_RELATED_OPERATIONS set.
		if hdr.IsRelated() {
			rh.Flags |= types.FlagRelated
		}
		responses = append(responses, compoundResponse{respHeader: rh, body: rb})

		// Collect any PostSend hook (CLOSE→CHANGE_NOTIFY cleanup) so it can
		// fire strictly after the compound frame has been written.
		if cmdCtx != nil && cmdCtx.PostSend != nil {
			postSendHooks = append(postSendHooks, cmdCtx.PostSend)
		}
	}

	// Send all responses as a single compound response frame.
	sendErr := sendCompoundResponses(responses, connInfo)
	if sendErr != nil {
		logger.Debug("Error sending compound responses", "error", sendErr)
	}

	// Per MS-SMB2 3.3.4.1: run deferred post-send hooks (e.g.
	// STATUS_NOTIFY_CLEANUP after CLOSE) only after the compound response
	// has been written. Skip them if the compound write failed — the
	// connection is likely dead and the hooks would just log spurious
	// SendMessage errors on a torn-down session.
	if sendErr == nil {
		for _, hook := range postSendHooks {
			hook()
		}
	}
}

// sendCompoundResponses sends all compound responses in a single NetBIOS frame.
//
// Per MS-SMB2 3.3.5.2.7 — Sending Compounded Responses:
//   - Each non-last response is padded to 8-byte alignment
//   - NextCommand in the header points to the next command's offset
//   - Per MS-SMB2 3.3.4.1.1: each command is signed individually over its own
//     bytes (header + body + padding) before concatenation
//   - Per MS-SMB2 3.3.4.1.3: the entire compound may be encrypted as one message
//     (AEAD replaces signing when encryption is active)
func sendCompoundResponses(responses []compoundResponse, connInfo *ConnInfo) error {
	if len(responses) == 0 {
		return nil
	}

	// Single response - no compound framing needed
	if len(responses) == 1 {
		return SendMessage(responses[0].respHeader, responses[0].body, connInfo)
	}

	// Per MS-SMB2 3.2.4.1.4: middle compound responses grant 0 credits;
	// only the last response grants credits to the client.
	applyCompoundCreditZeroing(responses, connInfo)

	// Build compound payload: sign each command individually, then concatenate.
	// Per Windows Server behavior (validated by smbtorture compound-padding test),
	// ALL responses in a compound frame are padded to 8-byte alignment, including
	// the last one. Only standalone (non-compound) responses are unpadded.
	//
	// Per MS-SMB2 3.3.4.1.1: each sub-response is signed individually.
	// When a sub-response has a SessionID that doesn't map to a known session
	// (e.g., compound commands with bogus SessionID), fall back to the first
	// response's session for signing. This ensures the entire compound frame
	// is signed consistently and the client can verify all sub-responses.
	var firstSession *session.Session
	if fid := responses[0].respHeader.SessionID; fid != 0 {
		if s, ok := connInfo.Handler.GetSession(fid); ok {
			firstSession = s
		}
	}

	var payload []byte
	for i := range responses {
		body := responses[i].body

		// Pad body to 8-byte boundary
		totalLen := header.HeaderSize + len(body)
		if padding := (8 - totalLen%8) % 8; padding > 0 {
			body = append(body, make([]byte, padding)...)
		}

		// Set NextCommand offset for non-last responses
		if i < len(responses)-1 {
			responses[i].respHeader.NextCommand = uint32(header.HeaderSize + len(body))
		}

		// Encode header (after setting NextCommand) and build full command bytes
		encoded := responses[i].respHeader.Encode()
		cmdBytes := make([]byte, len(encoded)+len(body))
		copy(cmdBytes, encoded)
		copy(cmdBytes[len(encoded):], body)

		// Sign this command individually (encrypted sessions use AEAD instead).
		// If the sub-response's SessionID doesn't map to a known session,
		// fall back to the first response's session for signing.
		if sid := responses[i].respHeader.SessionID; sid != 0 {
			sess, ok := connInfo.Handler.GetSession(sid)
			if !ok && firstSession != nil {
				sess = firstSession
				ok = true
			}
			if ok && sess.ShouldSign() && !sess.ShouldEncrypt() {
				sess.SignMessage(cmdBytes)
			}
		}

		payload = append(payload, cmdBytes...)
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
				writeErr := WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, encrypted)
				// NOTE: each sub-response's credit grant was extended on the
				// sequence window synchronously during buildResponseHeaderAndBody;
				// applyCompoundCreditZeroing reclaimed the now-zeroed middle
				// responses. No post-write Grant is needed here (#378).
				return writeErr
			}
		}
	}

	logger.Debug("Sending compound response",
		"commands", len(responses),
		"totalBytes", len(payload))

	// Each sub-response's credit grant was extended on the window during
	// buildResponseHeaderAndBody; applyCompoundCreditZeroing reclaimed the
	// zeroed middle responses. No post-write Grant needed (#378).
	return WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, payload)
}

// failEntireCompound generates error responses for all commands in the compound
// (first + remaining) and sends them via sendCompoundResponses.
// Used when compound-level credit validation fails.
func failEntireCompound(firstHeader *header.SMB2Header, compoundData []byte, status types.Status, connInfo *ConnInfo) {
	var errResponses []compoundResponse

	// Error for the first command
	rh, rb := buildErrorResponseHeaderAndBody(firstHeader, status, connInfo)
	errResponses = append(errResponses, compoundResponse{respHeader: rh, body: rb})

	// Error for remaining commands
	rem := compoundData
	for len(rem) >= header.HeaderSize {
		hdr, _, nextRem, err := ParseCompoundCommand(rem)
		if err != nil {
			break
		}
		rem = nextRem
		erh, erb := buildErrorResponseHeaderAndBody(hdr, status, connInfo)
		errResponses = append(errResponses, compoundResponse{respHeader: erh, body: erb})
	}

	if err := sendCompoundResponses(errResponses, connInfo); err != nil {
		logger.Debug("Error sending compound error responses", "error", err)
	}
}

// applyCompoundCreditZeroing applies compound-level credit accounting to responses.
// Per MS-SMB2 3.2.4.1.4: middle compound responses grant 0 credits; only the last
// response grants credits. For single-response compounds (len <= 1), no zeroing
// is applied since they go through SendMessage which handles granting normally.
//
// Each sub-response was built via buildResponseHeaderAndBody, which already
// extended the connection's sequence window by that response's grant. Zeroing
// the middle headers would leave the window over-extended relative to what
// the client sees, so after zeroing we Reclaim each middle response's grant
// back from the window. Per-response Reclaim (rather than summing into a
// single call) avoids capping at uint16 if the aggregate ever exceeds 65535.
func applyCompoundCreditZeroing(responses []compoundResponse, connInfo *ConnInfo) {
	if len(responses) <= 1 {
		return
	}
	for i := 0; i < len(responses)-1; i++ {
		credits := responses[i].respHeader.Credits
		responses[i].respHeader.Credits = 0
		if credits > 0 && connInfo.SequenceWindow != nil {
			connInfo.SequenceWindow.Reclaim(credits)
		}
	}
}

// isSessionLevelError returns true if the status indicates a session or tree
// validation failure. When such an error occurs in a compound, subsequent related
// commands cannot inherit a valid session/tree context and must get INVALID_PARAMETER.
func isSessionLevelError(status types.Status) bool {
	return status == types.StatusUserSessionDeleted ||
		status == types.StatusNetworkNameDeleted ||
		status == types.StatusNetworkSessionExpired
}

// buildErrorResponseHeaderAndBody creates a response header and error body for
// compound error responses (e.g., signature verification failures).
func buildErrorResponseHeaderAndBody(reqHeader *header.SMB2Header, status types.Status, connInfo *ConnInfo) (*header.SMB2Header, []byte) {
	credits := grantConnectionCredits(connInfo, reqHeader.SessionID, reqHeader.Credits, reqHeader.CreditCharge)
	respHeader := header.NewResponseHeaderWithCredits(reqHeader, status, credits)
	return respHeader, MakeErrorBody()
}

// ParseCompoundCommand parses the next command from compound data.
// Returns header, body, remaining data, and error.
//
// Per MS-SMB2 3.3.5.2.7: if NextCommand is non-zero and not 8-byte aligned,
// the server MUST return STATUS_INVALID_PARAMETER.
func ParseCompoundCommand(data []byte) (*header.SMB2Header, []byte, []byte, error) {
	if len(data) < header.HeaderSize {
		return nil, nil, nil, fmt.Errorf("compound data too small: %d bytes", len(data))
	}

	// Parse SMB2 header
	hdr, err := header.Parse(data[:header.HeaderSize])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse compound SMB2 header: %w", err)
	}

	// Per MS-SMB2 3.3.5.2.7: NextCommand must be 8-byte aligned if non-zero
	if hdr.NextCommand != 0 && hdr.NextCommand%8 != 0 {
		return nil, nil, nil, fmt.Errorf("compound NextCommand not 8-byte aligned: %d", hdr.NextCommand)
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
//
// Per MS-SMB2 3.3.5.2.7.2 — Handling Compounded Requests:
// Each command in a compound request is signed individually over its own bytes
// (from its SMB2 header to NextCommand offset, or end for the last command).
// The signature covers ONLY that command's bytes, not the entire compound.
//
// Per MS-SMB2 3.3.5.2.4: For dialect 3.1.1, unsigned unencrypted requests from
// authenticated (non-guest, non-null) sessions are rejected.
func VerifyCompoundCommandSignature(data []byte, hdr *header.SMB2Header, connInfo *ConnInfo) error {
	if hdr.SessionID == 0 || hdr.Command == types.SMB2Negotiate || hdr.Command == types.SMB2SessionSetup {
		return nil
	}

	sess, ok := connInfo.Handler.GetSession(hdr.SessionID)
	if !ok {
		return nil
	}

	if sess.LoggedOff.Load() || sess.IsExpired() {
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

// fileIDOffset returns the byte offset of the FileID field within the request
// body for a given SMB2 command, per [MS-SMB2] wire format specifications.
// Returns -1 for commands that do not carry a FileID.
func fileIDOffset(command types.Command) int {
	switch command {
	case types.SMB2Close, types.SMB2QueryDirectory, types.SMB2Ioctl,
		types.SMB2Flush, types.SMB2Lock, types.SMB2OplockBreak,
		types.SMB2ChangeNotify:
		return 8
	case types.SMB2Read, types.SMB2Write, types.SMB2SetInfo:
		return 16
	case types.SMB2QueryInfo:
		return 24
	default:
		return -1
	}
}

// ExtractFileID reads the FileID from a request body at the command-specific offset.
// Used in compound processing to track the FileID used by non-related commands
// so subsequent related commands can inherit it.
func ExtractFileID(command types.Command, body []byte) [16]byte {
	offset := fileIDOffset(command)
	if offset < 0 || len(body) < offset+16 {
		return [16]byte{}
	}
	var fid [16]byte
	copy(fid[:], body[offset:offset+16])
	return fid
}

// buildCompoundParseErrorResponse creates a minimal error response when a
// compound command fails to parse (invalid magic, bad structure size, bad
// NextCommand alignment). Since parsing failed, we construct a synthetic
// response header with the MessageID from the raw bytes if possible.
func buildCompoundParseErrorResponse(data []byte, connInfo *ConnInfo) (*header.SMB2Header, []byte) {
	// Try to extract MessageID from raw bytes (offset 24, 8 bytes LE) even
	// though the header itself is invalid. This allows the client to correlate.
	var messageID uint64
	if len(data) >= 32 {
		messageID = binary.LittleEndian.Uint64(data[24:32])
	}

	credits := grantConnectionCredits(connInfo, 0, 1, 0)
	respHeader := &header.SMB2Header{
		ProtocolID:    [4]byte{0xFE, 'S', 'M', 'B'},
		StructureSize: header.HeaderSize,
		Status:        types.StatusInvalidParameter,
		Flags:         types.FlagResponse,
		MessageID:     messageID,
		Credits:       credits,
	}
	return respHeader, MakeErrorBody()
}

// InjectFileID injects a FileID into the appropriate position in the request body.
// Offsets are per [MS-SMB2] specification for each command.
func InjectFileID(command types.Command, body []byte, fileID [16]byte) []byte {
	offset := fileIDOffset(command)
	if offset < 0 {
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
