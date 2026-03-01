package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// handleValidateNegotiateInfo validates the negotiation parameters [MS-SMB2] 2.2.31.4.
//
// This FSCTL is used by SMB 3.x clients to verify that the negotiation wasn't
// tampered with by a man-in-the-middle attack. The client sends its view of
// the negotiation parameters, and the server validates them against the
// ConnectionCryptoState (populated during NEGOTIATE).
//
// Per MS-SMB2 section 3.3.5.15.12:
//   - For SMB 3.1.1 connections: drop the TCP connection immediately without response.
//   - For SMB 3.0/3.0.2: validate all 4 fields (Capabilities, ServerGUID, SecurityMode, Dialect).
//   - On mismatch: drop the TCP connection (possible downgrade attack).
//   - On success: return 24-byte response with server values.
//
// Request format (VALIDATE_NEGOTIATE_INFO):
//
//	Offset  Size  Field
//	------  ----  ------------------
//	0       4     Capabilities
//	4       16    Guid (ClientGuid)
//	20      2     SecurityMode
//	22      2     DialectCount
//	24      2*N   Dialects
//
// Response format (VALIDATE_NEGOTIATE_INFO):
//
//	Offset  Size  Field
//	------  ----  ------------------
//	0       4     Capabilities
//	4       16    Guid (ServerGuid)
//	20      2     SecurityMode
//	22      2     Dialect (selected)
func (h *Handler) handleValidateNegotiateInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// Parse IOCTL envelope
	fileID, ok := parseIoctlFileID(body)
	if !ok || len(body) < 56 {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: request too small", "len", len(body))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Per [MS-SMB2] 2.2.31.4, FSCTL_VALIDATE_NEGOTIATE_INFO MUST use FileId
	// {0xFFFFFFFFFFFFFFFF, 0xFFFFFFFFFFFFFFFF} (all 0xFF bytes).
	if !bytes.Equal(fileID[:], allFFFileID) {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: unexpected FileId (expected all 0xFF)", "fileID", fileID)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	inputCountR := smbenc.NewReader(body[28:32])
	inputCount := inputCountR.ReadUint32()
	if inputCountR.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Minimum input size: 24 bytes (Capabilities + Guid + SecurityMode + DialectCount)
	if inputCount < 24 {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: input too small", "inputCount", inputCount)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Extract input data from buffer portion
	bufferStart := uint32(56)
	if uint32(len(body)) < bufferStart+inputCount {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: input data out of bounds",
			"bodyLen", len(body), "bufferStart", bufferStart, "inputCount", inputCount)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	inputData := body[bufferStart : bufferStart+inputCount]

	// Parse VALIDATE_NEGOTIATE_INFO request using smbenc
	r := smbenc.NewReader(inputData)
	clientCapabilities := r.ReadUint32()
	clientGUID := r.ReadBytes(16)
	clientSecurityMode := r.ReadUint16()
	dialectCount := r.ReadUint16()
	if r.Err() != nil {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: failed to parse input", "error", r.Err())
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Validate dialect count
	expectedSize := 24 + (int(dialectCount) * 2)
	if int(inputCount) < expectedSize {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: not enough dialects",
			"dialectCount", dialectCount, "inputCount", inputCount, "expectedSize", expectedSize)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Read dialects
	dialects := make([]types.Dialect, dialectCount)
	for i := range dialects {
		dialects[i] = types.Dialect(r.ReadUint16())
	}
	if r.Err() != nil {
		logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: failed to parse dialects", "error", r.Err())
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Per MS-SMB2 3.3.5.15.12: If Connection.Dialect is "3.1.1", the server
	// MUST disconnect the connection and not reply.
	cs := ctx.ConnCryptoState
	if cs != nil && cs.GetDialect() == types.Dialect0311 {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: 3.1.1 connection, dropping TCP",
			"client", ctx.ClientAddr)
		return &HandlerResult{DropConnection: true}, nil
	}

	// For 3.0/3.0.2: validate all 4 fields against CryptoState.
	// If CryptoState is nil (pre-plan-02 connections or tests), fall back to
	// the legacy re-computation path.
	if cs != nil {
		return h.validateFromCryptoState(ctx, cs, clientCapabilities, clientGUID, clientSecurityMode, dialects, fileID)
	}

	// Legacy fallback: re-compute values (for backward compat with pre-SMB3 connections)
	return h.validateLegacy(ctx, inputData, fileID)
}

// validateFromCryptoState validates VNEG parameters against the stored CryptoState.
// This is the primary code path for SMB 3.0/3.0.2 connections.
func (h *Handler) validateFromCryptoState(
	ctx *SMBHandlerContext,
	cs CryptoState,
	clientCapabilities uint32,
	clientGUID []byte,
	clientSecurityMode uint16,
	dialects []types.Dialect,
	fileID [16]byte,
) (*HandlerResult, error) {
	serverCaps := cs.GetServerCapabilities()
	serverGUID := cs.GetServerGUID()
	serverSecMode := cs.GetServerSecurityMode()

	// Validate Capabilities
	if uint32(serverCaps) != clientCapabilities {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: capabilities mismatch (possible downgrade)",
			"client", ctx.ClientAddr,
			"serverCaps", fmt.Sprintf("0x%08X", uint32(serverCaps)),
			"clientCaps", fmt.Sprintf("0x%08X", clientCapabilities))
		return &HandlerResult{DropConnection: true}, nil
	}

	// Validate ServerGUID
	if !bytes.Equal(serverGUID[:], clientGUID) {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: GUID mismatch (possible downgrade)",
			"client", ctx.ClientAddr,
			"serverGUID", fmt.Sprintf("%x", serverGUID),
			"clientGUID", fmt.Sprintf("%x", clientGUID))
		return &HandlerResult{DropConnection: true}, nil
	}

	// Validate SecurityMode
	if uint16(serverSecMode) != clientSecurityMode {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: security mode mismatch (possible downgrade)",
			"client", ctx.ClientAddr,
			"serverSecMode", fmt.Sprintf("0x%04X", uint16(serverSecMode)),
			"clientSecMode", fmt.Sprintf("0x%04X", clientSecurityMode))
		return &HandlerResult{DropConnection: true}, nil
	}

	// Validate Dialect: re-select from client dialects using same algorithm
	selectedDialect := h.selectDialectFromList(dialects)
	if selectedDialect == 0 {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: no common dialect in re-selection",
			"client", ctx.ClientAddr)
		return &HandlerResult{DropConnection: true}, nil
	}

	negotiatedDialect := cs.GetDialect()
	if selectedDialect != negotiatedDialect {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: dialect mismatch (possible downgrade)",
			"client", ctx.ClientAddr,
			"negotiatedDialect", negotiatedDialect.String(),
			"reselectedDialect", selectedDialect.String())
		return &HandlerResult{DropConnection: true}, nil
	}

	// All 4 fields match -- build success response using CryptoState values
	w := smbenc.NewWriter(24)
	w.WriteUint32(uint32(serverCaps))
	w.WriteBytes(serverGUID[:])
	w.WriteUint16(uint16(serverSecMode))
	w.WriteUint16(uint16(negotiatedDialect))

	if w.Err() != nil {
		logger.Error("IOCTL VALIDATE_NEGOTIATE_INFO: response encoding error", "error", w.Err())
		return NewErrorResult(types.StatusInternalError), nil
	}

	logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: success (CryptoState)",
		"dialect", negotiatedDialect.String(),
		"securityMode", fmt.Sprintf("0x%02X", uint16(serverSecMode)))

	resp := buildIoctlResponse(FsctlValidateNegotiateInfo, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp), nil
}

// selectDialectFromList re-selects a dialect from the offered list using
// the same priority-based algorithm as the negotiate handler.
// Respects the handler's MinDialect/MaxDialect range.
func (h *Handler) selectDialectFromList(dialects []types.Dialect) types.Dialect {
	var best types.Dialect
	bestPriority := 0

	minP := types.DialectPriority(h.MinDialect)
	maxP := types.DialectPriority(h.MaxDialect)

	for _, d := range dialects {
		p := types.DialectPriority(d)
		if p == 0 {
			continue // Unknown dialect
		}
		if p < minP || p > maxP {
			continue
		}
		if p > bestPriority {
			bestPriority = p
			best = d
		}
	}

	return best
}

// validateLegacy is the legacy VNEG validation path for connections that
// don't have a CryptoState (pre-SMB3 or test scenarios). It re-computes
// server values from handler configuration rather than reading from CryptoState.
func (h *Handler) validateLegacy(ctx *SMBHandlerContext, inputData []byte, fileID [16]byte) (*HandlerResult, error) {
	r := smbenc.NewReader(inputData)
	_ = r.ReadUint32()  // clientCapabilities (unused in legacy path)
	_ = r.ReadBytes(16) // clientGuid (unused in legacy path)
	_ = r.ReadUint16()  // clientSecurityMode (unused in legacy path)
	dialectCount := r.ReadUint16()
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse dialect list, filtering to pre-3.0 dialects only.
	// The legacy path is only reached when CryptoState is nil (pre-SMB3
	// connections), so 3.x dialects are not applicable here.
	dialects := make([]types.Dialect, 0, dialectCount)
	hasWildcard := false
	for range int(dialectCount) {
		d := types.Dialect(r.ReadUint16())
		switch d {
		case types.Dialect0202, types.Dialect0210:
			dialects = append(dialects, d)
		case types.DialectWildcard:
			hasWildcard = true
			dialects = append(dialects, d)
		}
	}
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Re-select using the same priority-based algorithm as negotiate
	selectedDialect := h.selectDialectFromList(dialects)
	// Wildcard implies 2.0.2 baseline if nothing better was selected
	if hasWildcard && selectedDialect == 0 {
		selectedDialect = types.Dialect0202
	}

	if selectedDialect == 0 {
		logger.Warn("IOCTL VALIDATE_NEGOTIATE_INFO: no common dialect")
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Mirror wildcard echo per MS-SMB2 3.3.5.3.2
	responseDialect := selectedDialect
	if hasWildcard && selectedDialect <= types.Dialect0202 {
		responseDialect = types.DialectWildcard
	}

	// Build SecurityMode from signing config
	var securityMode types.SecurityMode
	if h.SigningConfig.Enabled {
		securityMode |= types.NegSigningEnabled
	}
	if h.SigningConfig.Required {
		securityMode |= types.NegSigningRequired
	}

	// Build Capabilities to match NEGOTIATE response
	capabilities := uint32(h.buildCapabilities(selectedDialect))

	// Build response
	w := smbenc.NewWriter(24)
	w.WriteUint32(capabilities)
	w.WriteBytes(h.ServerGUID[:])
	w.WriteUint16(uint16(securityMode))
	w.WriteUint16(uint16(responseDialect))

	logger.Debug("IOCTL VALIDATE_NEGOTIATE_INFO: success (legacy)",
		"dialect", responseDialect.String(),
		"securityMode", fmt.Sprintf("0x%02X", uint16(securityMode)))

	resp := buildIoctlResponse(FsctlValidateNegotiateInfo, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp), nil
}
