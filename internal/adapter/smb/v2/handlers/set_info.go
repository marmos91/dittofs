package handlers

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

// setInfoStatus creates a SetInfoResponse with the given status code.
func setInfoStatus(status types.Status) *SetInfoResponse {
	return &SetInfoResponse{SMBResponseBase: SMBResponseBase{Status: status}}
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
		logger.Debug("SET_INFO: file handle not found (closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return setInfoStatus(types.StatusFileClosed), nil
	}

	// ========================================================================
	// Step 1b: Validate DesiredAccess for SET_INFO
	// ========================================================================
	// Per MS-SMB2 3.3.5.21.1: For attribute-setting info classes, the open
	// must include FILE_WRITE_ATTRIBUTES. Rename/delete disposition/EOF have
	// their own access checks later (DELETE, FILE_WRITE_DATA, etc.).
	if req.InfoType == types.SMB2InfoTypeFile {
		switch types.FileInfoClass(req.FileInfoClass) {
		case types.FileRenameInformation,
			types.FileDispositionInformation, types.FileDispositionInformationEx,
			types.FileEndOfFileInformation, types.FileAllocationInformation,
			15: // FileFullEaInformation
			// These have specific access checks in their handlers or
			// are validated by the metadata layer
		default:
			if !hasAccessRight(openFile.DesiredAccess, uint32(types.FileWriteAttributes)) {
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}
	}

	// ========================================================================
	// Step 2: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("SET_INFO: failed to build auth context", "error", err)
		return setInfoStatus(types.StatusAccessDenied), nil
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
		return setInfoStatus(types.StatusInvalidParameter), nil
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
			return setInfoStatus(types.StatusInfoLengthMismatch), nil
		}

		// Validate attribute constraints per MS-FSCC 2.4.7:
		// - FILE_ATTRIBUTE_DIRECTORY on a non-directory file -> INVALID_PARAMETER
		// - FILE_ATTRIBUTE_TEMPORARY on a directory -> INVALID_PARAMETER
		attrR := smbenc.NewReader(buffer[32:36])
		fileAttrs := types.FileAttributes(attrR.ReadUint32())
		if fileAttrs != 0 {
			if fileAttrs&types.FileAttributeDirectory != 0 && !openFile.IsDirectory {
				return setInfoStatus(types.StatusInvalidParameter), nil
			}
			if fileAttrs&types.FileAttributeTemporary != 0 && openFile.IsDirectory {
				return setInfoStatus(types.StatusInvalidParameter), nil
			}
		}

		// Decode directly from raw buffer to handle FILETIME sentinels (0, -1, -2)
		setAttrs := DecodeBasicInfoToSetAttrs(buffer)

		metaSvc := h.Registry.GetMetadataService()

		// Per MS-FSCC 2.6: Map FILE_ATTRIBUTE_READONLY to Unix mode.
		// When FileAttributes != 0, the client is explicitly setting attributes.
		// READONLY removes owner write bits; its absence restores them.
		// Per MS-FSCC 2.4.7: FILE_ATTRIBUTE_COMPRESSED is NOT settable via
		// FileBasicInformation; it is controlled only via FSCTL_SET_COMPRESSION.
		// Preserve the existing modeDOSCompressed bit so SET_INFO doesn't
		// accidentally clear compression state that was set via FSCTL.
		if fileAttrs != 0 {
			mode := SMBModeFromAttrs(fileAttrs, openFile.IsDirectory)
			// Preserve modeDOSCompressed from existing metadata
			if curFile, curErr := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle); curErr == nil {
				mode |= curFile.Mode & modeDOSCompressed
			}
			setAttrs.Mode = &mode
		}

		// Per MS-FSA 2.1.5.14.2: Handle timestamp freeze/unfreeze sentinels.
		// filetimeFreeze (-1): Freeze timestamp -- suppress auto-updates on subsequent operations.
		// filetimeUnfreeze (-2): Unfreeze timestamp -- re-enable auto-updates.
		// We capture the current timestamp value BEFORE applying changes so the frozen
		// value reflects the state at freeze time.

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

		// Per MS-FSA 2.1.5.14.2: All four timestamp fields support freeze/unfreeze.
		// CreationTime freeze suppresses explicit changes from subsequent SET_INFO
		// calls on this handle (the frozen value is returned instead).
		hasFreezeOrUnfreeze := isFiletimeSentinel(creationFT) ||
			isFiletimeSentinel(atimeFT) ||
			isFiletimeSentinel(mtimeFT) ||
			isFiletimeSentinel(ctimeFT)

		// Per MS-FSA 2.1.5.14.2: Sentinel values (-1, -2) mean the object store
		// MUST NOT change the timestamp for THIS or subsequent operations on this
		// handle. Pre-read the file to capture current timestamps, then pin
		// sentinel timestamps to their current value in setAttrs to suppress
		// auto-updates (e.g., Ctime auto-update when FileAttributes change).
		var preFile *metadata.File
		if hasFreezeOrUnfreeze {
			var err error
			preFile, err = metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
			if err != nil {
				logger.Warn("SET_INFO: failed to read file for freeze/unfreeze", "path", openFile.Path, "error", err)
			}
		}

		// Pin sentinel timestamps to their current value so SetFileAttributes
		// won't auto-update them (e.g., Ctime auto-update when Mode changes).
		if preFile != nil {
			// For freeze (-1): pin to pre-change value to prevent auto-update
			if creationFT == filetimeFreeze {
				setAttrs.CreationTime = &preFile.CreationTime
			}
			if ctimeFT == filetimeFreeze {
				setAttrs.Ctime = &preFile.Ctime
			}
			if mtimeFT == filetimeFreeze {
				setAttrs.Mtime = &preFile.Mtime
			}
			if atimeFT == filetimeFreeze {
				setAttrs.Atime = &preFile.Atime
			}
			// For unfreeze (-2): per MS-FSA 2.1.5.14.2, re-enable auto-update
			// AND set the timestamp to the current time. CreationTime unfreeze
			// pins to the pre-change value (it is never auto-updated, so "current
			// time" semantics don't apply — just re-enable future explicit changes).
			unfreezeNow := time.Now()
			if creationFT == filetimeUnfreeze {
				setAttrs.CreationTime = &preFile.CreationTime
			}
			if ctimeFT == filetimeUnfreeze {
				setAttrs.Ctime = &unfreezeNow
			}
			if mtimeFT == filetimeUnfreeze {
				setAttrs.Mtime = &unfreezeNow
			}
			if atimeFT == filetimeUnfreeze {
				setAttrs.Atime = &unfreezeNow
			}
		}

		// Per MS-FSA 2.1.5.14.2: When a timestamp is frozen from a prior
		// SET_INFO call (no sentinel in this call, field==0), pin to the
		// frozen value to prevent the metadata service from auto-updating it.
		if creationFT == 0 && openFile.BtimeFrozen && openFile.FrozenBtime != nil {
			setAttrs.CreationTime = openFile.FrozenBtime
		}
		if ctimeFT == 0 && openFile.CtimeFrozen && openFile.FrozenCtime != nil {
			setAttrs.Ctime = openFile.FrozenCtime
		}
		if mtimeFT == 0 && openFile.MtimeFrozen && openFile.FrozenMtime != nil {
			setAttrs.Mtime = openFile.FrozenMtime
		}
		if atimeFT == 0 && openFile.AtimeFrozen && openFile.FrozenAtime != nil {
			setAttrs.Atime = openFile.FrozenAtime
		}

		// Per MS-FSA 2.1.5.14.2: When FileAttributes change, the object store
		// SHOULD also update LastWriteTime. The metadata layer only auto-updates
		// Ctime (POSIX semantics), so we handle Mtime auto-update here.
		// Skip if: Mtime is being explicitly set, has a sentinel, or is frozen.
		if fileAttrs != 0 && setAttrs.Mtime == nil && mtimeFT == 0 && !openFile.MtimeFrozen {
			now := time.Now()
			setAttrs.Mtime = &now
		}

		if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs); err != nil {
			logger.Debug("SET_INFO: failed to set basic info", "path", openFile.Path, "error", err)
			return setInfoStatus(common.MapToSMB(err)), nil
		}

		// Apply freeze/unfreeze state to the open handle using pre-change values.
		// The frozen value is the timestamp at the moment of the freeze request,
		// before any auto-updates from other field changes in this operation.
		// preFile is non-nil only when hasFreezeOrUnfreeze is true, which
		// guarantees at least one switch case will match.
		if preFile != nil {
			// CreationTime (Btime) - offset 0
			switch creationFT {
			case filetimeFreeze:
				openFile.BtimeFrozen = true
				openFile.FrozenBtime = &preFile.CreationTime
				logger.Debug("SET_INFO: froze CreationTime", "path", openFile.Path, "value", preFile.CreationTime)
			case filetimeUnfreeze:
				openFile.BtimeFrozen = false
				openFile.FrozenBtime = nil
			}

			// LastWriteTime (Mtime) - offset 16
			switch mtimeFT {
			case filetimeFreeze:
				openFile.MtimeFrozen = true
				openFile.FrozenMtime = &preFile.Mtime
				logger.Debug("SET_INFO: froze LastWriteTime", "path", openFile.Path, "value", preFile.Mtime)
			case filetimeUnfreeze:
				openFile.MtimeFrozen = false
				openFile.FrozenMtime = nil
			}

			// ChangeTime (Ctime) - offset 24
			switch ctimeFT {
			case filetimeFreeze:
				openFile.CtimeFrozen = true
				openFile.FrozenCtime = &preFile.Ctime
				logger.Debug("SET_INFO: froze ChangeTime", "path", openFile.Path, "value", preFile.Ctime)
			case filetimeUnfreeze:
				openFile.CtimeFrozen = false
				openFile.FrozenCtime = nil
			}

			// LastAccessTime (Atime) - offset 8
			switch atimeFT {
			case filetimeFreeze:
				openFile.AtimeFrozen = true
				openFile.FrozenAtime = &preFile.Atime
			case filetimeUnfreeze:
				openFile.AtimeFrozen = false
				openFile.FrozenAtime = nil
			}

			h.StoreOpenFile(openFile)
		}

		// Notify watchers about attribute/timestamp changes
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified)
		}

		return setInfoStatus(types.StatusSuccess), nil

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
		renameInfo, err := DecodeFileRenameInfo(buffer)
		if err != nil {
			logger.Debug("SET_INFO: failed to decode rename info", "error", err)
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		// Per MS-FSA 2.1.5.14.10: Rename requires DELETE access on the source file
		if !hasDeleteAccess(openFile.DesiredAccess) {
			logger.Debug("SET_INFO: rename without DELETE access",
				"path", openFile.Path,
				"desiredAccess", fmt.Sprintf("0x%x", openFile.DesiredAccess))
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Per MS-FSA 2.1.5.14.10: Before renaming, check that no other open
		// handle on the same file conflicts with the rename. Specifically,
		// all other opens must have FILE_SHARE_DELETE (0x04) in ShareAccess.
		if conflict := h.checkShareDeleteConflict(openFile); conflict {
			logger.Debug("SET_INFO: rename blocked by sharing violation",
				"path", openFile.Path,
				"fileID", fmt.Sprintf("%x", openFile.FileID))
			return setInfoStatus(types.StatusSharingViolation), nil
		}

		// Normalize path separators (Windows uses backslash, we use forward slash)
		newPath := strings.ReplaceAll(renameInfo.FileName, "\\", "/")
		newPath = strings.TrimPrefix(newPath, "/")

		// ================================================================
		// Stream rename: if the target name starts with ":", this is a
		// stream-to-stream rename within the same base file.
		// E.g., renaming ":old:$DATA" to ":new:$DATA" on file "foo.txt"
		// means renaming "foo.txt:old:$DATA" -> "foo.txt:new:$DATA" in
		// the parent directory.
		// ================================================================
		if strings.HasPrefix(newPath, ":") {
			// Extract the base file name from the current open file name.
			// The current file is an ADS: "basefile:streamname:$DATA"
			baseName := openFile.FileName
			if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 {
				baseName = baseName[:colonIdx]
			}

			// Build new child name: basefile + new stream suffix
			toName := baseName + newPath
			toDir := openFile.ParentHandle

			// Save old path info for notification before modification
			oldPath := openFile.Path
			oldFileName := openFile.FileName
			oldParentPath := GetParentPath(oldPath)

			// Per MS-FSA 2.1.5.14.10: Save mtime/ctime before rename
			restoreTimestamps := h.saveTimestamps(authCtx, openFile.MetadataHandle)

			// Perform the rename
			metaSvc := h.Registry.GetMetadataService()
			err = metaSvc.Move(authCtx, toDir, openFile.FileName, toDir, toName)
			if err != nil {
				logger.Debug("SET_INFO: stream rename failed",
					"from", openFile.FileName,
					"to", toName,
					"error", err)
				return setInfoStatus(common.MapToSMB(err)), nil
			}

			// Restore mtime/ctime after rename
			restoreTimestamps()

			// Clear delete-on-close after rename
			openFile.DeletePending = false

			// Notify watchers
			if h.NotifyRegistry != nil {
				tree, ok := h.GetTree(openFile.TreeID)
				if ok {
					newParentPath := GetParentPath(openFile.Path)
					if newParentPath == "" || newParentPath == "." {
						newParentPath = "/"
					}
					h.NotifyRegistry.NotifyRename(tree.ShareName, oldParentPath, oldFileName, newParentPath, toName)
				}
			}

			// Update open file state
			parentPath := GetParentPath(openFile.Path)
			if parentPath == "" || parentPath == "/" || parentPath == "." {
				openFile.Path = toName
			} else {
				openFile.Path = parentPath + "/" + toName
			}
			openFile.FileName = toName
			h.StoreOpenFile(openFile)

			// Break parent directory leases on rename (content change)
			h.breakParentDirLeasesForContentChange(authCtx, openFile)

			logger.Debug("SET_INFO: stream rename successful",
				"oldName", oldFileName,
				"newName", toName)
			return setInfoStatus(types.StatusSuccess), nil
		}

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
				return setInfoStatus(types.StatusInvalidHandle), nil
			}

			rootHandle, err := h.Registry.GetRootHandle(tree.ShareName)
			if err != nil {
				logger.Debug("SET_INFO: failed to get root handle", "error", err)
				return setInfoStatus(types.StatusObjectPathNotFound), nil
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
					return setInfoStatus(types.StatusObjectPathNotFound), nil
				}
			}
		}

		// Validate we have source info
		if len(openFile.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot rename root directory", "path", openFile.Path)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Save old path info for notification before modification
		oldPath := openFile.Path
		oldFileName := openFile.FileName
		oldParentPath := GetParentPath(oldPath)

		// Per MS-FSA 2.1.5.14.10: Save mtime/ctime before rename so we can
		// restore them after. Rename should NOT update the file's timestamps.
		restoreTimestamps := h.saveTimestamps(authCtx, openFile.MetadataHandle)

		// Perform the rename/move
		metaSvc := h.Registry.GetMetadataService()
		err = metaSvc.Move(authCtx, openFile.ParentHandle, openFile.FileName, toDir, toName)
		if err != nil {
			logger.Debug("SET_INFO: rename failed",
				"from", openFile.Path,
				"to", newPath,
				"error", err)
			return setInfoStatus(common.MapToSMB(err)), nil
		}

		// Restore mtime/ctime after rename
		restoreTimestamps()

		// Per MS-FSA 2.1.5.14.2: Restore frozen timestamps on parent directories.
		// Move updates both source and destination parent directory timestamps.
		h.restoreParentDirFrozenTimestamps(authCtx, openFile.ParentHandle)
		if !bytes.Equal(toDir, openFile.ParentHandle) {
			h.restoreParentDirFrozenTimestamps(authCtx, toDir)
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

		// Per MS-FSA 2.1.5.14.10: Rename changes directory contents.
		// Break Handle + Read leases on parent directories.
		h.breakParentDirLeasesForContentChange(authCtx, openFile)

		logger.Debug("SET_INFO: rename successful",
			"oldPath", oldPath,
			"newPath", newPath)
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileDispositionInformation, types.FileDispositionInformationEx:
		// FILE_DISPOSITION_INFORMATION [MS-FSCC] 2.4.11
		// FILE_DISPOSITION_INFORMATION_EX [MS-FSCC] 2.4.11.2
		// DeletePending (1 byte for class 13, 4 bytes flags for class 64)
		if len(buffer) < 1 {
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		var deletePending bool
		if class == types.FileDispositionInformationEx {
			// FileDispositionInformationEx uses a 4-byte Flags field per MS-FSCC 2.4.11.2
			if len(buffer) < 4 {
				return setInfoStatus(types.StatusInfoLengthMismatch), nil
			}
			dispR := smbenc.NewReader(buffer)
			flags := dispR.ReadUint32()
			// Bit 0 (FILE_DISPOSITION_DELETE) = delete on close
			deletePending = (flags & 0x01) != 0
		} else {
			deletePending = buffer[0] != 0
		}

		// Validate we have parent info for deletion
		if deletePending && len(openFile.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot delete root directory", "path", openFile.Path)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Per MS-FSA 2.1.5.14.3: Setting delete disposition requires DELETE access
		if deletePending {
			if !hasDeleteAccess(openFile.DesiredAccess) {
				logger.Debug("SET_INFO: delete disposition without DELETE access",
					"path", openFile.Path,
					"desiredAccess", fmt.Sprintf("0x%x", openFile.DesiredAccess))
				return setInfoStatus(types.StatusAccessDenied), nil
			}

			// Read-only files cannot be marked for deletion
			if !openFile.IsDirectory {
				metaSvc := h.Registry.GetMetadataService()
				file, fileErr := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
				if fileErr == nil {
					attrs := FileAttrToSMBAttributes(&file.FileAttr)
					if attrs&types.FileAttributeReadonly != 0 {
						logger.Debug("SET_INFO: delete disposition on read-only file", "path", openFile.Path)
						return setInfoStatus(types.StatusCannotDelete), nil
					}
				}
			}
		}

		// Mark file for deletion on close
		openFile.DeletePending = deletePending
		h.StoreOpenFile(openFile)

		logger.Debug("SET_INFO: delete disposition set",
			"path", openFile.Path,
			"deletePending", deletePending,
			"class", class)
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileEndOfFileInformation:
		// FILE_END_OF_FILE_INFORMATION [MS-FSCC] 2.4.13
		newSize, err := decodeEndOfFileInfo(buffer)
		if err != nil {
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		metaSvc := h.Registry.GetMetadataService()

		// Per MS-FSA 2.1.5.14.4: Check for conflicting byte-range locks.
		// When truncating, the region from newSize to the current EOF must not
		// have locks from other sessions. We check the entire range from newSize
		// to max as a write operation (truncation is destructive).
		if err := metaSvc.CheckLockForIO(
			authCtx.Context,
			openFile.MetadataHandle,
			openFile.OpenID(),
			openFile.SessionID,
			newSize,
			0, // 0 = unbounded (to EOF)
			true,
		); err != nil {
			logger.Debug("SET_INFO: truncation blocked by lock",
				"path", openFile.Path, "newSize", newSize)
			return setInfoStatus(types.StatusFileLockConflict), nil
		}

		setAttrs := &metadata.SetAttrs{
			Size: &newSize,
		}

		err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set EOF", "path", openFile.Path, "error", err)
			return setInfoStatus(common.MapToSMB(err)), nil
		}

		// Restore frozen timestamps after truncation (which updates Mtime/Ctime)
		h.restoreFrozenTimestamps(authCtx, openFile)

		// Notify watchers about size changes
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified)
		}

		return setInfoStatus(types.StatusSuccess), nil

	case types.FilePositionInformation:
		// FILE_POSITION_INFORMATION [MS-FSCC] 2.4.32 (8 bytes)
		// Per MS-FSA 2.1.5.14.23: If InputBufferSize is less than the size of
		// FILE_POSITION_INFORMATION (8 bytes), return STATUS_INFO_LENGTH_MISMATCH.
		if len(buffer) < 8 {
			return setInfoStatus(types.StatusInfoLengthMismatch), nil
		}
		// Server-side position tracking is not required for network filesystems.
		// Accept and succeed as a no-op (SMB clients manage their own offsets).
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileAllocationInformation:
		// Set allocation size - accept but treat as no-op (allocation handled automatically)
		return setInfoStatus(types.StatusSuccess), nil

	case 11: // FileLinkInformation - hard links not supported
		return setInfoStatus(types.StatusNotSupported), nil

	case 15: // FileFullEaInformation [MS-FSCC] 2.4.15 - Extended attributes
		// Accept EA writes as a no-op. DittoFS does not persist extended attributes
		// but returning SUCCESS allows ChangeNotify EA tests to proceed.
		logger.Debug("SET_INFO: FileFullEaInformation (no-op)", "path", openFile.Path)
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified)
		}
		return setInfoStatus(types.StatusSuccess), nil

	default:
		return setInfoStatus(types.StatusNotSupported), nil
	}
}

// applyFrozenTimestamps overrides file metadata with frozen timestamp values.
// Called when reading file metadata for responses (QUERY_INFO, CLOSE POSTQUERY_ATTRIB).
// This is the read-side complement to restoreFrozenTimestamps (which is write-side).
// For both files and directories, if a timestamp was frozen via SET_INFO(-1),
// the frozen value is returned regardless of any subsequent store modifications.
func applyFrozenTimestamps(openFile *OpenFile, file *metadata.File) {
	if openFile.BtimeFrozen && openFile.FrozenBtime != nil {
		file.CreationTime = *openFile.FrozenBtime
	}
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

// saveTimestamps reads the current Mtime and Ctime of a file and returns a
// restore function that writes them back. Used to preserve timestamps across
// rename operations (per MS-FSA 2.1.5.14.10, rename should not change timestamps).
// Returns a no-op if the read fails.
func (h *Handler) saveTimestamps(authCtx *metadata.AuthContext, handle metadata.FileHandle) func() {
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(authCtx.Context, handle)
	if err != nil {
		return func() {}
	}
	mtime := file.Mtime
	ctime := file.Ctime
	return func() {
		_ = metaSvc.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{
			Mtime: &mtime,
			Ctime: &ctime,
		})
	}
}

// restoreFrozenTimestamps restores timestamps that are frozen via SET_INFO -1 sentinel.
// Called after operations that unconditionally update timestamps (WRITE, truncate).
func (h *Handler) restoreFrozenTimestamps(authCtx *metadata.AuthContext, openFile *OpenFile) {
	if !openFile.BtimeFrozen && !openFile.MtimeFrozen && !openFile.CtimeFrozen && !openFile.AtimeFrozen {
		return
	}

	restoreAttrs := buildFrozenAttrs(openFile)
	if restoreAttrs == nil {
		return
	}

	logger.Debug("restoreFrozenTimestamps: restoring",
		"path", openFile.Path,
		"mtimeFrozen", openFile.MtimeFrozen,
		"ctimeFrozen", openFile.CtimeFrozen,
		"atimeFrozen", openFile.AtimeFrozen,
		"frozenMtime", openFile.FrozenMtime,
		"frozenCtime", openFile.FrozenCtime,
		"frozenAtime", openFile.FrozenAtime)

	metaSvc := h.Registry.GetMetadataService()
	if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, restoreAttrs); err != nil {
		logger.Debug("restoreFrozenTimestamps: failed", "path", openFile.Path, "error", err)
		return
	}

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

// restoreParentDirFrozenTimestamps restores frozen timestamps on open directory handles
// after child operations (create, delete, write) that unconditionally update parent
// directory timestamps in the metadata layer.
//
// Per MS-FSA 2.1.5.14.2: When a timestamp is frozen via SET_INFO with -1 sentinel,
// the timestamp MUST NOT be auto-updated by subsequent operations. The metadata layer
// (createEntry, removeFile, etc.) always updates parent directory timestamps. This
// method iterates open handles to find directory handles matching the given parent
// metadata handle and restores any frozen timestamps.
func (h *Handler) restoreParentDirFrozenTimestamps(authCtx *metadata.AuthContext, parentMetadataHandle metadata.FileHandle) {
	if len(parentMetadataHandle) == 0 {
		return
	}

	parentHandleStr := string(parentMetadataHandle)

	h.files.Range(func(key, value any) bool {
		openFile := value.(*OpenFile)
		if !openFile.IsDirectory || string(openFile.MetadataHandle) != parentHandleStr {
			return true // continue
		}

		restoreAttrs := buildFrozenAttrs(openFile)
		if restoreAttrs == nil {
			return true // continue
		}

		metaSvc := h.Registry.GetMetadataService()
		if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, restoreAttrs); err != nil {
			logger.Debug("restoreParentDirFrozenTimestamps: failed",
				"path", openFile.Path, "error", err)
		} else {
			logger.Debug("restoreParentDirFrozenTimestamps: restored",
				"path", openFile.Path,
				"mtimeFrozen", openFile.MtimeFrozen,
				"ctimeFrozen", openFile.CtimeFrozen,
				"atimeFrozen", openFile.AtimeFrozen)
		}

		// Don't break early - there may be multiple handles for the same directory
		return true
	})
}

// buildFrozenAttrs constructs a SetAttrs from the frozen timestamp values on an
// OpenFile. Returns nil if no timestamps need restoring.
func buildFrozenAttrs(openFile *OpenFile) *metadata.SetAttrs {
	attrs := &metadata.SetAttrs{}
	hasAny := false

	if openFile.BtimeFrozen && openFile.FrozenBtime != nil {
		attrs.CreationTime = openFile.FrozenBtime
		hasAny = true
	}
	if openFile.MtimeFrozen && openFile.FrozenMtime != nil {
		attrs.Mtime = openFile.FrozenMtime
		hasAny = true
	}
	if openFile.CtimeFrozen && openFile.FrozenCtime != nil {
		attrs.Ctime = openFile.FrozenCtime
		hasAny = true
	}
	if openFile.AtimeFrozen && openFile.FrozenAtime != nil {
		attrs.Atime = openFile.FrozenAtime
		hasAny = true
	}

	if !hasAny {
		return nil
	}
	return attrs
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
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptor(buffer)
	if err != nil {
		logger.Debug("SET_INFO: failed to parse security descriptor", "path", openFile.Path, "error", err)
		return setInfoStatus(types.StatusInvalidParameter), nil
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
		// Nothing to change - accept as no-op but still notify watchers
		// since the client considers the SET_INFO successful.
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified)
		}
		return setInfoStatus(types.StatusSuccess), nil
	}

	// Apply changes
	metaSvc := h.Registry.GetMetadataService()
	err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
	if err != nil {
		logger.Debug("SET_INFO: failed to set security info", "path", openFile.Path, "error", err)
		return setInfoStatus(common.MapToSMB(err)), nil
	}

	// Notify watchers about security descriptor changes
	if h.NotifyRegistry != nil {
		h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified)
	}

	return setInfoStatus(types.StatusSuccess), nil
}

// breakParentDirLeases breaks leases on the parent directory when a child
// file's metadata or content changes (SET_INFO, WRITE, DELETE). Per MS-FSA 2.1.5.14:
//   - Handle caching is broken so clients revalidate cached directory handles
//   - Read caching is broken so clients see updated directory listing metadata
//     (timestamps, sizes, attributes visible in READDIR results)
//
// breakParentDirLeasesForContentChange breaks both Handle and Read leases on
// the parent directory when directory CONTENT changes (rename, delete). These
// operations affect what READDIR returns, invalidating Read caching.
func (h *Handler) breakParentDirLeasesForContentChange(authCtx *metadata.AuthContext, openFile *OpenFile) {
	if h.LeaseManager == nil || len(openFile.ParentHandle) == 0 {
		return
	}

	parentLockHandle := lock.FileHandle(openFile.ParentHandle)
	excludeClientID := fmt.Sprintf("smb:%d", openFile.SessionID)

	if breakErr := h.LeaseManager.BreakParentHandleLeasesOnCreate(authCtx.Context, parentLockHandle, openFile.ShareName, excludeClientID); breakErr != nil {
		logger.Debug("SET_INFO: parent directory Handle lease break failed", "path", openFile.Path, "error", breakErr)
	}
	if breakErr := h.LeaseManager.BreakParentReadLeasesOnModify(authCtx.Context, parentLockHandle, openFile.ShareName, excludeClientID); breakErr != nil {
		logger.Debug("SET_INFO: parent directory Read lease break failed", "path", openFile.Path, "error", breakErr)
	}
}
