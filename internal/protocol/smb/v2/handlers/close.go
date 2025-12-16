package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Close handles SMB2 CLOSE command [MS-SMB2] 2.2.15, 2.2.16
func (h *Handler) Close(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeCloseRequest(body)
	if err != nil {
		logger.Debug("CLOSE: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("CLOSE request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"flags", req.Flags)

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("CLOSE: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Build response with optional attributes
	// ========================================================================

	resp := &CloseResponse{
		Flags: req.Flags,
	}

	// If SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB was set, return file attributes
	if types.CloseFlags(req.Flags)&types.SMB2ClosePostQueryAttrib != 0 {
		// Get metadata store to retrieve final attributes
		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err == nil {
			file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
			if err == nil {
				creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
				allocationSize := ((file.Size + 4095) / 4096) * 4096

				resp.CreationTime = creation
				resp.LastAccessTime = access
				resp.LastWriteTime = write
				resp.ChangeTime = change
				resp.AllocationSize = allocationSize
				resp.EndOfFile = file.Size
				resp.FileAttributes = FileAttrToSMBAttributes(&file.FileAttr)
			}
		}
	}

	// ========================================================================
	// Step 4: Remove the open file handle
	// ========================================================================

	h.DeleteOpenFile(req.FileID)

	logger.Debug("CLOSE successful",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"path", openFile.Path)

	// ========================================================================
	// Step 5: Encode response
	// ========================================================================

	respBytes, err := EncodeCloseResponse(resp)
	if err != nil {
		logger.Warn("CLOSE: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}
