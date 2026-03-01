package handlers

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// SetInfoRequest represents an SMB2 SET_INFO request from a client [MS-SMB2] 2.2.39.
// SET_INFO modifies metadata about a file, directory, filesystem, or security
// descriptor. The type of modification depends on InfoType and FileInfoClass.
// The fixed wire format is 32 bytes plus a variable-length buffer.
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
// The response is minimal -- a 2-byte structure with only a status code.
type SetInfoResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method
}

// FileRenameInfo represents FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34.
// Used to rename or move a file.
type FileRenameInfo struct {
	// ReplaceIfExists indicates whether to replace an existing file.
	ReplaceIfExists bool

	// RootDirectory is the file handle of the destination directory.
	// Per MS-SMB2 2.2.39: If zero, FileName is a full path from the share root.
	// If non-zero, FileName is relative to this directory handle.
	RootDirectory [8]byte

	// FileName is the new name for the file.
	// When RootDirectory is zero, this is a full path from the share root.
	// When RootDirectory is non-zero, this is relative to that directory.
	FileName string
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeSetInfoRequest parses an SMB2 SET_INFO request body [MS-SMB2] 2.2.39.
// Returns an error if the body is less than 32 bytes.
func DecodeSetInfoRequest(body []byte) (*SetInfoRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("SET_INFO request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	r.Skip(2) // StructureSize
	infoType := r.ReadUint8()
	fileInfoClass := r.ReadUint8()
	bufferLength := r.ReadUint32()
	bufferOffset := r.ReadUint16()
	r.Skip(2) // Reserved
	additionalInfo := r.ReadUint32()
	fileID := r.ReadBytes(16)
	if r.Err() != nil {
		return nil, fmt.Errorf("SET_INFO parse error: %w", r.Err())
	}

	req := &SetInfoRequest{
		InfoType:       infoType,
		FileInfoClass:  fileInfoClass,
		BufferLength:   bufferLength,
		BufferOffset:   bufferOffset,
		AdditionalInfo: additionalInfo,
	}
	copy(req.FileID[:], fileID)

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
func (resp *SetInfoResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(2)
	w.WriteUint16(2) // StructureSize
	return w.Bytes(), w.Err()
}

// DecodeFileRenameInfo parses FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34.
// Returns an error if the buffer is less than 20 bytes.
func DecodeFileRenameInfo(buffer []byte) (*FileRenameInfo, error) {
	if len(buffer) < 20 {
		return nil, fmt.Errorf("buffer too short for FILE_RENAME_INFORMATION: %d bytes", len(buffer))
	}

	info := &FileRenameInfo{
		ReplaceIfExists: buffer[0] != 0,
	}

	// Reserved (7 bytes at offset 1-7) - skip
	// RootDirectory (8 bytes at offset 8-15) - extract
	copy(info.RootDirectory[:], buffer[8:16])

	renameR := smbenc.NewReader(buffer[16:20])
	fileNameLength := renameR.ReadUint32()

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
	r := smbenc.NewReader(buffer)
	return r.ReadUint64(), r.Err()
}

// ============================================================================
// Protocol Handler
// ============================================================================

// SetInfo handles SMB2 SET_INFO command [MS-SMB2] 2.2.39, 2.2.40.
//
// SET_INFO modifies metadata for an open file handle including timestamps,
// attributes, file size, rename/move operations, delete-on-close disposition,
// and security descriptors. Dispatches to file or security info handlers
// based on InfoType.
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
	// Step 2: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("SET_INFO: failed to build auth context", "error", err)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 3: Handle set info based on type
	// ========================================================================

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		return h.setFileInfoFromStore(authCtx, openFile, types.FileInfoClass(req.FileInfoClass), req.Buffer)
	case types.SMB2InfoTypeSecurity:
		return h.setSecurityInfo(authCtx, openFile, req.AdditionalInfo, req.Buffer)
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
	openFile *OpenFile,
	class types.FileInfoClass,
	buffer []byte,
) (*SetInfoResponse, error) {
	switch class {
	case types.FileBasicInformation:
		// FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7 (40 bytes)
		// Per MS-FSCC, the structure is exactly 40 bytes. If the buffer is smaller,
		// the server MUST return STATUS_INFO_LENGTH_MISMATCH.
		if len(buffer) < 40 {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInfoLengthMismatch}}, nil
		}

		// Validate attribute constraints per MS-FSCC 2.4.7:
		// - FILE_ATTRIBUTE_DIRECTORY on a non-directory file -> INVALID_PARAMETER
		// - FILE_ATTRIBUTE_TEMPORARY on a directory -> INVALID_PARAMETER
		attrR := smbenc.NewReader(buffer[32:36])
		fileAttrs := types.FileAttributes(attrR.ReadUint32())
		if fileAttrs != 0 {
			if fileAttrs&types.FileAttributeDirectory != 0 && !openFile.IsDirectory {
				return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
			}
			if fileAttrs&types.FileAttributeTemporary != 0 && openFile.IsDirectory {
				return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
			}
		}

		// Decode directly from raw buffer to handle FILETIME sentinels (0, -1, -2)
		setAttrs := DecodeBasicInfoToSetAttrs(buffer)

		// Per MS-FSA 2.1.5.14.2: Handle timestamp freeze/unfreeze sentinels.
		// -1 (0xFFFFFFFFFFFFFFFF): Freeze timestamp -- suppress auto-updates on subsequent operations.
		// -2 (0xFFFFFFFFFFFFFFFE): Unfreeze timestamp -- re-enable auto-updates.
		// We capture the current timestamp value BEFORE applying changes so the frozen
		// value reflects the state at freeze time.
		metaSvc := h.Registry.GetMetadataService()

		// Extract sentinel values from raw buffer
		ftR := smbenc.NewReader(buffer)
		creationFT := ftR.ReadUint64()
		atimeFT := ftR.ReadUint64()
		mtimeFT := ftR.ReadUint64()
		ctimeFT := ftR.ReadUint64()

		logger.Debug("SET_INFO: FileBasicInformation raw FILETIME values",
			"path", openFile.Path,
			"creationFT", fmt.Sprintf("0x%016X", creationFT),
			"atimeFT", fmt.Sprintf("0x%016X", atimeFT),
			"mtimeFT", fmt.Sprintf("0x%016X", mtimeFT),
			"ctimeFT", fmt.Sprintf("0x%016X", ctimeFT))

		// Note: CreationTime freeze/unfreeze sentinels are detected but not acted upon.
		// MS-FSA does not require CreationTime auto-update suppression because
		// CreationTime is never auto-updated by the server after file creation.
		hasFreezeOrUnfreeze := atimeFT == 0xFFFFFFFFFFFFFFFF || atimeFT == 0xFFFFFFFFFFFFFFFE ||
			mtimeFT == 0xFFFFFFFFFFFFFFFF || mtimeFT == 0xFFFFFFFFFFFFFFFE ||
			ctimeFT == 0xFFFFFFFFFFFFFFFF || ctimeFT == 0xFFFFFFFFFFFFFFFE

		// Apply changes first (may auto-update Ctime if modified == true)
		if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs); err != nil {
			logger.Debug("SET_INFO: failed to set basic info", "path", openFile.Path, "error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

		// Read file state AFTER applying changes to capture the post-SET_INFO values.
		// Per MS-FSA 2.1.5.14.2, the frozen value is the current value of the timestamp
		// AFTER any other changes in this SET_INFO call take effect. For example, if
		// FileAttributes change in the same call, Ctime is auto-updated -- the frozen
		// value must reflect that auto-updated value, not the pre-change value.
		var postFile *metadata.File
		if hasFreezeOrUnfreeze {
			var err error
			postFile, err = metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
			if err != nil {
				logger.Warn("SET_INFO: failed to read file for freeze/unfreeze", "path", openFile.Path, "error", err)
			}
		}

		// Apply freeze/unfreeze state to the open handle
		if postFile != nil {
			needsUpdate := false

			// LastWriteTime (Mtime) - offset 16
			switch mtimeFT {
			case 0xFFFFFFFFFFFFFFFF:
				openFile.MtimeFrozen = true
				openFile.FrozenMtime = &postFile.Mtime
				needsUpdate = true
				logger.Debug("SET_INFO: froze LastWriteTime", "path", openFile.Path, "value", postFile.Mtime)
			case 0xFFFFFFFFFFFFFFFE:
				openFile.MtimeFrozen = false
				openFile.FrozenMtime = nil
				needsUpdate = true
			}

			// ChangeTime (Ctime) - offset 24
			switch ctimeFT {
			case 0xFFFFFFFFFFFFFFFF:
				openFile.CtimeFrozen = true
				openFile.FrozenCtime = &postFile.Ctime
				needsUpdate = true
				logger.Debug("SET_INFO: froze ChangeTime", "path", openFile.Path, "value", postFile.Ctime)
			case 0xFFFFFFFFFFFFFFFE:
				openFile.CtimeFrozen = false
				openFile.FrozenCtime = nil
				needsUpdate = true
			}

			// LastAccessTime (Atime) - offset 8
			switch atimeFT {
			case 0xFFFFFFFFFFFFFFFF:
				openFile.AtimeFrozen = true
				openFile.FrozenAtime = &postFile.Atime
				needsUpdate = true
			case 0xFFFFFFFFFFFFFFFE:
				openFile.AtimeFrozen = false
				openFile.FrozenAtime = nil
				needsUpdate = true
			}

			if needsUpdate {
				h.StoreOpenFile(openFile)
			}
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

		// Determine source and destination.
		//
		// Per MS-FSCC 2.4.34 / MS-SMB2 2.2.39:
		// - If RootDirectory is zero, FileName is a full path from the share root.
		//   Even a bare filename like "foo.txt" means "put file at share root/foo.txt".
		// - If RootDirectory is non-zero, FileName is relative to that directory handle.
		//   (Not yet implemented - we'd need to resolve the FileId to a directory handle.)
		var toDir metadata.FileHandle
		var toName string

		// Check if RootDirectory is non-zero (handle-relative rename)
		var zeroRootDir [8]byte
		if !bytes.Equal(renameInfo.RootDirectory[:], zeroRootDir[:]) {
			// RootDirectory is non-zero: FileName is relative to the directory
			// identified by RootDirectory. For now, we don't resolve FileId handles
			// to directory handles, so fall back to same-directory rename.
			logger.Debug("SET_INFO: rename with non-zero RootDirectory (using same-dir fallback)",
				"rootDirectory", fmt.Sprintf("%x", renameInfo.RootDirectory))
			toDir = openFile.ParentHandle
			toName = path.Base(newPath)
		} else {
			// RootDirectory is zero: FileName is a full path from the share root.
			// Get root handle for the share.
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

			toName = path.Base(newPath)
			dirPath := path.Dir(newPath)

			// Walk to destination directory (or use root if no directory component)
			if dirPath == "." || dirPath == "" || dirPath == "/" {
				toDir = rootHandle
			} else {
				toDir, err = h.walkPath(authCtx, rootHandle, dirPath)
				if err != nil {
					logger.Debug("SET_INFO: destination path not found", "path", dirPath, "error", err)
					return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectPathNotFound}}, nil
				}
			}
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
		metaSvc := h.Registry.GetMetadataService()
		err = metaSvc.Move(authCtx, openFile.ParentHandle, openFile.FileName, toDir, toName)
		if err != nil {
			logger.Debug("SET_INFO: rename failed",
				"from", openFile.Path,
				"to", newPath,
				"error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

		// Per MS-FSA 2.1.5.14.10: On successful completion of a rename,
		// if the file was marked for delete-on-close, clear that disposition.
		// This prevents the renamed file from being deleted when the handle closes.
		if openFile.DeletePending {
			openFile.DeletePending = false
			logger.Debug("SET_INFO: cleared delete-on-close after rename",
				"oldPath", oldPath,
				"newPath", newPath)
		}

		// Notify watchers about the rename using paired notification.
		// Per MS-FSCC 2.4.42, rename notifications MUST contain both
		// FILE_ACTION_RENAMED_OLD_NAME and FILE_ACTION_RENAMED_NEW_NAME
		// in a single response. CHANGE_NOTIFY is one-shot, so sending
		// them separately would cause the second to be silently dropped.
		if h.NotifyRegistry != nil {
			tree, ok := h.GetTree(openFile.TreeID)
			if ok {
				newParentPath := GetParentPath(newPath)
				if newParentPath == "" || newParentPath == "." {
					newParentPath = "/"
				}
				h.NotifyRegistry.NotifyRename(tree.ShareName, oldParentPath, oldFileName, newParentPath, toName)
			} else {
				logger.Debug("SET_INFO: rename notifications skipped, tree lookup failed",
					"treeID", openFile.TreeID,
					"from", openFile.Path,
					"to", newPath)
			}
		}

		// Update open file state to reflect the new path.
		// Compute actual resulting path from the destination directory and name,
		// since newPath may be relative when RootDirectory is non-zero.
		actualNewPath := newPath
		if !bytes.Equal(renameInfo.RootDirectory[:], zeroRootDir[:]) {
			// Handle-relative rename: build path from parent path + new name
			parentPath := GetParentPath(openFile.Path)
			if parentPath == "" || parentPath == "/" {
				actualNewPath = toName
			} else {
				actualNewPath = parentPath + "/" + toName
			}
		}
		openFile.Path = actualNewPath
		openFile.FileName = toName
		openFile.ParentHandle = toDir
		h.StoreOpenFile(openFile)

		logger.Debug("SET_INFO: rename successful",
			"oldPath", oldPath,
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
				dispR := smbenc.NewReader(buffer)
				flags := dispR.ReadUint32()
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
		newSize, err := decodeEndOfFileInfo(buffer)
		if err != nil {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}

		setAttrs := &metadata.SetAttrs{
			Size: &newSize,
		}

		metaSvc := h.Registry.GetMetadataService()
		err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set EOF", "path", openFile.Path, "error", err)
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

		// Restore frozen timestamps after truncation (which updates Mtime/Ctime)
		h.restoreFrozenTimestamps(authCtx, openFile)

		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case types.FilePositionInformation:
		// FILE_POSITION_INFORMATION [MS-FSCC] 2.4.32 (8 bytes)
		// Per MS-FSA 2.1.5.14.23: If InputBufferSize is less than the size of
		// FILE_POSITION_INFORMATION (8 bytes), return STATUS_INFO_LENGTH_MISMATCH.
		if len(buffer) < 8 {
			return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInfoLengthMismatch}}, nil
		}
		// Server-side position tracking is not required for network filesystems.
		// Accept and succeed as a no-op (SMB clients manage their own offsets).
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case types.FileAllocationInformation:
		// Set allocation size - accept but treat as no-op (allocation handled automatically)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil

	case 11: // FileLinkInformation - hard links not supported
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil

	default:
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil
	}
}

// applyFrozenTimestamps overrides file metadata with frozen timestamp values.
// Called when reading file metadata for responses (QUERY_INFO, CLOSE POSTQUERY_ATTRIB).
// This is the read-side complement to restoreFrozenTimestamps (which is write-side).
// For both files and directories, if a timestamp was frozen via SET_INFO(-1),
// the frozen value is returned regardless of any subsequent store modifications.
func applyFrozenTimestamps(openFile *OpenFile, file *metadata.File) {
	if openFile.MtimeFrozen && openFile.FrozenMtime != nil {
		file.Mtime = *openFile.FrozenMtime
	}
	if openFile.CtimeFrozen && openFile.FrozenCtime != nil {
		file.Ctime = *openFile.FrozenCtime
	}
	if openFile.AtimeFrozen && openFile.FrozenAtime != nil {
		file.Atime = *openFile.FrozenAtime
	}
}

// restoreFrozenTimestamps restores timestamps that are frozen via SET_INFO -1 sentinel.
// Called after operations that unconditionally update timestamps (WRITE, truncate).
func (h *Handler) restoreFrozenTimestamps(authCtx *metadata.AuthContext, openFile *OpenFile) {
	if !openFile.MtimeFrozen && !openFile.CtimeFrozen {
		return
	}
	restoreAttrs := &metadata.SetAttrs{}
	needRestore := false
	if openFile.MtimeFrozen && openFile.FrozenMtime != nil {
		restoreAttrs.Mtime = openFile.FrozenMtime
		needRestore = true
	}
	if openFile.CtimeFrozen && openFile.FrozenCtime != nil {
		restoreAttrs.Ctime = openFile.FrozenCtime
		needRestore = true
	}
	if needRestore {
		logger.Debug("restoreFrozenTimestamps: restoring",
			"path", openFile.Path,
			"mtimeFrozen", openFile.MtimeFrozen,
			"ctimeFrozen", openFile.CtimeFrozen,
			"frozenMtime", openFile.FrozenMtime,
			"frozenCtime", openFile.FrozenCtime)
		metaSvc := h.Registry.GetMetadataService()
		if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, restoreAttrs); err != nil {
			logger.Debug("restoreFrozenTimestamps: failed", "path", openFile.Path, "error", err)
		} else {
			// Also update the pending write state's LastMtime to the frozen value.
			// MetadataService.GetFile() merges pending state with stored state, using
			// max(pending.LastMtime, store.Mtime). If we only update the store but
			// leave pending.LastMtime at the original WRITE time, GetFile() will
			// return the non-frozen value. By updating pending.LastMtime to the frozen
			// Mtime, the merge produces the correct frozen value.
			if openFile.MtimeFrozen && openFile.FrozenMtime != nil {
				metaSvc.UpdatePendingMtime(openFile.MetadataHandle, *openFile.FrozenMtime)
			}
		}
	}
}

// setSecurityInfo handles SET_INFO for security descriptors.
//
// Parses the binary Security Descriptor from the client, extracts owner/group/ACL,
// and applies the changes to the file via MetadataService.SetFileAttributes.
func (h *Handler) setSecurityInfo(
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	additionalInfo uint32,
	buffer []byte,
) (*SetInfoResponse, error) {
	if len(buffer) == 0 {
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptor(buffer)
	if err != nil {
		logger.Debug("SET_INFO: failed to parse security descriptor", "path", openFile.Path, "error", err)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// Build SetAttrs from parsed SD
	setAttrs := &metadata.SetAttrs{}
	changed := false

	// Only apply sections that were requested via AdditionalInfo
	if (additionalInfo&OwnerSecurityInformation) != 0 && ownerUID != nil {
		setAttrs.UID = ownerUID
		changed = true
	}

	if (additionalInfo&GroupSecurityInformation) != 0 && ownerGID != nil {
		setAttrs.GID = ownerGID
		changed = true
	}

	if (additionalInfo&DACLSecurityInformation) != 0 && fileACL != nil {
		setAttrs.ACL = fileACL
		changed = true
	}

	if !changed {
		// Nothing to change - accept as no-op
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
	}

	// Apply changes
	metaSvc := h.Registry.GetMetadataService()
	err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
	if err != nil {
		logger.Debug("SET_INFO: failed to set security info", "path", openFile.Path, "error", err)
		return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
}
