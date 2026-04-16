package handlers

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// SESSION_SETUP request structure offsets [MS-SMB2] 2.2.5
const (
	sessionSetupStructureSizeOffset     = 0  // 2 bytes: Always 25
	sessionSetupFlagsOffset             = 2  // 1 byte: Binding flags
	sessionSetupSecurityModeOffset      = 3  // 1 byte: Security mode
	sessionSetupCapabilitiesOffset      = 4  // 4 bytes: Client capabilities
	sessionSetupChannelOffset           = 8  // 4 bytes: Channel (must be 0)
	sessionSetupSecBufferOffsetOffset   = 12 // 2 bytes: Security buffer offset
	sessionSetupSecBufferLengthOffset   = 14 // 2 bytes: Security buffer length
	sessionSetupPreviousSessionIDOffset = 16 // 8 bytes: Previous session ID
	sessionSetupFixedSize               = 24 // Fixed part size (without buffer)
	sessionSetupMinSize                 = 25 // Minimum request size (per spec)
)

// SESSION_SETUP response structure offsets [MS-SMB2] 2.2.6
const (
	sessionSetupRespStructureSizeOffset   = 0 // 2 bytes: Always 9
	sessionSetupRespSessionFlagsOffset    = 2 // 2 bytes: Session flags
	sessionSetupRespSecBufferOffsetOffset = 4 // 2 bytes: Security buffer offset
	sessionSetupRespSecBufferLengthOffset = 6 // 2 bytes: Security buffer length
	sessionSetupRespFixedSize             = 8 // Fixed response size
	sessionSetupRespStructureSize         = 9 // StructureSize field value (per spec)

	// Security buffer offset is relative to SMB2 header start
	smb2HeaderSize = 64
)

// SessionSetupRequest represents a parsed SESSION_SETUP request.
// [MS-SMB2] Section 2.2.5
type SessionSetupRequest struct {
	StructureSize     uint16 // Must be 25
	Flags             uint8  // Binding flags
	SecurityMode      uint8  // Security mode
	Capabilities      uint32 // Client capabilities
	Channel           uint32 // Channel (must be 0 for first request)
	SecurityBuffer    []byte // Authentication token (NTLM or SPNEGO)
	PreviousSessionID uint64 // Previous session for re-authentication
}

// parseSessionSetupRequest parses the SESSION_SETUP request body.
// Returns the parsed request or an error if the body is malformed.
func parseSessionSetupRequest(body []byte) (*SessionSetupRequest, error) {
	if len(body) < sessionSetupMinSize {
		return nil, fmt.Errorf("body too short: need %d bytes, got %d",
			sessionSetupMinSize, len(body))
	}

	r := smbenc.NewReader(body)
	req := &SessionSetupRequest{
		StructureSize: r.ReadUint16(), // offset 0
		Flags:         r.ReadUint8(),  // offset 2
		SecurityMode:  r.ReadUint8(),  // offset 3
		Capabilities:  r.ReadUint32(), // offset 4
		Channel:       r.ReadUint32(), // offset 8
	}
	secBufferOffset := r.ReadUint16()      // offset 12
	secBufferLength := r.ReadUint16()      // offset 14
	req.PreviousSessionID = r.ReadUint64() // offset 16
	if r.Err() != nil {
		return nil, fmt.Errorf("session setup decode error: %w", r.Err())
	}

	// Extract security buffer
	// SecurityBufferOffset is relative to the beginning of the SMB2 header
	// The body we receive starts after the header, so we adjust
	bufferStart := int(secBufferOffset) - smb2HeaderSize
	if bufferStart < sessionSetupFixedSize {
		bufferStart = sessionSetupFixedSize // Buffer starts after fixed fields
	}

	if secBufferLength > 0 && bufferStart+int(secBufferLength) <= len(body) {
		req.SecurityBuffer = body[bufferStart : bufferStart+int(secBufferLength)]
	}

	return req, nil
}

// SessionSetup handles SMB2 SESSION_SETUP command.
//
// This handler implements NTLM authentication for SMB2 connections.
// The authentication flow is:
//
//  1. Client sends Type 1 (NEGOTIATE) → handleNTLMNegotiate()
//     Server responds with Type 2 (CHALLENGE) + STATUS_MORE_PROCESSING_REQUIRED
//
//  2. Client sends Type 3 (AUTHENTICATE) → completeNTLMAuth()
//     Server creates session + STATUS_SUCCESS
//
// Both raw NTLM and SPNEGO-wrapped NTLM are supported.
//
// [MS-SMB2] Section 2.2.5, 2.2.6, 3.3.5.5
func (h *Handler) SessionSetup(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Wait for any in-progress session cleanups to complete before proceeding.
	// When a client disconnects, its session cleanup (file close, lease release,
	// notify unregister) runs asynchronously in the old connection's goroutine.
	// Without this barrier, a new connection's SESSION_SETUP can race with the
	// old cleanup on shared Handler state (files, leases, notify watchers),
	// causing stale state to interfere with the new session's operations.
	h.WaitForCleanup()

	// State leak detection: log a snapshot of shared state after the cleanup
	// barrier has been passed. In a clean state, all counters should be 0
	// (or only contain state from other active sessions).
	h.LogStateSnapshot("SESSION_SETUP: state after cleanup barrier", ctx.SessionID)

	// Parse request
	req, err := parseSessionSetupRequest(body)
	if err != nil {
		logger.Debug("SESSION_SETUP parse error", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Log request details
	if len(req.SecurityBuffer) > 0 {
		prefix := req.SecurityBuffer
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		logger.Debug("Security buffer content",
			"prefix", fmt.Sprintf("%x", prefix),
			"length", len(req.SecurityBuffer))
	}

	logger.Debug("SESSION_SETUP request",
		"securityBufferLength", len(req.SecurityBuffer),
		"previousSessionID", req.PreviousSessionID,
		"contextSessionID", ctx.SessionID)

	// Check if this is a continuation of pending authentication
	if ctx.SessionID != 0 {
		if _, ok := h.GetPendingAuth(ctx.SessionID); ok {
			return h.completeNTLMAuth(ctx, req.SecurityBuffer)
		}

		// Re-authentication: client sends SESSION_SETUP on an existing session
		// with no pending auth. Per MS-SMB2 3.3.5.5.2, this initiates a new
		// authentication on the existing session (identity update).
		// The existing session remains valid until re-auth completes.
		if _, ok := h.GetSession(ctx.SessionID); ok {
			logger.Debug("SESSION_SETUP: re-authentication on existing session",
				"sessionID", ctx.SessionID)
			// Fall through to normal auth flow — the NTLM negotiate handler
			// will use ctx.SessionID as the session ID for the pending auth
		}
	}

	// Handle PreviousSessionID: tear down old session per MS-SMB2 3.3.5.5.3.
	// When a client reconnects (e.g. after network disruption), it sets
	// PreviousSessionID to its old session. The server tears down the old
	// session's resources (open files, locks, tree connections) so the new
	// session starts clean. This prevents resource leaks and lock conflicts.
	if req.PreviousSessionID != 0 {
		if _, ok := h.GetSession(req.PreviousSessionID); ok {
			logger.Info("SESSION_SETUP: tearing down previous session",
				"previousSessionID", req.PreviousSessionID)
			h.SignalPendingCleanup(1)
			h.CleanupSession(ctx.Context, req.PreviousSessionID, false)
		}
	}

	// Try SPNEGO parsing to detect Kerberos vs NTLM
	if len(req.SecurityBuffer) >= 2 &&
		(req.SecurityBuffer[0] == 0x60 || req.SecurityBuffer[0] == 0xa0 || req.SecurityBuffer[0] == 0xa1) {
		parsed, err := auth.Parse(req.SecurityBuffer)
		if err == nil && parsed.Type == auth.TokenTypeInit && parsed.HasKerberos() && len(parsed.MechToken) > 0 {
			// SPNEGO contains a Kerberos token -- route to Kerberos auth
			logger.Debug("SPNEGO Kerberos token detected, routing to Kerberos auth",
				"mechTokenLen", len(parsed.MechToken))
			result, kerberosErr := h.handleKerberosAuth(ctx, parsed.MechToken, parsed)
			if kerberosErr != nil {
				return result, kerberosErr
			}

			// If Kerberos auth failed, return SPNEGO reject so client can retry with NTLM.
			// Per user decision: clean SPNEGO reject, client retries with fresh SessionId=0.
			if result.Status == types.StatusLogonFailure {
				rejectToken, buildErr := auth.BuildReject()
				if buildErr == nil {
					logger.Info("Kerberos authentication failed, returning SPNEGO reject for NTLM fallback")
					return h.buildSessionSetupResponse(
						types.StatusLogonFailure,
						0,
						rejectToken,
					), nil
				}
			}
			return result, nil
		}
	}

	// Extract NTLM token (unwrap SPNEGO if needed)
	ntlmToken, isWrapped, mechListBytes := extractNTLMToken(req.SecurityBuffer)

	// Process NTLM message
	if auth.IsValid(ntlmToken) {
		// Check NTLM disable policy
		if !h.NtlmEnabled {
			logger.Info("NTLM authentication disabled by policy")
			return NewErrorResult(types.StatusLogonFailure), nil
		}

		msgType := auth.GetMessageType(ntlmToken)
		logger.Debug("NTLM message detected",
			"type", msgType,
			"wrapped", isWrapped)

		switch msgType {
		case auth.Negotiate:
			return h.handleNTLMNegotiate(ctx, isWrapped, mechListBytes)
		case auth.Authenticate:
			// Type 3 without prior Type 1/2 exchange — protocol violation per MS-SMB2 3.3.5.5
			logger.Debug("SESSION_SETUP: TYPE_3 without pending auth, rejecting")
			return NewErrorResult(types.StatusLogonFailure), nil
		}
	}

	// No recognized auth mechanism - create guest session
	return h.createGuestSession(ctx)
}

// extractNTLMToken extracts the NTLM token from a security buffer.
// Handles both raw NTLM and SPNEGO-wrapped tokens.
//
// Returns: (token, wasSPNEGOWrapped, mechListBytes).
// mechListBytes is the DER SEQUENCE OF OID from the NegTokenInit (nil for
// raw NTLM, NegTokenResp messages, or when SPNEGO parse falls back to the
// raw signature scan).
func extractNTLMToken(securityBuffer []byte) ([]byte, bool, []byte) {
	if len(securityBuffer) == 0 {
		return securityBuffer, false, nil
	}

	// Check if this might be SPNEGO-wrapped (GSSAPI or NegTokenResp)
	if len(securityBuffer) >= 2 && (securityBuffer[0] == 0x60 || securityBuffer[0] == 0xa0 || securityBuffer[0] == 0xa1) {
		parsed, err := auth.Parse(securityBuffer)
		if err != nil {
			logger.Debug("SPNEGO parse failed, scanning for NTLMSSP signature", "error", err)
			// Fallback: scan for NTLMSSP signature within the SPNEGO blob.
			// Some clients send NegTokenResp formats that gokrb5 can't parse,
			// but the NTLM token is still embedded in the ASN.1 structure.
			if token := findNTLMSSP(securityBuffer); token != nil {
				return token, true, nil
			}
			return securityBuffer, false, nil
		}

		// Check if NTLM is offered
		if parsed.Type == auth.TokenTypeInit && !parsed.HasNTLM() {
			logger.Debug("SPNEGO token does not offer NTLM")
			return securityBuffer, false, nil
		}

		if len(parsed.MechToken) > 0 {
			return parsed.MechToken, true, parsed.MechListBytes
		}
	}

	// Already raw NTLM (or unknown format)
	return securityBuffer, false, nil
}

// ntlmsspSignature is the NTLMSSP signature that starts every NTLM message.
var ntlmsspSignature = []byte{'N', 'T', 'L', 'M', 'S', 'S', 'P', 0}

// findNTLMSSP scans a buffer for the NTLMSSP signature and returns
// the NTLM token starting at that offset. This is used as a fallback
// when SPNEGO parsing fails but the NTLM token is embedded in the blob.
func findNTLMSSP(data []byte) []byte {
	if i := bytes.Index(data, ntlmsspSignature); i >= 0 {
		return data[i:]
	}
	return nil
}

// handleNTLMNegotiate handles NTLM Type 1 (NEGOTIATE) message.
//
// This starts the NTLM handshake by:
//  1. Generating a new session ID for this authentication attempt
//  2. Storing a PendingAuth record to track the handshake state
//  3. Building and returning a Type 2 (CHALLENGE) message
//
// The client will respond with Type 3 (AUTHENTICATE) which completes
// the handshake in completeNTLMAuth().
func (h *Handler) handleNTLMNegotiate(ctx *SMBHandlerContext, usedSPNEGO bool, mechListBytes []byte) (*HandlerResult, error) {
	// Reuse existing session ID for re-authentication, otherwise generate new
	sessionID := ctx.SessionID
	isReauth := false
	if sessionID == 0 {
		sessionID = h.GenerateSessionID()
	} else if _, ok := h.GetSession(sessionID); ok {
		// Session already exists with this ID — this is a re-authentication.
		// Per MS-SMB2 3.3.5.5.2: existing session keys are retained.
		isReauth = true
	}

	// Initialize per-session preauth hash for SMB 3.1.1 key derivation.
	// Per [MS-SMB2] 3.3.5.5: each session gets its own preauth hash chain
	// initialized from the connection hash. We pass our own request bytes
	// directly (rather than reading from a per-connection stash, which used
	// to race when multiple SESSION_SETUPs were dispatched concurrently —
	// issue #362).
	if ctx.ConnCryptoState != nil {
		ctx.ConnCryptoState.InitSessionPreauthHash(sessionID, ctx.RawRequest)
	}

	// Build NTLM Type 2 (CHALLENGE) response
	// This also returns the server challenge for later validation
	challengeMsg, serverChallenge := auth.BuildChallenge()

	// Store pending auth to track handshake state
	// Include the server challenge for NTLMv2 validation in completeNTLMAuth
	pending := &PendingAuth{
		SessionID:       sessionID,
		ClientAddr:      ctx.ClientAddr,
		CreatedAt:       time.Now(),
		ServerChallenge: serverChallenge,
		UsedSPNEGO:      usedSPNEGO,
		IsReauth:        isReauth,
		MechListBytes:   mechListBytes,
	}
	h.StorePendingAuth(pending)

	logger.Debug("Stored pending auth with challenge",
		"sessionID", sessionID,
		"serverChallenge", fmt.Sprintf("%x", serverChallenge))

	// Update context so response includes the session ID
	ctx.SessionID = sessionID

	// Wrap the NTLM challenge in SPNEGO if the client used SPNEGO wrapping.
	// Windows SSPI requires SPNEGO-wrapped responses throughout the handshake.
	securityBuffer := challengeMsg
	if usedSPNEGO {
		spnegoResp, err := auth.BuildAcceptIncomplete(auth.OIDNTLMSSP, challengeMsg)
		if err != nil {
			logger.Debug("Failed to build SPNEGO challenge wrapper", "error", err)
			// Fall back to raw NTLM
		} else {
			securityBuffer = spnegoResp
		}
	}

	logger.Debug("Sending NTLM CHALLENGE",
		"sessionID", sessionID,
		"challengeLength", len(challengeMsg),
		"spnegoWrapped", usedSPNEGO)

	// Return response with STATUS_MORE_PROCESSING_REQUIRED
	// Client will send Type 3 (AUTHENTICATE) next
	return h.buildSessionSetupResponse(
		types.StatusMoreProcessingRequired,
		0, // No session flags yet
		securityBuffer,
	), nil
}

// completeNTLMAuth handles NTLM Type 3 (AUTHENTICATE) message.
//
// This completes the NTLM handshake by:
//  1. Validating the pending authentication exists
//  2. Parsing the AUTHENTICATE message to extract username
//  3. Looking up the user in the UserStore (if configured)
//  4. Validating NTLMv2 response using the stored ServerChallenge
//  5. Deriving session key for message signing
//  6. Creating an authenticated or guest session
//  7. Configuring session signing with the derived key
//  8. Cleaning up the pending authentication state
func (h *Handler) completeNTLMAuth(ctx *SMBHandlerContext, securityBuffer []byte) (*HandlerResult, error) {
	// Get and validate pending auth
	pending, ok := h.GetPendingAuth(ctx.SessionID)
	if !ok {
		logger.Debug("No pending auth for session", "sessionID", ctx.SessionID)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Remove pending auth (handshake complete)
	h.DeletePendingAuth(ctx.SessionID)

	// Extract NTLM token (unwrap SPNEGO if needed)
	ntlmToken, _, _ := extractNTLMToken(securityBuffer)

	// Parse the AUTHENTICATE message to extract username and domain
	authMsg, err := auth.ParseAuthenticate(ntlmToken)
	if err != nil {
		logger.Debug("Failed to parse NTLM AUTHENTICATE message", "error", err)
		// Fall back to guest session
		return h.createGuestSessionWithID(ctx, pending)
	}

	logger.Debug("NTLM AUTHENTICATE message parsed",
		"username", authMsg.Username,
		"domain", authMsg.Domain,
		"workstation", authMsg.Workstation,
		"isAnonymous", authMsg.IsAnonymous,
		"ntResponseLen", len(authMsg.NtChallengeResponse),
		"negotiateFlags", fmt.Sprintf("0x%08x", authMsg.NegotiateFlags),
		"encryptedRandomSessionKeyLen", len(authMsg.EncryptedRandomSessionKey))

	// If anonymous authentication requested
	if authMsg.IsAnonymous || authMsg.Username == "" {
		if pending.IsReauth {
			if result := h.tryReauthUpdate(pending, "anonymous", "", nil, true); result != nil {
				ctx.IsGuest = true
				return result, nil
			}
		}
		return h.createGuestSessionWithID(ctx, pending)
	}

	// Try to authenticate against UserStore
	userStore := h.Registry.GetUserStore()

	if userStore != nil {
		// Resolve identity mapping: check if this NTLM principal maps to a different
		// control plane username (enables cross-protocol uid/gid consistency).
		principal := formatNTLMPrincipal(authMsg.Domain, authMsg.Username)
		resolvedUsername, mappingFound := h.resolveIdentityMapping(ctx.Context, principal, authMsg.Username)

		// Look up user by resolved username
		user, err := userStore.GetUser(ctx.Context, resolvedUsername)
		if err == nil && user != nil && user.Enabled {
			// User found and enabled - validate NTLMv2 response if NT hash is available
			ntHash, hasNTHash := user.GetNTHash()

			if hasNTHash && len(authMsg.NtChallengeResponse) > 0 {
				// Validate NTLMv2 response and derive session base key.
				// Windows clients may compute the NTLMv2 hash using different domain values
				// depending on how credentials were specified. Try the domain from the
				// AUTHENTICATE message first, then fall back to common alternatives.
				// [MS-NLMP] Section 3.3.2: UserDom may be empty, the target name, or
				// the MsvAvNbDomainName from the challenge's TargetInfo.
				hostname, _ := os.Hostname()
				domainsToTry := uniqueStrings([]string{
					authMsg.Domain,            // Domain from AUTHENTICATE message
					"",                        // Empty domain
					strings.ToUpper(hostname), // Server hostname (TargetName)
					"WORKGROUP",               // Default workgroup
				})

				logger.Debug("NTLMv2 validation attempt",
					"username", authMsg.Username,
					"ntResponseLen", len(authMsg.NtChallengeResponse),
					"domainsToTry", domainsToTry)

				var sessionBaseKey [16]byte
				var validationErr error
				for _, domain := range domainsToTry {
					sessionBaseKey, validationErr = auth.ValidateNTLMv2Response(
						ntHash,
						authMsg.Username,
						domain,
						pending.ServerChallenge,
						authMsg.NtChallengeResponse,
					)
					if validationErr == nil {
						logger.Debug("NTLMv2 validation succeeded",
							"username", authMsg.Username,
							"domain", domain)
						break
					}
					logger.Debug("NTLMv2 domain attempt failed",
						"domain", domain)
				}

				if validationErr != nil {
					logger.Debug("NTLMv2 validation failed with all domain variants",
						"username", authMsg.Username,
						"triedDomains", domainsToTry,
						"error", validationErr,
						"serverChallenge", fmt.Sprintf("%x", pending.ServerChallenge),
						"ntHashPrefix", fmt.Sprintf("%x", ntHash[:4]),
						"pendingSessionID", pending.SessionID)
					if pending.IsReauth {
						// Per MS-SMB2 3.3.5.5.2: if re-authentication fails,
						// the session MUST be deleted. Clean up resources and
						// mark it as logged off so prepareDispatch rejects
						// subsequent requests with STATUS_USER_SESSION_DELETED.
						// The session itself is kept alive (not deleted from the
						// session manager) so that in-flight goroutines can
						// still sign their responses. Actual session deletion
						// happens on connection close via cleanupSessions().
						logger.Info("Re-authentication failed, destroying session",
							"sessionID", pending.SessionID,
							"username", authMsg.Username)
						if sess, ok := h.GetSession(pending.SessionID); ok {
							sess.LoggedOff.Store(true)
						}
						h.CloseAllFilesForSession(ctx.Context, pending.SessionID, false)
						h.releaseSessionLeasesAndNotifies(ctx.Context, pending.SessionID)
						h.DeleteAllTreesForSession(pending.SessionID)
					}
					return NewErrorResult(types.StatusLogonFailure), nil
				}

				// Derive the final signing key
				// When KEY_EXCH is negotiated, the client sends an encrypted random session key
				// that we need to decrypt to get the actual signing key.
				logger.Debug("NTLM key derivation",
					"sessionID", pending.SessionID,
					"keyExchFlag", (authMsg.NegotiateFlags&auth.FlagKeyExch) != 0,
					"signFlag", (authMsg.NegotiateFlags&auth.FlagSign) != 0,
					"encryptedKeyLen", len(authMsg.EncryptedRandomSessionKey))

				signingKey := auth.DeriveSigningKey(
					sessionBaseKey,
					authMsg.NegotiateFlags,
					authMsg.EncryptedRandomSessionKey,
				)

				logger.Debug("Derived signing key",
					"sessionID", pending.SessionID,
					"usedKeyExch", (authMsg.NegotiateFlags&auth.FlagKeyExch) != 0 && len(authMsg.EncryptedRandomSessionKey) == 16)

				// Authentication successful with validated credentials
				ctx.IsGuest = false

				if pending.IsReauth {
					// Per MS-SMB2 3.3.5.5.3: re-derive keys from the new SessionBaseKey
					if result := h.tryReauthUpdateWithKeys(pending, resolvedUsername, authMsg.Domain, user, false, signingKey[:], authMsg.NegotiateFlags, ctx); result != nil {
						return result, nil
					}
					// Fallthrough: session disappeared between negotiate and auth (unlikely)
				}

				sess := h.CreateSessionWithUser(pending.SessionID, pending.ClientAddr, user, authMsg.Domain)

				// Configure signing with derived signing key
				if errResult := h.configureSessionSigningWithKey(sess, signingKey[:], ctx); errResult != nil {
					return errResult, nil
				}

				logger.Debug("NTLM authentication complete (validated credentials)",
					"sessionID", sess.SessionID,
					"username", sess.Username,
					"domain", sess.Domain,
					"isGuest", sess.IsGuest,
					"signingEnabled", sess.ShouldSign(),
					"encryptData", sess.ShouldEncrypt())

				return h.buildAuthenticatedResponse(pending, signingKey[:], authMsg.NegotiateFlags, sess.ShouldEncrypt()), nil
			}

			// SECURITY: User exists but no valid NT hash configured.
			// This is a transitional mode that allows authentication without password validation.
			// Any client knowing the username can authenticate - this bypasses credential checks entirely.
			// To fix: run 'dittofs user passwd <username>' to set an NT hash for this user.
			// Client may have presented an NTLM response, but we can't verify it due to missing NT hash.
			logger.Warn("SECURITY: User authenticated without credential validation (no NT hash configured)",
				"username", authMsg.Username,
				"action", "run 'dittofs user passwd' to fix")

			ctx.IsGuest = false

			if pending.IsReauth {
				if result := h.tryReauthUpdate(pending, resolvedUsername, authMsg.Domain, user, false); result != nil {
					return result, nil
				}
			}

			sess := h.CreateSessionWithUser(pending.SessionID, pending.ClientAddr, user, authMsg.Domain)

			// No signing without proper session key derivation
			logger.Debug("NTLM authentication complete (no credential validation)",
				"sessionID", sess.SessionID,
				"username", sess.Username,
				"domain", sess.Domain,
				"isGuest", sess.IsGuest)

			return h.buildAuthenticatedResponse(pending, nil, authMsg.NegotiateFlags, false), nil
		}

		// User not found or disabled
		if err != nil {
			logger.Debug("User not found in UserStore", "username", resolvedUsername, "error", err)
		} else if user != nil && !user.Enabled {
			logger.Debug("User account disabled", "username", resolvedUsername)
			return NewErrorResult(types.StatusLogonFailure), nil
		}

		// If an identity mapping existed but the resolved user doesn't exist,
		// hard-fail rather than falling through to guest. An operator created
		// this mapping intentionally — silently granting guest access is wrong.
		if mappingFound {
			logger.Info("Identity mapping resolved but user not found, denying access",
				"principal", principal, "resolvedUsername", resolvedUsername)
			return NewErrorResult(types.StatusLogonFailure), nil
		}
	}

	// Fall back to guest session
	return h.createGuestSessionWithID(ctx, pending)
}

// createGuestSessionWithID creates a guest session with a specific session ID.
// Used when completing NTLM authentication as guest.
func (h *Handler) createGuestSessionWithID(ctx *SMBHandlerContext, pending *PendingAuth) (*HandlerResult, error) {
	if result := h.checkGuestPolicy(); result != nil {
		return result, nil
	}

	sess := h.CreateSessionWithID(pending.SessionID, pending.ClientAddr, true, "guest", "")
	ctx.IsGuest = true

	logger.Info("Guest session created",
		"sessionID", sess.SessionID,
		"username", sess.Username)

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		types.SMB2SessionFlagIsGuest,
		nil,
	), nil
}

// createGuestSession creates a guest session without NTLM handshake.
// Used when the client sends no auth token, an unrecognized mechanism,
// or Type 3 without prior Type 1.
func (h *Handler) createGuestSession(ctx *SMBHandlerContext) (*HandlerResult, error) {
	if result := h.checkGuestPolicy(); result != nil {
		return result, nil
	}

	sess := h.CreateSession(ctx.ClientAddr, true, "guest", "")

	ctx.SessionID = sess.SessionID
	ctx.IsGuest = true

	logger.Info("Guest session created", "sessionID", sess.SessionID)

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		types.SMB2SessionFlagIsGuest,
		nil,
	), nil
}

// configureSessionSigningWithKey sets up message signing and encryption for a
// session with a pre-derived session key from NTLMv2 authentication.
//
// For SMB 2.x sessions: creates an HMACSigner directly from the session key.
// In encryption required mode, rejects 2.x sessions (they cannot encrypt).
// For SMB 3.x sessions: derives all 4 keys via SP800-108 KDF using the
// negotiated dialect, preauth integrity hash, cipher ID, and signing algorithm.
// Key derivation always occurs for 3.x when encryption is enabled, even if
// signing itself is disabled.
//
// The ctx parameter provides access to the connection's CryptoState which holds
// the negotiated dialect and algorithm parameters from NEGOTIATE.
//
// Returns a non-nil *HandlerResult only when the session must be rejected
// (encryption required but 2.x dialect, or encryptor creation fails).
// On success, returns nil.
//
// [MS-SMB2] Section 3.3.5.5.3 - Session signing/encryption is established here
func (h *Handler) configureSessionSigningWithKey(sess *session.Session, sessionKey []byte, ctx *SMBHandlerContext) *HandlerResult {
	if len(sessionKey) == 0 {
		logger.Debug("Session crypto NOT configured (no session key)",
			"sessionID", sess.SessionID)
		return nil
	}

	// Determine the negotiated dialect from the connection's CryptoState.
	// If CryptoState is nil (legacy 2.x path or tests), default to 2.0.2.
	dialect := types.Dialect0202
	var preauthHash [64]byte
	var cipherId uint16
	var signingAlgId uint16

	if ctx != nil && ctx.ConnCryptoState != nil {
		dialect = ctx.ConnCryptoState.GetDialect()
		if dialect >= types.Dialect0300 {
			// Per [MS-SMB2] 3.3.5.5: use the per-session preauth hash for key
			// derivation, not the connection-level hash. Each session maintains
			// its own hash chain including only that session's NEGOTIATE and
			// SESSION_SETUP messages.
			preauthHash = ctx.ConnCryptoState.GetSessionPreauthHash(sess.SessionID)
			cipherId = ctx.ConnCryptoState.GetCipherId()
			signingAlgId = ctx.ConnCryptoState.GetSigningAlgorithmId()
		}

		// Clean up the per-session preauth hash entry now that keys are derived
		ctx.ConnCryptoState.DeleteSessionPreauthHash(sess.SessionID)
	}

	encryptionEnabled := h.EncryptionConfig.Mode == "preferred" || h.EncryptionConfig.Mode == "required"

	logger.Debug("Configuring session crypto",
		"sessionID", sess.SessionID,
		"dialect", dialect.String(),
		"signingKeyLen", len(sessionKey),
		"signingEnabled", h.SigningConfig.Enabled,
		"signingAlgId", signingAlgId,
		"cipherId", cipherId,
		"encryptionMode", h.EncryptionConfig.Mode,
		"is3x", dialect >= types.Dialect0300)

	if dialect >= types.Dialect0300 {
		// SMB 3.x: always derive keys via SP800-108 KDF when signing or encryption
		// is enabled. Key derivation must not be skipped when only encryption is
		// enabled, since encryption keys come from the same KDF derivation.
		cryptoState := session.DeriveAllKeys(sessionKey, dialect, preauthHash, cipherId, signingAlgId)

		if h.SigningConfig.Enabled {
			cryptoState.SigningEnabled = true
			// Per MS-SMB2 3.3.5.5: for dialect 3.1.1, Session.SigningRequired
			// SHOULD be set to TRUE. Both Windows Server and Samba enforce this.
			cryptoState.SigningRequired = h.SigningConfig.Required || dialect == types.Dialect0311
		}

		// Encryption: activate encryptors for preferred/required modes on 3.x sessions.
		// Guest sessions never reach here (no session key), so they are exempt.
		if encryptionEnabled {
			// SMB 3.0/3.0.2 don't use negotiate contexts, so cipherId may be 0.
			// Per MS-SMB2 spec, AES-128-CCM is the mandatory cipher for SMB 3.0.
			encCipherId := cipherId
			if encCipherId == 0 && (dialect == types.Dialect0300 || dialect == types.Dialect0302) {
				encCipherId = types.CipherAES128CCM
			}

			// SMB 3.1.1 with no encryption negotiate context: cipherId stays 0.
			// The client explicitly opted out of encryption; skip encryptor creation
			// in preferred mode. In required mode, reject below.
			if encCipherId == 0 && h.EncryptionConfig.Mode == "required" {
				logger.Warn("Rejecting session: encryption required but no cipher negotiated",
					"sessionID", sess.SessionID, "dialect", dialect.String())
				h.DeleteSession(sess.SessionID)
				return NewErrorResult(types.StatusAccessDenied)
			}

			if encCipherId != 0 {
				cryptoState.EncryptData = true
				if err := cryptoState.CreateEncryptors(encCipherId); err != nil {
					if h.EncryptionConfig.Mode == "required" {
						logger.Warn("Failed to create session encryptors in required mode, rejecting session",
							"sessionID", sess.SessionID, "error", err)
						h.DeleteSession(sess.SessionID)
						return NewErrorResult(types.StatusAccessDenied)
					}
					// Preferred mode: degrade gracefully
					logger.Warn("Failed to create session encryptors, disabling encryption",
						"sessionID", sess.SessionID, "error", err)
					cryptoState.EncryptData = false
				} else {
					logger.Info("SMB3 encryption enabled for session",
						"sessionID", sess.SessionID,
						"cipherId", fmt.Sprintf("0x%04x", encCipherId),
						"dialect", dialect.String())
				}
			}
			// encCipherId == 0 && preferred mode: no encryption for this session
		}

		sess.SetCryptoState(cryptoState)
	} else {
		// SMB 2.x: cannot encrypt. Reject in required mode.
		if h.EncryptionConfig.Mode == "required" {
			logger.Warn("Rejecting SMB 2.x session: encryption required but 2.x cannot encrypt",
				"sessionID", sess.SessionID,
				"dialect", dialect.String())
			h.DeleteSession(sess.SessionID)
			return NewErrorResult(types.StatusAccessDenied)
		}

		// SMB 2.x: direct HMAC-SHA256 from session key (signing only)
		if h.SigningConfig.Enabled {
			sess.SetSigningKey(sessionKey)
			sess.EnableSigning(h.SigningConfig.Required)
		}
	}

	logger.Debug("Session crypto configured",
		"sessionID", sess.SessionID,
		"signingEnabled", sess.ShouldSign(),
		"shouldVerify", sess.ShouldVerify(),
		"encryptData", sess.ShouldEncrypt(),
		"dialect", dialect.String())

	return nil
}

// buildSessionSetupResponse builds the SESSION_SETUP response.
//
// Response structure [MS-SMB2] 2.2.6:
//
//	Offset  Size  Field                 Description
//	------  ----  -------------------   ----------------------------------
//	0       2     StructureSize         Always 9
//	2       2     SessionFlags          SMB2_SESSION_FLAG_* flags
//	4       2     SecurityBufferOffset  Offset from header start
//	6       2     SecurityBufferLength  Length of security buffer
//	8       var   Buffer                Security buffer (if present)
func (h *Handler) buildSessionSetupResponse(
	status types.Status,
	sessionFlags uint16,
	securityBuffer []byte,
) *HandlerResult {
	// Calculate security buffer offset
	// Offset is from start of SMB2 header (64 bytes + 8 byte fixed response)
	var securityBufferOffset uint16
	if len(securityBuffer) > 0 {
		securityBufferOffset = smb2HeaderSize + sessionSetupRespFixedSize
	}

	w := smbenc.NewWriter(sessionSetupRespFixedSize + len(securityBuffer))
	w.WriteUint16(sessionSetupRespStructureSize) // StructureSize
	w.WriteUint16(sessionFlags)                  // SessionFlags
	w.WriteUint16(securityBufferOffset)          // SecurityBufferOffset
	w.WriteUint16(uint16(len(securityBuffer)))   // SecurityBufferLength
	if len(securityBuffer) > 0 {
		w.WriteBytes(securityBuffer)
	}

	return NewResult(status, w.Bytes())
}

// buildAuthenticatedResponse builds a SESSION_SETUP success response for an
// authenticated (non-guest) user. When the client used SPNEGO wrapping and
// we have both the original mechList bytes and an ExportedSessionKey, the
// response carries an accept-completed NegTokenResp with an NTLMSSP v2
// mechListMIC (MS-NLMP 2.2.2.9.1 / RFC 4178). Per RFC 4178 §4.2.2 the
// supportedMech field is only valid in the first server reply, so it is
// omitted here — matches Samba's wire format.
//
// When the key is absent (no-NT-hash transitional path or reauth without
// key re-derivation) we emit an accept-completed without a MIC.
func (h *Handler) buildAuthenticatedResponse(pending *PendingAuth, exportedSessionKey []byte, negFlags auth.NegotiateFlag, encryptData bool) *HandlerResult {
	var acceptToken []byte
	if pending != nil && pending.UsedSPNEGO {
		var mic []byte
		if len(pending.MechListBytes) > 0 && len(exportedSessionKey) == 16 {
			var key [16]byte
			copy(key[:], exportedSessionKey)
			mic = auth.ComputeNTLMSSPMechListMIC(key, pending.MechListBytes, negFlags, nil)
		}
		var err error
		acceptToken, err = auth.BuildAcceptCompleteWithMIC(nil, nil, mic)
		if err != nil {
			logger.Debug("Failed to build SPNEGO accept token", "error", err)
		}
	}

	var sessionFlags uint16
	if encryptData {
		sessionFlags = types.SMB2SessionFlagEncryptData
	}

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		sessionFlags,
		acceptToken,
	)
}

// tryReauthUpdate updates an existing session's identity during re-authentication.
// Per MS-SMB2 3.3.5.5.3: the session keys are re-derived from the new
// SessionBaseKey. The session's tree connects and open files are preserved.
// Returns a non-nil *HandlerResult if the session was found and updated,
// or nil if the session no longer exists (caller should fall through).
func (h *Handler) tryReauthUpdate(pending *PendingAuth, username, domain string, user *models.User, isGuest bool) *HandlerResult {
	existingSess, ok := h.GetSession(pending.SessionID)
	if !ok {
		return nil
	}
	existingSess.Username = username
	existingSess.Domain = domain
	existingSess.User = user
	existingSess.IsGuest = isGuest
	existingSess.IsNull = username == "" && !isGuest

	logger.Info("Session re-authenticated (identity updated, keys retained)",
		"sessionID", existingSess.SessionID,
		"username", existingSess.Username,
		"domain", existingSess.Domain,
		"signingEnabled", existingSess.ShouldSign(),
		"encryptData", existingSess.ShouldEncrypt())

	// Prior keys retained, no new ExportedSessionKey available.
	return h.buildAuthenticatedResponse(pending, nil, 0, existingSess.ShouldEncrypt())
}

// tryReauthUpdateWithKeys updates an existing session's identity and re-derives
// session keys during re-authentication. Per MS-SMB2 3.3.5.5.3: on successful
// re-authentication, the server MUST re-derive SigningKey, EncryptionKey, and
// DecryptionKey from the new SessionBaseKey. Tree connects and open files are
// preserved.
// Returns a non-nil *HandlerResult if the session was found and updated,
// or nil if the session no longer exists (caller should fall through).
func (h *Handler) tryReauthUpdateWithKeys(pending *PendingAuth, username, domain string, user *models.User, isGuest bool, signingKey []byte, negFlags auth.NegotiateFlag, ctx *SMBHandlerContext) *HandlerResult {
	existingSess, ok := h.GetSession(pending.SessionID)
	if !ok {
		return nil
	}
	existingSess.Username = username
	existingSess.Domain = domain
	existingSess.User = user
	existingSess.IsGuest = isGuest
	existingSess.IsNull = false

	// Re-derive session keys per MS-SMB2 3.3.5.5.3
	if len(signingKey) > 0 {
		if errResult := h.configureSessionSigningWithKey(existingSess, signingKey, ctx); errResult != nil {
			return errResult
		}
	}

	logger.Info("Session re-authenticated (keys re-derived)",
		"sessionID", existingSess.SessionID,
		"username", existingSess.Username,
		"domain", existingSess.Domain,
		"signingEnabled", existingSess.ShouldSign(),
		"encryptData", existingSess.ShouldEncrypt())

	return h.buildAuthenticatedResponse(pending, signingKey, negFlags, existingSess.ShouldEncrypt())
}

// checkGuestPolicy enforces guest session prerequisites.
// Returns an error result if guest sessions are not allowed, nil otherwise.
func (h *Handler) checkGuestPolicy() *HandlerResult {
	if !h.GuestEnabled {
		logger.Info("Guest session rejected: guest access disabled by policy")
		return NewErrorResult(types.StatusLogonFailure)
	}
	if h.SigningConfig.Required {
		logger.Info("Guest session rejected: server requires signing but guest has no session key")
		return NewErrorResult(types.StatusLogonFailure)
	}
	return nil
}

// uniqueStrings returns a deduplicated slice preserving order.
func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
