package handlers

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/auth/ntlm"
	"github.com/marmos91/dittofs/internal/auth/spnego"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/session"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// =============================================================================
// SESSION_SETUP Request Parsing
// =============================================================================

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

	req := &SessionSetupRequest{
		StructureSize: binary.LittleEndian.Uint16(
			body[sessionSetupStructureSizeOffset : sessionSetupStructureSizeOffset+2]),
		Flags:        body[sessionSetupFlagsOffset],
		SecurityMode: body[sessionSetupSecurityModeOffset],
		Capabilities: binary.LittleEndian.Uint32(
			body[sessionSetupCapabilitiesOffset : sessionSetupCapabilitiesOffset+4]),
		Channel: binary.LittleEndian.Uint32(
			body[sessionSetupChannelOffset : sessionSetupChannelOffset+4]),
		PreviousSessionID: binary.LittleEndian.Uint64(
			body[sessionSetupPreviousSessionIDOffset : sessionSetupPreviousSessionIDOffset+8]),
	}

	// Extract security buffer
	// SecurityBufferOffset is relative to the beginning of the SMB2 header
	// The body we receive starts after the header, so we adjust
	secBufferOffset := binary.LittleEndian.Uint16(
		body[sessionSetupSecBufferOffsetOffset : sessionSetupSecBufferOffsetOffset+2])
	secBufferLength := binary.LittleEndian.Uint16(
		body[sessionSetupSecBufferLengthOffset : sessionSetupSecBufferLengthOffset+2])

	// Calculate actual offset in body (subtract header size)
	bufferStart := int(secBufferOffset) - smb2HeaderSize
	if bufferStart < sessionSetupFixedSize {
		bufferStart = sessionSetupFixedSize // Buffer starts after fixed fields
	}

	if secBufferLength > 0 && bufferStart+int(secBufferLength) <= len(body) {
		req.SecurityBuffer = body[bufferStart : bufferStart+int(secBufferLength)]
	}

	return req, nil
}

// =============================================================================
// SESSION_SETUP Handler
// =============================================================================

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

	// Extract NTLM token (unwrap SPNEGO if needed)
	ntlmToken, isWrapped := extractNTLMToken(req.SecurityBuffer)

	// Process NTLM message
	if ntlm.IsValid(ntlmToken) {
		msgType := ntlm.GetMessageType(ntlmToken)
		logger.Debug("NTLM message detected",
			"type", msgType,
			"wrapped", isWrapped)

		switch msgType {
		case ntlm.Negotiate:
			return h.handleNTLMNegotiate(ctx)
		case ntlm.Authenticate:
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
		parsed, err := spnego.Parse(securityBuffer)
		if err != nil {
			logger.Debug("SPNEGO parse failed, treating as raw", "error", err)
			return securityBuffer, false
		}

		// Check if NTLM is offered
		if parsed.Type == spnego.TokenTypeInit && !parsed.HasNTLM() {
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

// =============================================================================
// NTLM Authentication Handlers
// =============================================================================

// handleNTLMNegotiate handles NTLM Type 1 (NEGOTIATE) message.
//
// This starts the NTLM handshake by:
//  1. Generating a new session ID for this authentication attempt
//  2. Storing a PendingAuth record to track the handshake state
//  3. Building and returning a Type 2 (CHALLENGE) message
//
// The client will respond with Type 3 (AUTHENTICATE) which completes
// the handshake in completeNTLMAuth().
func (h *Handler) handleNTLMNegotiate(ctx *SMBHandlerContext) (*HandlerResult, error) {
	// Generate session ID for this authentication attempt
	sessionID := h.GenerateSessionID()

	// Store pending auth to track handshake state
	pending := &PendingAuth{
		SessionID:  sessionID,
		ClientAddr: ctx.ClientAddr,
		CreatedAt:  time.Now(),
	}
	h.StorePendingAuth(pending)

	// Update context so response includes the session ID
	ctx.SessionID = sessionID

	// Build NTLM Type 2 (CHALLENGE) response
	challengeMsg := ntlm.BuildChallenge()

	logger.Debug("Sending NTLM CHALLENGE",
		"sessionID", sessionID,
		"challengeLength", len(challengeMsg))

	// Return response with STATUS_MORE_PROCESSING_REQUIRED
	// Client will send Type 3 (AUTHENTICATE) next
	return h.buildSessionSetupResponse(
		types.StatusMoreProcessingRequired,
		0, // No session flags yet
		challengeMsg,
	), nil
}

// completeNTLMAuth handles NTLM Type 3 (AUTHENTICATE) message.
//
// This completes the NTLM handshake by:
//  1. Validating the pending authentication exists
//  2. Parsing the AUTHENTICATE message to extract username
//  3. Looking up the user in the UserStore (if configured)
//  4. Creating an authenticated or guest session
//  5. Cleaning up the pending authentication state
//
// Note: This implementation validates users by username lookup only.
// Full NTLMv2 response verification requires storing the server challenge
// and computing/comparing NT hashes, which is not yet implemented.
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
	authMsg, err := ntlm.ParseAuthenticate(ntlmToken)
	if err != nil {
		logger.Debug("Failed to parse NTLM AUTHENTICATE message", "error", err)
		// Fall back to guest session
		return h.createGuestSessionWithID(ctx, pending)
	}

	logger.Debug("NTLM AUTHENTICATE message parsed",
		"username", authMsg.Username,
		"domain", authMsg.Domain,
		"workstation", authMsg.Workstation,
		"isAnonymous", authMsg.IsAnonymous)

	// If anonymous authentication requested, create guest session
	if authMsg.IsAnonymous || authMsg.Username == "" {
		return h.createGuestSessionWithID(ctx, pending)
	}

	// Try to authenticate against UserStore
	var sess *session.Session
	userStore := h.Registry.GetUserStore()

	if userStore != nil {
		// Look up user by username
		user, err := userStore.GetUser(authMsg.Username)
		if err == nil && user != nil && user.Enabled {
			// User found and enabled - create authenticated session
			sess = h.CreateSessionWithUser(pending.SessionID, pending.ClientAddr, user, authMsg.Domain)
			ctx.IsGuest = false

			logger.Debug("NTLM authentication complete (authenticated user)",
				"sessionID", sess.SessionID,
				"username", sess.Username,
				"domain", sess.Domain,
				"isGuest", sess.IsGuest)

			// Return success without guest flag
			return h.buildSessionSetupResponse(
				types.StatusSuccess,
				0, // No guest flag - authenticated user
				nil,
			), nil
		}

		// User not found or disabled - check if guest fallback is allowed
		if err != nil {
			logger.Debug("User not found in UserStore", "username", authMsg.Username, "error", err)
		} else if !user.Enabled {
			logger.Debug("User account disabled", "username", authMsg.Username)
			return NewErrorResult(types.StatusLogonFailure), nil
		}
	}

	// Fall back to guest session
	return h.createGuestSessionWithID(ctx, pending)
}

// createGuestSessionWithID creates a guest session with a specific session ID.
// Used when completing NTLM authentication as guest.
func (h *Handler) createGuestSessionWithID(ctx *SMBHandlerContext, pending *PendingAuth) (*HandlerResult, error) {
	sess := h.CreateSessionWithID(pending.SessionID, pending.ClientAddr, true, "guest", "")
	ctx.IsGuest = true

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
func (h *Handler) createGuestSession(ctx *SMBHandlerContext) (*HandlerResult, error) {
	// Create session using SessionManager (includes credit tracking)
	sess := h.CreateSession(ctx.ClientAddr, true, "guest", "")

	ctx.SessionID = sess.SessionID
	ctx.IsGuest = true

	logger.Debug("Created guest session", "sessionID", sess.SessionID)

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		types.SMB2SessionFlagIsGuest,
		nil,
	), nil
}

// =============================================================================
// Response Building
// =============================================================================

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

	// Allocate response buffer
	respLen := sessionSetupRespFixedSize + len(securityBuffer)
	resp := make([]byte, respLen)

	// Write fixed fields
	binary.LittleEndian.PutUint16(
		resp[sessionSetupRespStructureSizeOffset:sessionSetupRespStructureSizeOffset+2],
		sessionSetupRespStructureSize,
	)
	binary.LittleEndian.PutUint16(
		resp[sessionSetupRespSessionFlagsOffset:sessionSetupRespSessionFlagsOffset+2],
		sessionFlags,
	)
	binary.LittleEndian.PutUint16(
		resp[sessionSetupRespSecBufferOffsetOffset:sessionSetupRespSecBufferOffsetOffset+2],
		securityBufferOffset,
	)
	binary.LittleEndian.PutUint16(
		resp[sessionSetupRespSecBufferLengthOffset:sessionSetupRespSecBufferLengthOffset+2],
		uint16(len(securityBuffer)),
	)

	// Copy security buffer
	if len(securityBuffer) > 0 {
		copy(resp[sessionSetupRespFixedSize:], securityBuffer)
	}

	return NewResult(status, resp)
}
