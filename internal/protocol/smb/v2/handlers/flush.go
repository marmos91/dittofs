package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Flush handles SMB2 FLUSH command [MS-SMB2] 2.2.17, 2.2.18
func (h *Handler) Flush(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 24 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.17
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 24
	// reserved1 := binary.LittleEndian.Uint16(body[2:4])
	// reserved2 := binary.LittleEndian.Uint32(body[4:8])
	var fileID [16]byte
	copy(fileID[:], body[8:24])

	// Validate file handle
	_, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// For Phase 1 with mock data, flush is a no-op
	// In a real implementation, this would flush data to the content store

	// Build response [MS-SMB2] 2.2.18 (4 bytes)
	resp := make([]byte, 4)
	binary.LittleEndian.PutUint16(resp[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 0) // Reserved

	return NewResult(types.StatusSuccess, resp), nil
}
