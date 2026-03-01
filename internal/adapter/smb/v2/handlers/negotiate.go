package handlers

import (
	"crypto/rand"
	"slices"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// Negotiate handles the SMB2 NEGOTIATE command [MS-SMB2] 2.2.3, 2.2.4.
//
// It negotiates the protocol dialect (2.0.2, 2.1, 3.0, 3.0.2, or 3.1.1),
// security mode (signing enabled/required), and server capabilities.
// For SMB 3.1.1, it also processes negotiate contexts (preauth integrity,
// encryption capabilities) and populates the connection's CryptoState.
//
// The handler respects the configured MinDialect/MaxDialect range, selecting
// the highest mutually supported dialect within that range. If no dialect in
// the configured range is offered by the client, STATUS_NOT_SUPPORTED is returned.
//
// Capability gating per [MS-SMB2] 3.3.5.4:
//   - SMB 2.0.2: capabilities = 0 (reserved)
//   - SMB 2.1:   CapLeasing | CapLargeMTU
//   - SMB 3.0+:  CapLeasing | CapLargeMTU | CapDirectoryLeasing (if enabled) | CapEncryption (if enabled, not for 3.1.1)
//   - SMB 3.1.1: CapLeasing | CapLargeMTU | CapDirectoryLeasing (encryption via contexts)
func (h *Handler) Negotiate(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 36 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request using smbenc
	r := smbenc.NewReader(body)
	_ = r.ReadUint16()                       // StructureSize (always 36)
	dialectCount := r.ReadUint16()           // DialectCount
	clientSecurityMode := r.ReadUint16()     // SecurityMode
	_ = r.ReadUint16()                       // Reserved
	clientCapabilities := r.ReadUint32()     // Capabilities
	clientGUID := r.ReadBytes(16)            // ClientGUID (16 bytes)
	negotiateContextOffset := r.ReadUint32() // NegotiateContextOffset (3.1.1 only)
	negotiateContextCount := r.ReadUint16()  // NegotiateContextCount (3.1.1 only)
	_ = r.ReadUint16()                       // Reserved2

	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse dialect list (starts at offset 36)
	dialects := make([]types.Dialect, 0, dialectCount)
	for range int(dialectCount) {
		d := r.ReadUint16()
		if r.Err() != nil {
			break
		}
		dialects = append(dialects, types.Dialect(d))
	}

	logger.Debug("SMB2 NEGOTIATE request",
		"dialectCount", dialectCount,
		"bodyLen", len(body))

	// Select highest dialect within configured [MinDialect, MaxDialect] range.
	selectedDialect, hasWildcard := h.selectDialect(dialects)

	// Per MS-SMB2 section 3.3.5.3.2: When the client sends the wildcard dialect
	// (0x02FF), echo it back unless a dialect > 2.0.2 was selected.
	responseDialect := selectedDialect
	if hasWildcard && selectedDialect <= types.Dialect0202 {
		responseDialect = types.DialectWildcard
	}

	logger.Debug("SMB2 NEGOTIATE dialect selection",
		"dialect", selectedDialect.String(),
		"responseDialect", responseDialect.String(),
		"supported", selectedDialect != 0)

	if selectedDialect == 0 {
		return NewErrorResult(types.StatusNotSupported), nil
	}

	// Build capabilities based on selected dialect
	capabilities := h.buildCapabilities(selectedDialect)

	// Set SecurityMode based on signing configuration
	var securityMode types.SecurityMode
	if h.SigningConfig.Enabled {
		securityMode |= types.NegSigningEnabled
	}
	if h.SigningConfig.Required {
		securityMode |= types.NegSigningRequired
	}

	// Process negotiate contexts (3.1.1 only)
	var responseContexts []types.NegotiateContext
	var selectedCipher uint16
	is311 := selectedDialect == types.Dialect0311

	if is311 && negotiateContextCount > 0 && negotiateContextOffset > 0 {
		responseContexts, selectedCipher = h.processNegotiateContexts(
			body, negotiateContextOffset, negotiateContextCount)
	}

	// Build response body (65 bytes fixed + optional negotiate contexts).
	//
	// [MS-SMB2] 2.2.4 NEGOTIATE Response:
	//   Offset 0:  StructureSize (2) = 65
	//   Offset 2:  SecurityMode (2)
	//   Offset 4:  DialectRevision (2)
	//   Offset 6:  NegotiateContextCount/Reserved (2)
	//   Offset 8:  ServerGuid (16)
	//   Offset 24: Capabilities (4)
	//   Offset 28: MaxTransactSize (4)
	//   Offset 32: MaxReadSize (4)
	//   Offset 36: MaxWriteSize (4)
	//   Offset 40: SystemTime (8)
	//   Offset 48: ServerStartTime (8)
	//   Offset 56: SecurityBufferOffset (2)
	//   Offset 58: SecurityBufferLength (2)
	//   Offset 60: NegotiateContextOffset/Reserved2 (4)
	//   Offset 64: SecurityBuffer (variable) -- we send 0 bytes
	//   Total fixed: 65 bytes (StructureSize includes the 1-byte variable portion)
	w := smbenc.NewWriter(65)
	w.WriteUint16(65)                      // StructureSize
	w.WriteUint16(uint16(securityMode))    // SecurityMode
	w.WriteUint16(uint16(responseDialect)) // DialectRevision

	if is311 {
		w.WriteUint16(uint16(len(responseContexts))) // NegotiateContextCount
	} else {
		w.WriteUint16(0) // Reserved
	}

	w.WriteBytes(h.ServerGUID[:])                    // ServerGuid (16 bytes)
	w.WriteUint32(uint32(capabilities))              // Capabilities
	w.WriteUint32(h.MaxTransactSize)                 // MaxTransactSize
	w.WriteUint32(h.MaxReadSize)                     // MaxReadSize
	w.WriteUint32(h.MaxWriteSize)                    // MaxWriteSize
	w.WriteUint64(types.TimeToFiletime(time.Now()))  // SystemTime
	w.WriteUint64(types.TimeToFiletime(h.StartTime)) // ServerStartTime
	w.WriteUint16(128)                               // SecurityBufferOffset
	w.WriteUint16(0)                                 // SecurityBufferLength
	w.WriteUint32(0)                                 // NegotiateContextOffset/Reserved2 (placeholder)
	// Offset 64: SecurityBuffer (0 bytes -- omitted)
	// Total = 65 bytes (the 1-byte variable portion is the empty security buffer)
	w.WriteUint8(0) // 1-byte variable portion for StructureSize=65

	resp := w.Bytes()

	// For 3.1.1 with negotiate contexts, append them after the fixed body,
	// padded to 8-byte alignment.
	if is311 && len(responseContexts) > 0 {
		// Security buffer is 0 bytes, so contexts follow immediately after the
		// 65-byte body. Pad to 8-byte alignment relative to SMB2 header start.
		// SMB2 header = 64 bytes, body starts at 64. Current body end = 64 + 65 = 129.
		absEnd := 64 + len(resp) // 129
		if absEnd%8 != 0 {
			padding := 8 - (absEnd % 8)
			resp = append(resp, make([]byte, padding)...)
		}

		// NegotiateContextOffset is relative to SMB2 header start
		// Backpatch at offset 60 (NegotiateContextOffset field)
		contextOffset := uint32(64 + len(resp))
		wp := smbenc.NewWriter(4)
		wp.WriteUint32(contextOffset)
		copy(resp[60:64], wp.Bytes())

		// Encode and append negotiate contexts
		ctxBytes := types.EncodeNegotiateContextList(responseContexts)
		resp = append(resp, ctxBytes...)
	}
	// else: resp[60:64] already 0 (NegotiateContextOffset = 0)

	// Populate CryptoState with negotiate parameters
	if ctx.ConnCryptoState != nil {
		var guid [16]byte
		if len(clientGUID) == 16 {
			copy(guid[:], clientGUID)
		}
		ctx.ConnCryptoState.SetDialect(selectedDialect)
		ctx.ConnCryptoState.SetServerGUID(h.ServerGUID)
		ctx.ConnCryptoState.SetServerCapabilities(capabilities)
		ctx.ConnCryptoState.SetServerSecurityMode(securityMode)
		ctx.ConnCryptoState.SetClientGUID(guid)
		ctx.ConnCryptoState.SetClientCapabilities(types.Capabilities(clientCapabilities))
		ctx.ConnCryptoState.SetClientSecurityMode(types.SecurityMode(clientSecurityMode))
		ctx.ConnCryptoState.SetClientDialects(dialects)

		if is311 {
			ctx.ConnCryptoState.SetCipherId(selectedCipher)
			ctx.ConnCryptoState.SetPreauthIntegrityHashId(types.HashAlgSHA512)
		}
	}

	return NewResult(types.StatusSuccess, resp), nil
}

// selectDialect selects the highest dialect from the client's list that falls
// within the server's [MinDialect, MaxDialect] range. Also detects the wildcard.
// Returns (selectedDialect, hasWildcard). selectedDialect is 0 if no match.
func (h *Handler) selectDialect(clientDialects []types.Dialect) (types.Dialect, bool) {
	var selected types.Dialect
	var selectedPriority int
	hasWildcard := false

	minP := types.DialectPriority(h.MinDialect)
	maxP := types.DialectPriority(h.MaxDialect)

	for _, d := range clientDialects {
		if d == types.DialectWildcard {
			hasWildcard = true
			// Wildcard implies 2.0.2 baseline
			p := types.DialectPriority(types.Dialect0202)
			if p >= minP && p <= maxP && p > selectedPriority {
				selected = types.Dialect0202
				selectedPriority = p
			}
			continue
		}

		p := types.DialectPriority(d)
		if p == 0 {
			continue // Unknown dialect
		}

		// Only consider dialects within configured range
		if p < minP || p > maxP {
			continue
		}

		if p > selectedPriority {
			selected = d
			selectedPriority = p
		}
	}

	return selected, hasWildcard
}

// buildCapabilities returns the appropriate capabilities for the selected dialect.
func (h *Handler) buildCapabilities(dialect types.Dialect) types.Capabilities {
	switch dialect {
	case types.Dialect0202:
		// SMB 2.0.2: capabilities field is reserved, SHOULD be 0.
		return 0

	case types.Dialect0210:
		// SMB 2.1: CapLeasing | CapLargeMTU
		return types.CapLeasing | types.CapLargeMTU

	case types.Dialect0300, types.Dialect0302:
		// SMB 3.0/3.0.2: CapLeasing | CapLargeMTU | [CapDirectoryLeasing] | [CapEncryption]
		caps := types.CapLeasing | types.CapLargeMTU
		if h.DirectoryLeasingEnabled {
			caps |= types.CapDirectoryLeasing
		}
		if h.EncryptionEnabled {
			caps |= types.CapEncryption
		}
		return caps

	case types.Dialect0311:
		// SMB 3.1.1: CapLeasing | CapLargeMTU | [CapDirectoryLeasing]
		// Encryption is signaled via negotiate contexts, not capabilities field.
		caps := types.CapLeasing | types.CapLargeMTU
		if h.DirectoryLeasingEnabled {
			caps |= types.CapDirectoryLeasing
		}
		return caps

	default:
		return 0
	}
}

// processNegotiateContexts parses client negotiate contexts and builds response contexts.
// Only called for SMB 3.1.1 negotiation.
//
// Returns the response contexts and the selected cipher ID.
func (h *Handler) processNegotiateContexts(
	body []byte,
	contextOffset uint32,
	contextCount uint16,
) ([]types.NegotiateContext, uint16) {
	// Context offset is relative to the start of the SMB2 header (64 bytes before body).
	// Our body starts at header offset 64, so:
	//   bodyOffset = contextOffset - 64
	bodyOffset := int(contextOffset) - 64
	if bodyOffset < 0 || bodyOffset >= len(body) {
		logger.Debug("Negotiate context offset out of range",
			"offset", contextOffset, "bodyLen", len(body))
		return nil, 0
	}

	clientContexts, err := types.ParseNegotiateContextList(body[bodyOffset:], int(contextCount))
	if err != nil {
		logger.Debug("Failed to parse negotiate contexts", "error", err)
		return nil, 0
	}

	var responseContexts []types.NegotiateContext
	var selectedCipher uint16

	for _, nc := range clientContexts {
		switch nc.ContextType {
		case types.NegCtxPreauthIntegrity:
			preauth, err := types.DecodePreauthIntegrityCaps(nc.Data)
			if err != nil {
				logger.Debug("Failed to decode preauth integrity caps", "error", err)
				continue
			}

			// Verify client offers SHA-512
			if !slices.Contains(preauth.HashAlgorithms, types.HashAlgSHA512) {
				logger.Debug("Client does not offer SHA-512 for preauth integrity")
				continue
			}

			// Generate server salt (32 bytes of random data)
			serverSalt := make([]byte, 32)
			_, _ = rand.Read(serverSalt)

			// Build response: SHA-512 selected, server's random salt
			respPreauth := types.PreauthIntegrityCaps{
				HashAlgorithms: []uint16{types.HashAlgSHA512},
				Salt:           serverSalt,
			}
			responseContexts = append(responseContexts, types.NegotiateContext{
				ContextType: types.NegCtxPreauthIntegrity,
				Data:        respPreauth.Encode(),
			})

		case types.NegCtxEncryptionCaps:
			enc, err := types.DecodeEncryptionCaps(nc.Data)
			if err != nil {
				logger.Debug("Failed to decode encryption caps", "error", err)
				continue
			}

			// Select preferred cipher from client's list.
			// Server preference: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM
			selectedCipher = selectCipher(enc.Ciphers)
			if selectedCipher == 0 {
				logger.Debug("No mutually supported cipher found")
				continue
			}

			respEnc := types.EncryptionCaps{
				Ciphers: []uint16{selectedCipher},
			}
			responseContexts = append(responseContexts, types.NegotiateContext{
				ContextType: types.NegCtxEncryptionCaps,
				Data:        respEnc.Encode(),
			})

		case types.NegCtxNetnameContextID:
			netname, err := types.DecodeNetnameContext(nc.Data)
			if err != nil {
				logger.Debug("Failed to decode netname context", "error", err)
				continue
			}
			logger.Debug("Client netname", "netname", netname.NetName)
			// Server does not include netname in response

		default:
			logger.Debug("Skipping unrecognized negotiate context",
				"contextType", nc.ContextType)
		}
	}

	return responseContexts, selectedCipher
}

// selectCipher selects the server's preferred cipher from the client's offered list.
// Server preference order: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM
func selectCipher(clientCiphers []uint16) uint16 {
	for _, preferred := range []uint16{
		types.CipherAES128GCM,
		types.CipherAES128CCM,
		types.CipherAES256GCM,
		types.CipherAES256CCM,
	} {
		if slices.Contains(clientCiphers, preferred) {
			return preferred
		}
	}
	return 0
}
