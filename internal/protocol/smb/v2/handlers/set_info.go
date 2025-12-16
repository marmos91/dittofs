package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

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
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil
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

		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
		// TODO: Implement rename operation using metadataStore.Move()
		logger.Debug("SET_INFO: rename not implemented", "path", openFile.Path)
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case 13: // FileDispositionInformation [MS-FSCC] 2.4.11
		// TODO: Implement delete on close
		logger.Debug("SET_INFO: delete disposition not implemented", "path", openFile.Path)
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case 20: // FileEndOfFileInformation [MS-FSCC] 2.4.13
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

		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

	case 19: // FileAllocationInformation [MS-FSCC] 2.4.4
		// Set allocation size - accept but treat as no-op (allocation handled automatically)
		return NewResult(types.StatusSuccess, make([]byte, 0)), nil

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
