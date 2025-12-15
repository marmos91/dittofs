package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Logoff handles SMB2 LOGOFF command [MS-SMB2] 2.2.7, 2.2.8
func (h *Handler) Logoff(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 4 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Verify session exists
	_, ok := h.GetSession(ctx.SessionID)
	if !ok {
		return NewErrorResult(types.StatusUserSessionDeleted), nil
	}

	// Delete the session
	h.DeleteSession(ctx.SessionID)

	// Build response (4 bytes)
	resp := make([]byte, 4)
	binary.LittleEndian.PutUint16(resp[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 0) // Reserved

	return NewResult(types.StatusSuccess, resp), nil
}
