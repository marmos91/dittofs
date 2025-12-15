package handlers

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/auth"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// SessionSetup handles SMB2 SESSION_SETUP command [MS-SMB2] 2.2.5, 2.2.6
// Implements NTLM authentication handshake for macOS/Windows client compatibility.
func (h *Handler) SessionSetup(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 25 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.5
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 25
	// flags := body[2]
	// securityMode := body[3]
	// capabilities := binary.LittleEndian.Uint32(body[4:8])
	// channel := binary.LittleEndian.Uint32(body[8:12])
	securityBufferOffset := binary.LittleEndian.Uint16(body[12:14])
	securityBufferLength := binary.LittleEndian.Uint16(body[14:16])
	previousSessionID := binary.LittleEndian.Uint64(body[16:24])

	// Extract security buffer
	// SecurityBufferOffset is relative to the beginning of the SMB2 header (64 bytes)
	// The body we receive starts after the header, so we subtract 64
	secBufferStart := int(securityBufferOffset) - 64
	if secBufferStart < 24 {
		secBufferStart = 24 // Security buffer starts after fixed 24-byte fields
	}

	var securityBuffer []byte
	if securityBufferLength > 0 && secBufferStart+int(securityBufferLength) <= len(body) {
		securityBuffer = body[secBufferStart : secBufferStart+int(securityBufferLength)]
	}

	// Debug: log first few bytes of security buffer to see what we're getting
	if len(securityBuffer) > 0 {
		prefix := securityBuffer
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		logger.Debug("Security buffer content", "prefix", fmt.Sprintf("%x", prefix), "length", len(securityBuffer))
	}

	logger.Debug("SESSION_SETUP request",
		"securityBufferLength", securityBufferLength,
		"securityBufferOffset", securityBufferOffset,
		"previousSessionID", previousSessionID,
		"hasSecurityBuffer", len(securityBuffer) > 0)

	// Check if this is a continuation of an existing auth (Type 3 message)
	if ctx.SessionID != 0 {
		// This is the second round of NTLM (Type 3 - AUTHENTICATE)
		if _, ok := h.GetPendingAuth(ctx.SessionID); ok {
			return h.completeNTLMAuth(ctx, securityBuffer)
		}
	}

	// Check what kind of security token we received
	if len(securityBuffer) > 0 && auth.IsNTLMSSP(securityBuffer) {
		msgType := auth.GetNTLMMessageType(securityBuffer)
		logger.Debug("NTLMSSP message detected", "type", msgType)

		switch msgType {
		case auth.NTLMNegotiate:
			// Type 1 (NEGOTIATE) - respond with Type 2 (CHALLENGE)
			return h.handleNTLMNegotiate(ctx)
		case auth.NTLMAuthenticate:
			// Type 3 (AUTHENTICATE) - this shouldn't happen without a pending auth
			// but handle it gracefully as a new guest session
			return h.createGuestSession(ctx)
		}
	}

	// Check for SPNEGO wrapper
	if len(securityBuffer) > 0 && isGSSAPI(securityBuffer) {
		// SPNEGO wrapped - extract inner token
		innerToken := extractGSSAPIToken(securityBuffer)
		if auth.IsNTLMSSP(innerToken) {
			msgType := auth.GetNTLMMessageType(innerToken)
			logger.Debug("SPNEGO/NTLMSSP message detected", "type", msgType)

			switch msgType {
			case auth.NTLMNegotiate:
				return h.handleNTLMNegotiate(ctx)
			case auth.NTLMAuthenticate:
				// Complete the authentication
				if ctx.SessionID != 0 {
					if _, ok := h.GetPendingAuth(ctx.SessionID); ok {
						return h.completeNTLMAuth(ctx, innerToken)
					}
				}
				return h.createGuestSession(ctx)
			}
		}
	}

	// No recognized auth - create guest session
	return h.createGuestSession(ctx)
}

// handleNTLMNegotiate handles NTLM Type 1 (NEGOTIATE) message
func (h *Handler) handleNTLMNegotiate(ctx *SMBHandlerContext) (*HandlerResult, error) {
	// Generate session ID for this auth attempt
	sessionID := h.GenerateSessionID()

	// Store pending auth
	pending := &PendingAuth{
		SessionID:  sessionID,
		ClientAddr: ctx.ClientAddr,
		CreatedAt:  time.Now(),
	}
	h.StorePendingAuth(pending)

	// Update context so the response includes the session ID
	ctx.SessionID = sessionID

	// Build NTLM Type 2 (CHALLENGE) response
	challengeMsg := auth.BuildNTLMChallenge()

	logger.Debug("Sending NTLM CHALLENGE",
		"sessionID", sessionID,
		"challengeLength", len(challengeMsg))

	// Build SESSION_SETUP response with STATUS_MORE_PROCESSING_REQUIRED
	return h.buildSessionSetupResponse(types.StatusMoreProcessingRequired, 0, challengeMsg), nil
}

// completeNTLMAuth handles NTLM Type 3 (AUTHENTICATE) message
func (h *Handler) completeNTLMAuth(ctx *SMBHandlerContext, securityBuffer []byte) (*HandlerResult, error) {
	// Get pending auth
	pending, ok := h.GetPendingAuth(ctx.SessionID)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Remove pending auth
	h.DeletePendingAuth(ctx.SessionID)

	// For anonymous/guest auth, we don't validate credentials
	// Just create the session
	session := &Session{
		SessionID:  pending.SessionID,
		IsGuest:    true,
		IsNull:     false,
		CreatedAt:  time.Now(),
		ClientAddr: pending.ClientAddr,
		Username:   "guest",
		Domain:     "",
	}

	h.StoreSession(session)

	// Update context
	ctx.IsGuest = true

	logger.Debug("NTLM authentication complete",
		"sessionID", session.SessionID,
		"username", session.Username,
		"isGuest", session.IsGuest)

	// Build success response
	return h.buildSessionSetupResponse(types.StatusSuccess, types.SMB2SessionFlagIsGuest, nil), nil
}

// createGuestSession creates a guest session without NTLM handshake
func (h *Handler) createGuestSession(ctx *SMBHandlerContext) (*HandlerResult, error) {
	sessionID := h.GenerateSessionID()

	session := &Session{
		SessionID:  sessionID,
		IsGuest:    true,
		IsNull:     false,
		CreatedAt:  time.Now(),
		ClientAddr: ctx.ClientAddr,
		Username:   "guest",
		Domain:     "",
	}

	h.StoreSession(session)

	ctx.SessionID = sessionID
	ctx.IsGuest = true

	logger.Debug("Created guest session",
		"sessionID", sessionID)

	return h.buildSessionSetupResponse(types.StatusSuccess, types.SMB2SessionFlagIsGuest, nil), nil
}

// buildSessionSetupResponse builds the SESSION_SETUP response [MS-SMB2] 2.2.6
func (h *Handler) buildSessionSetupResponse(status uint32, sessionFlags uint16, securityBuffer []byte) *HandlerResult {
	// Calculate security buffer offset (header + fixed response fields)
	// SMB2 header (64) + response fixed part (8) = 72
	securityBufferOffset := uint16(0)
	if len(securityBuffer) > 0 {
		securityBufferOffset = 64 + 8 // Header + fixed response
	}

	// Build response body
	// StructureSize (2) + SessionFlags (2) + SecurityBufferOffset (2) + SecurityBufferLength (2) + Buffer
	respLen := 8 + len(securityBuffer)
	resp := make([]byte, respLen)

	binary.LittleEndian.PutUint16(resp[0:2], 9) // StructureSize (always 9 per spec)
	binary.LittleEndian.PutUint16(resp[2:4], sessionFlags)
	binary.LittleEndian.PutUint16(resp[4:6], securityBufferOffset)
	binary.LittleEndian.PutUint16(resp[6:8], uint16(len(securityBuffer)))

	if len(securityBuffer) > 0 {
		copy(resp[8:], securityBuffer)
	}

	return NewResult(status, resp)
}

// isGSSAPI checks if the buffer starts with GSSAPI/SPNEGO wrapper
func isGSSAPI(buf []byte) bool {
	// GSSAPI tokens start with 0x60 (APPLICATION 0)
	// or 0xa1 (CONTEXT SPECIFIC 1) for negTokenResp
	if len(buf) < 2 {
		return false
	}
	return buf[0] == 0x60 || buf[0] == 0xa1
}

// extractGSSAPIToken extracts the inner token from a GSSAPI/SPNEGO wrapper
// This is a simplified extraction that looks for the NTLMSSP signature
func extractGSSAPIToken(buf []byte) []byte {
	// Look for NTLMSSP signature within the buffer
	for i := 0; i < len(buf)-8; i++ {
		if buf[i] == 'N' && buf[i+1] == 'T' && buf[i+2] == 'L' && buf[i+3] == 'M' &&
			buf[i+4] == 'S' && buf[i+5] == 'S' && buf[i+6] == 'P' && buf[i+7] == 0 {
			return buf[i:]
		}
	}
	return buf
}
