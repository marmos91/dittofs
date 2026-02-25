package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// =============================================================================
// FSCTL_VALIDATE_NEGOTIATE_INFO Tests [MS-SMB2] 2.2.31.4
// =============================================================================

// buildValidateNegotiateInfoRequest builds a complete IOCTL request with
// VALIDATE_NEGOTIATE_INFO input data.
func buildValidateNegotiateInfoRequest(capabilities uint32, clientGUID [16]byte, securityMode uint16, dialects []types.Dialect) []byte {
	// IOCTL request structure:
	// - StructureSize (2 bytes) - offset 0
	// - Reserved (2 bytes) - offset 2
	// - CtlCode (4 bytes) - offset 4
	// - FileId (16 bytes) - offset 8
	// - InputOffset (4 bytes) - offset 24
	// - InputCount (4 bytes) - offset 28
	// - MaxInputResponse (4 bytes) - offset 32
	// - OutputOffset (4 bytes) - offset 36
	// - OutputCount (4 bytes) - offset 40
	// - MaxOutputResponse (4 bytes) - offset 44
	// - Flags (4 bytes) - offset 48
	// - Reserved2 (4 bytes) - offset 52
	// - Buffer (variable) - offset 56

	// Calculate input size: 24 bytes fixed + 2 bytes per dialect
	inputSize := 24 + (len(dialects) * 2)

	// Total body size: 56 bytes header + input buffer
	bodySize := 56 + inputSize
	body := make([]byte, bodySize)

	// IOCTL header
	binary.LittleEndian.PutUint16(body[0:2], 57)                         // StructureSize
	binary.LittleEndian.PutUint32(body[4:8], FsctlValidateNegotiateInfo) // CtlCode
	// FileId at 8:24 must be all 0xFF per MS-SMB2 2.2.31.4
	for i := 8; i < 24; i++ {
		body[i] = 0xFF
	}
	binary.LittleEndian.PutUint32(body[24:28], 56+64)             // InputOffset (after SMB header)
	binary.LittleEndian.PutUint32(body[28:32], uint32(inputSize)) // InputCount
	binary.LittleEndian.PutUint32(body[44:48], 24)                // MaxOutputResponse

	// Build input buffer (VALIDATE_NEGOTIATE_INFO request)
	input := body[56:]
	binary.LittleEndian.PutUint32(input[0:4], capabilities)
	copy(input[4:20], clientGUID[:])
	binary.LittleEndian.PutUint16(input[20:22], securityMode)
	binary.LittleEndian.PutUint16(input[22:24], uint16(len(dialects)))
	for i, dialect := range dialects {
		binary.LittleEndian.PutUint16(input[24+(i*2):26+(i*2)], uint16(dialect))
	}

	return body
}

func TestHandleValidateNegotiateInfo(t *testing.T) {
	t.Run("ValidRequest_SMB21Dialect", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		// Build request with SMB 2.1 as highest dialect
		var clientGUID [16]byte
		for i := range clientGUID {
			clientGUID[i] = byte(i)
		}
		dialects := []types.Dialect{types.SMB2Dialect0202, types.SMB2Dialect0210}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Expected StatusSuccess, got %v", result.Status)
		}

		// Verify response contains valid data
		// IOCTL response: 48 bytes header + 24 bytes VALIDATE_NEGOTIATE_INFO output
		if len(result.Data) < 48+24 {
			t.Fatalf("Response too short: %d bytes", len(result.Data))
		}

		// Parse response to verify selected dialect
		// Output buffer starts at offset 48 in IOCTL response body
		respOutput := result.Data[48:]
		selectedDialect := types.Dialect(binary.LittleEndian.Uint16(respOutput[22:24]))
		if selectedDialect != types.SMB2Dialect0210 {
			t.Errorf("Expected dialect SMB 2.1, got %v", selectedDialect)
		}
	})

	t.Run("ValidRequest_SMB202OnlyDialect", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		dialects := []types.Dialect{types.SMB2Dialect0202}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Expected StatusSuccess, got %v", result.Status)
		}

		// Verify SMB 2.0.2 was selected
		respOutput := result.Data[48:]
		selectedDialect := types.Dialect(binary.LittleEndian.Uint16(respOutput[22:24]))
		if selectedDialect != types.SMB2Dialect0202 {
			t.Errorf("Expected dialect SMB 2.0.2, got %v", selectedDialect)
		}
	})

	t.Run("RequestTooSmall", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		// Body smaller than 56 bytes
		body := make([]byte, 50)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Expected StatusInvalidParameter, got %v", result.Status)
		}
	})

	t.Run("NonNullFileID", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		dialects := []types.Dialect{types.SMB2Dialect0210}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		// Set non-NULL FileID
		body[8] = 0x01 // First byte of FileID is non-zero

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Expected StatusInvalidParameter for non-NULL FileId, got %v", result.Status)
		}
	})

	t.Run("InputCountTooSmall", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		// Build a minimal body with InputCount < 24
		body := make([]byte, 80)
		binary.LittleEndian.PutUint32(body[28:32], 20) // InputCount too small

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Expected StatusInvalidParameter for small InputCount, got %v", result.Status)
		}
	})

	t.Run("DialectCountMismatch", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		// Claim 5 dialects but only provide 1
		dialects := []types.Dialect{types.SMB2Dialect0202}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		// Override dialect count to claim more dialects than provided
		binary.LittleEndian.PutUint16(body[56+22:56+24], 5) // DialectCount in input buffer

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Expected StatusInvalidParameter for dialect count mismatch, got %v", result.Status)
		}
	})

	t.Run("NoCommonDialect", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		// Use only unsupported dialects (SMB 3.x)
		dialects := []types.Dialect{types.SMB2Dialect0300, types.SMB2Dialect0311}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Expected StatusInvalidParameter for no common dialect, got %v", result.Status)
		}
	})

	t.Run("SecurityModeReflectsSigningConfig", func(t *testing.T) {
		h := NewHandler()
		h.SigningConfig.Enabled = true
		h.SigningConfig.Required = true

		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		dialects := []types.Dialect{types.SMB2Dialect0210}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x03, dialects)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Expected StatusSuccess, got %v", result.Status)
		}

		// Verify security mode in response
		respOutput := result.Data[48:]
		securityMode := binary.LittleEndian.Uint16(respOutput[20:22])
		expectedMode := uint16(0x03) // Both SIGNING_ENABLED and SIGNING_REQUIRED

		if securityMode != expectedMode {
			t.Errorf("Expected security mode 0x%02X, got 0x%02X", expectedMode, securityMode)
		}
	})

	t.Run("ServerGUIDInResponse", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		dialects := []types.Dialect{types.SMB2Dialect0210}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Expected StatusSuccess, got %v", result.Status)
		}

		// Verify server GUID matches handler's GUID
		respOutput := result.Data[48:]
		var respGUID [16]byte
		copy(respGUID[:], respOutput[4:20])

		if respGUID != h.ServerGUID {
			t.Errorf("Response GUID doesn't match handler's ServerGUID")
		}
	})

	t.Run("WildcardDialectSelectsSMB202", func(t *testing.T) {
		h := NewHandler()
		ctx := &SMBHandlerContext{
			Context: context.Background(),
		}

		var clientGUID [16]byte
		// SMB 2.x wildcard
		dialects := []types.Dialect{types.SMB2DialectWild}
		body := buildValidateNegotiateInfoRequest(0, clientGUID, 0x01, dialects)

		result, err := h.handleValidateNegotiateInfo(ctx, body)
		if err != nil {
			t.Fatalf("handleValidateNegotiateInfo() error = %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Expected StatusSuccess, got %v", result.Status)
		}

		// Wildcard should select SMB 2.0.2 as baseline
		respOutput := result.Data[48:]
		selectedDialect := types.Dialect(binary.LittleEndian.Uint16(respOutput[22:24]))
		if selectedDialect != types.SMB2Dialect0202 {
			t.Errorf("Expected dialect SMB 2.0.2 for wildcard, got %v", selectedDialect)
		}
	})
}
