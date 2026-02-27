package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// =============================================================================
// Test Helper Functions
// =============================================================================

// buildNegotiateRequest builds a NEGOTIATE request body for the given dialects.
// The body follows [MS-SMB2] 2.2.3 format:
//
//	Offset  Size  Field
//	0       2     StructureSize (always 36)
//	2       2     DialectCount
//	4       2     SecurityMode
//	6       2     Reserved
//	8       4     Capabilities
//	12      16    ClientGUID
//	28      4     NegotiateContextOffset (SMB 3.1.1 only)
//	32      2     NegotiateContextCount (SMB 3.1.1 only)
//	34      2     Reserved2
//	36+     var   Dialects (2 bytes each)
func buildNegotiateRequest(dialects []uint16) []byte {
	body := make([]byte, 36+len(dialects)*2)
	binary.LittleEndian.PutUint16(body[0:2], 36)                    // StructureSize
	binary.LittleEndian.PutUint16(body[2:4], uint16(len(dialects))) // DialectCount
	// SecurityMode, Reserved, Capabilities, ClientGUID, etc. left as zero
	for i, d := range dialects {
		binary.LittleEndian.PutUint16(body[36+i*2:], d)
	}
	return body
}

// newNegotiateTestContext creates a test context for NEGOTIATE.
func newNegotiateTestContext() *SMBHandlerContext {
	return NewSMBHandlerContext(
		context.Background(),
		"127.0.0.1:12345",
		0, // No session yet
		0, // No tree yet
		1, // MessageID
	)
}

// =============================================================================
// SMB 2.1 Dialect Tests
// =============================================================================

func TestNegotiate_SMB210(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Verify status
	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess (0x%x)",
			result.Status, types.StatusSuccess)
	}

	// Verify response length
	if len(result.Data) != 65 {
		t.Fatalf("Response should be 65 bytes, got %d", len(result.Data))
	}

	// Verify StructureSize
	structSize := binary.LittleEndian.Uint16(result.Data[0:2])
	if structSize != 65 {
		t.Errorf("StructureSize = %d, expected 65", structSize)
	}

	// Verify DialectRevision is 0x0210
	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0210 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0210", dialectRevision)
	}

	// Verify ServerGUID matches handler's ServerGUID
	var responseGUID [16]byte
	copy(responseGUID[:], result.Data[8:24])
	if responseGUID != h.ServerGUID {
		t.Errorf("ServerGUID mismatch: response = %x, handler = %x",
			responseGUID, h.ServerGUID)
	}

	// Verify Capabilities: SMB 2.1 should advertise CapLeasing | CapLargeMTU
	caps := binary.LittleEndian.Uint32(result.Data[24:28])
	expectedCaps := uint32(types.CapLeasing | types.CapLargeMTU)
	if caps != expectedCaps {
		t.Errorf("Capabilities = 0x%08x, expected 0x%08x (CapLeasing|CapLargeMTU)", caps, expectedCaps)
	}

	// Verify MaxTransactSize
	maxTransact := binary.LittleEndian.Uint32(result.Data[28:32])
	if maxTransact != h.MaxTransactSize {
		t.Errorf("MaxTransactSize = %d, expected %d", maxTransact, h.MaxTransactSize)
	}

	// Verify MaxReadSize
	maxRead := binary.LittleEndian.Uint32(result.Data[32:36])
	if maxRead != h.MaxReadSize {
		t.Errorf("MaxReadSize = %d, expected %d", maxRead, h.MaxReadSize)
	}

	// Verify MaxWriteSize
	maxWrite := binary.LittleEndian.Uint32(result.Data[36:40])
	if maxWrite != h.MaxWriteSize {
		t.Errorf("MaxWriteSize = %d, expected %d", maxWrite, h.MaxWriteSize)
	}
}

// =============================================================================
// SMB 2.0.2 Dialect Tests
// =============================================================================

func TestNegotiate_SMB202(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0202)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess (0x%x)",
			result.Status, types.StatusSuccess)
	}

	if len(result.Data) < 6 {
		t.Fatalf("Response too short: %d bytes", len(result.Data))
	}

	// Verify DialectRevision is 0x0202
	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0202 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0202", dialectRevision)
	}

	// Verify Capabilities: SMB 2.0.2 should have no capabilities (reserved field)
	caps := binary.LittleEndian.Uint32(result.Data[24:28])
	if caps != 0 {
		t.Errorf("Capabilities = 0x%08x, expected 0x00000000 for SMB 2.0.2", caps)
	}
}

// =============================================================================
// Multiple Dialect Tests
// =============================================================================

func TestNegotiate_MultipleDialects(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Send both 0x0202 and 0x0210 - should select highest supported (0x0210)
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0202),
		uint16(types.SMB2Dialect0210),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0210 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0210 (highest supported)",
			dialectRevision)
	}
}

// =============================================================================
// Unsupported Dialect Tests
// =============================================================================

func TestNegotiate_UnsupportedDialect(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Send only SMB 3.1.1 which is not supported
	body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0311)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusNotSupported {
		t.Errorf("Status = 0x%x, expected StatusNotSupported (0x%x)",
			result.Status, types.StatusNotSupported)
	}
}

// =============================================================================
// Empty Dialect List Tests
// =============================================================================

func TestNegotiate_EmptyDialectList(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// dialectCount = 0, no dialects in the body
	body := buildNegotiateRequest([]uint16{})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusNotSupported {
		t.Errorf("Status = 0x%x, expected StatusNotSupported (0x%x)",
			result.Status, types.StatusNotSupported)
	}
}

// =============================================================================
// Short Body Tests
// =============================================================================

func TestNegotiate_ShortBody(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Body less than 36 bytes should be rejected
	shortBody := make([]byte, 20)

	result, err := h.Negotiate(ctx, shortBody)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusInvalidParameter {
		t.Errorf("Status = 0x%x, expected StatusInvalidParameter (0x%x)",
			result.Status, types.StatusInvalidParameter)
	}
}

// =============================================================================
// Wildcard Dialect Tests
// =============================================================================

func TestNegotiate_WildcardDialect(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Per MS-SMB2 §3.3.5.3.2: wildcard dialect (0x02FF) alone should
	// respond with 0x02FF to signal multi-protocol negotiate to the client.
	body := buildNegotiateRequest([]uint16{uint16(types.SMB2DialectWild)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess (0x%x)",
			result.Status, types.StatusSuccess)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != uint16(types.SMB2DialectWild) {
		t.Errorf("DialectRevision = 0x%04x, expected 0x%04x (wildcard echoed back per MS-SMB2)",
			dialectRevision, types.SMB2DialectWild)
	}
}

// =============================================================================
// Signing Configuration Tests
// =============================================================================

func TestNegotiate_SigningEnabled(t *testing.T) {
	h := NewHandler()
	h.SigningConfig.Enabled = true
	h.SigningConfig.Required = false
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	// SecurityMode is at offset 2 in response body
	securityMode := result.Data[2]

	// Bit 0 (0x01) should be set: SMB2_NEGOTIATE_SIGNING_ENABLED
	if securityMode&0x01 == 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 0 (SIGNING_ENABLED) to be set",
			securityMode)
	}

	// Bit 1 (0x02) should NOT be set: SMB2_NEGOTIATE_SIGNING_REQUIRED
	if securityMode&0x02 != 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 1 (SIGNING_REQUIRED) to be clear",
			securityMode)
	}
}

func TestNegotiate_SigningRequired(t *testing.T) {
	h := NewHandler()
	h.SigningConfig.Enabled = true
	h.SigningConfig.Required = true
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	// SecurityMode is at offset 2 in response body
	securityMode := result.Data[2]

	// Bit 0 (0x01) should be set: SMB2_NEGOTIATE_SIGNING_ENABLED
	if securityMode&0x01 == 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 0 (SIGNING_ENABLED) to be set",
			securityMode)
	}

	// Bit 1 (0x02) should be set: SMB2_NEGOTIATE_SIGNING_REQUIRED
	if securityMode&0x02 == 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 1 (SIGNING_REQUIRED) to be set",
			securityMode)
	}
}

// =============================================================================
// Response Format Validation Tests
// =============================================================================

func TestNegotiate_ResponseFormat(t *testing.T) {
	t.Run("ResponseHasCorrectLength", func(t *testing.T) {
		h := NewHandler()
		ctx := newNegotiateTestContext()

		body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

		result, err := h.Negotiate(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Response should be exactly 65 bytes per MS-SMB2 2.2.4
		if len(result.Data) != 65 {
			t.Errorf("Response length = %d, expected 65", len(result.Data))
		}
	})

	t.Run("SecurityBufferOffsetIsCorrect", func(t *testing.T) {
		h := NewHandler()
		ctx := newNegotiateTestContext()

		body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

		result, err := h.Negotiate(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// SecurityBufferOffset should be 128 (64 header + 64 fixed body)
		secBufferOffset := binary.LittleEndian.Uint16(result.Data[56:58])
		if secBufferOffset != 128 {
			t.Errorf("SecurityBufferOffset = %d, expected 128", secBufferOffset)
		}

		// SecurityBufferLength should be 0 (no security blob)
		secBufferLen := binary.LittleEndian.Uint16(result.Data[58:60])
		if secBufferLen != 0 {
			t.Errorf("SecurityBufferLength = %d, expected 0", secBufferLen)
		}
	})

	t.Run("SystemTimeIsNonZero", func(t *testing.T) {
		h := NewHandler()
		ctx := newNegotiateTestContext()

		body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

		result, err := h.Negotiate(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// SystemTime at offset 40 should be non-zero
		systemTime := binary.LittleEndian.Uint64(result.Data[40:48])
		if systemTime == 0 {
			t.Error("SystemTime should be non-zero")
		}

		// ServerStartTime at offset 48 should be non-zero
		startTime := binary.LittleEndian.Uint64(result.Data[48:56])
		if startTime == 0 {
			t.Error("ServerStartTime should be non-zero")
		}
	})

	t.Run("NegotiateContextFieldsAreZero", func(t *testing.T) {
		h := NewHandler()
		ctx := newNegotiateTestContext()

		body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

		result, err := h.Negotiate(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// NegotiateContextCount at offset 6 should be 0 (not SMB 3.1.1)
		ctxCount := binary.LittleEndian.Uint16(result.Data[6:8])
		if ctxCount != 0 {
			t.Errorf("NegotiateContextCount = %d, expected 0", ctxCount)
		}

		// NegotiateContextOffset at offset 60 should be 0 (not SMB 3.1.1)
		ctxOffset := binary.LittleEndian.Uint32(result.Data[60:64])
		if ctxOffset != 0 {
			t.Errorf("NegotiateContextOffset = %d, expected 0", ctxOffset)
		}
	})
}

// =============================================================================
// Edge Case Tests
// =============================================================================

func TestNegotiate_OnlyUnsupportedDialects(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Send multiple unsupported dialects (SMB 3.x only)
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0300),
		uint16(types.SMB2Dialect0302),
		uint16(types.SMB2Dialect0311),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusNotSupported {
		t.Errorf("Status = 0x%x, expected StatusNotSupported (0x%x)",
			result.Status, types.StatusNotSupported)
	}
}

func TestNegotiate_MixedSupportedAndUnsupported(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Send a mix of supported and unsupported dialects
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0300), // Unsupported
		uint16(types.SMB2Dialect0202), // Supported
		uint16(types.SMB2Dialect0311), // Unsupported
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0202 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0202", dialectRevision)
	}
}

func TestNegotiate_ExactMinimumBody(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Exactly 36 bytes (minimum valid body with 0 dialects)
	body := make([]byte, 36)
	binary.LittleEndian.PutUint16(body[0:2], 36) // StructureSize
	binary.LittleEndian.PutUint16(body[2:4], 0)  // DialectCount = 0

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// No dialects means no supported dialect found
	if result.Status != types.StatusNotSupported {
		t.Errorf("Status = 0x%x, expected StatusNotSupported", result.Status)
	}
}

func TestNegotiate_SigningDisabled(t *testing.T) {
	h := NewHandler()
	h.SigningConfig.Enabled = false
	h.SigningConfig.Required = false
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{uint16(types.SMB2Dialect0210)})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	// SecurityMode should be 0 when signing is disabled
	securityMode := result.Data[2]
	if securityMode != 0 {
		t.Errorf("SecurityMode = 0x%02x, expected 0x00 (signing disabled)", securityMode)
	}
}

func TestNegotiate_WildcardWithSMB202(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Per MS-SMB2 §3.3.5.3.2: wildcard + SMB 2.0.2 should still respond
	// with 0x02FF. The wildcard signals multi-protocol negotiate; 2.0.2 is
	// the baseline that wildcard implies. Only SMB 3.x alongside wildcard
	// would suppress the wildcard echo.
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2DialectWild),
		uint16(types.SMB2Dialect0202),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != uint16(types.SMB2DialectWild) {
		t.Errorf("DialectRevision = 0x%04x, expected 0x%04x (wildcard echoed back)",
			dialectRevision, types.SMB2DialectWild)
	}
}

func TestNegotiate_WildcardWithHigherDialect(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Wildcard + SMB 2.1 - should select 0x0210 (higher than wildcard's 0x0202)
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2DialectWild),
		uint16(types.SMB2Dialect0210),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0210 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0210", dialectRevision)
	}
}
