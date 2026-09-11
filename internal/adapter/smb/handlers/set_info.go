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
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

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

// FileRenameInfo represents FILE_RENAME_INFORMATION [MS-FSCC] 2.4.42 (FileRenameInformation).
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

// DecodeFileRenameInfo parses FILE_RENAME_INFORMATION [MS-FSCC] 2.4.42 (FileRenameInformation).
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

// decodeEndOfFileInfo decodes FILE_END_OF_FILE_INFORMATION [MS-FSCC] 2.4.14 (FileEndOfFileInformation).

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
			types.FileFullEaInformation:
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
	if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
		return setInfoStatus(status), nil
	}

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
		return h.setFileInfoFromStore(ctx, authCtx, openFile, types.FileInfoClass(req.FileInfoClass), req.Buffer)
	case types.SMB2InfoTypeSecurity:
		// Authorise the SD-write under the opener's identity rather than
		// the session's current identity. MS-SMB2 §3.3.5.5.3 freezes the
		// open's SecurityContext at CREATE; the handler-level WRITE_DAC /
		// WRITE_OWNER / ACCESS_SYSTEM_SECURITY check inside setSecurityInfo
		// has already gated on OpenFile.GrantedAccess, so the metadata
		// ownership check that BuildAuthContext-from-session would trip
		// after a re-auth to a different principal would be wrong. See
		// smbtorture smb2.session.reauth4 / reauth5. Falls back to the
		// session-current authCtx when no opener snapshot exists.
		secAuthCtx := h.buildOpenerAuthContext(ctx, openFile)
		if secAuthCtx == nil {
			secAuthCtx = authCtx
		}
		return h.setSecurityInfo(secAuthCtx, openFile, req.AdditionalInfo, req.Buffer)
	default:
		return setInfoStatus(types.StatusInvalidParameter), nil
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// setFileInfoFromStore handles setting file information using metadata store.

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

// DecodeFileLinkInfo parses FILE_LINK_INFORMATION [MS-FSCC] 2.4.28.2 (FileLinkInformation for the SMB2 Protocol).
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
// 2.4.28 (FileLinkInformation): create a new hard link to the open file in the requested
// destination directory.
//
// Per MS-FSA 2.1.5.15.7 ("FileLinkInformation"): the operation creates a NEW directory entry in the
// destination directory that references the same file ID as the open file.
// Returns STATUS_OBJECT_NAME_COLLISION if the destination already exists and
// ReplaceIfExists is FALSE; STATUS_FILE_IS_A_DIRECTORY if the open file is a
// directory (hard-linking directories is forbidden, MS-FSA 2.1.5.15.7 ("FileLinkInformation")).
//
// Directory-lease coordination (smb2.dirlease.hardlink): a hardlink
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
	ctx *SMBHandlerContext,
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	buffer []byte,
) (*SetInfoResponse, error) {
	linkInfo, err := DecodeFileLinkInfo(buffer)
	if err != nil {
		logger.Debug("SET_INFO: failed to decode link info", "error", err)
		return setInfoStatus(types.StatusInvalidParameter), nil
	}

	// Hard-linking a directory is forbidden (MS-FSA 2.1.5.15.7 ("FileLinkInformation")).
	if openFile.IsDirectory {
		logger.Debug("SET_INFO: hardlink on directory rejected",
			"path", openFile.Name().Path)
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
		dstDir = openFile.Name().ParentHandle
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
	// ErrAlreadyExists → STATUS_OBJECT_NAME_COLLISION via smb/types.StatusForErr.
	metaSvc := h.Registry.GetMetadataService()
	if linkInfo.ReplaceIfExists {
		if existing, matchedName, lookupErr := metaSvc.LookupCaseInsensitive(authCtx, dstDir, linkName); lookupErr == nil && existing != nil {
			// A destination that already names the file being linked is the
			// requested end state, so there is nothing to do. Removing it
			// would drop the inode's last link and free its content, and the
			// CreateHardLink below would then resurrect the name over bytes
			// that no longer exist. Move takes the same shortcut for a rename
			// onto its own name.
			existingHandle, encErr := metadata.EncodeFileHandle(existing)
			if encErr != nil {
				// Identity is unprovable, so the removal below cannot be shown
				// to be safe. Refuse rather than fall through into it: the
				// wrong branch here destroys the caller's content.
				logger.Debug("SET_INFO: hardlink replace cannot identify existing destination",
					"name", matchedName, "error", encErr)
				return setInfoStatus(types.StatusInvalidParameter), nil
			}
			if bytes.Equal(existingHandle, openFile.MetadataHandle) {
				return setInfoStatus(types.StatusSuccess), nil
			}

			removed, _, rmErr := metaSvc.RemoveFile(authCtx, dstDir, matchedName)
			if rmErr != nil {
				logger.Debug("SET_INFO: hardlink replace failed to remove existing",
					"name", matchedName, "error", rmErr)
				return setInfoStatus(types.StatusForErr(rmErr)), nil
			}
			// RemoveFile drops the name and the inode but never the bytes; its
			// PayloadID is empty whenever the content must survive. Left
			// unreleased, the replaced file's records stay indexed as live in
			// the local tier, where no reclamation path can reach them.
			if removed != nil {
				h.purgeBlockStorePayload(authCtx.Context, dstDir, removed.PayloadID, matchedName, "SET_INFO hardlink replace")
			}
		}
	}

	// Thread the open file's ParentLeaseKey into the auth context so
	// MetadataService.notifyDirChange forwards it to OnDirChange and the
	// dir-lease parent-key suppression rule (Samba `dirlease_should_break`) skips the
	// matching parent dir lease (same-key holder does not get broken).
	PropagateOpenFileParentLeaseKey(authCtx, openFile)

	if _, err := metaSvc.CreateHardLink(authCtx, dstDir, linkName, openFile.MetadataHandle); err != nil {
		logger.Debug("SET_INFO: CreateHardLink failed",
			"src", openFile.Name().Path, "dst", newPath, "error", err)
		return setInfoStatus(types.StatusForErr(err)), nil
	}

	// Break parent directory leases on the destination parent to None
	// (MS-FSA 2.1.5.15.7 ("FileLinkInformation"): directory contents changed). Parent-key suppression
	// only — no ClientID exclusion per Samba dirlease_should_break. Single
	// break-to-None matches Samba `contend_dirleases` / `do_dirlease_break_to_none`
	// — required by smbtorture hardlink samedir-{wrong,no}-parent-leaskey
	// which expect exactly one LEASE_BREAK per holder. Fire-and-forget per
	// Samba `send_break_to_none`: the test sets lease_skip_ack=true AFTER
	// the setinfo returns and replays the captured ACK; waiting inline would
	// force-complete the lease on timeout and the replay would hit
	// STATUS_UNSUCCESSFUL.
	if h.LeaseManager != nil {
		logger.Debug("SET_INFO: hardlink dst-parent dir lease break-to-None recorded", "dst", newPath)
		dispatch := h.LeaseManager.PrepareParentDirLeaseBreakOnContentChange(
			lock.FileHandle(dstDir), openFile.ShareName, "",
			openFile.ParentLeaseKey, openFile.HasParentLeaseKey)
		// Defer until after the SET_INFO response is on the wire so the
		// client's lease handler runs in the next tevent cycle with
		// lease_skip_ack=true (see breakParentDirLeasesForContentChangeOn
		// for the Samba-parity rationale).
		if ctx != nil {
			AppendPostSend(ctx, dispatch)
		} else {
			dispatch()
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
		"src", openFile.Name().Path, "dst", newPath, "name", linkName)
	return setInfoStatus(types.StatusSuccess), nil
}

// ============================================================================
// FILE_FULL_EA_INFORMATION decoding (MS-FSCC §2.4.16 ("FileFullEaInformation"))
// ============================================================================

// reservedACLXattrName is the xattr name DittoFS reserves for the server's
// stored security descriptor blob — the EA-API equivalent of Samba's
// `security.NTACL`. Writes targeting this name through FileFullEaInformation
// MUST be rejected with STATUS_ACCESS_DENIED so a client cannot tamper with
// the stored ACL through the EA channel. Reads (FileFullEaInformation in
// QUERY_INFO) already omit this name from enumeration. The torture client
// learns the name via the `--option=acl_xattr_name=security.NTACL` setting
// (smbtorture smb2.ea.acl_xattr).
//
// The name is matched case-insensitively because EA names are NTFS-style
// case-insensitive on the wire; MS-FSCC §2.4.16 ("FileFullEaInformation") defines the
// structure but states no name-matching rule. Samba's vfs_acl_xattr uses a fixed lower-case
// constant; smbtorture's torture_setting_string returns the literal it was
// configured with. Match either casing.

const reservedACLXattrName = "security.NTACL"

// isReservedACLXattrName reports whether name (an EA name in canonical NT
// form, no domain prefix) matches the reserved ACL xattr slot. NT EA names
// are case-insensitive, so the comparison is folded.
