package handlers

import (
	"encoding/binary"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
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

	// Find highest supported dialect (we only support 0x0202 for Phase 1)
	var selectedDialect types.Dialect
	for _, d := range dialects {
		dialect := types.Dialect(d)
		if dialect == types.SMB2Dialect0202 || dialect == types.SMB2DialectWild {
			selectedDialect = types.SMB2Dialect0202
			break
		}
	}

	if selectedDialect == 0 {
		return NewErrorResult(types.StatusNotSupported), nil
	}

	// Build response (65 bytes structure size)
	// Note: SecurityBuffer comes after, but we leave it empty for Phase 1 (anonymous auth)
	resp := make([]byte, 65)

	binary.LittleEndian.PutUint16(resp[0:2], 65)             // StructureSize
	resp[2] = 0                                               // SecurityMode (no signing required)
	resp[3] = 0                                               // Reserved
	binary.LittleEndian.PutUint16(resp[4:6], uint16(selectedDialect)) // DialectRevision
	binary.LittleEndian.PutUint16(resp[6:8], 0)               // NegotiateContextCount (SMB 3.1.1 only)
	copy(resp[8:24], h.ServerGUID[:])                         // ServerGuid
	binary.LittleEndian.PutUint32(resp[24:28], 0)             // Capabilities (none for Phase 1)
	binary.LittleEndian.PutUint32(resp[28:32], h.MaxTransactSize)
	binary.LittleEndian.PutUint32(resp[32:36], h.MaxReadSize)
	binary.LittleEndian.PutUint32(resp[36:40], h.MaxWriteSize)
	binary.LittleEndian.PutUint64(resp[40:48], types.TimeToFiletime(time.Now()))
	binary.LittleEndian.PutUint64(resp[48:56], types.TimeToFiletime(h.StartTime))
	binary.LittleEndian.PutUint16(resp[56:58], 0) // SecurityBufferOffset (no security buffer)
	binary.LittleEndian.PutUint16(resp[58:60], 0) // SecurityBufferLength
	binary.LittleEndian.PutUint32(resp[60:64], 0) // NegotiateContextOffset (SMB 3.1.1 only)

	return NewResult(types.StatusSuccess, resp), nil
}
