package handlers

import (
	"fmt"
	"path"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// setInfoSuccessResponse returns the standard SET_INFO success response
func setInfoSuccessResponse() (*HandlerResult, error) {
	resp, _ := EncodeSetInfoResponse(&SetInfoResponse{})
	return NewResult(types.StatusSuccess, resp), nil
}

// SetInfo handles SMB2 SET_INFO command [MS-SMB2] 2.2.39, 2.2.40
func (h *Handler) SetInfo(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeSetInfoRequest(body)
	if err != nil {
		logger.Debug("SET_INFO: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("SET_INFO request",
		"infoType", req.InfoType,
		"fileInfoClass", req.FileInfoClass,
		"fileID", fmt.Sprintf("%x", req.FileID))

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("SET_INFO: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Get metadata store
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("SET_INFO: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// ========================================================================
	// Step 4: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("SET_INFO: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// ========================================================================
	// Step 5: Handle set info based on type
	// ========================================================================

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		return h.setFileInfoFromStore(authCtx, metadataStore, openFile, types.FileInfoClass(req.FileInfoClass), req.Buffer)
	case types.SMB2InfoTypeSecurity:
		// Accept but ignore security updates for now
		return setInfoSuccessResponse()
	default:
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
}

// setFileInfoFromStore handles setting file information using metadata store
func (h *Handler) setFileInfoFromStore(
	authCtx *metadata.AuthContext,
	metadataStore metadata.MetadataStore,
	openFile *OpenFile,
	class types.FileInfoClass,
	buffer []byte,
) (*HandlerResult, error) {
	switch class {
	case types.FileBasicInformation:
		// FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7 (40 bytes)
		if len(buffer) < 36 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}

		basicInfo, err := DecodeFileBasicInfo(buffer)
		if err != nil {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}

		// Convert to SetAttrs
		setAttrs := SMBTimesToSetAttrs(basicInfo)

		// Apply changes
		err = metadataStore.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set basic info", "path", openFile.Path, "error", err)
			return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
		}

		return setInfoSuccessResponse()

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
		renameInfo, err := DecodeFileRenameInfo(buffer)
		if err != nil {
			logger.Debug("SET_INFO: failed to decode rename info", "error", err)
			return NewErrorResult(types.StatusInvalidParameter), nil
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
				return NewErrorResult(types.StatusInvalidHandle), nil
			}

			rootHandle, err := h.Registry.GetRootHandle(tree.ShareName)
			if err != nil {
				logger.Debug("SET_INFO: failed to get root handle", "error", err)
				return NewErrorResult(types.StatusObjectPathNotFound), nil
			}

			// Walk to destination directory
			if dirPath == "." || dirPath == "" {
				toDir = rootHandle
			} else {
				toDir, err = h.walkPath(authCtx, metadataStore, rootHandle, dirPath)
				if err != nil {
					logger.Debug("SET_INFO: destination path not found", "path", dirPath, "error", err)
					return NewErrorResult(types.StatusObjectPathNotFound), nil
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
			return NewErrorResult(types.StatusAccessDenied), nil
		}

		// Perform the rename/move
		err = metadataStore.Move(authCtx, openFile.ParentHandle, openFile.FileName, toDir, toName)
		if err != nil {
			logger.Debug("SET_INFO: rename failed",
				"from", openFile.Path,
				"to", newPath,
				"error", err)
			return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
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
		return setInfoSuccessResponse()

	case types.FileDispositionInformation, types.FileDispositionInformationEx:
		// FILE_DISPOSITION_INFORMATION [MS-FSCC] 2.4.11
		// FILE_DISPOSITION_INFORMATION_EX [MS-FSCC] 2.4.11.2
		// DeletePending (1 byte for class 13, 4 bytes flags for class 64)
		if len(buffer) < 1 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}

		var deletePending bool
		if class == types.FileDispositionInformationEx {
			// FileDispositionInformationEx uses a 4-byte Flags field
			// Bit 0 (FILE_DISPOSITION_DELETE) = delete on close
			if len(buffer) >= 4 {
				flags := uint32(buffer[0]) | uint32(buffer[1])<<8 | uint32(buffer[2])<<16 | uint32(buffer[3])<<24
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
			return NewErrorResult(types.StatusAccessDenied), nil
		}

		// Mark file for deletion on close
		openFile.DeletePending = deletePending
		h.StoreOpenFile(openFile)

		logger.Debug("SET_INFO: delete disposition set",
			"path", openFile.Path,
			"deletePending", deletePending,
			"class", class)
		return setInfoSuccessResponse()

	case types.FileEndOfFileInformation:
		// FILE_END_OF_FILE_INFORMATION [MS-FSCC] 2.4.13
		// Set end of file (truncate/extend)
		if len(buffer) < 8 {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}

		newSize, err := decodeEndOfFileInfo(buffer)
		if err != nil {
			return NewErrorResult(types.StatusInvalidParameter), nil
		}

		setAttrs := &metadata.SetAttrs{
			Size: &newSize,
		}

		err = metadataStore.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set EOF", "path", openFile.Path, "error", err)
			return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
		}

		return setInfoSuccessResponse()

	case 19: // FileAllocationInformation [MS-FSCC] 2.4.4
		// Set allocation size - accept but treat as no-op (allocation handled automatically)
		return setInfoSuccessResponse()

	case 11: // FileLinkInformation - hard links not supported
		return NewErrorResult(types.StatusNotSupported), nil

	default:
		return NewErrorResult(types.StatusNotSupported), nil
	}
}

// decodeEndOfFileInfo decodes FILE_END_OF_FILE_INFORMATION
func decodeEndOfFileInfo(buffer []byte) (uint64, error) {
	if len(buffer) < 8 {
		return 0, fmt.Errorf("buffer too short")
	}
	return uint64(buffer[0]) | uint64(buffer[1])<<8 | uint64(buffer[2])<<16 | uint64(buffer[3])<<24 |
		uint64(buffer[4])<<32 | uint64(buffer[5])<<40 | uint64(buffer[6])<<48 | uint64(buffer[7])<<56, nil
}
