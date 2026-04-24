package smb

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
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

	// Credit validation: per MS-SMB2 3.3.5.2.3 and 3.3.5.2.5.
	// CreditCharge size validation is skipped for credit-exempt commands
	// (NEGOTIATE, CANCEL, first SESSION_SETUP with SessionID=0). We also
	// consume the sequence number for NEGOTIATE and first SESSION_SETUP so
	// Grant/Consume stay in lockstep with the client's cur_credits counter —
	// those commands burn fresh MessageIDs and receive a credit grant on
	// response; skipping Consume there would drift `available` up by one per
	// handshake (issue #378). The initial window covers both NEG MessageID
	// 0 and 1 (smbtorture uses 0, MS WPTS uses 1) so Consume succeeds
	// regardless of the client's choice.
	//
	// CANCEL is the exception: per MS-SMB2 3.3.5.16 it reuses the target
	// request's MessageID (the pending async operation's slot, already
	// consumed when that request arrived) and sends no response, so there
	// is nothing to Grant back. Calling Consume would double-consume the
	// slot and fail with STATUS_INVALID_PARAMETER, turning the CANCEL into
	// a spurious error response the client treats as a protocol violation
	// — observed in WPTS BVT_SMB2Basic_CancelRegisteredChangeNotify and
	// smbtorture smb2.notify.mask/tdis, replay.replay7.
	exempt := session.IsCreditExempt(reqHeader.Command, reqHeader.SessionID)
	if !exempt && connInfo.SupportsMultiCredit {
		if err := session.ValidateCreditCharge(reqHeader.Command, reqHeader.CreditCharge, body); err != nil {
			logger.Debug("Credit charge validation failed",
				"command", reqHeader.Command.String(),
				"creditCharge", reqHeader.CreditCharge,
				"error", err)
			return SendErrorResponse(reqHeader, types.StatusInvalidParameter, connInfo)
		}
	}
	if connInfo.SequenceWindow != nil && reqHeader.Command != types.CommandCancel {
		charge := session.EffectiveCreditCharge(reqHeader.CreditCharge)
		if !connInfo.SequenceWindow.Consume(reqHeader.MessageID, charge) {
			logger.Debug("Sequence window validation failed",
				"command", reqHeader.Command.String(),
				"messageID", reqHeader.MessageID,
				"creditCharge", charge,
				"exempt", exempt)
			return SendErrorResponse(reqHeader, types.StatusInvalidParameter, connInfo)
		}
	}

	cmd, handlerCtx, errStatus := prepareDispatch(ctx, reqHeader, connInfo)
	if errStatus != 0 {
		return SendErrorResponse(reqHeader, errStatus, connInfo)
	}

	handlerCtx.RequestEncrypted = isEncrypted
	// Make the raw request bytes available to handlers that need them
	// for preauth integrity hash chaining (SESSION_SETUP). See #362.
	handlerCtx.RawRequest = rawMessage

	// Per MS-SMB2 3.3.5.2.1: enforce encryption requirements.
	if errStatus := checkEncryptionRequired(reqHeader, connInfo, isEncrypted); errStatus != 0 {
		return SendErrorResponse(reqHeader, errStatus, connInfo)
	}

	// Wire async slot accounting for max_async_credits enforcement (MS-SMB2 §3.3.5.2.5).
	handlerCtx.TryReserveAsync = connInfo.TryReserveAsync
	handlerCtx.ReleaseAsync = connInfo.ReleaseAsync

	// For CHANGE_NOTIFY, set up async callback so notifications can be sent
	if reqHeader.Command == types.SMB2ChangeNotify && asyncNotifyCallback != nil {
		handlerCtx.AsyncNotifyCallback = asyncNotifyCallback
	}

	// For READ commands, provide the async pipe-read completion callback so that
	// handlePipeRead can pend reads on empty named pipes (MS-SMB2 §3.3.5.12).
	if reqHeader.Command == types.SMB2Read {
		ci := connInfo // capture for closure
		handlerCtx.AsyncPipeReadCallback = func(sessionID, messageID, asyncId uint64, status types.Status, data []byte) error {
			ci.ReleaseAsync()
			return SendAsyncCompletionResponse(sessionID, messageID, asyncId, types.SMB2Read, status, encodeReadResponseBody(data), ci)
		}
	}

	// For CANCEL, pass the request's AsyncId so the handler can identify
	// which async operation to cancel (e.g., pending CHANGE_NOTIFY).
	if reqHeader.Command == types.SMB2Cancel && reqHeader.Flags.IsAsync() {
		handlerCtx.RequestAsyncId = reqHeader.AsyncId
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
		// Return any pooled buffer the handler acquired before dropping the
		// connection. Today no handler combines DropConnection with a pooled
		// response, but this guard prevents a latent leak if one is added.
		if result.ReleaseData != nil {
			result.ReleaseData()
		}
		return connInfo.Conn.Close()
	}

	// Track session lifecycle for connection cleanup
	TrackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status, result.IsBinding, connInfo.SessionTracker)

	// Send response and run after-hooks with the response bytes. If the
	// write fails, return early — any registered PostSend hook is
	// intentionally dropped because the connection is likely dead and the
	// hook would just log spurious SendMessage errors on a torn-down
	// session. (Same contract as the compound dispatch path.)
	if err := SendResponseWithHooks(reqHeader, handlerCtx, result, connInfo); err != nil {
		return err
	}

	// Per MS-SMB2 3.3.4.1: some handlers (currently CLOSE with a pending
	// CHANGE_NOTIFY) need to deliver an async response strictly AFTER their
	// own synchronous response has been written. Invoke any PostSend hook
	// now, with writeMu released, so the hook's own SendMessage re-acquires
	// the lock cleanly and the cleanup notification is unambiguously ordered
	// after the CLOSE response.
	if handlerCtx != nil && handlerCtx.PostSend != nil {
		handlerCtx.PostSend()
	}

	// NOTE: We intentionally do NOT delete the session here. The session is
	// kept alive with LoggedOff=true so that in-flight request goroutines
	// (dispatched before LOGOFF was read) can still sign their responses via
	// SendMessage. Without this, a concurrent goroutine calling GetSession()
	// after deletion would get ok=false, send the response unsigned, and the
	// client would reject it with "Bad SMB2 signature" / STATUS_ACCESS_DENIED.
	//
	// The session is cleaned up on connection close via cleanupSessions().
	// The verifier and prepareDispatch already handle LoggedOff=true correctly:
	//   - verifier: skips signature verification (lets prepareDispatch handle it)
	//   - prepareDispatch: returns STATUS_USER_SESSION_DELETED

	return nil
}

// prepareDispatch looks up the command in the dispatch table, builds the handler context,
// and validates session/tree requirements. Returns the command, context, and an error
// status (0 on success). This consolidates the shared setup logic used by both
// ProcessSingleRequest and ProcessRequestWithFileIDAndCallback.
func prepareDispatch(ctx context.Context, reqHeader *header.SMB2Header, connInfo *ConnInfo) (*Command, *handlers.SMBHandlerContext, types.Status) {
	cmd, ok := DispatchTable[reqHeader.Command]
	if !ok {
		// Per MS-SMB2 3.3.5.2: invalid command codes → STATUS_INVALID_PARAMETER
		logger.Debug("Unknown SMB2 command", "command", reqHeader.Command)
		return nil, nil, types.StatusInvalidParameter
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

	// Thread the stable per-connection ID so SMB2 multi-channel session
	// binding (MS-SMB2 §3.3.5.5.2) can key per-channel signing state.
	handlerCtx.ConnID = connInfo.ConnID

	// Thread the connection's transport so completeSessionBind can attach
	// it to the newly-registered Channel, enabling break notifications
	// (MS-SMB2 §3.3.4.7) to fan out across bound secondary channels.
	handlerCtx.ConnTransport = connInfo

	if cmd.NeedsSession && reqHeader.SessionID != 0 {
		sess, ok := connInfo.Handler.GetSession(reqHeader.SessionID)
		if !ok || sess.LoggedOff.Load() {
			return nil, nil, types.StatusUserSessionDeleted
		}
		if sess.IsExpired() {
			logger.Debug("Kerberos ticket expired",
				"sessionID", reqHeader.SessionID,
				"username", sess.Username,
				"expiresAt", sess.ExpiresAt)
			return nil, nil, types.StatusNetworkSessionExpired
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

// ProcessRequestWithFileIDAndCallback processes a request and returns the FileID
// if applicable (for CREATE). Used in compound request processing where FileID
// propagation is needed. Also returns the handler context so callers (compound
// processing) can pass handler-populated fields (e.g. SessionID from
// SESSION_SETUP, TreeID from TREE_CONNECT) through to SendResponse.
// The asyncNotifyCallback wires async responses for CHANGE_NOTIFY in compounds.
// rawMessage is the exact wire bytes for this (sub)command — used by handlers
// that need to hash the original request (SESSION_SETUP preauth chain per
// [MS-SMB2] 3.3.5.5). Callers on the compound path MUST thread per-subcommand
// wire bytes here; the first-command caller can pass the concatenation of
// firstHeader.Encode() and firstBody when no sliced buffer is available.
func ProcessRequestWithFileIDAndCallback(ctx context.Context, reqHeader *header.SMB2Header, body []byte, rawMessage []byte, connInfo *ConnInfo, isEncrypted bool, asyncNotifyCallback handlers.AsyncResponseCallback) (*HandlerResult, [16]byte, *handlers.SMBHandlerContext) {
	var fileID [16]byte

	cmd, handlerCtx, errStatus := prepareDispatch(ctx, reqHeader, connInfo)
	if errStatus != 0 {
		return &HandlerResult{Status: errStatus, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	handlerCtx.RequestEncrypted = isEncrypted
	// Thread raw wire bytes so SESSION_SETUP preauth chaining sees the exact
	// input the client hashed — including compound framing fields like
	// NextCommand. Re-encoding reqHeader would diverge on those fields.
	handlerCtx.RawRequest = rawMessage

	// Per MS-SMB2 3.3.5.2.1: enforce encryption requirements.
	if errStatus := checkEncryptionRequired(reqHeader, connInfo, isEncrypted); errStatus != 0 {
		return &HandlerResult{Status: errStatus, Data: MakeErrorBody()}, fileID, handlerCtx
	}

	// Wire async slot accounting for max_async_credits enforcement (MS-SMB2 §3.3.5.2.5).
	handlerCtx.TryReserveAsync = connInfo.TryReserveAsync
	handlerCtx.ReleaseAsync = connInfo.ReleaseAsync

	// For CHANGE_NOTIFY in compound, wire the async callback so notifications
	// can be sent separately from the compound response.
	if reqHeader.Command == types.SMB2ChangeNotify && asyncNotifyCallback != nil {
		handlerCtx.AsyncNotifyCallback = asyncNotifyCallback
	}

	// For READ commands, wire the async pipe-read completion callback.
	if reqHeader.Command == types.SMB2Read {
		ci := connInfo
		handlerCtx.AsyncPipeReadCallback = func(sessionID, messageID, asyncId uint64, status types.Status, data []byte) error {
			ci.ReleaseAsync()
			return SendAsyncCompletionResponse(sessionID, messageID, asyncId, types.SMB2Read, status, encodeReadResponseBody(data), ci)
		}
	}

	// For CANCEL, pass the request's AsyncId so the handler can identify
	// which async operation to cancel.
	if reqHeader.Command == types.SMB2Cancel && reqHeader.Flags.IsAsync() {
		handlerCtx.RequestAsyncId = reqHeader.AsyncId
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
	TrackSessionLifecycle(reqHeader.Command, reqHeader.SessionID, handlerCtx.SessionID, result.Status, result.IsBinding, connInfo.SessionTracker)

	// Extract FileID from CREATE response (bytes 64-80)
	if reqHeader.Command == types.SMB2Create && result.Status == types.StatusSuccess && len(result.Data) >= 80 {
		copy(fileID[:], result.Data[64:80])
	}

	return result, fileID, handlerCtx
}

// ProcessRequestWithInheritedFileID processes a request using an inherited FileID.
// InjectFileID is a no-op for commands that do not use a FileID, so no pre-filtering is needed.
// The asyncNotifyCallback parameter wires async responses for CHANGE_NOTIFY in compound.
// rawMessage is the wire bytes for this (sub)command (see
// ProcessRequestWithFileIDAndCallback for details).
// Returns the handler result, any new FileID (from CREATE), and the handler context.
func ProcessRequestWithInheritedFileID(ctx context.Context, reqHeader *header.SMB2Header, body []byte, rawMessage []byte, inheritedFileID [16]byte, connInfo *ConnInfo, isEncrypted bool, asyncNotifyCallback handlers.AsyncResponseCallback) (*HandlerResult, [16]byte, *handlers.SMBHandlerContext) {
	body = InjectFileID(reqHeader.Command, body, inheritedFileID)
	result, fileID, handlerCtx := ProcessRequestWithFileIDAndCallback(ctx, reqHeader, body, rawMessage, connInfo, isEncrypted, asyncNotifyCallback)
	return result, fileID, handlerCtx
}

// checkEncryptionRequired enforces global and per-share encryption requirements.
// Per MS-SMB2 3.3.5.2.1: if the server requires encryption (globally or for the
// tree's share) and the request was not encrypted, return STATUS_ACCESS_DENIED.
// NEGOTIATE and SESSION_SETUP are exempt because encryption keys are not yet available.
func checkEncryptionRequired(reqHeader *header.SMB2Header, connInfo *ConnInfo, isEncrypted bool) types.Status {
	if isEncrypted {
		return 0
	}

	// NEGOTIATE and SESSION_SETUP are always allowed unencrypted
	if reqHeader.Command == types.SMB2Negotiate || reqHeader.Command == types.SMB2SessionSetup {
		return 0
	}

	// Per MS-SMB2 3.3.5.2.9: anonymous/null sessions bypass encryption requirements.
	// Anonymous sessions have no session key and therefore cannot encrypt/decrypt.
	// Also skip encryption enforcement for guest sessions (no signing key).
	if reqHeader.SessionID != 0 {
		if sess, ok := connInfo.Handler.GetSession(reqHeader.SessionID); ok {
			if sess.IsNull || sess.IsGuest {
				return 0
			}
		}
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
// exact wire plaintext bytes (post-sign, pre-encrypt). The after-hook must run
// BEFORE the wire write to close a race window on the preauth hash chain:
//
// Per MS-SMB2 3.3.5.5, each SESSION_SETUP request/response mutates a per-session
// preauth hash used to derive signing keys. If the response's hash update runs
// AFTER the wire write, the client can receive the response and pipeline its
// next SESSION_SETUP request; that request's before-hook (chaining ssReqN+1)
// may then execute before the previous response's after-hook (chaining ssRespN)
// completes — the server's chain diverges from the client's, producing a wrong
// signing key and a "Bad SMB2 signature" rejection. See issue #362.
func SendResponseWithHooks(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) error {
	respHeader, body := buildResponseHeaderAndBody(reqHeader, ctx, result, connInfo)

	preWrite := func(wirePlaintext []byte) {
		RunAfterHooks(connInfo, reqHeader.Command, wirePlaintext)
	}
	err := sendMessage(respHeader, body, connInfo, preWrite)
	// Per D-09: fire ReleaseData AFTER the wire write completes, regardless of
	// write success. The pooled buffer is no longer referenced once the write
	// attempt returns, so release is safe whether or not the bytes landed.
	if result.ReleaseData != nil {
		result.ReleaseData()
	}
	return err
}

// SendResponse sends an SMB2 response with credit management and signing.
func SendResponse(reqHeader *header.SMB2Header, ctx *handlers.SMBHandlerContext, result *HandlerResult, connInfo *ConnInfo) error {
	respHeader, body := buildResponseHeaderAndBody(reqHeader, ctx, result, connInfo)
	err := SendMessage(respHeader, body, connInfo)
	// Per D-09: see SendResponseWithHooks — release pooled buffer after wire write.
	if result.ReleaseData != nil {
		result.ReleaseData()
	}
	return err
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

	credits := grantConnectionCredits(connInfo, sessionID, reqHeader.Credits, reqHeader.CreditCharge)

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

	// Per [MS-SMB2] 3.3.5.15: When a handler returns STATUS_PENDING with an
	// AsyncId, the response is an interim async response. Set FlagAsync and
	// populate AsyncId on the header.
	if result.AsyncId != 0 {
		respHeader.Flags |= types.FlagAsync
		respHeader.AsyncId = result.AsyncId
	}

	// Per MS-SMB2 2.2.2: Error/warning responses use the ERROR format (9 bytes)
	// instead of the command-specific body.
	// Exceptions:
	//   - StatusMoreProcessingRequired: SPNEGO token in SESSION_SETUP
	//   - StatusBufferOverflow: truncated data in QUERY_INFO
	//   - IOCTL with handler-provided body: per MS-SMB2 3.3.5.15.6.2,
	//     FSCTL_SRV_COPYCHUNK error responses include SRV_COPYCHUNK_RESPONSE
	//     with server limits or partial results in the output buffer.
	body := result.Data
	if (result.Status.IsError() || result.Status.IsWarning()) &&
		result.Status != types.StatusMoreProcessingRequired &&
		result.Status != types.StatusBufferOverflow {
		// Preserve IOCTL error response bodies when the handler explicitly set one.
		// Several FSCTLs (e.g., COPYCHUNK) encode meaningful data in error responses.
		if reqHeader.Command == types.SMB2Ioctl && result.Data != nil {
			// keep body = result.Data
		} else {
			body = MakeErrorBody()
		}
	}

	// Per [MS-SMB2] 3.3.5.15: STATUS_PENDING interim responses use the
	// error response body format (9 bytes) even though STATUS_PENDING is
	// a success-class status code. Ensure a body is always present.
	if result.Status == types.StatusPending && body == nil {
		body = MakeErrorBody()
	}

	return respHeader, body
}

// grantConnectionCredits computes the per-session credit grant, then atomically
// extends the connection's sequence window to match the returned value.
// Returning the window's actual-granted amount (rather than the requested
// amount with a separate Remaining()-based clamp) closes the TOCTOU window
// that would otherwise let two concurrent responses on the same connection
// both advertise the full remaining capacity. Per MS-SMB2 3.3.1.2 and
// Samba's source3/smbd/smb2_server.c:smb2_set_operation_credit.
//
// The grant is extended BEFORE the response is written. If the write later
// fails, the server's view of available credits exceeds the client's — a
// harmless undergrant on the next response, not an overgrant.
func grantConnectionCredits(connInfo *ConnInfo, sessionID uint64, requested, creditCharge uint16) uint16 {
	credits := connInfo.SessionManager.GrantCredits(sessionID, requested, creditCharge)
	if connInfo.SequenceWindow != nil {
		credits = connInfo.SequenceWindow.Grant(credits)
	}
	return credits
}

// SendErrorResponse sends an SMB2 error response.
// Per user decision: all responses on encrypted sessions are encrypted,
// including error responses.
func SendErrorResponse(reqHeader *header.SMB2Header, status types.Status, connInfo *ConnInfo) error {
	credits := grantConnectionCredits(connInfo, reqHeader.SessionID, reqHeader.Credits, reqHeader.CreditCharge)
	respHeader := header.NewResponseHeaderWithCredits(reqHeader, status, credits)
	return SendMessage(respHeader, MakeErrorBody(), connInfo)
}

// SendMessage sends an SMB2 message with NetBIOS framing, optional encryption,
// and optional signing.
//
// Per MS-SMB2 3.3.4.1.1 — Signing an Outgoing Message:
// If the session has Session.SigningRequired set and the message is not encrypted,
// the server MUST sign the response using the session's signing key. Encrypted
// sessions use AEAD for integrity instead of signing (MS-SMB2 3.3.4.1.3).
//
// Per MS-SMB2 3.3.5.5.3: the initial SESSION_SETUP SUCCESS response for a newly
// created session MUST NOT be encrypted (client hasn't derived keys yet), but
// MUST be signed. Re-authentication SESSION_SETUP responses ARE encrypted.
func SendMessage(hdr *header.SMB2Header, body []byte, connInfo *ConnInfo) error {
	return sendMessage(hdr, body, connInfo, nil)
}

// sendMessage is the internal implementation used by SendMessage and
// SendResponseWithHooks. It optionally invokes preWrite with the final
// wire-plaintext payload (post-sign, pre-encrypt) right before the bytes are
// written to the TCP connection. SendResponseWithHooks uses this to run the
// preauth integrity hash update in the window where the client cannot yet have
// observed the response — see the SendResponseWithHooks docstring.
func sendMessage(hdr *header.SMB2Header, body []byte, connInfo *ConnInfo, preWrite func(wirePlaintext []byte)) error {
	smbPayload := append(hdr.Encode(), body...)

	if hdr.SessionID != 0 {
		sess, ok := connInfo.Handler.GetSession(hdr.SessionID)
		if ok {
			// Per MS-SMB2 3.3.5.5.3 and 3.3.5.5.2: SESSION_SETUP responses
			// MUST be signed and MUST NOT be encrypted, in all three cases:
			// new session (client has no encryption keys yet), re-auth, and
			// channel bind (client must validate Channel.SigningKey from the
			// plaintext response before treating the channel as bound).
			// Applies to both the final SUCCESS response AND interim
			// STATUS_MORE_PROCESSING_REQUIRED challenges — the bind peer has
			// no channel-scoped keys yet, so encrypting the challenge makes
			// the client drop the connection with ACCESS_DENIED (#361).
			isSessionSetup := hdr.Command == types.SMB2SessionSetup
			isSessionSetupSuccess := isSessionSetup && hdr.Status == types.StatusSuccess
			if isSessionSetupSuccess && sess.NewlyCreated {
				sess.NewlyCreated = false // Clear so subsequent messages get encrypted
			}
			if sess.ShouldEncrypt() && connInfo.EncryptionMiddleware != nil && !isSessionSetup {
				// Run pre-write hook on the PLAINTEXT bytes — the preauth chain
				// hashes plaintext on both sides, not the encrypted wire form.
				if preWrite != nil {
					preWrite(smbPayload)
				}
				encrypted, err := connInfo.EncryptionMiddleware.EncryptResponse(hdr.SessionID, smbPayload)
				if err != nil {
					return fmt.Errorf("encrypt response: %w", err)
				}
				logger.Debug("Encrypted outgoing SMB2 message",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
				writeErr := WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, encrypted)
				// NOTE: the sequence window is extended synchronously in
				// grantConnectionCredits, BEFORE the write, so no post-write
				// Grant is needed here (#378).
				// See comment on the non-encrypted branch below: a write failure
				// after a preWrite hook leaves the preauth-hash chain advanced
				// for a response the peer never observed, so poison the
				// connection rather than risk a desynced next SESSION_SETUP.
				if writeErr != nil && preWrite != nil {
					_ = connInfo.Conn.Close()
				}
				return writeErr
			}
			if sess.ShouldSign() {
				sess.SignMessageOnChannel(connInfo.ConnID, smbPayload)
				// Sync signature back so callers that re-encode the header see
				// the real signature. Flag-level mutations from signing (setting
				// SMB2_FLAGS_SIGNED) exist only on smbPayload — the preauth chain
				// uses smbPayload directly via the preWrite hook to stay in sync
				// with the client's wire-byte hash.
				copy(hdr.Signature[:], smbPayload[48:64])
				logger.Debug("Signed outgoing SMB2 message",
					"command", hdr.Command.String(),
					"sessionID", hdr.SessionID)
			}
		}
	}

	// Run pre-write hook with the finalized plaintext wire payload. Must happen
	// BEFORE WriteNetBIOSFrame so the preauth hash update is visible to any
	// concurrently-dispatched successor request on the same session.
	if preWrite != nil {
		preWrite(smbPayload)
	}

	logger.Debug("Sent SMB2 response",
		"command", hdr.Command.String(),
		"status", hdr.Status.String(),
		"messageID", hdr.MessageID,
		"bytes", len(smbPayload))

	err := WriteNetBIOSFrame(connInfo.Conn, connInfo.WriteMu, connInfo.WriteTimeout, smbPayload)

	// NOTE: the sequence window is extended synchronously in
	// grantConnectionCredits, BEFORE the write, so no post-write Grant is
	// needed here. If this write fails the server's view of outstanding
	// credits stays ahead of the client's — the next response just grants
	// less, which is harmless (#378).

	// If the write failed after we already ran the preWrite hook, the
	// connection's preauth-hash chain has been advanced for a response the
	// peer never observed. Future SESSION_SETUP exchanges on this connection
	// would derive signing keys from a hash that diverges from the client's
	// view. Force the connection closed so the read loop terminates and
	// cleanupSessions() runs, rather than leaving it in a poisoned state.
	// In practice the TCP layer has already signalled an error to the reader,
	// but this is defensive — we don't want the hash-desync condition to
	// depend on timing between the write error and the read-side detection.
	if err != nil && preWrite != nil {
		_ = connInfo.Conn.Close()
	}

	return err
}

// SendAsyncChangeNotifyResponse sends an asynchronous CHANGE_NOTIFY response.
// This is called when a filesystem change matches a pending watch, or when
// a pending request is cancelled (STATUS_CANCELLED).
// The asyncId must match the one sent in the interim STATUS_PENDING response.
func SendAsyncChangeNotifyResponse(sessionID, messageID, asyncId uint64, response *handlers.ChangeNotifyResponse, connInfo *ConnInfo) error {
	// Release the async slot reserved when the CHANGE_NOTIFY was first pended.
	// This must happen exactly once per operation, regardless of outcome.
	connInfo.ReleaseAsync()

	status := response.GetStatus()

	// Build async response header with matching AsyncId.
	// Grant credits through the connection window so the client's cur_credits
	// counter stays in sync with the server's bookkeeping (#378). A CHANGE_NOTIFY
	// completion normally arrives without a correlated client request, so ask
	// for 1 credit — the window may deliver 0 if the connection is already at
	// the client's uint16 cap.
	credits := uint16(0)
	if connInfo.SequenceWindow != nil {
		credits = connInfo.SequenceWindow.Grant(1)
	}
	respHeader := &header.SMB2Header{
		Command:   types.SMB2ChangeNotify,
		Status:    status,
		Flags:     types.FlagResponse | types.FlagAsync,
		MessageID: messageID,
		SessionID: sessionID,
		AsyncId:   asyncId,
		Credits:   credits,
	}

	// Body format selection (per MS-SMB2 2.2.2 + WPTS Smb2Decoder.IsErrorPacket):
	//   - Genuine errors → SMB2 ERROR Response body.
	//   - STATUS_NOTIFY_CLEANUP / STATUS_NOTIFY_ENUM_DIR on a CHANGE_NOTIFY are
	//     NOT classified as errors by the WPTS SDK (search Smb2Decoder.cs for
	//     "STATUS_NOTIFY_CLEANUP"). They parse the body as a regular
	//     CHANGE_NOTIFY Response with an empty output buffer. The encoder
	//     enforces the mandatory 1-byte variable pad so the response is 9
	//     bytes total (matches Samba).
	//   - Normal completions with notification data → CHANGE_NOTIFY Response body.
	var body []byte
	if status.IsError() {
		body = MakeErrorBody()
		logger.Debug("Sending async CHANGE_NOTIFY error response",
			"sessionID", sessionID,
			"messageID", messageID,
			"asyncId", asyncId,
			"status", status.String())
	} else {
		var err error
		body, err = response.Encode()
		if err != nil {
			return fmt.Errorf("encode change notify response: %w", err)
		}
		logger.Debug("Sending async CHANGE_NOTIFY response",
			"sessionID", sessionID,
			"messageID", messageID,
			"asyncId", asyncId,
			"bufferLen", len(response.Buffer))
	}

	return SendMessage(respHeader, body, connInfo)
}

// SendAsyncCompletionResponse sends a standalone async completion response for
// a previously pending operation. This is used when a handler returns
// STATUS_PENDING with an AsyncId in a compound request: the compound includes
// an interim response at that position, and this function delivers the final
// result as a separate message with the matching AsyncId.
//
// Per MS-SMB2 3.3.4.4: The async completion response uses the async header
// format (FlagAsync set, AsyncId in header) and carries the handler's final
// status and response body.
//
// This is the general-purpose counterpart to SendAsyncChangeNotifyResponse --
// it handles any command type, not just CHANGE_NOTIFY.
func SendAsyncCompletionResponse(sessionID uint64, messageID uint64, asyncId uint64, command types.Command, status types.Status, body []byte, connInfo *ConnInfo) error {
	// Route the credit grant through the connection window; see
	// SendAsyncChangeNotifyResponse for the #378 rationale.
	credits := uint16(0)
	if connInfo.SequenceWindow != nil {
		credits = connInfo.SequenceWindow.Grant(1)
	}
	respHeader := &header.SMB2Header{
		StructureSize: header.HeaderSize,
		Command:       command,
		Status:        status,
		Flags:         types.FlagResponse | types.FlagAsync,
		MessageID:     messageID,
		SessionID:     sessionID,
		AsyncId:       asyncId,
		Credits:       credits,
	}

	// Per MS-SMB2 2.2.2: error/warning responses use the ERROR format.
	if (status.IsError() || status.IsWarning()) &&
		status != types.StatusMoreProcessingRequired &&
		status != types.StatusBufferOverflow {
		body = MakeErrorBody()
	}

	if body == nil {
		body = MakeErrorBody()
	}

	logger.Debug("Sending async completion response",
		"command", command.String(),
		"status", status.String(),
		"sessionID", sessionID,
		"messageID", messageID,
		"asyncId", asyncId)

	return SendMessage(respHeader, body, connInfo)
}

// encodeReadResponseBody encodes a READ response body for async pipe-read completion.
// Format per MS-SMB2 §2.2.20: StructureSize(2) + DataOffset(1) + Reserved(1) +
// DataLength(4) + DataRemaining(4) + Reserved2(4) + Data(variable).
// DataOffset 0x50 = 80 = SMB2 header (64) + READ response fixed header (16).
func encodeReadResponseBody(data []byte) []byte {
	dataLen := len(data)
	buf := make([]byte, 16+max(dataLen, 1))
	buf[0] = 17   // StructureSize low byte
	buf[1] = 0    // StructureSize high byte
	buf[2] = 0x50 // DataOffset
	buf[3] = 0    // Reserved
	buf[4] = byte(dataLen)
	buf[5] = byte(dataLen >> 8)
	buf[6] = byte(dataLen >> 16)
	buf[7] = byte(dataLen >> 24)
	// DataRemaining (4 bytes) = 0
	// Reserved2 (4 bytes) = 0
	if dataLen > 0 {
		copy(buf[16:], data)
	}
	return buf
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
//
// Channel-bind responses (isBinding==true) are explicitly skipped: a
// successful bind does not create a session on this connection — the
// session lives on a different connection, and tracking it here would
// cause this connection's close to delete the original session via
// cleanupSessions() (MS-SMB2 §3.3.5.5.2).
func TrackSessionLifecycle(command types.Command, reqSessionID, ctxSessionID uint64, status types.Status, isBinding bool, tracker SessionTracker) {
	if tracker == nil {
		return
	}

	switch command {
	case types.SMB2SessionSetup:
		if status != types.StatusSuccess || isBinding {
			return
		}
		sessionIDToTrack := ctxSessionID
		if sessionIDToTrack == 0 {
			sessionIDToTrack = reqSessionID
		}
		if sessionIDToTrack != 0 {
			tracker.TrackSession(sessionIDToTrack)
		}
	case types.SMB2Logoff:
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
