package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TreeDisconnect handles SMB2 TREE_DISCONNECT command [MS-SMB2] 2.2.11, 2.2.12.
//
// **Purpose:**
//
// TREE_DISCONNECT terminates a tree connection (share mapping). All open files
// associated with the tree connection are closed and their resources freed.
//
// **Process:**
//
//  1. Verify tree connection exists
//  2. Close all open files for this tree (releases locks, flushes caches)
//  3. Delete the tree connection
//  4. Return success response
//
// **Parameters:**
//   - ctx: SMB handler context with tree information
//   - body: Raw request body (4 bytes minimum)
//
// **Returns:**
//   - *HandlerResult: Success or error result
//   - error: Internal error (rare)
func (h *Handler) TreeDisconnect(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 4 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// ========================================================================
	// Step 1: Verify tree connection exists
	// ========================================================================

	_, ok := h.GetTree(ctx.TreeID)
	if !ok {
		return NewErrorResult(types.StatusNetworkNameDeleted), nil
	}

	// ========================================================================
	// Step 2: Close all open files for this tree
	// ========================================================================
	//
	// CloseAllFilesForTree handles:
	// - Releasing byte-range locks
	// - Flushing cached data
	// - Closing pipe handles
	// - Removing file handles

	filesClosed := h.CloseAllFilesForTree(ctx.Context, ctx.TreeID, ctx.SessionID)
	if filesClosed > 0 {
		logger.Debug("TREE_DISCONNECT: closed files",
			"treeID", ctx.TreeID,
			"sessionID", ctx.SessionID,
			"count", filesClosed)
	}

	// ========================================================================
	// Step 3: Delete the tree connection
	// ========================================================================

	h.DeleteTree(ctx.TreeID)

	// ========================================================================
	// Step 4: Build and return success response
	// ========================================================================

	resp := make([]byte, 4)
	binary.LittleEndian.PutUint16(resp[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 0) // Reserved

	return NewResult(types.StatusSuccess, resp), nil
}
