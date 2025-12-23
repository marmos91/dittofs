// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 SET_INFO command handler [MS-SMB2] 2.2.39, 2.2.40.
// SET_INFO modifies file, filesystem, or security information for an open file.
package handlers

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// SetInfoRequest represents an SMB2 SET_INFO request from a client [MS-SMB2] 2.2.39.
//
// SET_INFO modifies metadata about a file, directory, filesystem, or security
// descriptor. The type of modification depends on InfoType and FileInfoClass.
//
// **Wire format (32 bytes fixed + variable buffer):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 33 (includes 1 byte of buffer)
//	2       1     InfoType           Type of info: file (1), filesystem (2), security (3), quota (4)
//	3       1     FileInfoClass      Class of info within type
//	4       4     BufferLength       Length of buffer data
//	8       2     BufferOffset       Offset from header to buffer
//	10      2     Reserved           Reserved (must be 0)
//	12      4     AdditionalInfo     Additional info for security
//	16      16    FileId             SMB2 file identifier
//	32+     var   Buffer             Info data to set
//
// **Example:**
//
//	req := &SetInfoRequest{
//	    InfoType:      types.SMB2InfoTypeFile,
//	    FileInfoClass: FileBasicInformation,
//	    FileID:        fileID,
//	    Buffer:        basicInfoBytes,
//	}
type SetInfoRequest struct {
	// InfoType specifies what type of information to set.
	// Valid values:
	//   - 1 (SMB2_0_INFO_FILE): File/directory information
	//   - 2 (SMB2_0_INFO_FILESYSTEM): Filesystem information (usually read-only)
	//   - 3 (SMB2_0_INFO_SECURITY): Security information
	//   - 4 (SMB2_0_INFO_QUOTA): Quota information
	InfoType uint8

	// FileInfoClass specifies the specific information class within the type.
	// For InfoType=1 (file):
	//   - FileBasicInformation (4): Set timestamps and attributes
	//   - FileRenameInformation (10): Rename/move file
	//   - FileDispositionInformation (13): Mark for deletion
	//   - FileEndOfFileInformation (20): Set file size
	FileInfoClass uint8

	// BufferLength is the length of the buffer data.
	BufferLength uint32

	// BufferOffset is the offset to the buffer from the SMB2 header.
	BufferOffset uint16

	// AdditionalInfo contains additional info (for security operations).
	AdditionalInfo uint32

	// FileID is the SMB2 file identifier from CREATE response.
	FileID [16]byte

	// Buffer contains the information to set.
	// Format depends on InfoType and FileInfoClass.
	Buffer []byte
}

// SetInfoResponse represents an SMB2 SET_INFO response to a client [MS-SMB2] 2.2.40.
//
// SET_INFO response is minimal - just a status code with no additional data.
//
// **Wire format (2 bytes):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 2
type SetInfoResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method
}

// FileRenameInfo represents FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34.
//
// This structure is used to rename or move a file.
//
// **Wire format (variable):**
//
//	Offset  Size  Field              Description
//	0       1     ReplaceIfExists    Replace existing file if true
//	1       7     Reserved           Reserved
//	8       8     RootDirectory      Root directory handle (usually 0)
//	16      4     FileNameLength     Length of filename in bytes
//	20      var   FileName           New filename (UTF-16LE)
type FileRenameInfo struct {
	// ReplaceIfExists indicates whether to replace an existing file.
	ReplaceIfExists bool

	// FileName is the new name for the file.
	// May be a full path or just a filename.
	FileName string
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeSetInfoRequest parses an SMB2 SET_INFO request body [MS-SMB2] 2.2.39.
//
// **Parameters:**
//   - body: Request body starting after the SMB2 header (64 bytes)
//
// **Returns:**
//   - *SetInfoRequest: Parsed request structure
//   - error: Decoding error if body is malformed
//
// **Example:**
//
//	req, err := DecodeSetInfoRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
func DecodeSetInfoRequest(body []byte) (*SetInfoRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("SET_INFO request too short: %d bytes", len(body))
	}

	req := &SetInfoRequest{
		InfoType:       body[2],
		FileInfoClass:  body[3],
		BufferLength:   binary.LittleEndian.Uint32(body[4:8]),
		BufferOffset:   binary.LittleEndian.Uint16(body[8:10]),
		AdditionalInfo: binary.LittleEndian.Uint32(body[12:16]),
	}
	copy(req.FileID[:], body[16:32])

	// Extract buffer
	// BufferOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so: body offset = BufferOffset - 64
	// Typical BufferOffset is 96 (64 header + 32 fixed part), giving body offset 32
	bufferStart := int(req.BufferOffset) - 64
	if bufferStart < 32 {
		bufferStart = 32 // Buffer can't start before the fixed part ends
	}
	if bufferStart+int(req.BufferLength) <= len(body) {
		req.Buffer = body[bufferStart : bufferStart+int(req.BufferLength)]
	}

	return req, nil
}

// Encode serializes the SetInfoResponse into SMB2 wire format [MS-SMB2] 2.2.40.
//
// **Returns:**
//   - []byte: 2-byte response body
//   - error: Encoding error (currently always nil)
func (resp *SetInfoResponse) Encode() ([]byte, error) {
	buf := make([]byte, 2)
	binary.LittleEndian.PutUint16(buf[0:2], 2) // StructureSize
	return buf, nil
}

// DecodeFileRenameInfo parses FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34.
//
// **Parameters:**
//   - buffer: Raw buffer containing rename information
//
// **Returns:**
//   - *FileRenameInfo: Parsed rename information
//   - error: Decoding error if buffer is malformed
func DecodeFileRenameInfo(buffer []byte) (*FileRenameInfo, error) {
	if len(buffer) < 20 {
		return nil, fmt.Errorf("buffer too short for FILE_RENAME_INFORMATION: %d bytes", len(buffer))
	}

	info := &FileRenameInfo{
		ReplaceIfExists: buffer[0] != 0,
	}

	// Skip: Reserved (7 bytes at offset 1-7)
	// Skip: RootDirectory (8 bytes at offset 8-15)
	fileNameLength := binary.LittleEndian.Uint32(buffer[16:20])

	// FileName starts at offset 20
	if len(buffer) < 20+int(fileNameLength) {
		return nil, fmt.Errorf("buffer too short for filename: need %d, have %d", 20+fileNameLength, len(buffer))
	}

	if fileNameLength > 0 {
		info.FileName = decodeUTF16LE(buffer[20 : 20+fileNameLength])
	}

	return info, nil
}

// decodeEndOfFileInfo decodes FILE_END_OF_FILE_INFORMATION [MS-FSCC] 2.4.13.
func decodeEndOfFileInfo(buffer []byte) (uint64, error) {
	if len(buffer) < 8 {
		return 0, fmt.Errorf("buffer too short for FILE_END_OF_FILE_INFORMATION")
	}
	return binary.LittleEndian.Uint64(buffer[0:8]), nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// SetInfo handles SMB2 SET_INFO command [MS-SMB2] 2.2.39, 2.2.40.
//
// SET_INFO modifies metadata for an open file handle. This includes
// file timestamps, attributes, size, and rename operations.
//
// **Purpose:**
//
// The SET_INFO command allows clients to:
//   - Set file timestamps and attributes (FileBasicInformation)
//   - Rename or move files (FileRenameInformation)
//   - Mark files for deletion (FileDispositionInformation)
//   - Set file size (FileEndOfFileInformation)
//   - Modify security descriptors
//
// **Process:**
//
//  1. Decode and validate the request
//  2. Look up the open file by FileID
//  3. Get the metadata store for the share
//  4. Build authentication context
//  5. Based on InfoType and FileInfoClass:
//     - Apply the requested modification
//     - Update open file state if needed
//  6. Return success/error status
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request
//   - StatusInvalidHandle: Invalid FileID
//   - StatusBadNetworkName: Share not found
//   - StatusAccessDenied: Permission denied
//   - StatusObjectPathNotFound: Rename destination not found
//   - StatusNotSupported: Unsupported info class
//
// **Parameters:**
//   - ctx: SMB handler context with session information
//   - req: Parsed SET_INFO request
//
// **Returns:**
//   - *SetInfoResponse: Response (status only)
//   - error: Internal error (rare)
func (h *Handler) SetInfo(ctx *SMBHandlerContext, req *SetInfoRequest) (*SetInfoResponse, error) {
	logger.Debug("SET_INFO request",
		"infoType", req.InfoType,
		"fileInfoClass", req.FileInfoClass,
		"fileID", fmt.Sprintf("%x", req.FileID))

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("SET_INFO: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("SET_INFO: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadNetworkName}}, nil
	}

	// ========================================================================
	// Step 3: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("SET_INFO: failed to build auth context", "error", err)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 4: Handle set info based on type
	// ========================================================================

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		return h.setFileInfoFromStore(authCtx, metadataStore, openFile, types.FileInfoClass(req.FileInfoClass), req.Buffer)
	case types.SMB2InfoTypeSecurity:
		// Accept but ignore security updates for now
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
	default:
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// setFileInfoFromStore handles setting file information using metadata store.
func (h *Handler) setFileInfoFromStore(
	authCtx *metadata.AuthContext,
	metadataStore metadata.MetadataStore,
	openFile *OpenFile,
	class types.FileInfoClass,
	buffer []byte,
) (*SetInfoResponse, error) {
	switch class {
	case types.FileBasicInformation:
		// FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7 (40 bytes)
		if len(buffer) < 36 {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		basicInfo, err := DecodeFileBasicInfo(buffer)
		if err != nil {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		// Convert to SetAttrs
		setAttrs := SMBTimesToSetAttrs(basicInfo)

		// Apply changes
		err = metadataStore.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set basic info", "path", openFile.Path, "error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
		renameInfo, err := DecodeFileRenameInfo(buffer)
		if err != nil {
			logger.Debug("SET_INFO: failed to decode rename info", "error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		// Normalize path separators (Windows uses backslash, we use forward slash)
		newPath := strings.ReplaceAll(renameInfo.FileName, "\\", "/")
		newPath = strings.TrimPrefix(newPath, "/")

		// Determine source and destination
		// If newPath contains a directory separator, it's a move to a different directory
		// Otherwise, it's a rename within the same directory
		var toDir metadata.FileHandle
		var toName string

		if strings.Contains(newPath, "/") {
			// Move to different directory
			dirPath := path.Dir(newPath)
			toName = path.Base(newPath)

			// Get root handle for the share
			tree, ok := h.GetTree(openFile.TreeID)
			if !ok {
				logger.Debug("SET_INFO: invalid tree for rename", "treeID", openFile.TreeID)
				return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
			}

			rootHandle, err := h.Registry.GetRootHandle(tree.ShareName)
			if err != nil {
				logger.Debug("SET_INFO: failed to get root handle", "error", err)
				return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectPathNotFound}}, nil
			}

			// Walk to destination directory
			if dirPath == "." || dirPath == "" {
				toDir = rootHandle
			} else {
				toDir, err = h.walkPath(authCtx, metadataStore, rootHandle, dirPath)
				if err != nil {
					logger.Debug("SET_INFO: destination path not found", "path", dirPath, "error", err)
					return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectPathNotFound}}, nil
				}
			}
		} else {
			// Simple rename within same directory
			toDir = openFile.ParentHandle
			toName = newPath
		}

		// Validate we have source info
		if len(openFile.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot rename root directory", "path", openFile.Path)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
		}

		// Save old path info for notification before modification
		oldPath := openFile.Path
		oldFileName := openFile.FileName
		oldParentPath := GetParentPath(oldPath)

		// Perform the rename/move
		err = metadataStore.Move(authCtx, openFile.ParentHandle, openFile.FileName, toDir, toName)
		if err != nil {
			logger.Debug("SET_INFO: rename failed",
				"from", openFile.Path,
				"to", newPath,
				"error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

		// Notify watchers about the rename
		if h.NotifyRegistry != nil {
			// Get tree for share name
			tree, ok := h.GetTree(openFile.TreeID)
			if ok {
				// Notify old location
				h.NotifyRegistry.NotifyChange(tree.ShareName, oldParentPath, oldFileName, FileActionRenamedOldName)
				// Notify new location
				newParentPath := GetParentPath(newPath)
				if newParentPath == "" || newParentPath == "." {
					newParentPath = "/"
				}
				h.NotifyRegistry.NotifyChange(tree.ShareName, newParentPath, toName, FileActionRenamedNewName)
			}
		}

		// Update open file state
		if strings.Contains(newPath, "/") {
			openFile.Path = newPath
		} else {
			// Update just the filename part
			dir := path.Dir(openFile.Path)
			if dir == "." {
				openFile.Path = toName
			} else {
				openFile.Path = dir + "/" + toName
			}
		}
		openFile.FileName = toName
		openFile.ParentHandle = toDir
		h.StoreOpenFile(openFile)

		logger.Debug("SET_INFO: rename successful",
			"oldPath", openFile.Path,
			"newPath", newPath)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case types.FileDispositionInformation, types.FileDispositionInformationEx:
		// FILE_DISPOSITION_INFORMATION [MS-FSCC] 2.4.11
		// FILE_DISPOSITION_INFORMATION_EX [MS-FSCC] 2.4.11.2
		// DeletePending (1 byte for class 13, 4 bytes flags for class 64)
		if len(buffer) < 1 {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		var deletePending bool
		if class == types.FileDispositionInformationEx {
			// FileDispositionInformationEx uses a 4-byte Flags field
			// Bit 0 (FILE_DISPOSITION_DELETE) = delete on close
			if len(buffer) >= 4 {
				flags := binary.LittleEndian.Uint32(buffer[0:4])
				deletePending = (flags & 0x01) != 0
			} else {
				deletePending = buffer[0] != 0
			}
		} else {
			deletePending = buffer[0] != 0
		}

		// Validate we have parent info for deletion
		if deletePending && len(openFile.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot delete root directory", "path", openFile.Path)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
		}

		// Mark file for deletion on close
		openFile.DeletePending = deletePending
		h.StoreOpenFile(openFile)

		logger.Debug("SET_INFO: delete disposition set",
			"path", openFile.Path,
			"deletePending", deletePending,
			"class", class)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case types.FileEndOfFileInformation:
		// FILE_END_OF_FILE_INFORMATION [MS-FSCC] 2.4.13
		// Set end of file (truncate/extend)
		if len(buffer) < 8 {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		newSize, err := decodeEndOfFileInfo(buffer)
		if err != nil {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		setAttrs := &metadata.SetAttrs{
			Size: &newSize,
		}

		err = metadataStore.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set EOF", "path", openFile.Path, "error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case 19: // FileAllocationInformation [MS-FSCC] 2.4.4
		// Set allocation size - accept but treat as no-op (allocation handled automatically)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case 11: // FileLinkInformation - hard links not supported
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil

	default:
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil
	}
}
