package handlers

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// =============================================================================
// Test Helper Functions
// =============================================================================

// buildNegotiateRequest builds a NEGOTIATE request body for the given dialects.
// Uses smbenc for encoding per CONTEXT.md guidelines.
func buildNegotiateRequest(dialects []uint16) []byte {
	return buildNegotiateRequestFull(dialects, 0, 0, [16]byte{}, nil)
}

// buildNegotiateRequestFull builds a NEGOTIATE request with all fields controllable.
func buildNegotiateRequestFull(dialects []uint16, securityMode, capabilities uint16, clientGUID [16]byte, contexts []types.NegotiateContext) []byte {
	dialectsSize := len(dialects) * 2
	w := smbenc.NewWriter(36 + dialectsSize + 256) // extra room for contexts

	w.WriteUint16(36)                    // StructureSize
	w.WriteUint16(uint16(len(dialects))) // DialectCount
	w.WriteUint16(securityMode)          // SecurityMode
	w.WriteUint16(0)                     // Reserved
	w.WriteUint32(uint32(capabilities))  // Capabilities (only lower 16 used here)
	w.WriteBytes(clientGUID[:])          // ClientGUID (16 bytes)

	if len(contexts) > 0 {
		// NegotiateContextOffset is relative to start of SMB2 header (64 bytes before body).
		// Offset = 64 (header) + 36 (fixed body) + dialectsSize + padding to 8-byte alignment
		dialectEnd := 36 + dialectsSize
		// Pad dialect end to 8-byte alignment (relative to SMB2 header start = offset 64)
		absDialectEnd := 64 + dialectEnd
		padding := 0
		if absDialectEnd%8 != 0 {
			padding = 8 - (absDialectEnd % 8)
		}
		contextOffset := uint32(absDialectEnd + padding)
		w.WriteUint32(contextOffset)         // NegotiateContextOffset
		w.WriteUint16(uint16(len(contexts))) // NegotiateContextCount
		w.WriteUint16(0)                     // Reserved2
	} else {
		w.WriteUint32(0) // NegotiateContextOffset
		w.WriteUint16(0) // NegotiateContextCount
		w.WriteUint16(0) // Reserved2
	}

	// Dialects
	for _, d := range dialects {
		w.WriteUint16(d)
	}

	if len(contexts) > 0 {
		// Pad to 8-byte alignment for negotiate contexts
		w.Pad(8)
		ctxBytes := types.EncodeNegotiateContextList(contexts)
		w.WriteBytes(ctxBytes)
	}

	return w.Bytes()
}

// buildPreauthContext creates a PREAUTH_INTEGRITY_CAPABILITIES negotiate context.
func buildPreauthContext(hashAlgorithms []uint16, salt []byte) types.NegotiateContext {
	caps := types.PreauthIntegrityCaps{
		HashAlgorithms: hashAlgorithms,
		Salt:           salt,
	}
	return types.NegotiateContext{
		ContextType: types.NegCtxPreauthIntegrity,
		Data:        caps.Encode(),
	}
}

// buildEncryptionContext creates an ENCRYPTION_CAPABILITIES negotiate context.
func buildEncryptionContext(ciphers []uint16) types.NegotiateContext {
	caps := types.EncryptionCaps{
		Ciphers: ciphers,
	}
	return types.NegotiateContext{
		ContextType: types.NegCtxEncryptionCaps,
		Data:        caps.Encode(),
	}
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

// mockCryptoState implements the CryptoState interface for testing.
type mockCryptoState struct {
	dialect            types.Dialect
	cipherId           uint16
	preauthHashId      uint16
	serverGUID         [16]byte
	serverCapabilities types.Capabilities
	serverSecurityMode types.SecurityMode
	clientGUID         [16]byte
	clientCapabilities types.Capabilities
	clientSecurityMode types.SecurityMode
	clientDialects     []types.Dialect
}

func (m *mockCryptoState) SetDialect(d types.Dialect)                    { m.dialect = d }
func (m *mockCryptoState) GetDialect() types.Dialect                     { return m.dialect }
func (m *mockCryptoState) SetCipherId(id uint16)                         { m.cipherId = id }
func (m *mockCryptoState) SetPreauthIntegrityHashId(id uint16)           { m.preauthHashId = id }
func (m *mockCryptoState) SetServerGUID(guid [16]byte)                   { m.serverGUID = guid }
func (m *mockCryptoState) SetServerCapabilities(caps types.Capabilities) { m.serverCapabilities = caps }
func (m *mockCryptoState) SetServerSecurityMode(mode types.SecurityMode) { m.serverSecurityMode = mode }
func (m *mockCryptoState) SetClientGUID(guid [16]byte)                   { m.clientGUID = guid }
func (m *mockCryptoState) SetClientCapabilities(caps types.Capabilities) { m.clientCapabilities = caps }
func (m *mockCryptoState) SetClientSecurityMode(mode types.SecurityMode) { m.clientSecurityMode = mode }
func (m *mockCryptoState) SetClientDialects(dialects []types.Dialect)    { m.clientDialects = dialects }
func (m *mockCryptoState) GetServerGUID() [16]byte                       { return m.serverGUID }
func (m *mockCryptoState) GetServerCapabilities() types.Capabilities     { return m.serverCapabilities }
func (m *mockCryptoState) GetServerSecurityMode() types.SecurityMode     { return m.serverSecurityMode }
func (m *mockCryptoState) GetClientDialects() []types.Dialect            { return m.clientDialects }

// newNegotiateTestContextWithCrypto creates a test context with a mock CryptoState.
func newNegotiateTestContextWithCrypto() (*SMBHandlerContext, *mockCryptoState) {
	ctx := newNegotiateTestContext()
	cs := &mockCryptoState{}
	ctx.ConnCryptoState = cs
	return ctx, cs
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

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess (0x%x)",
			result.Status, types.StatusSuccess)
	}

	if len(result.Data) < 65 {
		t.Fatalf("Response should be at least 65 bytes, got %d", len(result.Data))
	}

	structSize := binary.LittleEndian.Uint16(result.Data[0:2])
	if structSize != 65 {
		t.Errorf("StructureSize = %d, expected 65", structSize)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0210 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0210", dialectRevision)
	}

	var responseGUID [16]byte
	copy(responseGUID[:], result.Data[8:24])
	if responseGUID != h.ServerGUID {
		t.Errorf("ServerGUID mismatch: response = %x, handler = %x",
			responseGUID, h.ServerGUID)
	}

	caps := binary.LittleEndian.Uint32(result.Data[24:28])
	expectedCaps := uint32(types.CapLeasing | types.CapLargeMTU)
	if caps != expectedCaps {
		t.Errorf("Capabilities = 0x%08x, expected 0x%08x (CapLeasing|CapLargeMTU)", caps, expectedCaps)
	}

	maxTransact := binary.LittleEndian.Uint32(result.Data[28:32])
	if maxTransact != h.MaxTransactSize {
		t.Errorf("MaxTransactSize = %d, expected %d", maxTransact, h.MaxTransactSize)
	}

	maxRead := binary.LittleEndian.Uint32(result.Data[32:36])
	if maxRead != h.MaxReadSize {
		t.Errorf("MaxReadSize = %d, expected %d", maxRead, h.MaxReadSize)
	}

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

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0202 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0202", dialectRevision)
	}

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
// Empty/Short Body Tests
// =============================================================================

func TestNegotiate_EmptyDialectList(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

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

func TestNegotiate_ShortBody(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

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

func TestNegotiate_ExactMinimumBody(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	body := make([]byte, 36)
	binary.LittleEndian.PutUint16(body[0:2], 36)
	binary.LittleEndian.PutUint16(body[2:4], 0)

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusNotSupported {
		t.Errorf("Status = 0x%x, expected StatusNotSupported", result.Status)
	}
}

// =============================================================================
// Wildcard Dialect Tests
// =============================================================================

func TestNegotiate_WildcardDialect(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

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

func TestNegotiate_WildcardWithSMB202(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

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

	securityMode := result.Data[2]
	if securityMode&0x01 == 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 0 (SIGNING_ENABLED) to be set", securityMode)
	}
	if securityMode&0x02 != 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 1 (SIGNING_REQUIRED) to be clear", securityMode)
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

	securityMode := result.Data[2]
	if securityMode&0x01 == 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 0 (SIGNING_ENABLED) to be set", securityMode)
	}
	if securityMode&0x02 == 0 {
		t.Errorf("SecurityMode = 0x%02x, expected bit 1 (SIGNING_REQUIRED) to be set", securityMode)
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

	securityMode := result.Data[2]
	if securityMode != 0 {
		t.Errorf("SecurityMode = 0x%02x, expected 0x00 (signing disabled)", securityMode)
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
		// Response should be exactly 65 bytes for non-3.1.1 dialects
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
		secBufferOffset := binary.LittleEndian.Uint16(result.Data[56:58])
		if secBufferOffset != 128 {
			t.Errorf("SecurityBufferOffset = %d, expected 128", secBufferOffset)
		}
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
		systemTime := binary.LittleEndian.Uint64(result.Data[40:48])
		if systemTime == 0 {
			t.Error("SystemTime should be non-zero")
		}
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
		ctxCount := binary.LittleEndian.Uint16(result.Data[6:8])
		if ctxCount != 0 {
			t.Errorf("NegotiateContextCount = %d, expected 0", ctxCount)
		}
		ctxOffset := binary.LittleEndian.Uint32(result.Data[60:64])
		if ctxOffset != 0 {
			t.Errorf("NegotiateContextOffset = %d, expected 0", ctxOffset)
		}
	})
}

// =============================================================================
// SMB 3.0 Dialect Selection Tests
// =============================================================================

func TestNegotiate_SMB300(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0202),
		uint16(types.SMB2Dialect0210),
		uint16(types.SMB2Dialect0300),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0300 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0300", dialectRevision)
	}

	// SMB 3.0 should advertise CapLeasing | CapLargeMTU | CapDirectoryLeasing
	caps := binary.LittleEndian.Uint32(result.Data[24:28])
	expectedCaps := uint32(types.CapLeasing | types.CapLargeMTU | types.CapDirectoryLeasing)
	if caps != expectedCaps {
		t.Errorf("Capabilities = 0x%08x, expected 0x%08x", caps, expectedCaps)
	}
}

func TestNegotiate_SMB302(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0202),
		uint16(types.SMB2Dialect0210),
		uint16(types.SMB2Dialect0300),
		uint16(types.SMB2Dialect0302),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0302 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0302", dialectRevision)
	}
}

// =============================================================================
// SMB 3.1.1 Dialect Selection with Negotiate Contexts
// =============================================================================

func TestNegotiate_SMB311_FullDialectList(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	salt := make([]byte, 32)
	_, _ = rand.Read(salt)

	contexts := []types.NegotiateContext{
		buildPreauthContext([]uint16{types.HashAlgSHA512}, salt),
		buildEncryptionContext([]uint16{types.CipherAES128GCM, types.CipherAES128CCM}),
	}

	body := buildNegotiateRequestFull(
		[]uint16{0x0202, 0x0210, 0x0300, 0x0302, 0x0311},
		uint16(types.NegSigningEnabled),
		0,
		[16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		contexts,
	)

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	// Should select 3.1.1
	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0311 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0311", dialectRevision)
	}

	// NegotiateContextCount should be > 0 for 3.1.1
	ctxCount := binary.LittleEndian.Uint16(result.Data[6:8])
	if ctxCount < 2 {
		t.Errorf("NegotiateContextCount = %d, expected >= 2 (preauth + encryption)", ctxCount)
	}

	// NegotiateContextOffset should be non-zero for 3.1.1
	ctxOffset := binary.LittleEndian.Uint32(result.Data[60:64])
	if ctxOffset == 0 {
		t.Errorf("NegotiateContextOffset = 0, expected non-zero for 3.1.1")
	}

	// Response should be longer than 65 bytes (has contexts appended)
	if len(result.Data) <= 65 {
		t.Errorf("Response length = %d, expected > 65 (contexts appended)", len(result.Data))
	}
}

func TestNegotiate_SMB311_NegotiateContexts(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	salt := make([]byte, 32)
	_, _ = rand.Read(salt)

	contexts := []types.NegotiateContext{
		buildPreauthContext([]uint16{types.HashAlgSHA512}, salt),
		buildEncryptionContext([]uint16{types.CipherAES128CCM, types.CipherAES128GCM}),
	}

	body := buildNegotiateRequestFull(
		[]uint16{0x0311},
		uint16(types.NegSigningEnabled),
		0,
		[16]byte{},
		contexts,
	)

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	// Parse negotiate contexts from response
	ctxCount := binary.LittleEndian.Uint16(result.Data[6:8])
	ctxOffset := binary.LittleEndian.Uint32(result.Data[60:64])
	if ctxCount == 0 || ctxOffset == 0 {
		t.Fatalf("No negotiate contexts in response (count=%d, offset=%d)", ctxCount, ctxOffset)
	}

	// Offset is relative to SMB2 header start, but our Data starts at body (after header).
	// Body starts at offset 64 from header start. So context data starts at ctxOffset - 64.
	bodyCtxOffset := int(ctxOffset) - 64
	if bodyCtxOffset < 0 || bodyCtxOffset >= len(result.Data) {
		t.Fatalf("Invalid context offset: ctxOffset=%d, bodyOffset=%d, dataLen=%d",
			ctxOffset, bodyCtxOffset, len(result.Data))
	}

	respContexts, err := types.ParseNegotiateContextList(result.Data[bodyCtxOffset:], int(ctxCount))
	if err != nil {
		t.Fatalf("Failed to parse response negotiate contexts: %v", err)
	}

	// Verify PREAUTH_INTEGRITY_CAPABILITIES is in response
	var foundPreauth, foundEncryption bool
	for _, rc := range respContexts {
		switch rc.ContextType {
		case types.NegCtxPreauthIntegrity:
			foundPreauth = true
			preauth, err := types.DecodePreauthIntegrityCaps(rc.Data)
			if err != nil {
				t.Fatalf("Failed to decode preauth caps: %v", err)
			}
			if len(preauth.HashAlgorithms) != 1 || preauth.HashAlgorithms[0] != types.HashAlgSHA512 {
				t.Errorf("Preauth hash algorithms = %v, expected [SHA-512]", preauth.HashAlgorithms)
			}
			if len(preauth.Salt) != 32 {
				t.Errorf("Preauth salt length = %d, expected 32", len(preauth.Salt))
			}
		case types.NegCtxEncryptionCaps:
			foundEncryption = true
			enc, err := types.DecodeEncryptionCaps(rc.Data)
			if err != nil {
				t.Fatalf("Failed to decode encryption caps: %v", err)
			}
			if len(enc.Ciphers) != 1 {
				t.Errorf("Encryption ciphers count = %d, expected 1", len(enc.Ciphers))
			}
			// Server preference: AES-128-GCM > AES-128-CCM
			if len(enc.Ciphers) > 0 && enc.Ciphers[0] != types.CipherAES128GCM {
				t.Errorf("Selected cipher = 0x%04x, expected 0x%04x (AES-128-GCM)",
					enc.Ciphers[0], types.CipherAES128GCM)
			}
		}
	}

	if !foundPreauth {
		t.Error("Response missing PREAUTH_INTEGRITY_CAPABILITIES context")
	}
	if !foundEncryption {
		t.Error("Response missing ENCRYPTION_CAPABILITIES context")
	}
}

// =============================================================================
// Capability Gating Tests
// =============================================================================

func TestNegotiate_CapabilityGating(t *testing.T) {
	tests := []struct {
		name            string
		dialects        []uint16
		expectedDialect uint16
		expectedCaps    types.Capabilities
		encryption      bool
		dirLeasing      bool
	}{
		{
			name:            "SMB202_NoCaps",
			dialects:        []uint16{0x0202},
			expectedDialect: 0x0202,
			expectedCaps:    0,
		},
		{
			name:            "SMB210_LeasingAndLargeMTU",
			dialects:        []uint16{0x0210},
			expectedDialect: 0x0210,
			expectedCaps:    types.CapLeasing | types.CapLargeMTU,
		},
		{
			name:            "SMB300_WithDirLeasing",
			dialects:        []uint16{0x0300},
			expectedDialect: 0x0300,
			expectedCaps:    types.CapLeasing | types.CapLargeMTU | types.CapDirectoryLeasing,
			dirLeasing:      true,
		},
		{
			name:            "SMB300_WithEncryption",
			dialects:        []uint16{0x0300},
			expectedDialect: 0x0300,
			expectedCaps:    types.CapLeasing | types.CapLargeMTU | types.CapDirectoryLeasing | types.CapEncryption,
			encryption:      true,
			dirLeasing:      true,
		},
		{
			name:            "SMB302_WithEncryption",
			dialects:        []uint16{0x0302},
			expectedDialect: 0x0302,
			expectedCaps:    types.CapLeasing | types.CapLargeMTU | types.CapDirectoryLeasing | types.CapEncryption,
			encryption:      true,
			dirLeasing:      true,
		},
		{
			name:            "SMB311_NoCapsEncryption",
			dialects:        []uint16{0x0311},
			expectedDialect: 0x0311,
			expectedCaps:    types.CapLeasing | types.CapLargeMTU | types.CapDirectoryLeasing,
			dirLeasing:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler()
			h.EncryptionEnabled = tt.encryption
			h.DirectoryLeasingEnabled = tt.dirLeasing

			ctx := newNegotiateTestContext()

			var body []byte
			// For 3.1.1, include negotiate contexts
			if tt.expectedDialect == 0x0311 {
				salt := make([]byte, 32)
				_, _ = rand.Read(salt)
				contexts := []types.NegotiateContext{
					buildPreauthContext([]uint16{types.HashAlgSHA512}, salt),
					buildEncryptionContext([]uint16{types.CipherAES128GCM}),
				}
				body = buildNegotiateRequestFull(tt.dialects, 0, 0, [16]byte{}, contexts)
			} else {
				body = buildNegotiateRequest(tt.dialects)
			}

			result, err := h.Negotiate(ctx, body)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result.Status != types.StatusSuccess {
				t.Fatalf("Status = 0x%x, expected StatusSuccess", result.Status)
			}

			dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
			if dialectRevision != tt.expectedDialect {
				t.Errorf("DialectRevision = 0x%04x, expected 0x%04x",
					dialectRevision, tt.expectedDialect)
			}

			caps := types.Capabilities(binary.LittleEndian.Uint32(result.Data[24:28]))
			if caps != tt.expectedCaps {
				t.Errorf("Capabilities = 0x%08x, expected 0x%08x", caps, tt.expectedCaps)
			}
		})
	}
}

// =============================================================================
// Min/Max Dialect Range Tests
// =============================================================================

func TestNegotiate_DialectRange(t *testing.T) {
	t.Run("MinDialect_3.0_RejectsOnly2x", func(t *testing.T) {
		h := NewHandler()
		h.MinDialect = types.Dialect0300
		ctx := newNegotiateTestContext()

		body := buildNegotiateRequest([]uint16{0x0202, 0x0210})

		result, err := h.Negotiate(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusNotSupported {
			t.Errorf("Status = 0x%x, expected StatusNotSupported", result.Status)
		}
	})

	t.Run("MinDialect_3.0_SelectsFromRange", func(t *testing.T) {
		h := NewHandler()
		h.MinDialect = types.Dialect0300
		ctx := newNegotiateTestContext()

		salt := make([]byte, 32)
		_, _ = rand.Read(salt)
		contexts := []types.NegotiateContext{
			buildPreauthContext([]uint16{types.HashAlgSHA512}, salt),
			buildEncryptionContext([]uint16{types.CipherAES128GCM}),
		}
		body := buildNegotiateRequestFull(
			[]uint16{0x0202, 0x0300, 0x0311},
			0, 0, [16]byte{}, contexts,
		)

		result, err := h.Negotiate(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
		}

		dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
		if dialectRevision != 0x0311 {
			t.Errorf("DialectRevision = 0x%04x, expected 0x0311", dialectRevision)
		}
	})

	t.Run("MaxDialect_2.1_CapsAt210", func(t *testing.T) {
		h := NewHandler()
		h.MaxDialect = types.Dialect0210
		ctx := newNegotiateTestContext()

		body := buildNegotiateRequest([]uint16{0x0202, 0x0210, 0x0300, 0x0311})

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
	})
}

// =============================================================================
// CryptoState Population Tests
// =============================================================================

func TestNegotiate_CryptoState_Populated(t *testing.T) {
	h := NewHandler()
	ctx, cs := newNegotiateTestContextWithCrypto()

	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	contexts := []types.NegotiateContext{
		buildPreauthContext([]uint16{types.HashAlgSHA512}, salt),
		buildEncryptionContext([]uint16{types.CipherAES128GCM}),
	}

	clientGUID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	body := buildNegotiateRequestFull(
		[]uint16{0x0202, 0x0210, 0x0300, 0x0311},
		uint16(types.NegSigningEnabled),
		0,
		clientGUID,
		contexts,
	)

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Fatalf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	// Verify CryptoState was populated
	if cs.dialect != types.Dialect0311 {
		t.Errorf("CryptoState.Dialect = 0x%04x, expected 0x0311", cs.dialect)
	}

	if cs.cipherId != types.CipherAES128GCM {
		t.Errorf("CryptoState.CipherId = 0x%04x, expected 0x%04x (AES-128-GCM)",
			cs.cipherId, types.CipherAES128GCM)
	}

	if cs.preauthHashId != types.HashAlgSHA512 {
		t.Errorf("CryptoState.PreauthHashId = 0x%04x, expected 0x%04x (SHA-512)",
			cs.preauthHashId, types.HashAlgSHA512)
	}

	if cs.serverGUID != h.ServerGUID {
		t.Errorf("CryptoState.ServerGUID mismatch")
	}

	if cs.clientGUID != clientGUID {
		t.Errorf("CryptoState.ClientGUID mismatch")
	}

	if cs.clientSecurityMode != types.NegSigningEnabled {
		t.Errorf("CryptoState.ClientSecurityMode = %d, expected %d",
			cs.clientSecurityMode, types.NegSigningEnabled)
	}
}

// =============================================================================
// Multi-protocol Negotiate (0x02FF) with 3.x Tests
// =============================================================================

func TestNegotiate_WildcardWith3x(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Wildcard + 3.0: should select 3.0 (wildcard NOT echoed because 3.x > 2.0.2)
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2DialectWild),
		uint16(types.SMB2Dialect0300),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0300 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0300 (3.x suppresses wildcard echo)",
			dialectRevision)
	}

	// Negotiate contexts should NOT be present for wildcard negotiate
	// (even though 3.0 was selected, this was a multi-protocol negotiate)
	ctxCount := binary.LittleEndian.Uint16(result.Data[6:8])
	if ctxCount != 0 {
		t.Errorf("NegotiateContextCount = %d, expected 0 (no contexts for non-3.1.1)", ctxCount)
	}
}

// =============================================================================
// SMB 3.x Only (no 2.x) - should now succeed
// =============================================================================

func TestNegotiate_SMB3xOnly(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0300),
		uint16(types.SMB2Dialect0302),
	})

	result, err := h.Negotiate(ctx, body)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Status != types.StatusSuccess {
		t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
	}

	dialectRevision := binary.LittleEndian.Uint16(result.Data[4:6])
	if dialectRevision != 0x0302 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0302", dialectRevision)
	}
}

func TestNegotiate_MixedWithSMB3x(t *testing.T) {
	h := NewHandler()
	ctx := newNegotiateTestContext()

	// Mix of 2.x and 3.x - should select highest = 3.0
	body := buildNegotiateRequest([]uint16{
		uint16(types.SMB2Dialect0300),
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
	if dialectRevision != 0x0300 {
		t.Errorf("DialectRevision = 0x%04x, expected 0x0300", dialectRevision)
	}
}
