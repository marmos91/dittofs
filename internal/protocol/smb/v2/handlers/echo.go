package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Echo handles SMB2 ECHO command [MS-SMB2] 2.2.28, 2.2.29
// This is a keep-alive/ping command
func (h *Handler) Echo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ECHO request has 4-byte structure (StructureSize + Reserved)
	// ECHO response has 4-byte structure (StructureSize + Reserved)

	// Build response (4 bytes)
	resp := make([]byte, 4)
	binary.LittleEndian.PutUint16(resp[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 0) // Reserved

	return NewResult(types.StatusSuccess, resp), nil
}
