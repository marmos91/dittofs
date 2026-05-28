package handlers

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
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
	// Step 1b: Validate GrantedAccess for SET_INFO
	// ========================================================================
	// Per MS-SMB2 3.3.5.21.1: For attribute-setting info classes, the open
	// must include FILE_WRITE_ATTRIBUTES. Rename/delete disposition/EOF have
	// their own access checks later (DELETE, FILE_WRITE_DATA, etc.).
	// The gate consults Open.GrantedAccess (post-DACL intersection at CREATE),
	// not the pre-DACL DesiredAccess — otherwise a request that named
	// FILE_WRITE_ATTRIBUTES but had it stripped by the DACL would still be
	// allowed to mutate attributes (parity with #616 ChangeNotify fix).
	if req.InfoType == types.SMB2InfoTypeFile {
		switch types.FileInfoClass(req.FileInfoClass) {
		case types.FileRenameInformation,
			types.FileLinkInformation,
			types.FileDispositionInformation, types.FileDispositionInformationEx,
			types.FileEndOfFileInformation, types.FileAllocationInformation,
			15: // FileFullEaInformation
			// These have specific access checks in their handlers or
			// are validated by the metadata layer
		default:
			if !hasAccessRight(openFile.GrantedAccess, uint32(types.FileWriteAttributes)) {
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}
	}

	// ========================================================================
	// Step 2: Build AuthContext
	// ========================================================================
	// Prime ctx.User / IsGuest / TreeID from the OpenFile's recorded session
	// BEFORE BuildAuthContext — otherwise ctx.User==nil falls into the
	// anonymous arm and synthesises UID-0 (root), bypassing all DACL checks
	// in the metadata layer (#619, same class as #603).
	h.primeAuthContextFromOpenFile(ctx, openFile)

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
		// READONLY is stored in modeDOSReadonly (bit 0x100000); POSIX owner-write
		// bits are preserved. calculatePermissions in pkg/metadata enforces the
		// READONLY semantics for both NFS and SMB callers by clearing write when
		// modeDOSExplicit + modeDOSReadonly are both set.
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
			// Per MS-FSCC 2.6: propagate FILE_ATTRIBUTE_HIDDEN into the metadata
			// Hidden field so QUERY_INFO and QUERY_DIRECTORY round-trip correctly.
			hiddenVal := fileAttrs&types.FileAttributeHidden != 0
			setAttrs.Hidden = &hiddenVal
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

		// Pin sentinel timestamps (FREEZE -1 and THAW -2) to their pre-change
		// value so SetFileAttributes' ctime auto-update on `modified` does
		// not bump them. Both sentinels are no-ops on the target value per
		// Samba `lib/util/time.c::nt_time_to_full_timespec` (which maps both
		// to `make_omit_timespec`); FREEZE additionally suspends future
		// auto-updates and THAW re-enables them — that bookkeeping happens
		// in the per-field switch below after SetFileAttributes returns.
		if preFile != nil {
			if isFiletimeSentinel(creationFT) {
				setAttrs.CreationTime = &preFile.CreationTime
			}
			if isFiletimeSentinel(ctimeFT) {
				setAttrs.Ctime = &preFile.Ctime
			}
			if isFiletimeSentinel(mtimeFT) {
				setAttrs.Mtime = &preFile.Mtime
			}
			if isFiletimeSentinel(atimeFT) {
				setAttrs.Atime = &preFile.Atime
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

		// Samba parity (fileio.c): any SET_INFO BasicInfo — even with all
		// zero timestamps — collapses the pending delayed-write window so
		// the post-write Mtime becomes visible. An explicit, non-sentinel
		// write_time also makes the value sticky until close.
		flushSmbDelayedWrite(openFile)
		if setAttrs.Mtime != nil && mtimeFT != 0 && !isFiletimeSentinel(mtimeFT) {
			setSmbStickyWriteTime(openFile, *setAttrs.Mtime)
		}
		h.StoreOpenFile(openFile)

		// Break parent directory leases on child metadata change (#470:
		// smb2.dirlease.set{atime,btime,ctime,mtime,dos}). Per MS-FSA
		// 2.1.5.14: any child SET_INFO that modifies file attributes or
		// timestamps changes what READDIR returns, invalidating parent-dir
		// Read + Handle caching. Parent-key suppression (C2) flows through
		// the same breakParentDirLeasesForContentChange plumbing.
		h.breakParentDirLeasesForContentChange(authCtx, openFile)

		if h.NotifyRegistry != nil {
			var nf uint32
			if fileAttrs != 0 {
				nf |= FileNotifyChangeAttributes
			}
			if creationFT != 0 && !isFiletimeSentinel(creationFT) {
				nf |= FileNotifyChangeCreation
			}
			if atimeFT != 0 && !isFiletimeSentinel(atimeFT) {
				nf |= FileNotifyChangeLastAccess
			}
			if mtimeFT != 0 && !isFiletimeSentinel(mtimeFT) {
				nf |= FileNotifyChangeLastWrite
			}
			if nf != 0 {
				h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified, nf)
			}
		}

		return setInfoStatus(types.StatusSuccess), nil

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
		renameInfo, err := DecodeFileRenameInfo(buffer)
		if err != nil {
			logger.Debug("SET_INFO: failed to decode rename info", "error", err)
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		// Per MS-FSA 2.1.5.14.10: Rename requires DELETE access on the source file.
		// Gate consults Open.GrantedAccess (post-DACL intersection at CREATE), not
		// the pre-DACL DesiredAccess — same fix class as #616 (ChangeNotify).
		if !hasDeleteAccess(openFile.GrantedAccess) {
			logger.Debug("SET_INFO: rename without DELETE access",
				"path", openFile.Path,
				"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Per MS-FSA 2.1.5.14.10: Before renaming, check that no other open
		// handle on the same file conflicts with the rename. Specifically,
		// all other opens must have FILE_SHARE_DELETE (0x04) in ShareAccess.
		// (Destination-parent share-mode probe runs further below, after toDir
		// is resolved and the stream-rename early return has been ruled out.)
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
					renameFilter := NameChangeFilterFor(toName, openFile.IsDirectory)
					if NameChangeFilterFor(oldFileName, openFile.IsDirectory) == FileNotifyChangeStreamName {
						renameFilter = FileNotifyChangeStreamName
					}
					h.NotifyRegistry.NotifyRename(tree.ShareName, oldParentPath, oldFileName, newParentPath, toName, renameFilter)
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

		// Per MS-FSA 2.1.5.14.11.3 / Samba smbd_smb2_setinfo_rename_dst_parent_check:
		// rename takes an implicit open on the destination parent directory
		// with DELETE+FILE_ADD_FILE and ShareAccess=0. Any existing open of
		// that directory that lacks FILE_SHARE_DELETE or holds DELETE access
		// conflicts. Stream renames don't traverse the directory layer and
		// returned earlier above; we're past that branch here.
		//
		// Pre-conflict dst-parent dir-lease break (#470 C3 / smbtorture
		// smb2.dirlease.rename_dst_parent, lease.c:7331). Even when the
		// conflict surfaces SHARING_VIOLATION, the dst-parent's RH dir-lease
		// holder must observe an RH→R break: the rename's implicit destructive
		// open intent invalidates the holder's Handle caching regardless of
		// whether the rename ultimately succeeds. Parent-key suppression
		// applies (the renamer's RqLs ParentLeaseKey, if any, marks the holder
		// as the renamer itself). On Phase-2 of the test, the holder's break
		// handler closes its handle inside our wait, the post-break conflict
		// recheck observes the close, and the rename proceeds.
		h.breakDstParentDirHandleLeasesForRename(authCtx, toDir, openFile)
		if conflict := h.checkParentDirRenameConflict(openFile.FileID, toDir); conflict {
			logger.Debug("SET_INFO: rename blocked by destination-parent sharing violation",
				"path", openFile.Path,
				"toDir", fmt.Sprintf("%x", toDir),
				"fileID", fmt.Sprintf("%x", openFile.FileID))
			return setInfoStatus(types.StatusSharingViolation), nil
		}

		// Pre-rename lease break: per MS-FSA §2.1.5.14.10 + Samba
		// `source3/smbd/smb2_setinfo.c::smbd_smb2_rename`, dispatch breaks on
		// any other-key lease holder of the source file (and, on overwrite,
		// the destination too) before applying the rename. Sync wait (mirrors
		// BreakParentHandleLeasesOnCreate) — rename is not in the compound-
		// CREATE hot path, so we don't need the round-4 async-park machinery;
		// the bounded WaitForOtherKeyBreaks deadline auto-downgrades non-
		// acking holders identically to that path.
		//
		// Renamer's own lease (openFile.LeaseKey, zero for non-leased opens)
		// is excluded by lease key only — NOT by ClientID — because a single
		// client may hold two distinct leases on the same file (smbtorture
		// rename_wait LEASE1=h1 / LEASE2=h2); a ClientID exclusion would skip
		// the second lease and deadlock the rename behind a never-acked break
		// that was never sent.
		//
		// Destination handling: when ReplaceIfExists=true and the destination
		// exists, dispatch the dst H-lease break (RWH→RW). Even after that
		// break drains, ANY open handle on dst blocks the overwrite per
		// MS-FSA §2.1.5.14.10 — surface STATUS_ACCESS_DENIED. The dst close
		// path (smbtorture v2_rename_target_overwrite stage 3) clears
		// the open and the post-wait recheck then proceeds to the rename.
		isOverwrite := renameInfo.ReplaceIfExists
		metaSvc := h.Registry.GetMetadataService()
		var dstMetaHandle metadata.FileHandle
		if isOverwrite {
			dstFile, lookupErr := metaSvc.Lookup(authCtx, toDir, toName)
			if lookupErr == nil && dstFile != nil {
				if encoded, encErr := metadata.EncodeFileHandle(dstFile); encErr == nil {
					dstMetaHandle = encoded
				}
			}
		}

		if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			srcMetaHandle := lock.FileHandle(openFile.MetadataHandle)
			dstLockHandle := lock.FileHandle(dstMetaHandle) // empty when no dst
			if err := h.LeaseManager.BreakLeasesOnRename(
				srcMetaHandle,
				dstLockHandle,
				openFile.ShareName,
				openFile.LeaseKey,
				isOverwrite,
			); err != nil {
				logger.Debug("SET_INFO: rename lease break dispatch failed", "error", err)
			}
			waitCtx, cancelWait := context.WithTimeout(authCtx.Context, lease.AsyncCreateBreakWaitTimeout)
			if waitErr := h.LeaseManager.WaitForOtherKeyBreaks(
				waitCtx, srcMetaHandle, openFile.ShareName, openFile.LeaseKey,
			); waitErr != nil {
				logger.Debug("SET_INFO: rename src break wait completed", "error", waitErr)
			}
			cancelWait()

			if isOverwrite && len(dstMetaHandle) > 0 && srcMetaHandle != dstLockHandle {
				dstWaitCtx, cancelDst := context.WithTimeout(authCtx.Context, lease.AsyncCreateBreakWaitTimeout)
				// Zero exception key — dst's lease holder is by definition
				// someone other than the renamer.
				if waitErr := h.LeaseManager.WaitForOtherKeyBreaks(
					dstWaitCtx, dstLockHandle, openFile.ShareName, [16]byte{},
				); waitErr != nil {
					logger.Debug("SET_INFO: rename dst break wait completed", "error", waitErr)
				}
				cancelDst()
			}
		}

		// Post-break: any open handle on dst (other than the renamer's own
		// FileID) blocks the overwrite. The H-lease break above stripped
		// caching rights, but did NOT close the underlying handle — the
		// holder must do that itself (smbtorture v2_rename_target_overwrite
		// stages 1/2: ACK leaves dst open ⇒ ACCESS_DENIED).
		if isOverwrite && len(dstMetaHandle) > 0 && h.hasOpenHandleOnFile(dstMetaHandle, openFile.FileID) {
			logger.Debug("SET_INFO: rename overwrite blocked by open handle on destination",
				"src", openFile.Path,
				"dst", newPath)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Directory rename: break H-leases on every open child file (RH→R
		// strip-H) and wait for each break to drain. After the wait, ANY
		// remaining open child blocks the parent rename per MS-FSA §2.1.5.14
		// (smbtorture rename_dir_openfile: 8 sub-cases all hinge on whether
		// every H-leased child closes-on-break or stays open after ACK).
		// Children without an H-lease are no-op'd by ComputeLeaseBreakTo and
		// stay open → fall through to the open-child recheck below, which
		// produces the immediate ACCESS_DENIED for the "no-hleases" case.
		if openFile.IsDirectory && h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			children := h.snapshotOpenChildren(openFile.MetadataHandle)
			for _, child := range children {
				if err := h.LeaseManager.BreakHandleLeasesOnOpenAsync(
					lock.FileHandle(child), openFile.ShareName, lock.BreakReasonSharingViolation,
				); err != nil {
					logger.Debug("SET_INFO: dir-rename child break dispatch failed",
						"child", string(child), "error", err)
				}
			}
			for _, child := range children {
				waitCtx, cancelChild := context.WithTimeout(authCtx.Context, lease.AsyncCreateBreakWaitTimeout)
				if err := h.LeaseManager.WaitForOtherKeyBreaks(
					waitCtx, lock.FileHandle(child), openFile.ShareName, [16]byte{},
				); err != nil {
					logger.Debug("SET_INFO: dir-rename child break wait completed",
						"child", string(child), "error", err)
				}
				cancelChild()
			}
			if h.anyOpenChild(openFile.MetadataHandle) {
				logger.Debug("SET_INFO: dir rename blocked by open child",
					"dir", openFile.Path)
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}

		// Save old path info for notification before modification
		oldPath := openFile.Path
		oldFileName := openFile.FileName
		oldParentPath := GetParentPath(oldPath)
		// Snapshot src-parent handle so the post-rename dir-lease break can
		// fire on the source parent (line 862 update below overwrites
		// openFile.ParentHandle to toDir).
		srcParentHandle := openFile.ParentHandle

		// Per MS-FSA 2.1.5.14.10: Save mtime/ctime before rename so we can
		// restore them after. Rename should NOT update the file's timestamps.
		restoreTimestamps := h.saveTimestamps(authCtx, openFile.MetadataHandle)

		// Perform the rename/move
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
				renameFilter := NameChangeFilterFor(toName, openFile.IsDirectory)
				if NameChangeFilterFor(oldFileName, openFile.IsDirectory) == FileNotifyChangeStreamName {
					renameFilter = FileNotifyChangeStreamName
				}
				h.NotifyRegistry.NotifyRename(tree.ShareName, oldParentPath, oldFileName, newParentPath, toName, renameFilter)
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

		// Per MS-FSA 2.1.5.14.10 + #470 C3 (smbtorture smb2.dirlease.rename):
		// rename changes directory contents on BOTH source and destination
		// parents. Break Handle + Read dir leases on each (RH → ""), honoring
		// the renamer's ParentLeaseKey suppression from C2. Skip the dst
		// break when src == dst (same-dir rename) to avoid a redundant
		// double-break on a single dir-lease holder.
		h.breakParentDirLeasesForContentChangeOn(authCtx, srcParentHandle, openFile)
		if !bytes.Equal(toDir, srcParentHandle) {
			h.breakParentDirLeasesForContentChangeOn(authCtx, toDir, openFile)
		}

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

		// Capture pre-state to suppress redundant break dispatches when the
		// disposition is reaffirmed (deletePending stays true).
		wasDeletePending := openFile.DeletePending

		// Validate we have parent info for deletion
		if deletePending && len(openFile.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot delete root directory", "path", openFile.Path)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Per MS-FSA 2.1.5.14.3: Setting delete disposition requires DELETE access.
		// Gate consults Open.GrantedAccess (post-DACL intersection at CREATE), not
		// the pre-DACL DesiredAccess — same fix class as #616 (ChangeNotify).
		if deletePending {
			if !hasDeleteAccess(openFile.GrantedAccess) {
				logger.Debug("SET_INFO: delete disposition without DELETE access",
					"path", openFile.Path,
					"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
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

		// Per MS-FSA 2.1.5.14.3 / Samba source3/smbd/smb2_setinfo.c
		// (smbd_smb2_setinfo_lease_break_fsp_check): when delete disposition
		// is *being set* on a non-directory file, strip Handle caching from
		// every other holder's lease (RH -> R, RWH -> RW). The file is on
		// its way out, so cached handles cannot be reopened. Excluding by
		// our own LeaseKey honors the MS-SMB2 3.3.5.9 nobreakself rule.
		// Required by smbtorture smb2.lease.unlink.
		if deletePending && !openFile.IsDirectory && !wasDeletePending &&
			h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
			var excludeOwner *lock.LockOwner
			if openFile.LeaseKey != ([16]byte{}) {
				excludeOwner = &lock.LockOwner{ExcludeLeaseKey: openFile.LeaseKey}
			}
			if breakErr := h.LeaseManager.BreakFileHandleLeasesOnDelete(
				lockFileHandle, openFile.ShareName, excludeOwner,
			); breakErr != nil {
				logger.Debug("SET_INFO: delete-disposition handle lease break failed",
					"path", openFile.Path, "error", breakErr)
			}
		}

		// Mark file for deletion on close and record the DOC setter's
		// parent key for unlink parent-key suppression (#470 C6/C7).
		openFile.DeletePending = deletePending
		if deletePending {
			openFile.DeleteOnCloseParentKey = openFile.ParentLeaseKey
			openFile.HasDeleteOnCloseParentKey = openFile.HasParentLeaseKey
		}
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

		// Break Level II (Read) oplocks held by other clients.
		// Per MS-SMB2 3.3.5.21.2 / MS-FSA 2.1.5.14.4: truncation is a
		// data-modifying operation that invalidates read caches.
		// Required by smbtorture smb2.oplock.batch11/batch12.
		if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
			if breakErr := h.LeaseManager.BreakReadLeasesOnWrite(lockFileHandle, openFile.ShareName, openFile.LeaseKey); breakErr != nil {
				logger.Debug("SET_INFO: oplock break on EOF set failed (non-fatal)", "path", openFile.Path, "error", breakErr)
			}
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

		// Samba parity (fileio.c): SET_INFO EndOfFile also flushes the
		// pending delayed-write window.
		flushSmbDelayedWrite(openFile)
		h.StoreOpenFile(openFile)

		// Break parent directory leases on child EOF change (#470:
		// smb2.dirlease.seteof). Per MS-FSA 2.1.5.14: size changes
		// are visible in READDIR results, invalidating parent-dir
		// Read + Handle caching. Parent-key suppression (C2) applies.
		h.breakParentDirLeasesForContentChange(authCtx, openFile)

		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified, FileNotifyChangeSize)
		}

		return setInfoStatus(types.StatusSuccess), nil

	case types.FilePositionInformation:
		// FILE_POSITION_INFORMATION [MS-FSCC] 2.4.32 (8 bytes)
		// Per MS-FSA 2.1.5.14.23: If InputBufferSize is less than the size of
		// FILE_POSITION_INFORMATION (8 bytes), return STATUS_INFO_LENGTH_MISMATCH.
		if len(buffer) < 8 {
			return setInfoStatus(types.StatusInfoLengthMismatch), nil
		}
		// Network filesystems do not use the server-side position for I/O
		// dispatch (READ/WRITE carry explicit offsets), but the value must
		// round-trip through SET/GET FilePositionInformation and survive
		// durable-handle reconnect (smb2.durable-open.file-position).
		openFile.PositionInfo = smbenc.NewReader(buffer[:8]).ReadUint64()
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileAllocationInformation:
		// Set allocation size - accept but treat as no-op (allocation handled automatically)
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileLinkInformation:
		// FILE_LINK_INFORMATION [MS-FSCC] 2.4.21.2 — hard link creation.
		// Wire format mirrors FILE_RENAME_INFORMATION: ReplaceIfExists (1B),
		// Reserved (7B), RootDirectory (8B), FileNameLength (4B), FileName (UTF-16LE).
		return h.handleFileLinkInformation(authCtx, openFile, buffer)

	case 15: // FileFullEaInformation [MS-FSCC] 2.4.15 - Extended attributes
		// Accept EA writes as a no-op. DittoFS does not persist extended attributes
		// but returning SUCCESS allows ChangeNotify EA tests to proceed.
		logger.Debug("SET_INFO: FileFullEaInformation (no-op)", "path", openFile.Path)
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified, FileNotifyChangeEa)
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

// parseSDOptsForShare resolves the Security Descriptor parse options for the
// share that owns openFile. Returns Windows-canonical defaults
// (CanonicalizeAutoInherited=true) when the share lookup fails — the safe
// fallback per MS-DTYP §2.5.3.4.2. Refs #514 T4.
func (h *Handler) parseSDOptsForShare(shareName string) ParseSDOptions {
	opts := ParseSDOptions{CanonicalizeAutoInherited: true}
	if h.Registry == nil {
		return opts
	}
	share, err := h.Registry.GetShare(shareName)
	if err != nil {
		logger.Debug("SET_INFO: share lookup failed, defaulting to canonicalize",
			"share", shareName, "error", err)
		return opts
	}
	opts.CanonicalizeAutoInherited = share.AclFlagInheritedCanonicalization
	return opts
}

// setSecurityInfo handles SET_INFO for security descriptors.
//
// Parses the binary Security Descriptor from the client, extracts owner/group/ACL,
// and applies the changes to the file via MetadataService.SetFileAttributes.
//
// Per MS-SMB2 §3.3.5.21.3 and MS-FSA §2.1.5.14, the access authorization for
// SET_INFO Security is performed against the OPEN'S granted access mask
// (captured at CREATE time), NOT against the file's current DACL. The new
// SD being installed is irrelevant to the authorization decision — installing
// a DACL that strips WRITE_DAC must still succeed if the handle was opened
// with WRITE_DAC. Section→bit mapping (mirrors Samba
// source3/smbd/smb2_setinfo.c::smbd_smb2_setinfo_security):
//
//	SECINFO_DACL  → SEC_STD_WRITE_DAC
//	SECINFO_OWNER → SEC_STD_WRITE_OWNER
//	SECINFO_GROUP → SEC_STD_WRITE_OWNER
//	SECINFO_SACL  → ACCESS_SYSTEM_SECURITY
//
// Each requested section is gated independently; the request is denied as a
// whole if any requested section lacks the corresponding bit on the handle.
// Refs #559.
func (h *Handler) setSecurityInfo(
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	additionalInfo uint32,
	buffer []byte,
) (*SetInfoResponse, error) {
	if len(buffer) == 0 {
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	// MS-SMB2 §3.3.5.21.3: authorize each requested SD section against the
	// open's GrantedAccess. The new SD is not consulted — the handle's mask
	// already captured the DACL-evaluated rights at CREATE time.
	if status, ok := checkSetInfoSecurityAccess(openFile.GrantedAccess, additionalInfo); !ok {
		logger.Debug("SET_INFO Security: handle lacks required access",
			"path", openFile.Path,
			"additionalInfo", fmt.Sprintf("0x%x", additionalInfo),
			"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
		return setInfoStatus(status), nil
	}

	// Per-share opt-out of MS-DTYP §2.5.3.4.2 canonicalization. Default true
	// matches Windows (and Samba's default). The toggle was populated onto
	// the runtime Share at AddShare time (refs #514 T1).
	opts := h.parseSDOptsForShare(openFile.ShareName)

	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptorWithOptions(buffer, opts)
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

	if (additionalInfo & DACLSecurityInformation) != 0 {
		if fileACL != nil {
			setAttrs.ACL = fileACL
			changed = true
		} else {
			// DACL section requested but no DACL in SD → null DACL
			setAttrs.ACL = &acl.ACL{NullDACL: true}
			changed = true
		}
	}

	if !changed {
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified, FileNotifyChangeSecurity)
		}
		return setInfoStatus(types.StatusSuccess), nil
	}

	metaSvc := h.Registry.GetMetadataService()
	err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
	if err != nil {
		logger.Debug("SET_INFO: failed to set security info", "path", openFile.Path, "error", err)
		return setInfoStatus(common.MapToSMB(err)), nil
	}

	if h.NotifyRegistry != nil {
		h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), openFile.FileName, FileActionModified, FileNotifyChangeSecurity)
	}

	return setInfoStatus(types.StatusSuccess), nil
}

// checkSetInfoSecurityAccess maps requested SECURITY_INFORMATION sections to
// the access mask bits MS-SMB2 §3.3.5.21.3 / MS-FSA §2.1.5.14 require on the
// open's GrantedAccess, and verifies each requested section against the mask.
//
// Returns (StatusSuccess, true) when every requested section has the matching
// bit on the open; (StatusAccessDenied, false) otherwise. An additionalInfo
// of zero authorizes (no sections to gate).
//
// Mirrors Samba source3/smbd/smb2_setinfo.c::smbd_smb2_setinfo_security and
// source3/smbd/posix_acls.c::set_nt_acl — both consult `fsp->access_mask`
// (the equivalent of OpenFile.GrantedAccess), never re-evaluate against the
// file's current DACL. Refs #559.
func checkSetInfoSecurityAccess(grantedAccess, additionalInfo uint32) (types.Status, bool) {
	if additionalInfo&DACLSecurityInformation != 0 {
		if !hasAccessRight(grantedAccess, uint32(types.WriteDac)) {
			return types.StatusAccessDenied, false
		}
	}
	// SECINFO_OWNER and SECINFO_GROUP both require WRITE_OWNER per MS-DTYP
	// §2.5.3.3 (the algorithm folds owner+group under one privilege gate).
	if additionalInfo&(OwnerSecurityInformation|GroupSecurityInformation) != 0 {
		if !hasAccessRight(grantedAccess, uint32(types.WriteOwner)) {
			return types.StatusAccessDenied, false
		}
	}
	if additionalInfo&SACLSecurityInformation != 0 {
		if !hasAccessRight(grantedAccess, uint32(types.AccessSystemSecurity)) {
			return types.StatusAccessDenied, false
		}
	}
	return types.StatusSuccess, true
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
	if len(openFile.ParentHandle) == 0 {
		return
	}
	h.breakParentDirLeasesForContentChangeOn(authCtx, openFile.ParentHandle, openFile)
}

// breakParentDirLeasesForContentChangeOn is the multi-parent variant used by
// the rename branch (#470 C3): break Handle + Read leases on an arbitrary
// directory handle (src-parent or dst-parent), honoring the originating
// handle's ParentLeaseKey suppression from C2.
//
// Per Samba dirlease_should_break: ClientID is NOT used for suppression —
// a same-client SET_INFO / WRITE / CLOSE / RENAME with a mismatched (or
// absent) ParentLeaseKey MUST still break the parent dir lease held by that
// same client. The smbtorture dirlease tests (setinfo, rename, hardlink,
// unlink, v2_request) all exercise same-client scenarios where the dir
// lease is expected to break when the parent key doesn't match.
func (h *Handler) breakParentDirLeasesForContentChangeOn(authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, openFile *OpenFile) {
	if h.LeaseManager == nil || len(parentHandle) == 0 {
		return
	}

	parentLockHandle := lock.FileHandle(parentHandle)

	// Apply parent-key suppression only (MS-SMB2 §3.3.4.20, #470 C2): if
	// the originating handle's CREATE carried an RqLs with ParentLeaseKey
	// set, the matching parent dir lease MUST NOT be broken. No ClientID
	// exclusion — same-client breaks fire when the key doesn't match.
	excludeParentKey := openFile.ParentLeaseKey
	hasExcludeKey := openFile.HasParentLeaseKey

	if breakErr := h.LeaseManager.BreakParentHandleLeasesOnCreate(authCtx.Context, parentLockHandle, openFile.ShareName, "", excludeParentKey, hasExcludeKey); breakErr != nil {
		logger.Debug("SET_INFO: parent directory Handle lease break failed", "path", openFile.Path, "parent", fmt.Sprintf("%x", parentHandle), "error", breakErr)
	}
	if breakErr := h.LeaseManager.BreakParentReadLeasesOnModify(authCtx.Context, parentLockHandle, openFile.ShareName, "", excludeParentKey, hasExcludeKey); breakErr != nil {
		logger.Debug("SET_INFO: parent directory Read lease break failed", "path", openFile.Path, "parent", fmt.Sprintf("%x", parentHandle), "error", breakErr)
	}
}

// breakDstParentDirHandleLeasesForRename strips the Handle bit only (RH -> R)
// on dst-parent dir leases held by holders that conflict with the rename's
// implicit DELETE+FILE_ADD_FILE open. Called BEFORE the dst-parent share-mode
// conflict check so the break notification is observed even when the conflict
// surfaces STATUS_SHARING_VIOLATION (smbtorture smb2.dirlease.rename_dst_parent
// stage 1, lease.c:7331). Read caching is preserved (RH -> R) — the rename
// hasn't mutated directory contents yet, only the dst-parent's Handle caching
// is invalidated by the implicit destructive open intent.
//
// Honors ParentLeaseKey suppression from C2 only — no ClientID exclusion,
// same-client dir leases break when the key doesn't match.
func (h *Handler) breakDstParentDirHandleLeasesForRename(authCtx *metadata.AuthContext, dstParent metadata.FileHandle, openFile *OpenFile) {
	if h.LeaseManager == nil || len(dstParent) == 0 {
		return
	}
	parentLockHandle := lock.FileHandle(dstParent)
	excludeParentKey := openFile.ParentLeaseKey
	hasExcludeKey := openFile.HasParentLeaseKey
	if breakErr := h.LeaseManager.BreakParentHandleLeasesOnCreate(authCtx.Context, parentLockHandle, openFile.ShareName, "", excludeParentKey, hasExcludeKey); breakErr != nil {
		logger.Debug("SET_INFO: dst-parent dir Handle lease pre-break failed", "path", openFile.Path, "dstParent", fmt.Sprintf("%x", dstParent), "error", breakErr)
	}
}

// FileLinkInfo represents FILE_LINK_INFORMATION [MS-FSCC] 2.4.21.2.
// The wire format mirrors FILE_RENAME_INFORMATION (same byte layout).
type FileLinkInfo struct {
	// ReplaceIfExists indicates whether to replace an existing file at the
	// destination. Hard-link creation rejects collisions when this is false.
	ReplaceIfExists bool

	// RootDirectory is the file handle of the destination directory, or all
	// zeros to indicate FileName is a full path relative to the share root.
	RootDirectory [8]byte

	// FileName is the path to the new hard link (UTF-16LE on the wire).
	FileName string
}

// DecodeFileLinkInfo parses FILE_LINK_INFORMATION [MS-FSCC] 2.4.21.2.
// Returns an error if the buffer is less than 20 bytes (fixed header) or the
// declared FileNameLength would read past buffer end.
func DecodeFileLinkInfo(buffer []byte) (*FileLinkInfo, error) {
	if len(buffer) < 20 {
		return nil, fmt.Errorf("buffer too short for FILE_LINK_INFORMATION: %d bytes", len(buffer))
	}

	info := &FileLinkInfo{
		ReplaceIfExists: buffer[0] != 0,
	}
	// Reserved (7 bytes at offset 1-7) - skip
	copy(info.RootDirectory[:], buffer[8:16])

	r := smbenc.NewReader(buffer[16:20])
	fileNameLength := r.ReadUint32()

	if len(buffer) < 20+int(fileNameLength) {
		return nil, fmt.Errorf("buffer too short for filename: need %d, have %d", 20+fileNameLength, len(buffer))
	}
	if fileNameLength > 0 {
		info.FileName = decodeUTF16LE(buffer[20 : 20+fileNameLength])
	}
	return info, nil
}

// handleFileLinkInformation implements SET_INFO FileLinkInformation [MS-FSCC]
// 2.4.21.2: create a new hard link to the open file in the requested
// destination directory.
//
// Per MS-FSA 2.1.5.14.5: the operation creates a NEW directory entry in the
// destination directory that references the same file ID as the open file.
// Returns STATUS_OBJECT_NAME_COLLISION if the destination already exists and
// ReplaceIfExists is FALSE; STATUS_FILE_IS_A_DIRECTORY if the open file is a
// directory (hard-linking directories is forbidden, MS-FSA 2.1.5.14.5).
//
// Directory-lease coordination (#470 C5 — smb2.dirlease.hardlink): a hardlink
// is an add-entry in the destination parent. We thread the open file's RqLs
// ParentLeaseKey into the auth context so:
//   - MetadataService.notifyDirChange forwards it to OnDirChange, which
//     suppresses the matching dst-parent dir lease (parent-key match).
//   - LeaseManager.BreakParentHandleLeasesOnCreate / BreakParentReadLeasesOnModify
//     called directly on the dst-parent honor the same suppression rule.
//
// This mirrors Samba `dlt_hardlinks` matrix (source4/torture/smb2/lease.c):
// same-dir + same-parent-key suppresses; same-dir + different-parent-key
// breaks; cross-dir always breaks the dst parent unless its key matches.
func (h *Handler) handleFileLinkInformation(
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	buffer []byte,
) (*SetInfoResponse, error) {
	linkInfo, err := DecodeFileLinkInfo(buffer)
	if err != nil {
		logger.Debug("SET_INFO: failed to decode link info", "error", err)
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	// Hard-linking a directory is forbidden (MS-FSA 2.1.5.14.5).
	if openFile.IsDirectory {
		logger.Debug("SET_INFO: hardlink on directory rejected",
			"path", openFile.Path)
		return setInfoStatus(types.StatusFileIsADirectory), nil
	}

	// Normalize path separators (Windows uses backslash, we use forward slash).
	newPath := strings.ReplaceAll(linkInfo.FileName, "\\", "/")
	newPath = strings.TrimPrefix(newPath, "/")
	if newPath == "" {
		logger.Debug("SET_INFO: hardlink with empty destination name")
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	// Resolve destination directory + link name.
	var dstDir metadata.FileHandle
	var linkName string

	var zeroRootDir [8]byte
	if !bytes.Equal(linkInfo.RootDirectory[:], zeroRootDir[:]) {
		// Non-zero RootDirectory: FileName is relative to it. We don't yet
		// resolve FileId handles to directory handles (parity with rename);
		// fall back to same-directory link.
		logger.Debug("SET_INFO: hardlink with non-zero RootDirectory (using same-dir fallback)",
			"rootDirectory", fmt.Sprintf("%x", linkInfo.RootDirectory))
		dstDir = openFile.ParentHandle
		linkName = path.Base(newPath)
	} else {
		tree, ok := h.GetTree(openFile.TreeID)
		if !ok {
			logger.Debug("SET_INFO: invalid tree for hardlink", "treeID", openFile.TreeID)
			return setInfoStatus(types.StatusInvalidHandle), nil
		}
		rootHandle, err := h.Registry.GetRootHandle(tree.ShareName)
		if err != nil {
			logger.Debug("SET_INFO: failed to get root handle for hardlink", "error", err)
			return setInfoStatus(types.StatusObjectPathNotFound), nil
		}

		linkName = path.Base(newPath)
		dirPath := path.Dir(newPath)
		if dirPath == "." || dirPath == "" || dirPath == "/" {
			dstDir = rootHandle
		} else {
			dstDir, err = h.walkPath(authCtx, rootHandle, dirPath)
			if err != nil {
				logger.Debug("SET_INFO: hardlink destination path not found",
					"path", dirPath, "error", err)
				return setInfoStatus(types.StatusObjectPathNotFound), nil
			}
		}
	}

	// Replace-if-exists for hardlink is rare (most clients pass FALSE). Honor
	// it by attempting a delete of the existing destination before linking.
	// If ReplaceIfExists=false and the target exists, CreateHardLink returns
	// ErrAlreadyExists → STATUS_OBJECT_NAME_COLLISION via common.MapToSMB.
	metaSvc := h.Registry.GetMetadataService()
	if linkInfo.ReplaceIfExists {
		if existing, lookupErr := metaSvc.Lookup(authCtx, dstDir, linkName); lookupErr == nil && existing != nil {
			if _, rmErr := metaSvc.RemoveFile(authCtx, dstDir, linkName); rmErr != nil {
				logger.Debug("SET_INFO: hardlink replace failed to remove existing",
					"name", linkName, "error", rmErr)
				return setInfoStatus(common.MapToSMB(rmErr)), nil
			}
		}
	}

	// #470 C5: thread the open file's ParentLeaseKey into the auth context so
	// MetadataService.notifyDirChange forwards it to OnDirChange and the
	// dir-lease parent-key suppression rule (MS-SMB2 §3.3.4.20) skips the
	// matching parent dir lease (same-key holder does not get broken).
	PropagateOpenFileParentLeaseKey(authCtx, openFile)

	if err := metaSvc.CreateHardLink(authCtx, dstDir, linkName, openFile.MetadataHandle); err != nil {
		logger.Debug("SET_INFO: CreateHardLink failed",
			"src", openFile.Path, "dst", newPath, "error", err)
		return setInfoStatus(common.MapToSMB(err)), nil
	}

	// Break Handle/Read leases on the destination parent (MS-FSA 2.1.5.14:
	// directory contents changed). Parent-key suppression only — no ClientID
	// exclusion per Samba dirlease_should_break.
	if h.LeaseManager != nil {
		dstParentLock := lock.FileHandle(dstDir)
		excludeParentKey := openFile.ParentLeaseKey
		hasExcludeKey := openFile.HasParentLeaseKey
		if breakErr := h.LeaseManager.BreakParentHandleLeasesOnCreate(
			authCtx.Context, dstParentLock, openFile.ShareName,
			"", excludeParentKey, hasExcludeKey,
		); breakErr != nil {
			logger.Debug("SET_INFO: hardlink dst-parent Handle lease break failed",
				"dst", newPath, "error", breakErr)
		}
		if breakErr := h.LeaseManager.BreakParentReadLeasesOnModify(
			authCtx.Context, dstParentLock, openFile.ShareName,
			"", excludeParentKey, hasExcludeKey,
		); breakErr != nil {
			logger.Debug("SET_INFO: hardlink dst-parent Read lease break failed",
				"dst", newPath, "error", breakErr)
		}
	}

	// Notify change-notify watchers: an entry was added in the destination
	// parent directory.
	if h.NotifyRegistry != nil {
		tree, ok := h.GetTree(openFile.TreeID)
		if ok {
			dstParentPath := GetParentPath(newPath)
			if dstParentPath == "" || dstParentPath == "." {
				dstParentPath = "/"
			}
			h.NotifyRegistry.NotifyChange(tree.ShareName, dstParentPath, linkName, FileActionAdded, FileNotifyChangeFileName)
		}
	}

	logger.Debug("SET_INFO: hardlink created",
		"src", openFile.Path, "dst", newPath, "name", linkName)
	return setInfoStatus(types.StatusSuccess), nil
}
