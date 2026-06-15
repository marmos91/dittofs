package smb

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// compoundResponse holds a single command's response for compound batching.
type compoundResponse struct {
	respHeader *header.SMB2Header
	body       []byte
	// releaseData, when non-nil, returns a pooled response buffer to the pool
	// AFTER the single composed wire write completes. Sub-response Data
	// buffers are still referenced inside the composed frame until the write
	// returns, so firing between sub-responses would be a use-after-release;
	// sendCompoundResponses collects these closures and invokes every non-nil
	// one in a loop post-write, on both success and error paths.
	//
	// Non-pooled sub-responses leave this nil — the sender null-checks.
	releaseData func()
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
		if err := sendCompoundResponses(errResponses, connInfo, isEncrypted); err != nil {
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
			failEntireCompound(firstHeader, compoundData, types.StatusInvalidParameter, connInfo, isEncrypted)
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
			failEntireCompound(firstHeader, compoundData, types.StatusInvalidParameter, connInfo, isEncrypted)
			return
		}
	}

	// Shared per-subcommand tracking (FileID/session/tree inheritance, failure
	// propagation, response + post-send accumulators). Seeded from the first
	// command below, then handed to processRemaining for the trailing commands.
	state := compoundLoopState{
		lastSessionID: firstHeader.SessionID,
		lastTreeID:    firstHeader.TreeID,
	}

	// Process first command
	logger.Debug("Processing compound request - first command",
		"command", firstHeader.Command.String(),
		"messageID", firstHeader.MessageID)

	result, fileID, handlerCtx := ProcessRequestWithFileIDAndCallback(ctx, firstHeader, firstBody, firstRaw, connInfo, isEncrypted, asyncNotifyCallback)

	// Per MS-SMB2 §3.3.4.4 and smbtorture compound_async.getinfo_middle:
	// When a compound command returns STATUS_PENDING with an AsyncId AND
	// there are remaining commands, send the interim response standalone and
	// defer the remaining compound commands to the async completion callback.
	// Without this, the compound processor would try to process subsequent
	// related commands without a FileID (the CREATE hasn't completed yet),
	// and the inline sync wait would deadlock because the test/client cannot
	// ACK the lease break until it receives the STATUS_PENDING interim.
	if result != nil && result.Status == types.StatusPending && result.AsyncId != 0 && len(compoundData) >= header.HeaderSize {
		// The parkCreateOnLeaseBreak goroutine will call
		// AsyncCreateCompleteCallback when the CREATE completes. We
		// overwrite that callback (it was set to send a standalone
		// completion) with one that continues compound processing.
		// The original callback was captured by the goroutine's closure
		// via PendingCreate.Callback; we replace PendingCreate.Callback
		// in the registry so the goroutine picks up our wrapper.
		//
		// Order matters: replace BEFORE sending the interim. The goroutine
		// is parked on a break-wait that can only complete after the client
		// observes the interim and ACKs the lease break, so as long as the
		// replacement happens before the interim hits the wire, the goroutine
		// cannot wake with the stale standalone callback (compound-break
		// regression — without the reorder the wake can race ReplaceCallback
		// on a fast localhost loop, causing the CREATE to complete
		// standalone, the GETINFO never to run, and the client to time out).
		if connInfo.Handler.PendingCreateRegistry != nil {
			connInfo.Handler.PendingCreateRegistry.ReplaceCallback(result.AsyncId, func(sessionID, messageID, asyncID uint64, status types.Status, createBody []byte) error {
				connInfo.ReleaseAsync()
				// Build a compound response starting with the CREATE's final result,
				// followed by the remaining compound commands processed with the
				// CREATE's FileID.
				completeCompoundAfterAsyncCreate(
					ctx, firstHeader, status, createBody, asyncID,
					compoundData, connInfo, isEncrypted, asyncNotifyCallback,
				)
				return nil
			})
		}

		// Send interim STATUS_PENDING as a standalone async response.
		interimCredits := grantConnectionCredits(connInfo, firstHeader.SessionID, firstHeader.Credits, firstHeader.CreditCharge)
		interimHeader := header.NewResponseHeaderWithCredits(firstHeader, types.StatusPending, interimCredits)
		interimHeader.Flags |= types.FlagAsync
		interimHeader.AsyncId = result.AsyncId
		if err := SendMessage(interimHeader, MakeErrorBody(), connInfo); err != nil {
			logger.Debug("Error sending compound async interim response", "error", err)
		}

		// Release the resume goroutine only AFTER the interim STATUS_PENDING is
		// on the wire. The callback was swapped above (ReplaceCallback) so the
		// goroutine picks up the continue-compound wrapper, and MarkStarted runs
		// post-interim so the final compound response can never overtake the
		// interim (MS-SMB2 §3.3.4.4 ordering; same class as the standalone path
		// in response.go that smb2.bench.oplock1 trips). Skipping the release
		// would deadlock the CREATE after the wait drains (smbtorture
		// compound.compound-break IO_TIMEOUT race).
		if connInfo.Handler.PendingCreateRegistry != nil {
			connInfo.Handler.PendingCreateRegistry.MarkStarted(result.AsyncId)
		}
		return
	}

	// Fall-through path: the first command returned STATUS_PENDING + AsyncId
	// AND there were no more commands in the compound. The interim is buffered
	// into the compound response below; the resume goroutine fires the original
	// (standalone-completion) callback with the final response. Defer releasing
	// its started gate until AFTER the compound frame is on the wire so the
	// final response can never overtake the interim.
	if result != nil && result.Status == types.StatusPending && result.AsyncId != 0 &&
		connInfo.Handler.PendingCreateRegistry != nil {
		state.markStartedAsyncIDs = append(state.markStartedAsyncIDs, result.AsyncId)
	}

	if fileID != [16]byte{} {
		state.lastFileID = fileID
	} else {
		// For non-CREATE first commands, extract the FileID from the request
		// body so subsequent related commands can inherit it. This is important
		// when the first command fails (e.g., IOCTL with invalid handle): the
		// related command should inherit the handle and also get FILE_CLOSED,
		// not INVALID_PARAMETER.
		if extracted := ExtractFileID(firstHeader.Command, firstBody); extracted != [16]byte{} {
			state.lastFileID = extracted
		}
	}
	// Use handler context for response so handler-assigned SessionID/TreeID
	// (e.g. from SESSION_SETUP or TREE_CONNECT) propagate to the response.
	if handlerCtx != nil {
		if handlerCtx.SessionID != 0 {
			state.lastSessionID = handlerCtx.SessionID
		}
		if handlerCtx.TreeID != 0 {
			state.lastTreeID = handlerCtx.TreeID
		}
	}

	// A nil result signals "no response should be sent" — only CANCEL does this
	// per MS-SMB2 §3.3.5.16. Compounding CANCEL is prohibited by spec, but a
	// malicious or buggy client can still send it; building a response from the
	// nil result would dereference result.Status and panic the connection
	// goroutine (DoS). Skip emitting a response for this subcommand and continue
	// processing the rest of the chain, mirroring the standalone path.
	if result != nil {
		respHeader, body := buildResponseHeaderAndBody(firstHeader, handlerCtx, result, connInfo)
		state.responses = append(state.responses, compoundResponse{respHeader: respHeader, body: body, releaseData: result.ReleaseData})
		if handlerCtx != nil && handlerCtx.PostSend != nil {
			state.postSendHooks = append(state.postSendHooks, handlerCtx.PostSend)
		}
		state.lastCmdSessionFailed = isSessionLevelError(result.Status)
		state.lastCmdFailed = result.Status.IsError()
		if state.lastCmdFailed {
			state.lastCmdStatus = result.Status
		}
	}

	// Process remaining commands from compound data via the shared loop.
	state.processRemaining(ctx, compoundData, connInfo, isEncrypted, asyncNotifyCallback)

	// Send all responses as a single compound response frame.
	sendErr := sendCompoundResponses(state.responses, connInfo, isEncrypted)
	if sendErr != nil {
		logger.Debug("Error sending compound responses", "error", sendErr)
	}

	// Release any parked-CREATE resume goroutines now that the interim
	// STATUS_PENDING is on the wire (see compoundLoopState.markStartedAsyncIDs).
	state.releaseStartedGates(connInfo.Handler.PendingCreateRegistry)

	// Per MS-SMB2 3.3.4.1: run deferred post-send hooks (e.g.
	// STATUS_NOTIFY_CLEANUP after CLOSE) only after the compound response
	// has been written. Skip them if the compound write failed — the
	// connection is likely dead and the hooks would just log spurious
	// SendMessage errors on a torn-down session.
	if sendErr == nil {
		for _, hook := range state.postSendHooks {
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
//
// requestEncrypted indicates the compound request arrived inside an SMB3
// Transform Header. Per MS-SMB2 §3.3.4.1.4 the response MUST then be encrypted
// too, even when the session's standing encryption policy (Session.ShouldEncrypt)
// is off — a client that turned encryption on per-connection
// (smb2cli_session_encryption_on) rejects an unencrypted compound reply as a
// security violation. smbtorture smb2.session.expire2e drives an ENCRYPTED
// CREATE+QUERY_DIRECTORY+CLOSE compound on an expired session and requires the
// STATUS_NETWORK_SESSION_EXPIRED compound reply to come back encrypted.
func sendCompoundResponses(responses []compoundResponse, connInfo *ConnInfo, requestEncrypted bool) error {
	if len(responses) == 0 {
		return nil
	}

	// fire every sub-response's ReleaseData closure AFTER the single
	// composed wire write completes. Deferring the loop covers the single-
	// response shortcut, plain compound, and encrypted compound paths alike,
	// and still runs on write error — the pooled buffer is no longer
	// referenced once WriteNetBIOSFrame returns. Firing between sub-responses
	// would be a use-after-release because sub-bodies live inside the
	// composed frame until the write completes.
	defer func() {
		for i := range responses {
			if responses[i].releaseData != nil {
				responses[i].releaseData()
			}
		}
	}()

	// Single response - no compound framing needed. SendMessage already
	// encrypts when the session's standing policy requires it; the inbound-
	// encrypted hint additionally covers a per-connection encrypt-on session
	// (preferred mode) so it still gets an encrypted reply (MS-SMB2 §3.3.4.1.4).
	if len(responses) == 1 {
		hdr := responses[0].respHeader
		if requestEncrypted && compoundShouldEncrypt(connInfo, hdr, requestEncrypted) {
			plaintext := append(hdr.Encode(), responses[0].body...)
			encrypted, err := connInfo.EncryptionMiddleware.EncryptResponse(hdr.SessionID, plaintext)
			if err != nil {
				return fmt.Errorf("encrypt single compound response: %w", err)
			}
			return WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, encrypted)
		}
		return SendMessage(hdr, responses[0].body, connInfo)
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

	// Decide up-front whether the composed frame will be encrypted (AEAD
	// replaces per-command signing).
	willEncrypt := compoundShouldEncrypt(connInfo, responses[0].respHeader, requestEncrypted)

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
			// Skip per-command signing when the whole frame will be AEAD-encrypted.
			if ok && sess.ShouldSign() && !willEncrypt {
				sess.SignMessageOnChannel(connInfo.ConnID, cmdBytes)
			}
		}

		payload = append(payload, cmdBytes...)
	}

	// Handle encryption for the whole compound (decision computed above).
	if willEncrypt {
		sessionID := responses[0].respHeader.SessionID
		encrypted, err := connInfo.EncryptionMiddleware.EncryptResponse(sessionID, payload)
		if err != nil {
			return fmt.Errorf("encrypt compound response: %w", err)
		}
		logger.Debug("Encrypted compound response",
			"sessionID", sessionID,
			"commands", len(responses),
			"requestEncrypted", requestEncrypted)
		writeErr := WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, encrypted)
		// NOTE: each sub-response's credit grant was extended on the
		// sequence window synchronously during buildResponseHeaderAndBody;
		// applyCompoundCreditZeroing reclaimed the now-zeroed middle
		// responses. No post-write Grant is needed here (#378).
		return writeErr
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
func failEntireCompound(firstHeader *header.SMB2Header, compoundData []byte, status types.Status, connInfo *ConnInfo, requestEncrypted bool) {
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

	if err := sendCompoundResponses(errResponses, connInfo, requestEncrypted); err != nil {
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

// compoundShouldEncrypt reports whether a compound response (or its single-
// command shortcut) must be AEAD-encrypted. Encrypt when the session's standing
// policy requires it (Session.ShouldEncrypt) OR the inbound compound was itself
// encrypted (requestEncrypted — MS-SMB2 §3.3.4.1.4, smbtorture
// smb2.session.expire2e), provided the session resolves and holds an encryptor.
// SESSION_SETUP SUCCESS is never encrypted here: the peer has no derivable keys
// for the channel yet.
func compoundShouldEncrypt(connInfo *ConnInfo, hdr *header.SMB2Header, requestEncrypted bool) bool {
	if hdr.SessionID == 0 || connInfo.EncryptionMiddleware == nil {
		return false
	}
	if hdr.Command == types.SMB2SessionSetup && hdr.Status == types.StatusSuccess {
		return false
	}
	sess, ok := connInfo.Handler.GetSession(hdr.SessionID)
	if !ok || sess.CryptoState == nil || sess.CryptoState.Encryptor == nil {
		return false
	}
	return sess.ShouldEncrypt() || requestEncrypted
}

// relatedSessionFailureStatus picks the status returned to a related compound
// command whose predecessor failed at the session/tree validation level.
//
// Default is STATUS_INVALID_PARAMETER: a NETWORK_NAME_DELETED / USER_SESSION_
// DELETED predecessor leaves no valid session/tree context for the follower to
// inherit (MS-SMB2 §3.3.5.2.7.2).
//
// Session expiry is the exception: when the predecessor failed with
// STATUS_NETWORK_SESSION_EXPIRED the whole session is expired, so every command
// in the compound would independently hit the per-command expiry gate with the
// same status. Samba returns SESSION_EXPIRED for each member; propagate it
// instead of masking it as INVALID_PARAMETER (smbtorture
// smb2.session.expire2s/expire2e).
func relatedSessionFailureStatus(prevStatus types.Status) types.Status {
	if prevStatus == types.StatusNetworkSessionExpired {
		return types.StatusNetworkSessionExpired
	}
	return types.StatusInvalidParameter
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

	// NextCommand must point past the header; a smaller value (e.g. 8, 16, 32 —
	// all 8-byte aligned) would produce a negative-length slice at body
	// extraction below (data[HeaderSize:NextCommand]).
	if hdr.NextCommand > 0 && hdr.NextCommand < uint32(header.HeaderSize) {
		return nil, nil, nil, fmt.Errorf("compound NextCommand too small: %d", hdr.NextCommand)
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

	// Unknown SessionID: skip verification here and rely on prepareDispatch to
	// reject the command. This is safe ONLY because prepareDispatch
	// independently rejects every NeedsSession command on an unknown/logged-off
	// SessionID with STATUS_USER_SESSION_DELETED before the handler runs, so no
	// dispatched mutating op escapes the signing gate (M-A2). INVARIANT: any new
	// command that touches session-bound state MUST set NeedsSession=true (see
	// the dispatch table) so this skip cannot become a bypass.
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

		if !sess.VerifyMessageOnChannel(connInfo.ConnID, verifyBytes) {
			logger.Warn("SMB2 compound command signature verification failed",
				"command", hdr.Command.String(),
				"sessionID", hdr.SessionID,
				"connID", connInfo.ConnID,
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

// compoundLoopState carries the mutable per-subcommand tracking shared by the
// sync (ProcessCompoundRequest) and async-CREATE-resume
// (completeCompoundAfterAsyncCreate) compound loops. Both seed it from their
// already-processed first command, then call processRemaining to handle the
// trailing related commands with identical MS-SMB2 §3.3.5.2.7.2 semantics.
type compoundLoopState struct {
	// FileID/Session/Tree inherited by the next related command.
	lastFileID    [16]byte
	lastSessionID uint64
	lastTreeID    uint32

	// Failure tracking for related-command error propagation.
	lastCmdSessionFailed bool
	lastCmdFailed        bool
	lastCmdStatus        types.Status

	// Accumulated responses + deferred post-send hooks.
	responses     []compoundResponse
	postSendHooks []func()

	// AsyncIds of parked CREATEs whose interim STATUS_PENDING is buffered into
	// responses (not yet on the wire). MarkStarted MUST fire only after the
	// compound frame is written, so the resume goroutine's final response can
	// never overtake the interim (MS-SMB2 §3.3.4.4; same ordering class as the
	// standalone path that smb2.bench.oplock1 trips).
	markStartedAsyncIDs []uint64
}

// releaseStartedGates unblocks the resume goroutines for every parked CREATE
// whose interim STATUS_PENDING was buffered into this compound frame. It MUST
// be called only after the frame is on the wire so the final responses cannot
// overtake the interims. Fired unconditionally (even on a write error) so a
// resume goroutine is never left blocked on PendingCreate.started.
func (s *compoundLoopState) releaseStartedGates(reg *handlers.PendingCreateRegistry) {
	if reg == nil {
		return
	}
	for _, asyncID := range s.markStartedAsyncIDs {
		reg.MarkStarted(asyncID)
	}
}

// processRemaining processes every command after the first in a compound,
// appending each response to s.responses and any PostSend hook to
// s.postSendHooks. It implements the MS-SMB2 §3.3.5.2.7.2 related-command rules
// (session-level failure propagation, FileID inheritance, the CHANGE_NOTIFY
// non-last gate, per-subcommand signature verification). This is the single
// source of truth for both the sync and async-resume paths so the two cannot
// drift (see M-A1).
func (s *compoundLoopState) processRemaining(
	ctx context.Context,
	remaining []byte,
	connInfo *ConnInfo,
	isEncrypted bool,
	asyncNotifyCallback handlers.AsyncResponseCallback,
) {
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
			s.responses = append(s.responses, compoundResponse{respHeader: errRh, body: errRb})
			break
		}
		remaining = nextRemaining

		// Handle related operations - inherit IDs from previous command.
		// Per MS-SMB2 2.2.3.1, related operations use 0xFFFFFFFFFFFFFFFF for
		// SessionID and 0xFFFFFFFF for TreeID to indicate "use previous value".
		//
		// This MUST run before signature verification: the per-sub-command
		// signing check (VerifyCompoundCommandSignature) looks up the session by
		// hdr.SessionID. If the wire sentinel (0xFFFFFFFFFFFFFFFF) reached that
		// lookup it would miss (ok=false) and silently skip the integrity check
		// for every related sub-command — an auth bypass. Resolving the sentinel
		// to the real SessionID first ensures the signing gate sees the session.
		if hdr.IsRelated() {
			if hdr.SessionID == 0 || hdr.SessionID == 0xFFFFFFFFFFFFFFFF {
				hdr.SessionID = s.lastSessionID
			}
			if hdr.TreeID == 0 || hdr.TreeID == 0xFFFFFFFF {
				hdr.TreeID = s.lastTreeID
			}
		}

		// Verify signature for this compound sub-command.
		// Per MS-SMB2 3.2.5.1.1: skip signing verification when the message was
		// received inside an encrypted (TRANSFORM_HEADER) envelope — encryption
		// already provides integrity protection.
		if !isEncrypted {
			if err := VerifyCompoundCommandSignature(currentCommandData, hdr, connInfo); err != nil {
				logger.Warn("Compound command signature verification failed", "error", err)
				errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, types.StatusAccessDenied, connInfo)
				s.responses = append(s.responses, compoundResponse{respHeader: errHeader, body: errBody})
				break
			}
		}

		// Per MS-SMB2 3.3.5.2.7.2: if a related command follows a predecessor
		// that failed at the session/tree validation level, return INVALID_PARAMETER
		// because there is no valid session/tree context to inherit.
		//
		// Exception — session expiry (STATUS_NETWORK_SESSION_EXPIRED): the
		// session itself is expired, so every command in the compound would
		// independently fail the per-command expiry gate (prepareDispatch,
		// response.go) with the SAME status. Samba processes each compound
		// member through smb2_validate_sequence_number / session lookup before
		// FileID resolution, so all members of a compound on an expired session
		// return STATUS_NETWORK_SESSION_EXPIRED — not INVALID_PARAMETER. The
		// CREATE+QUERY_DIRECTORY+CLOSE compound in smbtorture
		// smb2.session.expire2s/expire2e asserts this: each recv must surface
		// SESSION_EXPIRED. Propagate the expired status to the related
		// followers instead of masking it as INVALID_PARAMETER.
		if hdr.IsRelated() && s.lastCmdSessionFailed {
			sessFailStatus := relatedSessionFailureStatus(s.lastCmdStatus)
			errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, sessFailStatus, connInfo)
			errHeader.Flags |= types.FlagRelated
			s.responses = append(s.responses, compoundResponse{respHeader: errHeader, body: errBody})
			// This command also failed at the session level for the next command
			s.lastCmdSessionFailed = true
			s.lastCmdFailed = true
			s.lastCmdStatus = sessFailStatus
			continue
		}

		// Per MS-SMB2 3.3.5.2.7.2: if a related command follows a predecessor
		// that failed and the inherited FileID is invalid (all zeros), propagate
		// the predecessor's error status. Windows Server propagates the original
		// error (e.g., OBJECT_NAME_NOT_FOUND from a failed CREATE) rather than
		// always returning INVALID_PARAMETER.
		if hdr.IsRelated() && s.lastCmdFailed && s.lastFileID == [16]byte{} {
			propagatedStatus := s.lastCmdStatus
			if propagatedStatus == 0 {
				propagatedStatus = types.StatusInvalidParameter
			}
			errHeader, errBody := buildErrorResponseHeaderAndBody(hdr, propagatedStatus, connInfo)
			errHeader.Flags |= types.FlagRelated
			s.responses = append(s.responses, compoundResponse{respHeader: errHeader, body: errBody})
			s.lastCmdFailed = true
			continue
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
			s.responses = append(s.responses, compoundResponse{respHeader: errHeader, body: errBody})
			s.lastCmdFailed = true
			s.lastCmdStatus = types.StatusInternalError
			s.lastCmdSessionFailed = false
			continue
		}

		logger.Debug("Processing compound request - command",
			"command", hdr.Command.String(),
			"messageID", hdr.MessageID,
			"isRelated", hdr.IsRelated(),
			"usingFileID", s.lastFileID != [16]byte{})

		// Raw wire bytes for this subcommand (header + body, up to NextCommand
		// offset if compounded further). Passed to dispatch so handlers that
		// hash the request (SESSION_SETUP preauth chain per [MS-SMB2] 3.3.5.5)
		// see the exact bytes the client hashed.
		subRaw := currentCommandData[:header.HeaderSize+len(cmdBody)]

		// Process with the inherited FileID for related operations
		var cmdResult *HandlerResult
		var cmdCtx *handlers.SMBHandlerContext
		if hdr.IsRelated() && s.lastFileID != [16]byte{} {
			var fid [16]byte
			cmdResult, fid, cmdCtx = ProcessRequestWithInheritedFileID(ctx, hdr, cmdBody, subRaw, s.lastFileID, connInfo, isEncrypted, asyncNotifyCallback)
			// Update lastFileID if a related CREATE returned a new FileID.
			// This is critical for compound sequences like CREATE+CLOSE+CREATE+NOTIFY
			// where the second CREATE produces a new handle that NOTIFY must inherit.
			if fid != [16]byte{} {
				s.lastFileID = fid
			}
		} else {
			var fid [16]byte
			cmdResult, fid, cmdCtx = ProcessRequestWithFileIDAndCallback(ctx, hdr, cmdBody, subRaw, connInfo, isEncrypted, asyncNotifyCallback)
			if fid != [16]byte{} {
				// CREATE returns the new FileID explicitly
				s.lastFileID = fid
			} else if !hdr.IsRelated() {
				// For non-related commands (CLOSE, READ, etc.), extract the FileID
				// from the request body so subsequent related commands inherit it.
				// This is critical: related commands inherit from the immediately
				// preceding command's context, not from the last CREATE.
				if extracted := ExtractFileID(hdr.Command, cmdBody); extracted != [16]byte{} {
					s.lastFileID = extracted
				}
			}
		}

		// Update tracking from handler context (preserves handler-assigned IDs)
		if cmdCtx != nil {
			if cmdCtx.SessionID != 0 {
				s.lastSessionID = cmdCtx.SessionID
			}
			if cmdCtx.TreeID != 0 {
				s.lastTreeID = cmdCtx.TreeID
			}
		}

		// Track session-level failures and general failures for related command error propagation
		if cmdResult != nil {
			s.lastCmdSessionFailed = isSessionLevelError(cmdResult.Status)
			s.lastCmdFailed = cmdResult.Status.IsError()
			if s.lastCmdFailed {
				s.lastCmdStatus = cmdResult.Status
			}
		} else {
			s.lastCmdSessionFailed = false
			s.lastCmdFailed = false
			s.lastCmdStatus = 0
		}

		// A tail-of-chain compound subcommand that returns STATUS_PENDING + AsyncId
		// (parked CREATE) inherits the original standalone-completion callback —
		// no ReplaceCallback redirect happens here, since the subcommand is
		// already the tail of the chain. Defer releasing its resume-goroutine gate
		// until AFTER the compound frame is written (markStartedAsyncIDs) so the
		// final response cannot overtake the buffered interim. Non-tail parked
		// CREATEs MUST NOT be released here: trailing related commands still need
		// to defer behind the CREATE completion, so the chain is not split.
		if isLastCommand && cmdResult != nil && cmdResult.Status == types.StatusPending && cmdResult.AsyncId != 0 &&
			connInfo.Handler.PendingCreateRegistry != nil {
			s.markStartedAsyncIDs = append(s.markStartedAsyncIDs, cmdResult.AsyncId)
		}

		// A nil result signals "no response should be sent" (CANCEL, per
		// MS-SMB2 §3.3.5.16). Passing it to buildResponseHeaderAndBody would
		// dereference cmdResult.Status and panic the connection goroutine.
		// Compounding CANCEL is prohibited by spec, but the server must not
		// crash on malformed client input — skip this subcommand's response.
		if cmdResult == nil {
			continue
		}

		rh, rb := buildResponseHeaderAndBody(hdr, cmdCtx, cmdResult, connInfo)
		// Per MS-SMB2 3.3.5.2.7: if the request had FLAGS_RELATED_OPERATIONS,
		// the response MUST also have FLAGS_RELATED_OPERATIONS set.
		if hdr.IsRelated() {
			rh.Flags |= types.FlagRelated
		}
		// propagate the sub-result's ReleaseData so the composed wire
		// write can fire it post-write (see compoundResponse doc comment).
		s.responses = append(s.responses, compoundResponse{respHeader: rh, body: rb, releaseData: cmdResult.ReleaseData})

		// Collect any PostSend hook (CLOSE→CHANGE_NOTIFY cleanup) so it can
		// fire strictly after the compound frame has been written.
		if cmdCtx != nil && cmdCtx.PostSend != nil {
			s.postSendHooks = append(s.postSendHooks, cmdCtx.PostSend)
		}
	}
}

// completeCompoundAfterAsyncCreate runs from the async CREATE resume goroutine
// to finish a compound request whose first command (CREATE) went async with
// STATUS_PENDING. It builds a compound response containing the CREATE's final
// result and all subsequent commands, then sends it as a single compound frame.
//
// Per MS-SMB2 §3.3.4.4: the CREATE's interim STATUS_PENDING was already sent
// standalone by ProcessCompoundRequest. completeCompoundAfterAsyncCreate delivers
// the remaining responses (the CREATE completion + GETINFO + CLOSE etc.) as a compound.
//
// The asyncID is used to set FlagAsync on the CREATE's final response header
// so the client correlates it with the earlier interim.
func completeCompoundAfterAsyncCreate(
	ctx context.Context,
	firstHeader *header.SMB2Header,
	createStatus types.Status,
	createBody []byte,
	asyncID uint64,
	compoundData []byte,
	connInfo *ConnInfo,
	isEncrypted bool,
	asyncNotifyCallback handlers.AsyncResponseCallback,
) {
	// Build CREATE completion response (async header format).
	createCredits := grantConnectionCredits(connInfo, firstHeader.SessionID, firstHeader.Credits, firstHeader.CreditCharge)
	createRespHeader := header.NewResponseHeaderWithCredits(firstHeader, createStatus, createCredits)
	createRespHeader.Flags |= types.FlagAsync
	createRespHeader.AsyncId = asyncID

	createRespBody := createBody
	if (createStatus.IsError() || createStatus.IsWarning()) &&
		createStatus != types.StatusMoreProcessingRequired &&
		createStatus != types.StatusBufferOverflow {
		createRespBody = MakeErrorBody()
	}
	if createRespBody == nil {
		createRespBody = MakeErrorBody()
	}

	// Seed the shared loop state from the completed CREATE, then process the
	// trailing related commands through the same processRemaining used by the
	// sync path (see M-A1: keeps the two paths from drifting — e.g. the
	// SESSION_EXPIRED propagation in relatedSessionFailureStatus now applies
	// here too).
	state := compoundLoopState{
		lastSessionID:        firstHeader.SessionID,
		lastTreeID:           firstHeader.TreeID,
		lastCmdFailed:        createStatus.IsError(),
		lastCmdSessionFailed: isSessionLevelError(createStatus),
		responses:            []compoundResponse{{respHeader: createRespHeader, body: createRespBody}},
	}
	if state.lastCmdFailed {
		state.lastCmdStatus = createStatus
	}
	// Extract the FileID from the CREATE response so related commands can inherit it.
	if createStatus == types.StatusSuccess && len(createBody) >= 80 {
		copy(state.lastFileID[:], createBody[64:80])
	}

	state.processRemaining(ctx, compoundData, connInfo, isEncrypted, asyncNotifyCallback)

	sendErr := sendCompoundResponses(state.responses, connInfo, isEncrypted)
	if sendErr != nil {
		logger.Debug("Error sending compound async completion responses", "error", sendErr)
	}

	// Release any parked-CREATE resume goroutines whose interim was buffered
	// into this completion frame, only after it is on the wire.
	state.releaseStartedGates(connInfo.Handler.PendingCreateRegistry)

	if sendErr == nil {
		for _, hook := range state.postSendHooks {
			hook()
		}
	}
}
