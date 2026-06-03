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
func (h *Handler) setFileInfoFromStore(
	ctx *SMBHandlerContext,
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
		// Likewise, FILE_ATTRIBUTE_SPARSE_FILE is set only via FSCTL_SET_SPARSE.
		// Preserve both FSCTL-managed bits so SET_INFO does not accidentally
		// clear compression or sparse state.
		if fileAttrs != 0 {
			mode := SMBModeFromAttrs(fileAttrs, openFile.IsDirectory)
			// Preserve FSCTL-managed bits from existing metadata:
			// modeDOSCompressed (FSCTL_SET_COMPRESSION) and modeDOSSparse
			// (FSCTL_SET_SPARSE) are both controlled exclusively by IOCTLs —
			// they must not be cleared by a FileBasicInformation SET_INFO that
			// only intends to update DOS attributes (HIDDEN, READONLY, etc.).
			if curFile, curErr := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle); curErr == nil {
				mode |= curFile.Mode & (modeDOSCompressed | modeDOSSparse)
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
		//
		// Per NTFS: ADS (alternate data streams) share the base file's
		// timestamps. When the open is for a stream, capture the base file's
		// timestamps so the frozen value reflects the base file, not the
		// stream entry.
		//
		// We also need the pre-image whenever the caller sends ChangeTime = 0
		// ("don't change") alongside any other mutation — the metadata layer
		// auto-bumps Ctime on any modification, but smbtorture `smb2.setinfo`
		// (setinfo.c:203) asserts the previously-set Ctime survives a no-op
		// SET_INFO. The mutation can be attribute-only, timestamp-only
		// (e.g. LastWriteTime), or both; pinning Ctime to the current value
		// suppresses the auto-bump in all of these cases.
		anyBasicMutation := fileAttrs != 0 || creationFT != 0 || atimeFT != 0 || mtimeFT != 0
		// Serialize concurrent SET_INFO BasicInfo / READ / WRITE / QUERY_INFO on
		// the same handle (#606). The freeze flags (BtimeFrozen / MtimeFrozen /
		// CtimeFrozen / AtimeFrozen) plus their Frozen* timestamp pointers and
		// the SMB delayed-write fields are read and written here, and observed
		// by QUERY_INFO / READ / WRITE / COPYCHUNK on parallel goroutines. We
		// release before any callbacks that themselves take openFile.mu
		// (breakParentDirLeasesForContentChange via restoreParentDirFrozenTimestamps).
		openFile.mu.Lock()
		needPreFile := hasFreezeOrUnfreeze || (ctimeFT == 0 && anyBasicMutation && !openFile.CtimeFrozen)
		var preFile *metadata.File
		if needPreFile {
			var err error
			if colonIdx := strings.Index(openFile.FileName, ":"); colonIdx > 0 && len(openFile.ParentHandle) > 0 {
				// ADS: capture base file timestamps.
				baseFileName := openFile.FileName[:colonIdx]
				if baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, openFile.ParentHandle, baseFileName); baseFile != nil {
					preFile = baseFile
				}
			}
			if preFile == nil {
				preFile, err = metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
				if err != nil {
					logger.Warn("SET_INFO: failed to read file for freeze/unfreeze", "path", openFile.Path, "error", err)
				}
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

		// ChangeTime stickiness: once SET_INFO BasicInfo has been used on a
		// handle to mutate attributes or any timestamp, the metadata layer's
		// automatic Ctime bump (file_modify.go: `if attrs.Ctime == nil {
		// file.Ctime = now }`) must NOT overwrite a previously-set ChangeTime
		// when the caller sends ChangeTime = 0. Pin Ctime to the current value
		// so the auto-bump is a no-op. smbtorture `smb2.setinfo`
		// (setinfo.c:203) asserts this for attribute changes; the same
		// constraint applies to timestamp-only mutations (e.g. LastWriteTime).
		if ctimeFT == 0 && setAttrs.Ctime == nil && !openFile.CtimeFrozen && preFile != nil {
			setAttrs.Ctime = &preFile.Ctime
		}

		// Per MS-FSA 2.1.5.14.2: When FileAttributes change, the object store
		// SHOULD also update LastWriteTime. The metadata layer only auto-updates
		// Ctime (POSIX semantics), so we handle Mtime auto-update here.
		// Skip if: Mtime is being explicitly set, has a sentinel, or is frozen.
		if fileAttrs != 0 && setAttrs.Mtime == nil && mtimeFT == 0 && !openFile.MtimeFrozen {
			now := time.Now()
			setAttrs.Mtime = &now
		}

		if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs); err != nil {
			openFile.mu.Unlock() // release before returning; refs #606.
			logger.Debug("SET_INFO: failed to set basic info", "path", openFile.Path, "error", err)
			return setInfoStatus(common.MapToSMB(err)), nil
		}

		// NTFS contract: SET_INFO BasicInformation on an ADS handle MUST be
		// observable on the base file via QUERY_INFO, and vice-versa — base
		// and stream always report identical FileAttributes.
		// smb2.streams.attributes2 (source4/torture/smb2/streams.c) round-
		// trips this both ways: setting attribs through the stream and
		// re-querying the base must agree, and the reverse. QUERY_INFO on
		// a stream already resolves to base via resolveBaseFileAttrForADS,
		// so the only piece that closes the loop is propagating the writes
		// from the stream's SET_INFO back onto the base. The propagation:
		//
		//   - Forwards CreationTime / LastAccessTime / LastWriteTime /
		//     ChangeTime onto the base. nil-valued pointers are skipped,
		//     so a "timestamp-only" SET_INFO that leaves FileAttributes=0
		//     does not touch the base's DOS bits or Hidden flag.
		//   - Forwards the Hidden flag when FileAttributes was set.
		//   - Overlays only the four explicit DOS bits
		//     (modeDOSExplicit | modeDOSArchive | modeDOSSystem |
		//     modeDOSReadonly) from the stream's computed mode onto the
		//     base's existing mode. All other bits in the base's mode
		//     are preserved:
		//       * POSIX permission bits (0o7777) — an out-of-band NFS
		//         chmod must survive a stream SET_INFO.
		//       * modeDOSCompressed (0x40000) — FSCTL-managed (FSCTL_SET_COMPRESSION)
		//         * modeDOSSparse (0x200000) — FSCTL-managed (FSCTL_SET_SPARSE)
		//         Both live on the base file and are never derived from FileAttributes.
		//
		// The base SetFileAttributes call is fire-and-forget: a failure to
		// propagate is logged at the metadata layer and does not roll back
		// the stream's own SET_INFO (the stream is the explicitly-targeted
		// handle and its write has already succeeded above).
		if colonIdx := strings.Index(openFile.FileName, ":"); colonIdx > 0 && len(openFile.ParentHandle) > 0 {
			baseFileName := openFile.FileName[:colonIdx]
			if baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, openFile.ParentHandle, baseFileName); baseFile != nil {
				basePropagate := &metadata.SetAttrs{
					Mtime:        setAttrs.Mtime,
					Ctime:        setAttrs.Ctime,
					Atime:        setAttrs.Atime,
					CreationTime: setAttrs.CreationTime,
					Hidden:       setAttrs.Hidden,
				}
				if setAttrs.Mode != nil {
					// Strip the stream-derived POSIX bits and keep only the
					// DOS attribute bits (Explicit/Archive/System/Readonly).
					// modeDOSCompressed lives in the base file's mode and is
					// FSCTL-managed, so leave it untouched.
					const dosBits = modeDOSExplicit | modeDOSArchive | modeDOSSystem | modeDOSReadonly
					newMode := (baseFile.Mode &^ dosBits) | (*setAttrs.Mode & dosBits)
					if newMode != baseFile.Mode {
						basePropagate.Mode = &newMode
					}
				}
				if basePropagate.Mtime != nil || basePropagate.Ctime != nil ||
					basePropagate.Atime != nil || basePropagate.CreationTime != nil ||
					basePropagate.Mode != nil || basePropagate.Hidden != nil {
					if baseHandle, encErr := metadata.EncodeFileHandle(baseFile); encErr == nil {
						_, _ = metaSvc.SetFileAttributes(authCtx, baseHandle, basePropagate)
					}
				}
			}
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

		// Per MS-FSA 2.1.5.14.2: an explicit (non-zero, non-sentinel) timestamp
		// set suppresses the automatic update of that field until the next
		// explicit handle operation that would update it. smbtorture
		// `smb2.setinfo` sets all four timestamps in one BasicInfo call, then a
		// follow-up BasicInfo call mutates only FileAttributes (attrib=NORMAL)
		// while sending zero timestamps, and asserts the previously-set values
		// survive. Without sticky state the attribute-change path auto-bumps
		// LastWriteTime (set_info.go) and ChangeTime (metadata file_modify.go),
		// clobbering the explicit values. We reuse the existing freeze mechanism
		// (Frozen* flags + Frozen* pointers): an explicit set freezes the field
		// to the value just written, so the field==0 re-pin block above and the
		// auto-bump guards (mtimeFT==0 && !MtimeFrozen / ctimeFT==0 &&
		// !CtimeFrozen) suppress the bump on the next operation. A subsequent
		// sentinel (-1/-2) or explicit value on the same field overrides this,
		// exactly as the sentinel switch above does.
		freezeOnExplicitSet := func(ft uint64, frozen *bool, frozenVal **time.Time, val *time.Time, label string) {
			if val == nil || ft == 0 || isFiletimeSentinel(ft) {
				return
			}
			v := *val
			*frozen = true
			*frozenVal = &v
			logger.Debug("SET_INFO: explicit set pins timestamp", "field", label, "path", openFile.Path, "value", v)
		}
		freezeOnExplicitSet(creationFT, &openFile.BtimeFrozen, &openFile.FrozenBtime, setAttrs.CreationTime, "CreationTime")
		freezeOnExplicitSet(mtimeFT, &openFile.MtimeFrozen, &openFile.FrozenMtime, setAttrs.Mtime, "LastWriteTime")
		freezeOnExplicitSet(ctimeFT, &openFile.CtimeFrozen, &openFile.FrozenCtime, setAttrs.Ctime, "ChangeTime")
		freezeOnExplicitSet(atimeFT, &openFile.AtimeFrozen, &openFile.FrozenAtime, setAttrs.Atime, "LastAccessTime")

		// Samba parity (fileio.c): any SET_INFO BasicInfo — even with all
		// zero timestamps — collapses the pending delayed-write window so
		// the post-write Mtime becomes visible. An explicit, non-sentinel
		// write_time also makes the value sticky until close. We still hold
		// openFile.mu (write) here, so use the *Locked helpers.
		flushSmbDelayedWriteLocked(openFile)
		if setAttrs.Mtime != nil && mtimeFT != 0 && !isFiletimeSentinel(mtimeFT) {
			setSmbStickyWriteTimeLocked(openFile, *setAttrs.Mtime)
		}
		openFile.mu.Unlock()
		h.StoreOpenFile(openFile)

		// Break parent directory leases on child metadata change (#470:
		// smb2.dirlease.set{atime,btime,ctime,mtime,dos}). Per MS-FSA
		// 2.1.5.14: any child SET_INFO that modifies file attributes or
		// timestamps changes what READDIR returns, invalidating parent-dir
		// Read + Handle caching. Parent-key suppression (C2) flows through
		// the same breakParentDirLeasesForContentChange plumbing.
		h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)

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
				h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), notifyStreamName(openFile.FileName), FileActionModified, nf)
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
			// The current file is an ADS: "basefile:streamname"
			baseName := openFile.FileName
			if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 {
				baseName = baseName[:colonIdx]
			}

			// Strip :$DATA type suffix from rename target.
			if strings.HasSuffix(strings.ToUpper(newPath), ":$DATA") {
				newPath = newPath[:len(newPath)-len(":$DATA")]
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
			_, err = metaSvc.Move(authCtx, toDir, openFile.FileName, toDir, toName)
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
					h.NotifyRegistry.NotifyRename(tree.ShareName, oldParentPath, notifyStreamName(oldFileName), newParentPath, notifyStreamName(toName), renameFilter)
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
			h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)

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
		// Conflict-gated dst-parent dir-lease pre-break (smbtorture
		// smb2.dirlease.rename_dst_parent, lease.c:7331): when an existing
		// holder on dst-parent denies the implicit destructive open with
		// SHARING_VIOLATION, the dst-parent's RH dir-lease holder must observe
		// an RH→R strip-Handle break first; the break may give the holder a
		// chance to close its conflicting handle. After the break drains we
		// re-check share-mode — on phase-2 of the test the holder's handler
		// closes its handle inside our wait so the recheck observes the
		// shrunk open table and the rename proceeds.
		//
		// On a clean rename (no conflict to start with), Skip the strip-H
		// pre-break — the post-rename content-change break on dst-parent
		// (single LEASE_BREAK to None per Samba `contend_dirleases`) already
		// invalidates the holder's caching, and dispatching strip-H FIRST
		// would emit two separate notifications where smbtorture rename
		// otherdir-* (.expect_dstdir_break=true) expects exactly one RH→"".
		conflict := h.checkParentDirRenameConflict(openFile.FileID, toDir)
		if conflict && !bytes.Equal(toDir, openFile.ParentHandle) {
			h.breakDstParentDirHandleLeasesForRename(authCtx, toDir, openFile)
			// Re-check after the strip-H break drained (the holder's break
			// handler may have closed its conflicting open). If the
			// re-check is clean the rename can proceed without surfacing
			// SHARING_VIOLATION — required by dirlease.rename_dst_parent
			// phase-2 (lease.c:7361 expects NT_STATUS_OK after the holder
			// upgrades the lease and the second setinfo runs).
			conflict = h.checkParentDirRenameConflict(openFile.FileID, toDir)
		}
		if conflict {
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
		// dstMatchedName is the on-disk name of the destination entry when
		// the case-insensitive lookup succeeds. Move's underlying GetChild is
		// exact-case, so if the client supplied a different-case spelling
		// (e.g. "FOO.TXT" while the on-disk entry is "Foo.txt") we MUST hand
		// the canonical casing to Move — otherwise Move would silently
		// create a second sibling entry instead of overwriting. Empty when
		// there is no destination or the lookup failed.
		var dstMatchedName string
		if isOverwrite {
			dstFile, matched, lookupErr := metaSvc.LookupCaseInsensitive(authCtx, toDir, toName)
			if lookupErr == nil && dstFile != nil {
				if encoded, encErr := metadata.EncodeFileHandle(dstFile); encErr == nil {
					dstMetaHandle = encoded
				}
				dstMatchedName = matched
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

			// MS-SMB2 §3.3.4.18 / §3.3.5.21: RENAME breaks Handle leases on
			// the renamed file (and on the overwrite target). Any
			// disconnected durable handle with a different lease key loses
			// H — the disconnected client cannot ack the break, so the
			// durable is purged. smbtorture
			// smb2.durable-v2-open.purge-disconnected-rh-with-rename.
			if h.DurableStore != nil {
				if purged := h.purgeConflictingDisconnectedHandlesForDataChange(
					authCtx.Context,
					openFile.MetadataHandle,
					openFile.LeaseKey,
					true, // RENAME break_to strips H.
				); purged > 0 {
					logger.Debug("SET_INFO: purged disconnected handles on rename",
						"src", openFile.Path,
						"dst", newPath,
						"count", purged)
				}
				if isOverwrite && len(dstMetaHandle) > 0 && !bytes.Equal(dstMetaHandle, openFile.MetadataHandle) {
					if purged := h.purgeConflictingDisconnectedHandlesForDataChange(
						authCtx.Context,
						dstMetaHandle,
						[16]byte{}, // dst holder is by definition not the renamer
						true,
					); purged > 0 {
						logger.Debug("SET_INFO: purged disconnected handles on rename target overwrite",
							"dst", newPath,
							"count", purged)
					}
				}
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

		// Pre-overwrite the case-mismatched destination: Move's destination
		// probe is exact-case GetChild(toName), so a destination that exists
		// under a different casing (e.g. on disk "Foo.txt", client said
		// "FOO.TXT") would be missed and Move would silently create a second
		// sibling entry. Remove the matched-case destination upfront so Move
		// inserts the source under the client-requested casing.
		if isOverwrite && dstMatchedName != "" && dstMatchedName != toName {
			if _, _, rmErr := metaSvc.RemoveFile(authCtx, toDir, dstMatchedName); rmErr != nil {
				logger.Debug("SET_INFO: rename overwrite pre-remove failed",
					"name", dstMatchedName, "error", rmErr)
				return setInfoStatus(common.MapToSMB(rmErr)), nil
			}
		}

		// Perform the rename/move
		_, err = metaSvc.Move(authCtx, openFile.ParentHandle, openFile.FileName, toDir, toName)
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

		// Per MS-FSA 2.1.5.14.10 (smbtorture smb2.dirlease.rename):
		// rename changes directory contents on BOTH source and destination
		// parents. Break Handle + Read dir leases on each (RH → ""), honoring
		// the renamer's ParentLeaseKey suppression from C2. Skip the dst
		// break when src == dst (same-dir rename) to avoid a redundant
		// double-break on a single dir-lease holder.
		h.breakParentDirLeasesForContentChangeOn(ctx, authCtx, srcParentHandle, openFile)
		if !bytes.Equal(toDir, srcParentHandle) {
			h.breakParentDirLeasesForContentChangeOn(ctx, authCtx, toDir, openFile)
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
		// parent key for unlink parent-key suppression.
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
			// MS-SMB2 §3.3.4.18: truncation is a data-modifying op that
			// breaks Level-II Read leases to NONE — purge any disconnected
			// durable handle whose lease holds R-caching from a different key.
			if h.DurableStore != nil {
				if purged := h.purgeConflictingDisconnectedHandlesForDataChange(
					authCtx.Context,
					openFile.MetadataHandle,
					openFile.LeaseKey,
					true,
				); purged > 0 {
					logger.Debug("SET_INFO: purged disconnected handles on EOF set",
						"path", openFile.Path,
						"count", purged)
				}
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

		_, err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
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
		h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)

		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), notifyStreamName(openFile.FileName), FileActionModified, FileNotifyChangeSize)
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
		// FILE_ALLOCATION_INFORMATION [MS-FSCC] 2.4.4.
		//
		// Allocation size is not persisted (DittoFS does not preallocate), but
		// per MS-FSA 2.1.5.14.1 and Samba `smbd_smb2_setinfo_lease_break_fsp_check`
		// (source3/smbd/smb2_setinfo.c) setting allocation is a data-modifying
		// operation: it must break Read (Level II) leases on the same file the
		// same way SET_EOF does. Without this, a remote reader that cached the
		// pre-set state can serve stale data. Required by smbtorture
		// smb2.oplock.batch12 (path-based composite SetAlloc must produce two
		// break notifications: one from the transient CREATE used to address
		// the path, plus this one).
		if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
			if breakErr := h.LeaseManager.BreakReadLeasesOnWrite(lockFileHandle, openFile.ShareName, openFile.LeaseKey); breakErr != nil {
				logger.Debug("SET_INFO: oplock break on allocation set failed (non-fatal)", "path", openFile.Path, "error", breakErr)
			}
		}
		// Record the requested reservation per-handle so a later QUERY_INFO on
		// this handle reports a consistent AllocationSize. We do not preallocate;
		// this only raises the reported allocation, never the file's EndOfFile.
		// AllocationSize is an 8-byte LE value at offset 0 [MS-FSCC] 2.4.4.
		// allocReservationFor drops the request for directories.
		if len(buffer) >= 8 {
			requested := smbenc.NewReader(buffer[:8]).ReadUint64()
			openFile.RequestedAllocSize = allocReservationFor(openFile.IsDirectory, requested)

			// Per MS-FSA 2.1.5.14.1: when the requested AllocationSize is
			// smaller than the file's current EndOfFile, the EndOfFile is
			// truncated down to the allocation size (allocation can never be
			// less than the valid data length). smbtorture smb2.setinfo
			// (setinfo.c:239) sets AllocationInformation = 0 on a 7-byte file
			// and asserts the subsequent all_info2 reports size == 0. Only
			// applies to regular files. Reuse the EOF truncation path so the
			// block list is pruned and Mtime/Ctime update consistently.
			if !openFile.IsDirectory {
				metaSvc := h.Registry.GetMetadataService()
				if curFile, getErr := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle); getErr == nil &&
					requested < curFile.Size {
					if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{
						Size: &requested,
					}); err != nil {
						logger.Debug("SET_INFO: allocation-driven truncate failed",
							"path", openFile.Path, "error", err)
						return setInfoStatus(common.MapToSMB(err)), nil
					}
					h.restoreFrozenTimestamps(authCtx, openFile)
					flushSmbDelayedWrite(openFile)
					h.StoreOpenFile(openFile)
					h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)
					if h.NotifyRegistry != nil {
						h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), notifyStreamName(openFile.FileName), FileActionModified, FileNotifyChangeSize)
					}
				}
			}
		}
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileModeInformation:
		// FILE_MODE_INFORMATION [MS-FSCC] 2.4.24 (4 bytes). SET adjusts the
		// open's mode flags (FILE_WRITE_THROUGH, FILE_SEQUENTIAL_ONLY,
		// FILE_NO_INTERMEDIATE_BUFFERING, FILE_SYNCHRONOUS_IO_*,
		// FILE_DELETE_ON_CLOSE). DittoFS does not change I/O behaviour based on
		// these advisory flags, but the value must round-trip through QUERY_INFO
		// FileModeInformation and the request must succeed. smbtorture
		// smb2.setinfo (setinfo.c:264) sets this level and asserts NT_STATUS_OK.
		if len(buffer) < 4 {
			return setInfoStatus(types.StatusInfoLengthMismatch), nil
		}
		modeMask := fileModeInformationModeMask
		mode := types.CreateOptions(smbenc.NewReader(buffer[:4]).ReadUint32())
		// Per MS-FSA 2.1.5.14.13: any bit set outside the valid FILE_MODE_*
		// set is invalid and the server MUST return STATUS_INVALID_PARAMETER.
		// smbtorture smb2.setinfo (setinfo.c:269) sets a reserved-bit value
		// (e.g. FILE_DIRECTORY_FILE, 0x1) and asserts the rejection.
		if mode&^modeMask != 0 {
			return setInfoStatus(types.StatusInvalidParameter), nil
		}
		// Overlay only the mode-information bits onto the open's CreateOptions so
		// a later QUERY_INFO FileModeInformation reflects the SET; preserve all
		// other create-option bits. We intentionally do NOT flip delete-on-close
		// here — that disposition is owned by FileDispositionInformation, which
		// carries its own DELETE-access gate (Samba's setinfo mode handler is
		// likewise advisory-only).
		openFile.CreateOptions = (openFile.CreateOptions &^ modeMask) | (mode & modeMask)
		h.StoreOpenFile(openFile)
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileLinkInformation:
		// FILE_LINK_INFORMATION [MS-FSCC] 2.4.21.2 — hard link creation.
		// Wire format mirrors FILE_RENAME_INFORMATION: ReplaceIfExists (1B),
		// Reserved (7B), RootDirectory (8B), FileNameLength (4B), FileName (UTF-16LE).
		return h.handleFileLinkInformation(ctx, authCtx, openFile, buffer)

	case types.FileFullEaInformation: // [MS-FSCC] 2.4.15 - Extended attributes
		// Reject SET on the reserved ACL xattr name with ACCESS_DENIED so the
		// server-stored security descriptor cannot be tampered with through the
		// FILE_FULL_EA_INFORMATION channel. Mirrors Samba vfs_acl_xattr (which
		// stores the NT ACL as `security.NTACL` and shields that xattr from the
		// EA API): smbtorture smb2.ea.acl_xattr asserts ACCESS_DENIED when a
		// client tries to overwrite the reserved name. The reserved name is
		// surfaced to the torture client via the `--option=acl_xattr_name=...`
		// torture setting and omitted from the QUERY_INFO EA enumeration.
		entries, decErr := decodeFullEaEntries(buffer)
		if decErr != nil {
			logger.Debug("SET_INFO: FileFullEaInformation decode failed",
				"path", openFile.Path, "error", decErr)
			return setInfoStatus(types.StatusInvalidParameter), nil
		}
		for _, e := range entries {
			if isReservedACLXattrName(e.name) {
				logger.Debug("SET_INFO: FileFullEaInformation reserved name rejected",
					"path", openFile.Path, "name", e.name)
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}

		// Persist the EA set/delete mutations through the metadata layer.
		// A zero-length value deletes the named EA; a non-empty value upserts
		// it (MS-FSCC §2.4.15). EA names are case-insensitive and the metadata
		// layer resolves them so casing round-trips.
		metaSvc := h.Registry.GetMetadataService()
		setAttrs := &metadata.SetAttrs{EAMutations: eaMutationsFromEntries(entries)}
		if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs); err != nil {
			logger.Debug("SET_INFO: FileFullEaInformation persist failed",
				"path", openFile.Path, "error", err)
			return setInfoStatus(common.MapToSMB(err)), nil
		}

		logger.Debug("SET_INFO: FileFullEaInformation persisted",
			"path", openFile.Path, "count", len(entries))
		if h.NotifyRegistry != nil {
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), notifyStreamName(openFile.FileName), FileActionModified, FileNotifyChangeEa)
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
//
// Takes openFile.mu (read) — the freeze flags and Frozen* pointers are mutated
// under the write lock in SET_INFO BasicInfo and must be observed atomically
// against a concurrent freeze/thaw on the same handle (#606).
func applyFrozenTimestamps(openFile *OpenFile, file *metadata.File) {
	openFile.mu.RLock()
	defer openFile.mu.RUnlock()
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
		_, _ = metaSvc.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{
			Mtime: &mtime,
			Ctime: &ctime,
		})
	}
}

// restoreFrozenTimestamps restores timestamps that are frozen via SET_INFO -1 sentinel.
// Called after operations that unconditionally update timestamps (WRITE, truncate).
//
// All reads of the freeze flags / Frozen* pointers go through buildFrozenAttrs
// (which takes openFile.mu read), snapshotMtimeFrozen (likewise), or the local
// snapshot taken under openFile.mu — so a concurrent SET_INFO freeze/thaw on
// the same handle cannot tear our view (#606).
func (h *Handler) restoreFrozenTimestamps(authCtx *metadata.AuthContext, openFile *OpenFile) {
	restoreAttrs := buildFrozenAttrs(openFile)
	if restoreAttrs == nil {
		return
	}

	// Snapshot the fields used by the logger and the pending-mtime fast path
	// under the read lock so the values used here are consistent with what
	// buildFrozenAttrs above produced.
	openFile.mu.RLock()
	mtimeFrozen := openFile.MtimeFrozen
	ctimeFrozen := openFile.CtimeFrozen
	atimeFrozen := openFile.AtimeFrozen
	var frozenMtime, frozenCtime, frozenAtime *time.Time
	if openFile.FrozenMtime != nil {
		v := *openFile.FrozenMtime
		frozenMtime = &v
	}
	if openFile.FrozenCtime != nil {
		v := *openFile.FrozenCtime
		frozenCtime = &v
	}
	if openFile.FrozenAtime != nil {
		v := *openFile.FrozenAtime
		frozenAtime = &v
	}
	openFile.mu.RUnlock()

	logger.Debug("restoreFrozenTimestamps: restoring",
		"path", openFile.Path,
		"mtimeFrozen", mtimeFrozen,
		"ctimeFrozen", ctimeFrozen,
		"atimeFrozen", atimeFrozen,
		"frozenMtime", frozenMtime,
		"frozenCtime", frozenCtime,
		"frozenAtime", frozenAtime)

	metaSvc := h.Registry.GetMetadataService()
	if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, restoreAttrs); err != nil {
		logger.Debug("restoreFrozenTimestamps: failed", "path", openFile.Path, "error", err)
		return
	}

	// Also update the pending write state's LastMtime to the frozen value.
	// MetadataService.GetFile() merges pending state with stored state, using
	// max(pending.LastMtime, store.Mtime). If we only update the store but
	// leave pending.LastMtime at the original WRITE time, GetFile() will
	// return the non-frozen value. By updating pending.LastMtime to the frozen
	// Mtime, the merge produces the correct frozen value.
	if mtimeFrozen && frozenMtime != nil {
		metaSvc.UpdatePendingMtime(openFile.MetadataHandle, *frozenMtime)
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
		if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, restoreAttrs); err != nil {
			logger.Debug("restoreParentDirFrozenTimestamps: failed",
				"path", openFile.Path, "error", err)
		} else {
			// IsMtimeFrozen / IsCtimeFrozen / IsAtimeFrozen each take
			// openFile.mu (read); see #606. Cheap because the parent-dir
			// frozen log line is debug-gated.
			logger.Debug("restoreParentDirFrozenTimestamps: restored",
				"path", openFile.Path,
				"mtimeFrozen", openFile.IsMtimeFrozen(),
				"ctimeFrozen", openFile.IsCtimeFrozen(),
				"atimeFrozen", openFile.IsAtimeFrozen())
		}

		// Don't break early - there may be multiple handles for the same directory
		return true
	})
}

// buildFrozenAttrs constructs a SetAttrs from the frozen timestamp values on an
// OpenFile. Returns nil if no timestamps need restoring.
//
// Takes openFile.mu (read); see applyFrozenTimestamps for rationale.
// Snapshots the time pointers so callers using the returned SetAttrs after
// unlock cannot tear against a concurrent thaw clearing them. (#606)
func buildFrozenAttrs(openFile *OpenFile) *metadata.SetAttrs {
	openFile.mu.RLock()
	defer openFile.mu.RUnlock()
	attrs := &metadata.SetAttrs{}
	hasAny := false

	if openFile.BtimeFrozen && openFile.FrozenBtime != nil {
		v := *openFile.FrozenBtime
		attrs.CreationTime = &v
		hasAny = true
	}
	if openFile.MtimeFrozen && openFile.FrozenMtime != nil {
		v := *openFile.FrozenMtime
		attrs.Mtime = &v
		hasAny = true
	}
	if openFile.CtimeFrozen && openFile.FrozenCtime != nil {
		v := *openFile.FrozenCtime
		attrs.Ctime = &v
		hasAny = true
	}
	if openFile.AtimeFrozen && openFile.FrozenAtime != nil {
		v := *openFile.FrozenAtime
		attrs.Atime = &v
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
			h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), notifyStreamName(openFile.FileName), FileActionModified, FileNotifyChangeSecurity)
		}
		return setInfoStatus(types.StatusSuccess), nil
	}

	metaSvc := h.Registry.GetMetadataService()
	_, err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
	if err != nil {
		logger.Debug("SET_INFO: failed to set security info", "path", openFile.Path, "error", err)
		return setInfoStatus(common.MapToSMB(err)), nil
	}

	if h.NotifyRegistry != nil {
		h.NotifyRegistry.NotifyChange(openFile.ShareName, GetParentPath(openFile.Path), notifyStreamName(openFile.FileName), FileActionModified, FileNotifyChangeSecurity)
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
//
// ctx may be nil — callers that don't carry a SMBHandlerContext (e.g. cross-
// protocol cleanup paths) fall back to inline dispatch. Carrying ctx lets the
// helper defer the dispatch via PostSend so the break notification arrives
// after the triggering request's response, matching Samba's tevent-cycle
// `send_break_to_none` semantics.
func (h *Handler) breakParentDirLeasesForContentChange(ctx *SMBHandlerContext, authCtx *metadata.AuthContext, openFile *OpenFile) {
	if len(openFile.ParentHandle) == 0 {
		return
	}
	h.breakParentDirLeasesForContentChangeOn(ctx, authCtx, openFile.ParentHandle, openFile)
}

// breakParentDirLeasesForContentChangeOn is the multi-parent variant used by
// the rename branch: break parent directory leases to None on an
// arbitrary directory handle (src-parent or dst-parent), honoring the
// originating handle's ParentLeaseKey suppression from C2.
//
// Per Samba `contend_dirleases` / `do_dirlease_break_to_none`
// (source3/smbd/smb2_oplock.c): a directory-content change emits a SINGLE
// LEASE_BREAK to None per holder, not the two-step strip-H / strip-R pattern
// used for file leases. The dispatch is FIRE-AND-FORGET: Samba's
// `send_break_to_none` does not wait for the ACK, and the triggering
// request (rename / hardlink / setinfo / close) returns as soon as the
// notification is queued. Required by smbtorture smb2.dirlease.{rename,
// hardlink, unlink_different_set_and_close, unlink_*_initial_and_close}
// which set lease_skip_ack=true AFTER the triggering request returns and
// then replay the captured ACK manually — waiting inline would let the
// 5 s ack-timeout force-complete the lease, so the manual replay would
// hit STATUS_UNSUCCESSFUL (the lease is no longer in BREAKING state).
//
// Per Samba dirlease_should_break: ClientID is NOT used for suppression —
// a same-client SET_INFO / WRITE / CLOSE / RENAME with a mismatched (or
// absent) ParentLeaseKey MUST still break the parent dir lease held by that
// same client.
func (h *Handler) breakParentDirLeasesForContentChangeOn(ctx *SMBHandlerContext, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, openFile *OpenFile) {
	if h.LeaseManager == nil || len(parentHandle) == 0 {
		return
	}

	parentLockHandle := lock.FileHandle(parentHandle)

	// Apply parent-key suppression only (MS-SMB2 §3.3.4.20): if
	// the originating handle's CREATE carried an RqLs with ParentLeaseKey
	// set, the matching parent dir lease MUST NOT be broken. No ClientID
	// exclusion — same-client breaks fire when the key doesn't match.
	excludeParentKey := openFile.ParentLeaseKey
	hasExcludeKey := openFile.HasParentLeaseKey
	path := openFile.Path
	parentDbg := fmt.Sprintf("%x", parentHandle)
	shareName := openFile.ShareName

	dispatch := func() {
		if breakErr := h.LeaseManager.BreakParentDirLeasesOnContentChangeAsync(
			parentLockHandle, shareName, "",
			excludeParentKey, hasExcludeKey,
		); breakErr != nil {
			logger.Debug("SET_INFO: parent directory lease break-to-None failed", "path", path, "parent", parentDbg, "error", breakErr)
		}
	}

	// Defer dispatch until after the triggering request's response is on the
	// wire when an SMB ctx is available. Mirrors Samba `send_break_to_none`
	// (source3/smbd/smb2_oplock.c) which schedules the break via the
	// messaging context for a later tevent cycle — required by smbtorture
	// smb2.dirlease.{rename, hardlink, unlink_different_set_and_close,
	// unlink_different_initial_and_close} which set lease_skip_ack=true
	// AFTER the triggering request returns. With inline dispatch the break
	// arrives before the response, the client's lease handler observes
	// skip_ack=false and auto-acks, and the test's manual replay ACK then
	// fails STATUS_UNSUCCESSFUL because the lease is no longer breaking.
	//
	// authCtx is retained for signature parity with sync-variant call sites;
	// the async dispatch does not consume it.
	_ = authCtx
	if ctx != nil {
		AppendPostSend(ctx, dispatch)
	} else {
		dispatch()
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
		if existing, matchedName, lookupErr := metaSvc.LookupCaseInsensitive(authCtx, dstDir, linkName); lookupErr == nil && existing != nil {
			if _, _, rmErr := metaSvc.RemoveFile(authCtx, dstDir, matchedName); rmErr != nil {
				logger.Debug("SET_INFO: hardlink replace failed to remove existing",
					"name", matchedName, "error", rmErr)
				return setInfoStatus(common.MapToSMB(rmErr)), nil
			}
		}
	}

	// Thread the open file's ParentLeaseKey into the auth context so
	// MetadataService.notifyDirChange forwards it to OnDirChange and the
	// dir-lease parent-key suppression rule (MS-SMB2 §3.3.4.20) skips the
	// matching parent dir lease (same-key holder does not get broken).
	PropagateOpenFileParentLeaseKey(authCtx, openFile)

	if _, err := metaSvc.CreateHardLink(authCtx, dstDir, linkName, openFile.MetadataHandle); err != nil {
		logger.Debug("SET_INFO: CreateHardLink failed",
			"src", openFile.Path, "dst", newPath, "error", err)
		return setInfoStatus(common.MapToSMB(err)), nil
	}

	// Break parent directory leases on the destination parent to None
	// (MS-FSA 2.1.5.14: directory contents changed). Parent-key suppression
	// only — no ClientID exclusion per Samba dirlease_should_break. Single
	// break-to-None matches Samba `contend_dirleases` / `do_dirlease_break_to_none`
	// — required by smbtorture hardlink samedir-{wrong,no}-parent-leaskey
	// which expect exactly one LEASE_BREAK per holder. Fire-and-forget per
	// Samba `send_break_to_none`: the test sets lease_skip_ack=true AFTER
	// the setinfo returns and replays the captured ACK; waiting inline would
	// force-complete the lease on timeout and the replay would hit
	// STATUS_UNSUCCESSFUL.
	if h.LeaseManager != nil {
		dstParentLock := lock.FileHandle(dstDir)
		excludeParentKey := openFile.ParentLeaseKey
		hasExcludeKey := openFile.HasParentLeaseKey
		shareName := openFile.ShareName
		dstDbg := newPath
		dispatch := func() {
			if breakErr := h.LeaseManager.BreakParentDirLeasesOnContentChangeAsync(
				dstParentLock, shareName,
				"", excludeParentKey, hasExcludeKey,
			); breakErr != nil {
				logger.Debug("SET_INFO: hardlink dst-parent dir lease break-to-None failed",
					"dst", dstDbg, "error", breakErr)
			}
		}
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
		"src", openFile.Path, "dst", newPath, "name", linkName)
	return setInfoStatus(types.StatusSuccess), nil
}

// ============================================================================
// FILE_FULL_EA_INFORMATION decoding (MS-FSCC §2.4.15)
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
// case-insensitive on the wire even though MS-FSCC §2.4.15 reserves the right
// to canonicalize the casing. Samba's vfs_acl_xattr uses a fixed lower-case
// constant; smbtorture's torture_setting_string returns the literal it was
// configured with. Match either casing.
const reservedACLXattrName = "security.NTACL"

// isReservedACLXattrName reports whether name (an EA name in canonical NT
// form, no domain prefix) matches the reserved ACL xattr slot. NT EA names
// are case-insensitive, so the comparison is folded.
func isReservedACLXattrName(name string) bool {
	return strings.EqualFold(name, reservedACLXattrName)
}
