package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// TreeDisconnect handles SMB2 TREE_DISCONNECT command [MS-SMB2] 2.2.11, 2.2.12
func (h *Handler) TreeDisconnect(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 4 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Verify tree connection exists
	_, ok := h.GetTree(ctx.TreeID)
	if !ok {
		return NewErrorResult(types.StatusNetworkNameDeleted), nil
	}

	// Delete the tree connection
	h.DeleteTree(ctx.TreeID)

	// Build response (4 bytes)
	resp := make([]byte, 4)
	binary.LittleEndian.PutUint16(resp[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 0) // Reserved

	return NewResult(types.StatusSuccess, resp), nil
}
