package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildVNEGRequest builds a VALIDATE_NEGOTIATE_INFO IOCTL request body.
// The IOCTL envelope wraps the VNEG payload at offset 56.
func buildVNEGRequest(capabilities uint32, guid [16]byte, securityMode uint16, dialects []uint16) []byte {
	// Build the VNEG input payload using smbenc
	payload := smbenc.NewWriter(64)
	payload.WriteUint32(capabilities)
	payload.WriteBytes(guid[:])
	payload.WriteUint16(securityMode)
	payload.WriteUint16(uint16(len(dialects)))
	for _, d := range dialects {
		payload.WriteUint16(d)
	}
	payloadBytes := payload.Bytes()

	// Build the full IOCTL request envelope (56 bytes header + payload)
	w := smbenc.NewWriter(56 + len(payloadBytes))
	w.WriteUint16(57)                         // StructureSize
	w.WriteUint16(0)                          // Reserved
	w.WriteUint32(FsctlValidateNegotiateInfo) // CtlCode
	w.WriteBytes(allFFFileID)                 // FileId (all 0xFF)
	w.WriteUint32(uint32(64 + 56))            // InputOffset (header + fixed body start)
	w.WriteUint32(uint32(len(payloadBytes)))  // InputCount
	w.WriteUint32(0)                          // MaxInputResponse
	w.WriteUint32(0)                          // OutputOffset
	w.WriteUint32(0)                          // OutputCount
	w.WriteUint32(0)                          // MaxOutputResponse
	w.WriteUint32(0)                          // Flags
	w.WriteUint32(0)                          // Reserved2
	w.WriteBytes(payloadBytes)                // Buffer (VNEG payload)

	return w.Bytes()
}

func TestValidateNegotiateInfo_311_DropConnection(t *testing.T) {
	h := NewHandler()
	cs := &mockCryptoState{
		dialect: types.Dialect0311,
	}
	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	body := buildVNEGRequest(
		0x00000006, // capabilities
		[16]byte{1, 2, 3, 4},
		0x0001, // signing enabled
		[]uint16{uint16(types.Dialect0300), uint16(types.Dialect0311)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.DropConnection {
		t.Error("expected DropConnection=true for 3.1.1 connection")
	}
}

func TestValidateNegotiateInfo_300_Success(t *testing.T) {
	h := NewHandler()

	serverGUID := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22,
		0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}
	serverCaps := types.CapLeasing | types.CapLargeMTU
	serverSecMode := types.NegSigningEnabled

	cs := &mockCryptoState{
		dialect:            types.Dialect0300,
		serverGUID:         serverGUID,
		serverCapabilities: serverCaps,
		serverSecurityMode: serverSecMode,
		clientDialects:     []types.Dialect{types.Dialect0202, types.Dialect0210, types.Dialect0300},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	// Client sends matching parameters
	body := buildVNEGRequest(
		uint32(serverCaps),
		serverGUID,
		uint16(serverSecMode),
		[]uint16{uint16(types.Dialect0202), uint16(types.Dialect0210), uint16(types.Dialect0300)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.DropConnection {
		t.Error("expected DropConnection=false for matching params")
	}
	if result.Status != types.StatusSuccess {
		t.Errorf("expected StatusSuccess, got %v", result.Status)
	}

	// Parse the response: skip IOCTL response header (48 bytes), then read VNEG output (24 bytes)
	if len(result.Data) < 48+24 {
		t.Fatalf("response too short: %d bytes", len(result.Data))
	}
	output := result.Data[48:]
	r := smbenc.NewReader(output)
	respCaps := r.ReadUint32()
	respGUID := r.ReadBytes(16)
	respSecMode := r.ReadUint16()
	respDialect := r.ReadUint16()
	if r.Err() != nil {
		t.Fatalf("failed to parse VNEG response: %v", r.Err())
	}

	if respCaps != uint32(serverCaps) {
		t.Errorf("capabilities mismatch: got 0x%08X, want 0x%08X", respCaps, uint32(serverCaps))
	}
	if !bytes.Equal(respGUID, serverGUID[:]) {
		t.Errorf("GUID mismatch: got %x, want %x", respGUID, serverGUID)
	}
	if respSecMode != uint16(serverSecMode) {
		t.Errorf("security mode mismatch: got 0x%04X, want 0x%04X", respSecMode, uint16(serverSecMode))
	}
	if respDialect != uint16(types.Dialect0300) {
		t.Errorf("dialect mismatch: got 0x%04X, want 0x%04X", respDialect, uint16(types.Dialect0300))
	}
}

func TestValidateNegotiateInfo_302_Success(t *testing.T) {
	h := NewHandler()

	serverGUID := [16]byte{0x11, 0x22, 0x33, 0x44}
	serverCaps := types.CapLeasing | types.CapLargeMTU
	serverSecMode := types.NegSigningEnabled | types.NegSigningRequired

	cs := &mockCryptoState{
		dialect:            types.Dialect0302,
		serverGUID:         serverGUID,
		serverCapabilities: serverCaps,
		serverSecurityMode: serverSecMode,
		clientDialects:     []types.Dialect{types.Dialect0300, types.Dialect0302},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	body := buildVNEGRequest(
		uint32(serverCaps),
		serverGUID,
		uint16(serverSecMode),
		[]uint16{uint16(types.Dialect0300), uint16(types.Dialect0302)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.DropConnection {
		t.Error("expected DropConnection=false for matching params")
	}
	if result.Status != types.StatusSuccess {
		t.Errorf("expected StatusSuccess, got %v", result.Status)
	}
}

func TestValidateNegotiateInfo_MismatchCapabilities_DropConnection(t *testing.T) {
	h := NewHandler()
	cs := &mockCryptoState{
		dialect:            types.Dialect0300,
		serverGUID:         [16]byte{1, 2, 3, 4},
		serverCapabilities: types.CapLeasing | types.CapLargeMTU,
		serverSecurityMode: types.NegSigningEnabled,
		clientDialects:     []types.Dialect{types.Dialect0300},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	// Send wrong capabilities (0x0 instead of CapLeasing|CapLargeMTU)
	body := buildVNEGRequest(
		0x00000000, // WRONG capabilities
		cs.serverGUID,
		uint16(cs.serverSecurityMode),
		[]uint16{uint16(types.Dialect0300)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.DropConnection {
		t.Error("expected DropConnection=true for mismatched capabilities")
	}
}

func TestValidateNegotiateInfo_MismatchGUID_DropConnection(t *testing.T) {
	h := NewHandler()
	cs := &mockCryptoState{
		dialect:            types.Dialect0300,
		serverGUID:         [16]byte{1, 2, 3, 4},
		serverCapabilities: types.CapLeasing | types.CapLargeMTU,
		serverSecurityMode: types.NegSigningEnabled,
		clientDialects:     []types.Dialect{types.Dialect0300},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	wrongGUID := [16]byte{0xFF, 0xFE, 0xFD, 0xFC}
	body := buildVNEGRequest(
		uint32(cs.serverCapabilities),
		wrongGUID, // WRONG GUID
		uint16(cs.serverSecurityMode),
		[]uint16{uint16(types.Dialect0300)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.DropConnection {
		t.Error("expected DropConnection=true for mismatched GUID")
	}
}

func TestValidateNegotiateInfo_MismatchSecurityMode_DropConnection(t *testing.T) {
	h := NewHandler()
	cs := &mockCryptoState{
		dialect:            types.Dialect0300,
		serverGUID:         [16]byte{1, 2, 3, 4},
		serverCapabilities: types.CapLeasing | types.CapLargeMTU,
		serverSecurityMode: types.NegSigningEnabled,
		clientDialects:     []types.Dialect{types.Dialect0300},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	body := buildVNEGRequest(
		uint32(cs.serverCapabilities),
		cs.serverGUID,
		0x0003, // WRONG security mode (signing enabled + required instead of just enabled)
		[]uint16{uint16(types.Dialect0300)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.DropConnection {
		t.Error("expected DropConnection=true for mismatched security mode")
	}
}

func TestValidateNegotiateInfo_MismatchDialect_DropConnection(t *testing.T) {
	h := NewHandler()
	cs := &mockCryptoState{
		dialect:            types.Dialect0300,
		serverGUID:         [16]byte{1, 2, 3, 4},
		serverCapabilities: types.CapLeasing | types.CapLargeMTU,
		serverSecurityMode: types.NegSigningEnabled,
		// Client originally offered 0300, but now sends only 0202
		clientDialects: []types.Dialect{types.Dialect0300},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	// Send only 0x0202 dialect -- server originally negotiated 0x0300
	// Re-selection from {0x0202} would yield 0x0202, not 0x0300 -> mismatch
	body := buildVNEGRequest(
		uint32(cs.serverCapabilities),
		cs.serverGUID,
		uint16(cs.serverSecurityMode),
		[]uint16{uint16(types.Dialect0202)}, // only 0202, would select 0202 != 0300
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.DropConnection {
		t.Error("expected DropConnection=true for mismatched dialect selection")
	}
}

func TestIoctlDispatchTable_RoutesCorrectly(t *testing.T) {
	h := NewHandler()
	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// Test that unsupported IOCTL codes return StatusNotSupported
	w := smbenc.NewWriter(16)
	w.WriteUint16(57)             // StructureSize
	w.WriteUint16(0)              // Reserved
	w.WriteUint32(0x00FFFFFF)     // Unknown CtlCode
	w.WriteBytes(make([]byte, 4)) // Padding to make minimum size
	body := w.Bytes()

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusNotSupported {
		t.Errorf("expected StatusNotSupported for unknown IOCTL, got %v", result.Status)
	}

	// Test that known IOCTL code FSCTL_QUERY_NETWORK_INTERFACE_INFO returns StatusNotSupported
	w2 := smbenc.NewWriter(16)
	w2.WriteUint16(57)
	w2.WriteUint16(0)
	w2.WriteUint32(FsctlQueryNetworkInterfInfo) // Known but unsupported
	w2.WriteBytes(make([]byte, 4))
	body2 := w2.Bytes()

	result2, err := h.Ioctl(ctx, body2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Status != types.StatusNotSupported {
		t.Errorf("expected StatusNotSupported for FSCTL_QUERY_NETWORK_INTERFACE_INFO, got %v", result2.Status)
	}
}

func TestValidateNegotiateInfo_ResponseContainsCryptoStateValues(t *testing.T) {
	h := NewHandler()

	serverGUID := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	serverCaps := types.CapLeasing | types.CapLargeMTU | types.CapDirectoryLeasing
	serverSecMode := types.NegSigningEnabled | types.NegSigningRequired

	cs := &mockCryptoState{
		dialect:            types.Dialect0300,
		serverGUID:         serverGUID,
		serverCapabilities: serverCaps,
		serverSecurityMode: serverSecMode,
		clientDialects:     []types.Dialect{types.Dialect0202, types.Dialect0210, types.Dialect0300},
	}
	h.MinDialect = types.Dialect0202
	h.MaxDialect = types.Dialect0311

	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		ClientAddr:      "192.168.1.100:12345",
		ConnCryptoState: cs,
	}

	body := buildVNEGRequest(
		uint32(serverCaps),
		serverGUID,
		uint16(serverSecMode),
		[]uint16{uint16(types.Dialect0202), uint16(types.Dialect0210), uint16(types.Dialect0300)},
	)

	result, err := h.Ioctl(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusSuccess {
		t.Fatalf("expected StatusSuccess, got %v", result.Status)
	}

	// Verify the response contains values from CryptoState
	output := result.Data[48:] // skip IOCTL header
	r := smbenc.NewReader(output)
	respCaps := types.Capabilities(r.ReadUint32())
	respGUID := r.ReadBytes(16)
	respSecMode := types.SecurityMode(r.ReadUint16())
	respDialect := types.Dialect(r.ReadUint16())

	if respCaps != serverCaps {
		t.Errorf("capabilities: got %v, want %v", respCaps, serverCaps)
	}
	if !bytes.Equal(respGUID, serverGUID[:]) {
		t.Errorf("GUID: got %x, want %x", respGUID, serverGUID[:])
	}
	if respSecMode != serverSecMode {
		t.Errorf("security mode: got %v, want %v", respSecMode, serverSecMode)
	}
	if respDialect != types.Dialect0300 {
		t.Errorf("dialect: got %v, want %v", respDialect, types.Dialect0300)
	}
}
