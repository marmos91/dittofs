package handlers

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Create handles SMB2 CREATE command [MS-SMB2] 2.2.13, 2.2.14
func (h *Handler) Create(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 57 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.13
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 57
	// securityFlags := body[2] // Reserved
	// requestedOplockLevel := body[3]
	// impersonationLevel := binary.LittleEndian.Uint32(body[4:8])
	// smbCreateFlags := binary.LittleEndian.Uint64(body[8:16])
	// reserved := binary.LittleEndian.Uint64(body[16:24])
	desiredAccess := binary.LittleEndian.Uint32(body[24:28])
	fileAttributes := binary.LittleEndian.Uint32(body[28:32])
	// shareAccess := binary.LittleEndian.Uint32(body[32:36])
	createDisposition := binary.LittleEndian.Uint32(body[36:40])
	createOptions := binary.LittleEndian.Uint32(body[40:44])
	nameOffset := binary.LittleEndian.Uint16(body[44:46])
	nameLength := binary.LittleEndian.Uint16(body[46:48])
	// createContextsOffset := binary.LittleEndian.Uint32(body[48:52])
	// createContextsLength := binary.LittleEndian.Uint32(body[52:56])

	// Extract filename (UTF-16LE encoded)
	// nameOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so adjust
	adjustedOffset := int(nameOffset) - 64 - 56 // header size + fixed request size before variable data

	var filename string
	if nameLength > 0 {
		// Try multiple offset strategies
		if adjustedOffset >= 0 && adjustedOffset+int(nameLength) <= len(body) {
			filename = decodeUTF16LE(body[adjustedOffset : adjustedOffset+int(nameLength)])
		} else {
			// Name might be right after the structure
			startOffset := 56 // After the 56-byte fixed part
			if len(body) >= startOffset+int(nameLength) {
				filename = decodeUTF16LE(body[startOffset : startOffset+int(nameLength)])
			}
		}
	}

	// Get tree connection for share name
	tree, ok := h.GetTree(ctx.TreeID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Check if requesting directory
	isDirectoryRequest := createOptions&types.FileDirectoryFile != 0

	// Look up mock file
	mockFile := h.GetMockFile(tree.ShareName, filename)

	// Handle create disposition
	var createAction uint32
	switch createDisposition {
	case types.FileOpen:
		// Open existing only
		if mockFile == nil {
			return NewErrorResult(types.StatusObjectNameNotFound), nil
		}
		createAction = types.FileOpened

	case types.FileCreate:
		// Create new only (fail if exists)
		if mockFile != nil {
			return NewErrorResult(types.StatusObjectNameCollision), nil
		}
		// Create temporary mock file
		attrs := types.FileAttributeNormal
		if isDirectoryRequest {
			attrs = types.FileAttributeDirectory
		}
		if fileAttributes != 0 {
			attrs = fileAttributes
		}
		mockFile = &MockFile{
			Name:       filename,
			IsDir:      isDirectoryRequest,
			Size:       0,
			Created:    time.Now(),
			Modified:   time.Now(),
			Accessed:   time.Now(),
			Attributes: attrs,
		}
		createAction = types.FileCreated

	case types.FileOpenIf:
		// Open or create
		if mockFile == nil {
			attrs := types.FileAttributeNormal
			if isDirectoryRequest {
				attrs = types.FileAttributeDirectory
			}
			if fileAttributes != 0 {
				attrs = fileAttributes
			}
			mockFile = &MockFile{
				Name:       filename,
				IsDir:      isDirectoryRequest,
				Size:       0,
				Created:    time.Now(),
				Modified:   time.Now(),
				Accessed:   time.Now(),
				Attributes: attrs,
			}
			createAction = types.FileCreated
		} else {
			createAction = types.FileOpened
		}

	case types.FileSupersede:
		// Replace if exists, create if not
		if mockFile != nil {
			createAction = types.FileSuperseded
		} else {
			createAction = types.FileCreated
		}
		attrs := types.FileAttributeNormal
		if isDirectoryRequest {
			attrs = types.FileAttributeDirectory
		}
		mockFile = &MockFile{
			Name:       filename,
			IsDir:      isDirectoryRequest,
			Size:       0,
			Created:    time.Now(),
			Modified:   time.Now(),
			Accessed:   time.Now(),
			Attributes: attrs,
		}

	case types.FileOverwrite:
		// Open and overwrite (fail if not exists)
		if mockFile == nil {
			return NewErrorResult(types.StatusObjectNameNotFound), nil
		}
		mockFile.Size = 0
		mockFile.Content = nil
		mockFile.Modified = time.Now()
		createAction = types.FileOverwritten

	case types.FileOverwriteIf:
		// Overwrite or create
		if mockFile == nil {
			attrs := types.FileAttributeNormal
			if isDirectoryRequest {
				attrs = types.FileAttributeDirectory
			}
			mockFile = &MockFile{
				Name:       filename,
				IsDir:      isDirectoryRequest,
				Size:       0,
				Created:    time.Now(),
				Modified:   time.Now(),
				Accessed:   time.Now(),
				Attributes: attrs,
			}
			createAction = types.FileCreated
		} else {
			mockFile.Size = 0
			mockFile.Content = nil
			mockFile.Modified = time.Now()
			createAction = types.FileOverwritten
		}

	default:
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Validate directory vs file request
	if isDirectoryRequest && mockFile != nil && !mockFile.IsDir {
		return NewErrorResult(types.StatusNotADirectory), nil
	}
	if createOptions&types.FileNonDirectoryFile != 0 && mockFile != nil && mockFile.IsDir {
		return NewErrorResult(types.StatusFileIsADirectory), nil
	}

	// Generate FileID
	fileID := h.GenerateFileID()
	logger.Debug("CREATE generated FileID",
		"fileID", fmt.Sprintf("%x", fileID),
		"filename", filename,
		"shareName", tree.ShareName)

	// Store open file
	openFile := &OpenFile{
		FileID:        fileID,
		TreeID:        ctx.TreeID,
		SessionID:     ctx.SessionID,
		Path:          filename,
		ShareName:     tree.ShareName,
		OpenTime:      time.Now(),
		DesiredAccess: desiredAccess,
		IsDirectory:   mockFile.IsDir,
	}
	h.StoreOpenFile(openFile)

	// Build response [MS-SMB2] 2.2.14 (89 bytes)
	resp := make([]byte, 89)
	binary.LittleEndian.PutUint16(resp[0:2], 89) // StructureSize
	resp[2] = 0                                   // OplockLevel (none)
	resp[3] = 0                                   // Flags
	binary.LittleEndian.PutUint32(resp[4:8], createAction)
	binary.LittleEndian.PutUint64(resp[8:16], types.TimeToFiletime(mockFile.Created))   // CreationTime
	binary.LittleEndian.PutUint64(resp[16:24], types.TimeToFiletime(mockFile.Accessed)) // LastAccessTime
	binary.LittleEndian.PutUint64(resp[24:32], types.TimeToFiletime(mockFile.Modified)) // LastWriteTime
	binary.LittleEndian.PutUint64(resp[32:40], types.TimeToFiletime(mockFile.Modified)) // ChangeTime
	binary.LittleEndian.PutUint64(resp[40:48], uint64(mockFile.Size))                   // AllocationSize
	binary.LittleEndian.PutUint64(resp[48:56], uint64(mockFile.Size))                   // EndOfFile
	binary.LittleEndian.PutUint32(resp[56:60], mockFile.Attributes)                     // FileAttributes
	binary.LittleEndian.PutUint32(resp[60:64], 0)                                       // Reserved2
	copy(resp[64:80], fileID[:])                                                        // FileId (persistent + volatile)
	binary.LittleEndian.PutUint32(resp[80:84], 0)                                       // CreateContextsOffset
	binary.LittleEndian.PutUint32(resp[84:88], 0)                                       // CreateContextsLength

	return NewResult(types.StatusSuccess, resp), nil
}
