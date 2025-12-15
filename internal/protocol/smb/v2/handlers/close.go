package handlers

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Close handles SMB2 CLOSE command [MS-SMB2] 2.2.15, 2.2.16
func (h *Handler) Close(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 24 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.15
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 24
	flags := binary.LittleEndian.Uint16(body[2:4])
	// reserved := binary.LittleEndian.Uint32(body[4:8])
	var fileID [16]byte
	copy(fileID[:], body[8:24])

	// Validate and get file handle
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Delete the file handle
	h.DeleteOpenFile(fileID)

	// Build response [MS-SMB2] 2.2.16 (60 bytes)
	resp := make([]byte, 60)
	binary.LittleEndian.PutUint16(resp[0:2], 60) // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], flags)

	// If SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB was set, return file attributes
	if flags&types.SMB2ClosePostQueryAttrib != 0 {
		mockFile := h.GetMockFile(openFile.ShareName, openFile.Path)
		if mockFile != nil {
			binary.LittleEndian.PutUint64(resp[8:16], types.TimeToFiletime(mockFile.Created))   // CreationTime
			binary.LittleEndian.PutUint64(resp[16:24], types.TimeToFiletime(mockFile.Accessed)) // LastAccessTime
			binary.LittleEndian.PutUint64(resp[24:32], types.TimeToFiletime(mockFile.Modified)) // LastWriteTime
			binary.LittleEndian.PutUint64(resp[32:40], types.TimeToFiletime(mockFile.Modified)) // ChangeTime
			binary.LittleEndian.PutUint64(resp[40:48], uint64(mockFile.Size))                   // AllocationSize
			binary.LittleEndian.PutUint64(resp[48:56], uint64(mockFile.Size))                   // EndOfFile
			binary.LittleEndian.PutUint32(resp[56:60], mockFile.Attributes)                     // FileAttributes
		}
	}

	return NewResult(types.StatusSuccess, resp), nil
}
