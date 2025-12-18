package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/jcmturner/gofork/encoding/asn1"
	gokrbspnego "github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/marmos91/dittofs/internal/auth/ntlm"
	authspnego "github.com/marmos91/dittofs/internal/auth/spnego"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// =============================================================================
// Test Helper Functions
// =============================================================================

// buildSessionSetupRequestBody builds a SESSION_SETUP request body.
// securityBuffer is placed at offset 24 (after the fixed 24-byte fields).
func buildSessionSetupRequestBody(securityBuffer []byte) []byte {
	// Fixed size: 24 bytes + security buffer
	body := make([]byte, 24+len(securityBuffer)+1) // +1 for minimum size requirement

	// StructureSize at offset 0 (always 25)
	binary.LittleEndian.PutUint16(body[0:2], 25)

	// Flags at offset 2
	body[2] = 0

	// SecurityMode at offset 3
	body[3] = 0

	// Capabilities at offset 4
	binary.LittleEndian.PutUint32(body[4:8], 0)

	// Channel at offset 8
	binary.LittleEndian.PutUint32(body[8:12], 0)

	// SecurityBufferOffset at offset 12 (64 header + 24 fixed = 88)
	binary.LittleEndian.PutUint16(body[12:14], 88)

	// SecurityBufferLength at offset 14
	binary.LittleEndian.PutUint16(body[14:16], uint16(len(securityBuffer)))

	// PreviousSessionID at offset 16
	binary.LittleEndian.PutUint64(body[16:24], 0)

	// Security buffer starts at offset 24 in body
	if len(securityBuffer) > 0 {
		copy(body[24:], securityBuffer)
	}

	return body
}

// validNTLMNegotiateMessage creates a valid NTLM Type 1 message.
func validNTLMNegotiateMessage() []byte {
	msg := make([]byte, 32)
	copy(msg[0:8], ntlm.Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(ntlm.Negotiate))
	return msg
}

// validNTLMAuthenticateMessage creates a valid NTLM Type 3 message.
func validNTLMAuthenticateMessage() []byte {
	msg := make([]byte, 88)
	copy(msg[0:8], ntlm.Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(ntlm.Authenticate))
	return msg
}

// wrapInSPNEGO wraps an NTLM token in a proper SPNEGO NegTokenInit structure.
func wrapInSPNEGO(ntlmToken []byte) []byte {
	initToken := gokrbspnego.NegTokenInit{
		MechTypes:      []asn1.ObjectIdentifier{authspnego.OIDNTLMSSP},
		MechTokenBytes: ntlmToken,
	}

	data, err := initToken.Marshal()
	if err != nil {
		// Fall back to returning raw token on error (shouldn't happen in tests)
		return ntlmToken
	}
	return data
}

// newTestContext creates a test context with the given session ID.
func newTestContext(sessionID uint64) *SMBHandlerContext {
	return NewSMBHandlerContext(
		context.Background(),
		"127.0.0.1:12345",
		sessionID,
		0,
		1,
	)
}

// =============================================================================
// parseSessionSetupRequest Tests
// =============================================================================

func TestParseSessionSetupRequest(t *testing.T) {
	t.Run("ValidRequest", func(t *testing.T) {
		secBuffer := []byte("test security buffer")
		body := buildSessionSetupRequestBody(secBuffer)

		req, err := parseSessionSetupRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.StructureSize != 25 {
			t.Errorf("StructureSize = %d, expected 25", req.StructureSize)
		}
	})

	t.Run("TooShortBody", func(t *testing.T) {
		body := make([]byte, 20) // Less than 25 bytes

		_, err := parseSessionSetupRequest(body)
		if err == nil {
			t.Error("Expected error for short body")
		}
	})

	t.Run("MinimumValidBody", func(t *testing.T) {
		body := make([]byte, 25)
		binary.LittleEndian.PutUint16(body[0:2], 25)

		req, err := parseSessionSetupRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if len(req.SecurityBuffer) != 0 {
			t.Errorf("Expected empty security buffer, got %d bytes", len(req.SecurityBuffer))
		}
	})

	t.Run("WithSecurityBuffer", func(t *testing.T) {
		ntlm := validNTLMNegotiateMessage()
		body := buildSessionSetupRequestBody(ntlm)

		req, err := parseSessionSetupRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if len(req.SecurityBuffer) == 0 {
			t.Error("Expected non-empty security buffer")
		}
	})

	t.Run("ParsesPreviousSessionID", func(t *testing.T) {
		body := make([]byte, 30)
		binary.LittleEndian.PutUint16(body[0:2], 25)
		binary.LittleEndian.PutUint64(body[16:24], 0x123456789ABCDEF0)

		req, err := parseSessionSetupRequest(body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if req.PreviousSessionID != 0x123456789ABCDEF0 {
			t.Errorf("PreviousSessionID = 0x%x, expected 0x123456789ABCDEF0", req.PreviousSessionID)
		}
	})
}

// =============================================================================
// SessionSetup Handler Tests
// =============================================================================

func TestSessionSetup(t *testing.T) {
	t.Run("ReturnsErrorForTooShortBody", func(t *testing.T) {
		h := NewHandler()
		ctx := newTestContext(0)

		result, err := h.SessionSetup(ctx, []byte{0x00, 0x01, 0x02})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Status = 0x%x, expected StatusInvalidParameter (0x%x)",
				result.Status, types.StatusInvalidParameter)
		}
	})

	t.Run("HandlesNTLMNegotiate", func(t *testing.T) {
		h := NewHandler()
		ctx := newTestContext(0)

		ntlm := validNTLMNegotiateMessage()
		body := buildSessionSetupRequestBody(ntlm)

		result, err := h.SessionSetup(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusMoreProcessingRequired {
			t.Errorf("Status = 0x%x, expected StatusMoreProcessingRequired",
				result.Status)
		}

		// Session ID should be set
		if ctx.SessionID == 0 {
			t.Error("SessionID should be set after NEGOTIATE")
		}

		// Pending auth should be stored
		_, ok := h.GetPendingAuth(ctx.SessionID)
		if !ok {
			t.Error("PendingAuth should be stored")
		}
	})

	t.Run("HandlesSPNEGOWrappedNTLM", func(t *testing.T) {
		h := NewHandler()
		ctx := newTestContext(0)

		ntlm := validNTLMNegotiateMessage()
		spnego := wrapInSPNEGO(ntlm)
		body := buildSessionSetupRequestBody(spnego)

		result, err := h.SessionSetup(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusMoreProcessingRequired {
			t.Errorf("Status = 0x%x, expected StatusMoreProcessingRequired",
				result.Status)
		}
	})

	t.Run("CreatesGuestSessionWithoutAuth", func(t *testing.T) {
		h := NewHandler()
		ctx := newTestContext(0)

		body := buildSessionSetupRequestBody(nil) // No security buffer

		result, err := h.SessionSetup(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
		}

		if !ctx.IsGuest {
			t.Error("Should be guest session")
		}

		// Session should be stored
		session, ok := h.GetSession(ctx.SessionID)
		if !ok {
			t.Error("Session should be stored")
		}
		if !session.IsGuest {
			t.Error("Session should be marked as guest")
		}
	})

	t.Run("CreatesGuestSessionWithUnknownToken", func(t *testing.T) {
		h := NewHandler()
		ctx := newTestContext(0)

		unknownToken := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
		body := buildSessionSetupRequestBody(unknownToken)

		result, err := h.SessionSetup(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
		}

		if !ctx.IsGuest {
			t.Error("Should be guest session for unknown token")
		}
	})
}

// =============================================================================
// Full NTLM Handshake Tests
// =============================================================================

func TestSessionSetup_FullHandshake(t *testing.T) {
	t.Run("CompletesNTLMHandshake", func(t *testing.T) {
		h := NewHandler()

		// Step 1: NEGOTIATE
		ctx1 := newTestContext(0)
		negotiate := validNTLMNegotiateMessage()
		body1 := buildSessionSetupRequestBody(negotiate)

		result1, err := h.SessionSetup(ctx1, body1)
		if err != nil {
			t.Fatalf("NEGOTIATE error: %v", err)
		}

		if result1.Status != types.StatusMoreProcessingRequired {
			t.Fatalf("NEGOTIATE should return STATUS_MORE_PROCESSING_REQUIRED")
		}

		sessionID := ctx1.SessionID
		if sessionID == 0 {
			t.Fatal("SessionID should be set after NEGOTIATE")
		}

		// Step 2: AUTHENTICATE
		ctx2 := newTestContext(sessionID)
		authenticate := validNTLMAuthenticateMessage()
		body2 := buildSessionSetupRequestBody(authenticate)

		result2, err := h.SessionSetup(ctx2, body2)
		if err != nil {
			t.Fatalf("AUTHENTICATE error: %v", err)
		}

		if result2.Status != types.StatusSuccess {
			t.Errorf("AUTHENTICATE should return STATUS_SUCCESS, got 0x%x", result2.Status)
		}

		// Verify session was created
		session, ok := h.GetSession(sessionID)
		if !ok {
			t.Error("Session should be created after AUTHENTICATE")
		}
		if !session.IsGuest {
			t.Error("Session should be marked as guest")
		}

		// Verify pending auth was removed
		_, ok = h.GetPendingAuth(sessionID)
		if ok {
			t.Error("PendingAuth should be removed after AUTHENTICATE")
		}
	})

	t.Run("RejectsAuthenticateWithoutPendingAuth", func(t *testing.T) {
		h := NewHandler()

		// Skip NEGOTIATE, go straight to AUTHENTICATE with a non-zero session ID
		ctx := newTestContext(12345) // Random session ID with no pending auth

		authenticate := validNTLMAuthenticateMessage()
		body := buildSessionSetupRequestBody(authenticate)

		result, err := h.SessionSetup(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Should create guest session as fallback
		if result.Status != types.StatusSuccess {
			t.Errorf("Should fallback to guest session, got 0x%x", result.Status)
		}
	})
}

// =============================================================================
// buildSessionSetupResponse Tests
// =============================================================================

func TestBuildSessionSetupResponse(t *testing.T) {
	h := NewHandler()

	t.Run("SuccessResponse", func(t *testing.T) {
		result := h.buildSessionSetupResponse(
			types.StatusSuccess,
			types.SMB2SessionFlagIsGuest,
			nil,
		)

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
		}

		if len(result.Data) < 8 {
			t.Fatalf("Response body too short: %d bytes", len(result.Data))
		}

		// StructureSize should be 9
		structSize := binary.LittleEndian.Uint16(result.Data[0:2])
		if structSize != 9 {
			t.Errorf("StructureSize = %d, expected 9", structSize)
		}

		// SessionFlags
		flags := binary.LittleEndian.Uint16(result.Data[2:4])
		if flags != types.SMB2SessionFlagIsGuest {
			t.Errorf("SessionFlags = 0x%x, expected 0x%x",
				flags, types.SMB2SessionFlagIsGuest)
		}
	})

	t.Run("ResponseWithSecurityBuffer", func(t *testing.T) {
		secBuffer := []byte("test security data")
		result := h.buildSessionSetupResponse(
			types.StatusMoreProcessingRequired,
			0,
			secBuffer,
		)

		if result.Status != types.StatusMoreProcessingRequired {
			t.Errorf("Status = 0x%x, expected StatusMoreProcessingRequired",
				result.Status)
		}

		// Response should include security buffer
		expectedLen := 8 + len(secBuffer)
		if len(result.Data) != expectedLen {
			t.Errorf("Response length = %d, expected %d", len(result.Data), expectedLen)
		}

		// SecurityBufferOffset should be set
		secOffset := binary.LittleEndian.Uint16(result.Data[4:6])
		if secOffset == 0 {
			t.Error("SecurityBufferOffset should be non-zero")
		}

		// SecurityBufferLength
		secLen := binary.LittleEndian.Uint16(result.Data[6:8])
		if secLen != uint16(len(secBuffer)) {
			t.Errorf("SecurityBufferLength = %d, expected %d", secLen, len(secBuffer))
		}
	})

	t.Run("ResponseWithoutSecurityBuffer", func(t *testing.T) {
		result := h.buildSessionSetupResponse(types.StatusSuccess, 0, nil)

		// SecurityBufferOffset should be 0 when no buffer
		secOffset := binary.LittleEndian.Uint16(result.Data[4:6])
		if secOffset != 0 {
			t.Errorf("SecurityBufferOffset = %d, expected 0", secOffset)
		}

		// SecurityBufferLength should be 0
		secLen := binary.LittleEndian.Uint16(result.Data[6:8])
		if secLen != 0 {
			t.Errorf("SecurityBufferLength = %d, expected 0", secLen)
		}
	})
}

// =============================================================================
// extractNTLMToken Tests
// =============================================================================

func TestExtractNTLMToken(t *testing.T) {
	t.Run("PassesThroughRawNTLM", func(t *testing.T) {
		ntlmMsg := validNTLMNegotiateMessage()
		result, isWrapped := extractNTLMToken(ntlmMsg)

		if !ntlm.IsValid(result) {
			t.Error("Should pass through raw NTLM unchanged")
		}
		if isWrapped {
			t.Error("Raw NTLM should not be marked as wrapped")
		}
	})

	t.Run("ExtractsFromSPNEGO", func(t *testing.T) {
		ntlmMsg := validNTLMNegotiateMessage()
		spnegoMsg := wrapInSPNEGO(ntlmMsg)
		result, _ := extractNTLMToken(spnegoMsg)

		if !ntlm.IsValid(result) {
			t.Error("Should extract NTLM from SPNEGO")
		}
	})

	t.Run("ReturnsEmptyForEmpty", func(t *testing.T) {
		result, _ := extractNTLMToken(nil)
		if len(result) != 0 {
			t.Error("Should return empty for nil input")
		}

		result, _ = extractNTLMToken([]byte{})
		if len(result) != 0 {
			t.Error("Should return empty for empty input")
		}
	})
}

// =============================================================================
// Handler State Tests
// =============================================================================

func TestSessionSetup_HandlerState(t *testing.T) {
	t.Run("MultipleConcurrentHandshakes", func(t *testing.T) {
		h := NewHandler()

		// Start multiple handshakes
		var sessionIDs []uint64
		for i := 0; i < 10; i++ {
			ctx := newTestContext(0)
			ntlm := validNTLMNegotiateMessage()
			body := buildSessionSetupRequestBody(ntlm)

			result, _ := h.SessionSetup(ctx, body)
			if result.Status != types.StatusMoreProcessingRequired {
				t.Errorf("Handshake %d: unexpected status", i)
			}

			sessionIDs = append(sessionIDs, ctx.SessionID)
		}

		// Complete all handshakes
		for _, sessionID := range sessionIDs {
			ctx := newTestContext(sessionID)
			auth := validNTLMAuthenticateMessage()
			body := buildSessionSetupRequestBody(auth)

			result, _ := h.SessionSetup(ctx, body)
			if result.Status != types.StatusSuccess {
				t.Errorf("Complete handshake for session %d: unexpected status", sessionID)
			}
		}

		// Verify all sessions created
		for _, sessionID := range sessionIDs {
			_, ok := h.GetSession(sessionID)
			if !ok {
				t.Errorf("Session %d should exist", sessionID)
			}
		}
	})
}

// =============================================================================
// Constants Tests
// =============================================================================

func TestSessionSetupConstants(t *testing.T) {
	t.Run("RequestOffsets", func(t *testing.T) {
		// Verify offset constants are correct per MS-SMB2 spec
		tests := []struct {
			name     string
			offset   int
			expected int
		}{
			{"StructureSize", sessionSetupStructureSizeOffset, 0},
			{"Flags", sessionSetupFlagsOffset, 2},
			{"SecurityMode", sessionSetupSecurityModeOffset, 3},
			{"Capabilities", sessionSetupCapabilitiesOffset, 4},
			{"Channel", sessionSetupChannelOffset, 8},
			{"SecurityBufferOffset", sessionSetupSecBufferOffsetOffset, 12},
			{"SecurityBufferLength", sessionSetupSecBufferLengthOffset, 14},
			{"PreviousSessionID", sessionSetupPreviousSessionIDOffset, 16},
		}

		for _, tt := range tests {
			if tt.offset != tt.expected {
				t.Errorf("%s: offset = %d, expected %d", tt.name, tt.offset, tt.expected)
			}
		}
	})

	t.Run("ResponseConstants", func(t *testing.T) {
		if sessionSetupRespStructureSize != 9 {
			t.Errorf("StructureSize = %d, expected 9", sessionSetupRespStructureSize)
		}
		if sessionSetupRespFixedSize != 8 {
			t.Errorf("FixedSize = %d, expected 8", sessionSetupRespFixedSize)
		}
	})

	t.Run("SMB2HeaderSize", func(t *testing.T) {
		if smb2HeaderSize != 64 {
			t.Errorf("SMB2 header size = %d, expected 64", smb2HeaderSize)
		}
	})
}
