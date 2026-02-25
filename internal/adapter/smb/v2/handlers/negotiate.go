package handlers

import (
	"encoding/binary"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Negotiate handles SMB2 NEGOTIATE command [MS-SMB2] 2.2.3, 2.2.4
func (h *Handler) Negotiate(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 36 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 36
	dialectCount := binary.LittleEndian.Uint16(body[2:4])
	// securityMode := binary.LittleEndian.Uint16(body[4:6])
	// reserved := binary.LittleEndian.Uint16(body[6:8])
	// capabilities := binary.LittleEndian.Uint32(body[8:12])
	// clientGUID := body[12:28]
	// negotiateContextOffset := binary.LittleEndian.Uint32(body[28:32]) // SMB 3.1.1 only
	// negotiateContextCount := binary.LittleEndian.Uint16(body[32:34]) // SMB 3.1.1 only
	// reserved2 := binary.LittleEndian.Uint16(body[34:36])

	// Parse dialect list (starts at offset 36)
	dialectOffset := 36
	var dialects []uint16
	for i := uint16(0); i < dialectCount && dialectOffset+2 <= len(body); i++ {
		dialect := binary.LittleEndian.Uint16(body[dialectOffset:])
		dialects = append(dialects, dialect)
		dialectOffset += 2
	}

	logger.Debug("SMB2 NEGOTIATE request",
		"dialectCount", dialectCount,
		"bodyLen", len(body))

	// Find highest supported dialect
	// We support SMB 2.0.2 and SMB 2.1 for broader compatibility
	var selectedDialect types.Dialect
	for _, d := range dialects {
		dialect := types.Dialect(d)
		switch dialect {
		case types.SMB2Dialect0210:
			// SMB 2.1 is our highest supported dialect
			if selectedDialect < types.SMB2Dialect0210 {
				selectedDialect = types.SMB2Dialect0210
			}
		case types.SMB2Dialect0202, types.SMB2DialectWild:
			// SMB 2.0.2 is our baseline; wildcard maps to lowest supported
			if selectedDialect < types.SMB2Dialect0202 {
				selectedDialect = types.SMB2Dialect0202
			}
		}
	}

	logger.Debug("SMB2 NEGOTIATE dialect selection",
		"dialect", selectedDialect.String(),
		"supported", selectedDialect != 0)

	if selectedDialect == 0 {
		return NewErrorResult(types.StatusNotSupported), nil
	}

	// Build response (65 bytes structure size)
	// Note: SecurityBuffer comes after, but we leave it empty for Phase 1 (anonymous auth)
	resp := make([]byte, 65)

	// Set SecurityMode based on signing configuration [MS-SMB2 2.2.4]
	// Bit 0 (0x01): SMB2_NEGOTIATE_SIGNING_ENABLED
	// Bit 1 (0x02): SMB2_NEGOTIATE_SIGNING_REQUIRED
	var securityMode byte
	if h.SigningConfig.Enabled {
		securityMode |= 0x01 // SMB2_NEGOTIATE_SIGNING_ENABLED
	}
	if h.SigningConfig.Required {
		securityMode |= 0x02 // SMB2_NEGOTIATE_SIGNING_REQUIRED
	}

	binary.LittleEndian.PutUint16(resp[0:2], 65)                      // StructureSize
	resp[2] = securityMode                                            // SecurityMode
	resp[3] = 0                                                       // Reserved
	binary.LittleEndian.PutUint16(resp[4:6], uint16(selectedDialect)) // DialectRevision
	binary.LittleEndian.PutUint16(resp[6:8], 0)                       // NegotiateContextCount (SMB 3.1.1 only)
	copy(resp[8:24], h.ServerGUID[:])                                 // ServerGuid
	binary.LittleEndian.PutUint32(resp[24:28], 0)                     // Capabilities (none for Phase 1)
	binary.LittleEndian.PutUint32(resp[28:32], h.MaxTransactSize)
	binary.LittleEndian.PutUint32(resp[32:36], h.MaxReadSize)
	binary.LittleEndian.PutUint32(resp[36:40], h.MaxWriteSize)
	binary.LittleEndian.PutUint64(resp[40:48], types.TimeToFiletime(time.Now()))
	binary.LittleEndian.PutUint64(resp[48:56], types.TimeToFiletime(h.StartTime))
	// SecurityBufferOffset: offset from start of SMB2 header to security buffer
	// This is SMB2_HDR_BODY (64) + fixed_body_size (64) = 128 (0x80)
	// Even when the buffer is empty, the offset must be correct per MS-SMB2.
	binary.LittleEndian.PutUint16(resp[56:58], 128) // SecurityBufferOffset (points past fixed body)
	binary.LittleEndian.PutUint16(resp[58:60], 0)   // SecurityBufferLength (no security blob for Phase 1)
	binary.LittleEndian.PutUint32(resp[60:64], 0)   // NegotiateContextOffset (SMB 3.1.1 only)

	return NewResult(types.StatusSuccess, resp), nil
}
