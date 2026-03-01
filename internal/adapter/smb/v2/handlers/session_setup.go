package handlers

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
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
	}

	// Try SPNEGO parsing to detect Kerberos vs NTLM
	if len(req.SecurityBuffer) >= 2 &&
		(req.SecurityBuffer[0] == 0x60 || req.SecurityBuffer[0] == 0xa0 || req.SecurityBuffer[0] == 0xa1) {
		parsed, err := auth.Parse(req.SecurityBuffer)
		if err == nil && parsed.Type == auth.TokenTypeInit && parsed.HasKerberos() && len(parsed.MechToken) > 0 {
			// SPNEGO contains a Kerberos token -- route to Kerberos auth
			logger.Debug("SPNEGO Kerberos token detected, routing to Kerberos auth",
				"mechTokenLen", len(parsed.MechToken))
			return h.handleKerberosAuth(ctx, parsed.MechToken)
		}
	}

	// Extract NTLM token (unwrap SPNEGO if needed)
	ntlmToken, isWrapped := extractNTLMToken(req.SecurityBuffer)

	// Process NTLM message
	if auth.IsValid(ntlmToken) {
		msgType := auth.GetMessageType(ntlmToken)
		logger.Debug("NTLM message detected",
			"type", msgType,
			"wrapped", isWrapped)

		switch msgType {
		case auth.Negotiate:
			return h.handleNTLMNegotiate(ctx, isWrapped)
		case auth.Authenticate:
			// Type 3 without pending auth - create guest session
			return h.createGuestSession(ctx)
		}
	}

	// No recognized auth mechanism - create guest session
	return h.createGuestSession(ctx)
}

// extractNTLMToken extracts the NTLM token from a security buffer.
// Handles both raw NTLM and SPNEGO-wrapped tokens.
// Returns the token and whether it was wrapped in SPNEGO.
func extractNTLMToken(securityBuffer []byte) ([]byte, bool) {
	if len(securityBuffer) == 0 {
		return securityBuffer, false
	}

	// Check if this might be SPNEGO-wrapped (GSSAPI or NegTokenResp)
	if len(securityBuffer) >= 2 && (securityBuffer[0] == 0x60 || securityBuffer[0] == 0xa0 || securityBuffer[0] == 0xa1) {
		parsed, err := auth.Parse(securityBuffer)
		if err != nil {
			logger.Debug("SPNEGO parse failed, treating as raw", "error", err)
			return securityBuffer, false
		}

		// Check if NTLM is offered
		if parsed.Type == auth.TokenTypeInit && !parsed.HasNTLM() {
			logger.Debug("SPNEGO token does not offer NTLM")
			return securityBuffer, false
		}

		if len(parsed.MechToken) > 0 {
			return parsed.MechToken, true
		}
	}

	// Already raw NTLM (or unknown format)
	return securityBuffer, false
}

// handleKerberosAuth handles Kerberos authentication via SPNEGO.
//
// This method validates the AP-REQ from the SPNEGO MechToken using the service
// keytab (shared with NFS Kerberos layer), extracts the client principal, maps
// it to a control plane user, and creates an authenticated session.
//
// The Kerberos auth path is a single round-trip (unlike NTLM's multi-step
// handshake): client sends AP-REQ, server validates and responds with success
// or failure.
//
// Parameters:
//   - ctx: SMB handler context
//   - mechToken: The Kerberos AP-REQ extracted from the SPNEGO NegTokenInit
//
// Returns:
//   - SUCCESS with SPNEGO accept-complete wrapping the AP-REP on successful auth
//   - STATUS_LOGON_FAILURE on invalid ticket, expired ticket, or unknown user
func (h *Handler) handleKerberosAuth(ctx *SMBHandlerContext, mechToken []byte) (*HandlerResult, error) {
	// Check that Kerberos provider is configured
	if h.KerberosProvider == nil {
		logger.Debug("Kerberos auth attempted but no provider configured")
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Parse the AP-REQ from the mech token
	var apReq messages.APReq
	if err := apReq.Unmarshal(mechToken); err != nil {
		logger.Debug("Failed to unmarshal Kerberos AP-REQ", "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Derive the SMB (CIFS) service principal from the configured NFS principal.
	// The shared Kerberos provider is configured with the NFS SPN (nfs/host@REALM),
	// but SMB clients present tickets for the CIFS SPN (cifs/host@REALM).
	smbPrincipal := h.KerberosProvider.ServicePrincipal()
	if strings.HasPrefix(smbPrincipal, "nfs/") {
		smbPrincipal = "cifs/" + strings.TrimPrefix(smbPrincipal, "nfs/")
	}

	// Build gokrb5 service settings using the shared keytab
	settings := service.NewSettings(
		h.KerberosProvider.Keytab(),
		service.MaxClockSkew(h.KerberosProvider.MaxClockSkew()),
		service.DecodePAC(false),
		service.KeytabPrincipal(smbPrincipal),
	)

	// Verify the AP-REQ
	ok, creds, err := service.VerifyAPREQ(&apReq, settings)
	if err != nil || !ok {
		logger.Info("Kerberos AP-REQ verification failed", "error", err, "ok", ok)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Extract principal name and realm
	principalName := creds.CName().PrincipalNameString()
	realm := creds.Domain()

	logger.Debug("Kerberos authentication succeeded",
		"principal", principalName,
		"realm", realm)

	// Map principal to control plane user.
	// Strip realm suffix (e.g., "alice@REALM" -> "alice") and service prefix
	// (e.g., "service/host" -> "service"). User principals are typically just
	// "alice" without "/" but we handle service principals gracefully.
	username := principalName
	if idx := strings.LastIndex(username, "@"); idx > 0 {
		username = username[:idx]
	}
	if idx := strings.Index(username, "/"); idx >= 0 {
		username = username[:idx]
	}

	// Look up the user in the control plane UserStore
	userStore := h.Registry.GetUserStore()
	if userStore == nil {
		logger.Debug("Kerberos auth: no UserStore configured, creating guest session")
		return h.createGuestSession(ctx)
	}

	user, err := userStore.GetUser(ctx.Context, username)
	if err != nil || user == nil || !user.Enabled {
		logger.Info("Kerberos auth: user lookup failed",
			"username", username, "principal", principalName,
			"found", user != nil, "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Create an authenticated session with the resolved user identity
	sessionID := h.GenerateSessionID()
	sess := h.CreateSessionWithUser(sessionID, ctx.ClientAddr, user, realm)
	ctx.SessionID = sessionID
	ctx.IsGuest = false

	logger.Debug("Kerberos session created",
		"sessionID", sess.SessionID,
		"username", sess.Username,
		"domain", sess.Domain,
		"isGuest", sess.IsGuest,
		"principal", principalName,
		"realm", realm)

	// Build SPNEGO accept-complete response.
	// For Kerberos, we wrap the success in a NegTokenResp with accept-completed state.
	// The mechToken (AP-REP) is nil because SMB2 does not require it: the SPNEGO
	// accept-completed negState is sufficient to signal success. Windows clients
	// and Samba both accept a nil responseToken in the final leg.
	spnegoResp, err := auth.BuildAcceptComplete(auth.OIDKerberosV5, nil)
	if err != nil {
		logger.Debug("Failed to build SPNEGO accept response", "error", err)
		// Auth succeeded but response building failed -- still return success without SPNEGO wrapper
		return h.buildSessionSetupResponse(
			types.StatusSuccess,
			0,
			nil,
		), nil
	}

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		0, // No guest flag - authenticated user
		spnegoResp,
	), nil
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
func (h *Handler) handleNTLMNegotiate(ctx *SMBHandlerContext, usedSPNEGO bool) (*HandlerResult, error) {
	// Generate session ID for this authentication attempt
	sessionID := h.GenerateSessionID()

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
	}
	h.StorePendingAuth(pending)

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
	ntlmToken, _ := extractNTLMToken(securityBuffer)

	// Parse the AUTHENTICATE message to extract username and domain
	authMsg, err := auth.ParseAuthenticate(ntlmToken)
	if err != nil {
		logger.Debug("Failed to parse NTLM AUTHENTICATE message", "error", err)
		// Fall back to guest session
		return h.createGuestSessionWithID(ctx, pending, nil)
	}

	logger.Debug("NTLM AUTHENTICATE message parsed",
		"username", authMsg.Username,
		"domain", authMsg.Domain,
		"workstation", authMsg.Workstation,
		"isAnonymous", authMsg.IsAnonymous,
		"ntResponseLen", len(authMsg.NtChallengeResponse),
		"negotiateFlags", fmt.Sprintf("0x%08x", authMsg.NegotiateFlags),
		"encryptedRandomSessionKeyLen", len(authMsg.EncryptedRandomSessionKey))

	// If anonymous authentication requested, create guest session
	if authMsg.IsAnonymous || authMsg.Username == "" {
		return h.createGuestSessionWithID(ctx, pending, nil)
	}

	// Try to authenticate against UserStore
	userStore := h.Registry.GetUserStore()

	if userStore != nil {
		// Look up user by username
		user, err := userStore.GetUser(ctx.Context, authMsg.Username)
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
						"error", validationErr)
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
				sess := h.CreateSessionWithUser(pending.SessionID, pending.ClientAddr, user, authMsg.Domain)
				ctx.IsGuest = false

				// Configure signing with derived signing key
				h.configureSessionSigningWithKey(sess, signingKey[:], ctx)

				logger.Debug("NTLM authentication complete (validated credentials)",
					"sessionID", sess.SessionID,
					"username", sess.Username,
					"domain", sess.Domain,
					"isGuest", sess.IsGuest,
					"signingEnabled", sess.ShouldSign())

				return h.buildAuthenticatedResponse(pending.UsedSPNEGO), nil
			}

			// SECURITY: User exists but no valid NT hash configured.
			// This is a transitional mode that allows authentication without password validation.
			// Any client knowing the username can authenticate - this bypasses credential checks entirely.
			// To fix: run 'dittofs user passwd <username>' to set an NT hash for this user.
			// Client may have presented an NTLM response, but we can't verify it due to missing NT hash.
			logger.Warn("SECURITY: User authenticated without credential validation (no NT hash configured)",
				"username", authMsg.Username,
				"action", "run 'dittofs user passwd' to fix")

			sess := h.CreateSessionWithUser(pending.SessionID, pending.ClientAddr, user, authMsg.Domain)
			ctx.IsGuest = false

			// No signing without proper session key derivation
			logger.Debug("NTLM authentication complete (no credential validation)",
				"sessionID", sess.SessionID,
				"username", sess.Username,
				"domain", sess.Domain,
				"isGuest", sess.IsGuest)

			return h.buildAuthenticatedResponse(pending.UsedSPNEGO), nil
		}

		// User not found or disabled
		if err != nil {
			logger.Debug("User not found in UserStore", "username", authMsg.Username, "error", err)
		} else if user != nil && !user.Enabled {
			logger.Debug("User account disabled", "username", authMsg.Username)
			return NewErrorResult(types.StatusLogonFailure), nil
		}
	}

	// Fall back to guest session
	return h.createGuestSessionWithID(ctx, pending, nil)
}

// createGuestSessionWithID creates a guest session with a specific session ID.
// Used when completing NTLM authentication as guest.
// The sessionKey parameter is ignored for guest sessions (signing not supported).
//
// Guest sessions set SMB2_SESSION_FLAG_IS_GUEST (0x0001) in the response flags,
// which tells the client that signing is not required for this session.
//
// Windows 11 24H2 note: Insecure guest logons are blocked by default.
// Users must enable the "AllowInsecureGuestAuth" Group Policy setting
// (Computer Configuration > Administrative Templates > Network > Lanman Workstation
// > Enable insecure guest logons) to connect as guest.
func (h *Handler) createGuestSessionWithID(ctx *SMBHandlerContext, pending *PendingAuth, _ []byte) (*HandlerResult, error) {
	sess := h.CreateSessionWithID(pending.SessionID, pending.ClientAddr, true, "guest", "")
	ctx.IsGuest = true

	// Note: Signing is not configured for guest sessions because there's no
	// valid session key derivation possible without proper credentials.

	logger.Debug("NTLM authentication complete (guest)",
		"sessionID", sess.SessionID,
		"username", sess.Username,
		"isGuest", sess.IsGuest)

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		types.SMB2SessionFlagIsGuest,
		nil,
	), nil
}

// createGuestSession creates a guest session without NTLM handshake.
//
// This is used when:
//   - Client sends no authentication token
//   - Client sends unrecognized authentication mechanism
//   - Client sends Type 3 without prior Type 1 (graceful handling)
//
// Guest sessions set SMB2_SESSION_FLAG_IS_GUEST (0x0001) in the response flags,
// which tells the client that signing is not required for this session.
//
// Windows 11 24H2 note: Insecure guest logons are blocked by default.
// Users must enable the "AllowInsecureGuestAuth" Group Policy setting
// (Computer Configuration > Administrative Templates > Network > Lanman Workstation
// > Enable insecure guest logons) to connect as guest.
func (h *Handler) createGuestSession(ctx *SMBHandlerContext) (*HandlerResult, error) {
	// Create session using SessionManager (includes credit tracking)
	sess := h.CreateSession(ctx.ClientAddr, true, "guest", "")

	ctx.SessionID = sess.SessionID
	ctx.IsGuest = true

	// Note: Signing is not configured for guest sessions because there's no
	// valid session key derivation possible without proper credentials.

	logger.Debug("Created guest session", "sessionID", sess.SessionID)

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		types.SMB2SessionFlagIsGuest,
		nil,
	), nil
}

// configureSessionSigningWithKey sets up message signing for a session with
// a pre-derived session key from NTLMv2 authentication.
//
// For SMB 2.x sessions: creates an HMACSigner directly from the session key.
// For SMB 3.x sessions: derives all 4 keys via SP800-108 KDF using the
// negotiated dialect, preauth integrity hash, cipher ID, and signing algorithm.
//
// The ctx parameter provides access to the connection's CryptoState which holds
// the negotiated dialect and algorithm parameters from NEGOTIATE.
//
// [MS-SMB2] Section 3.3.5.5.3 - Session signing is established here
func (h *Handler) configureSessionSigningWithKey(sess *session.Session, sessionKey []byte, ctx *SMBHandlerContext) {
	if !h.SigningConfig.Enabled || len(sessionKey) == 0 {
		logger.Debug("Session signing NOT configured",
			"sessionID", sess.SessionID,
			"signingConfigEnabled", h.SigningConfig.Enabled,
			"sessionKeyLen", len(sessionKey))
		return
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
			preauthHash = ctx.ConnCryptoState.GetPreauthHash()
			cipherId = ctx.ConnCryptoState.GetCipherId()
			signingAlgId = ctx.ConnCryptoState.GetSigningAlgorithmId()
		}
	}

	logger.Debug("Configuring session signing",
		"sessionID", sess.SessionID,
		"dialect", dialect.String(),
		"signingKeyLen", len(sessionKey),
		"signingAlgId", signingAlgId,
		"cipherId", cipherId,
		"is3x", dialect >= types.Dialect0300)

	if dialect >= types.Dialect0300 {
		// SMB 3.x: derive all 4 keys via SP800-108 KDF
		cryptoState := session.DeriveAllKeys(sessionKey, dialect, preauthHash, cipherId, signingAlgId)
		cryptoState.SigningEnabled = true
		cryptoState.SigningRequired = h.SigningConfig.Required
		sess.SetCryptoState(cryptoState)
	} else {
		// SMB 2.x: direct HMAC-SHA256 from session key
		sess.SetSigningKey(sessionKey)
		sess.EnableSigning(h.SigningConfig.Required)
	}

	logger.Debug("Session signing configured",
		"sessionID", sess.SessionID,
		"enabled", sess.ShouldSign(),
		"shouldVerify", sess.ShouldVerify(),
		"required", h.SigningConfig.Required,
		"dialect", dialect.String())
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
// authenticated (non-guest) user. If the client used SPNEGO wrapping, the
// response includes an accept-completed token to finalize the GSSAPI context.
func (h *Handler) buildAuthenticatedResponse(usedSPNEGO bool) *HandlerResult {
	var acceptToken []byte
	if usedSPNEGO {
		var err error
		acceptToken, err = auth.BuildAcceptComplete(auth.OIDNTLMSSP, nil)
		if err != nil {
			logger.Debug("Failed to build SPNEGO accept token", "error", err)
		}
	}

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		0, // No guest flag - authenticated user
		acceptToken,
	)
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
