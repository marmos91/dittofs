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
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// File-information classes: the SET_INFO info-class switch against the
// metadata store, and the frozen-timestamp restore helpers.
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

		// Validate attribute constraints per MS-FSA 2.1.5.15.2 ("FileBasicInformation").
		// MS-FSCC 2.4.7 defines the wire structure only:
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

		// MS-FSCC 2.6 ("File Attributes") defines FILE_ATTRIBUTE_READONLY's meaning;
		// mapping it onto a Unix mode bit is DittoFS's own storage rule.
		// When FileAttributes != 0, the client is explicitly setting attributes.
		// READONLY is stored in modeDOSReadonly (bit 0x100000); POSIX owner-write
		// bits are preserved. calculatePermissions in pkg/metadata enforces the
		// READONLY semantics for both NFS and SMB callers by clearing write when
		// modeDOSExplicit + modeDOSReadonly are both set.
		// Per MS-FSA 2.1.5.15.2 ("FileBasicInformation"), which lists the
		// settable attributes: FILE_ATTRIBUTE_COMPRESSED is NOT settable via
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
			// Propagate FILE_ATTRIBUTE_HIDDEN (MS-FSCC 2.6 "File Attributes") into the metadata
			// Hidden field so QUERY_INFO and QUERY_DIRECTORY round-trip correctly.
			hiddenVal := fileAttrs&types.FileAttributeHidden != 0
			setAttrs.Hidden = &hiddenVal
		}

		// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): Handle timestamp freeze/unfreeze sentinels.
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
			"path", openFile.Name().Path,
			"creationFT", fmt.Sprintf("0x%016X", creationFT),
			"atimeFT", fmt.Sprintf("0x%016X", atimeFT),
			"mtimeFT", fmt.Sprintf("0x%016X", mtimeFT),
			"ctimeFT", fmt.Sprintf("0x%016X", ctimeFT))

		// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): All four timestamp fields support freeze/unfreeze.
		// CreationTime freeze suppresses explicit changes from subsequent SET_INFO
		// calls on this handle (the frozen value is returned instead).
		hasFreezeOrUnfreeze := isFiletimeSentinel(creationFT) ||
			isFiletimeSentinel(atimeFT) ||
			isFiletimeSentinel(mtimeFT) ||
			isFiletimeSentinel(ctimeFT)

		// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): Sentinel values (-1, -2) mean the object store
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
			name := openFile.Name()
			if colonIdx := strings.Index(name.FileName, ":"); colonIdx > 0 && len(name.ParentHandle) > 0 {
				// ADS: capture base file timestamps.
				baseFileName := name.FileName[:colonIdx]
				if baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, name.ParentHandle, baseFileName); baseFile != nil {
					preFile = baseFile
				}
			}
			if preFile == nil {
				preFile, err = metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
				if err != nil {
					logger.Warn("SET_INFO: failed to read file for freeze/unfreeze", "path", openFile.Name().Path, "error", err)
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

		// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): When a timestamp is frozen from a prior
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

		// Per MS-FSA 2.1.5.15.2 ("FileBasicInformation") a timestamp write is
		// authorized by FILE_WRITE_ATTRIBUTES on the open, not by ownership of
		// the file. Carry that grant so the metadata layer's ownership gate
		// reflects the protocol's rule. It is scoped to this call rather than
		// stamped on authCtx, which the rename path also hands to the
		// parent-directory restore — a different object, on which this handle's
		// grant says nothing. It relaxes nothing else either: a SET_INFO that
		// also changes DOS attributes is still ownership checked.
		basicAuthCtx := withTimestampHandleAuth(authCtx, openFile.GrantedAccess)

		if _, err := metaSvc.SetFileAttributes(basicAuthCtx, openFile.MetadataHandle, setAttrs); err != nil {
			openFile.mu.Unlock() // release before returning; refs #606.
			logger.Debug("SET_INFO: failed to set basic info", "path", openFile.Name().Path, "error", err)
			return setInfoStatus(types.StatusForErr(err)), nil
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
		//       * modeDOSCompressed (0x40000) — FSCTL-managed (FSCTL_SET_COMPRESSION).
		//       * modeDOSSparse (0x200000) — FSCTL-managed (FSCTL_SET_SPARSE).
		//         Both live on the base file and are never derived from FileAttributes.
		//
		// The base SetFileAttributes call is fire-and-forget: a failure to
		// propagate is logged at the metadata layer and does not roll back
		// the stream's own SET_INFO (the stream is the explicitly-targeted
		// handle and its write has already succeeded above).
		adsName := openFile.Name()
		if colonIdx := strings.Index(adsName.FileName, ":"); colonIdx > 0 && len(adsName.ParentHandle) > 0 {
			baseFileName := adsName.FileName[:colonIdx]
			if baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, adsName.ParentHandle, baseFileName); baseFile != nil {
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
						// A stream shares its base file's security descriptor, so
						// the grant carried on this handle is a grant on the base.
						_, _ = metaSvc.SetFileAttributes(basicAuthCtx, baseHandle, basePropagate)
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
				logger.Debug("SET_INFO: froze CreationTime", "path", openFile.Name().Path, "value", preFile.CreationTime)
			case filetimeUnfreeze:
				openFile.BtimeFrozen = false
				openFile.FrozenBtime = nil
			}

			// LastWriteTime (Mtime) - offset 16
			switch mtimeFT {
			case filetimeFreeze:
				openFile.MtimeFrozen = true
				openFile.FrozenMtime = &preFile.Mtime
				logger.Debug("SET_INFO: froze LastWriteTime", "path", openFile.Name().Path, "value", preFile.Mtime)
			case filetimeUnfreeze:
				openFile.MtimeFrozen = false
				openFile.FrozenMtime = nil
			}

			// ChangeTime (Ctime) - offset 24
			switch ctimeFT {
			case filetimeFreeze:
				openFile.CtimeFrozen = true
				openFile.FrozenCtime = &preFile.Ctime
				logger.Debug("SET_INFO: froze ChangeTime", "path", openFile.Name().Path, "value", preFile.Ctime)
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

		// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): an explicit (non-zero, non-sentinel) timestamp
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
			logger.Debug("SET_INFO: explicit set pins timestamp", "field", label, "path", openFile.Name().Path, "value", v)
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
		// Any LastAccessTime action on this handle — an explicit value or a
		// freeze/thaw sentinel — makes the client's value authoritative, so drop
		// the coalesced READ access time CLOSE would otherwise flush over it.
		if atimeFT != 0 {
			openFile.SmbPendingAtime = time.Time{}
		}
		if setAttrs.Mtime != nil && mtimeFT != 0 && !isFiletimeSentinel(mtimeFT) {
			setSmbStickyWriteTimeLocked(openFile, *setAttrs.Mtime)
		}
		openFile.mu.Unlock()
		h.StoreOpenFile(openFile)

		// Break parent directory leases on child metadata change (#470:
		// smb2.dirlease.set{atime,btime,ctime,mtime,dos}). Per MS-FSA
		// 2.1.5.15.2 ("FileBasicInformation"): any child SET_INFO that modifies file attributes or
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
				h.notifyOpenFileModified(openFile, nf)
			}
		}

		return setInfoStatus(types.StatusSuccess), nil

	case types.FileRenameInformation:
		// FILE_RENAME_INFORMATION [MS-FSCC] 2.4.42.2 (FileRenameInformation for SMB2)
		renameInfo, err := DecodeFileRenameInfo(buffer)
		if err != nil {
			logger.Debug("SET_INFO: failed to decode rename info", "error", err)
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		// Per MS-FSA 2.1.5.15.12 ("FileRenameInformation"): Rename requires DELETE access on the source file.
		// Gate consults Open.GrantedAccess (post-DACL intersection at CREATE), not
		// the pre-DACL DesiredAccess — same fix class as #616 (ChangeNotify).
		if !hasDeleteAccess(openFile.GrantedAccess) {
			logger.Debug("SET_INFO: rename without DELETE access",
				"path", openFile.Name().Path,
				"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Per MS-FSA 2.1.5.15.12 ("FileRenameInformation"): if Open.Link.IsDeleted
		// is TRUE, the operation MUST be failed with STATUS_ACCESS_DENIED.
		//
		// IsDeleted belongs to the link, not to the handle asking, so this is the
		// cross-handle scan the CREATE gate already uses rather than this open's
		// own flag: a disposition committed on one handle is not copied onto its
		// siblings until the CLOSE election runs, so reading only openFile would
		// let a second handle rename a link already marked for deletion.
		//
		// The analogue is the disposition set through SET_INFO, not the
		// FILE_DELETE_ON_CLOSE create option: that is held per-handle as
		// InitialDeleteOnClose, is deliberately invisible to this scan, and is
		// promoted only at CLOSE, matching 2.1.5.5 phase 1.
		if h.isFileDeletePending(openFile.MetadataHandle) {
			logger.Debug("SET_INFO: rename of a link already marked for deletion",
				"path", openFile.Name().Path)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Before renaming, check that no other open handle on the same file
		// conflicts with the rename: all other opens must have FILE_SHARE_DELETE
		// (0x04) in ShareAccess. MS-FSA 2.1.5.15.12 ("FileRenameInformation")
		// specifies no share-mode check; this follows Samba `can_rename`.
		// (Destination-parent share-mode probe runs further below, after toDir
		// is resolved and the stream-rename early return has been ruled out.)
		//
		// Held under renameScanMu so the scan-and-decision is atomic vs a
		// concurrent CLOSE removing the conflicting holder from h.files (which
		// would otherwise yield an intermittent spurious SHARING_VIOLATION).
		// No break-wait precedes this gate, so taking the mutex here cannot
		// deadlock against a CLOSE.
		h.renameScanMu.Lock()
		shareDeleteConflict := h.checkShareDeleteConflict(openFile)
		h.renameScanMu.Unlock()
		if shareDeleteConflict {
			logger.Debug("SET_INFO: rename blocked by sharing violation",
				"path", openFile.Name().Path,
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
			// One snapshot: the move below must not take its directory from
			// one rename and its name from another.
			oldName := openFile.Name()
			baseName := oldName.FileName
			if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 {
				baseName = baseName[:colonIdx]
			}

			// Strip :$DATA type suffix from rename target.
			if strings.HasSuffix(strings.ToUpper(newPath), ":$DATA") {
				newPath = newPath[:len(newPath)-len(":$DATA")]
			}

			// Build new child name: basefile + new stream suffix
			toName := baseName + newPath
			toDir := oldName.ParentHandle

			// Save old path info for notification before modification
			oldFileName := oldName.FileName
			oldParentPath := GetParentPath(oldName.Path)

			// Move stamps the renamed inode's LastChangeTime. A client
			// holding this handle open must keep observing the ChangeTime it
			// was handed at CREATE, so put the pre-rename value back.
			metaSvc := h.Registry.GetMetadataService()

			var clobberedStream *metadata.File
			var renameWcc *metadata.RenameWcc
			clobberedStream, renameWcc, err = metaSvc.Move(authCtx, toDir, oldFileName, toDir, toName)
			if err != nil {
				logger.Debug("SET_INFO: stream rename failed",
					"from", oldFileName,
					"to", toName,
					"error", err)
				return setInfoStatus(types.StatusForErr(err)), nil
			}

			h.restorePreRenameChangeTime(authCtx.Context, openFile.MetadataHandle, renameWcc)

			// Renaming a stream onto an existing stream name unlinks the
			// stream that was there; free its content.
			if clobberedStream != nil {
				h.purgeBlockStorePayload(ctx.Context, toDir, clobberedStream.PayloadID, toName, "SET_INFO stream rename")
			}

			// Move's LastChangeTime stamp is an automatic update, so a
			// timestamp frozen on this handle has to be put back the same way
			// WRITE and truncate put theirs back.
			h.restoreFrozenTimestamps(authCtx, openFile)

			// Clear delete-on-close after rename. Written under the handle
			// lock: the delete-pending gates and the CLOSE delete-on-close
			// election read this field under it.
			openFile.mu.Lock()
			openFile.DeletePending = false
			openFile.mu.Unlock()

			// Notify watchers
			if h.NotifyRegistry != nil {
				tree, ok := h.GetTree(openFile.TreeID)
				if ok {
					// A stream rename stays in the same directory.
					newParentPath := oldParentPath
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

			// Update open file state. The handle lock serializes this
			// read-modify-write against a concurrent rename on the same handle.
			openFile.mu.Lock()
			newName := openFile.Name()
			parentPath := GetParentPath(newName.Path)
			newName.FileName = toName
			if parentPath == "" || parentPath == "/" || parentPath == "." {
				newName.Path = toName
			} else {
				newName.Path = parentPath + "/" + toName
			}
			openFile.SetName(newName)
			openFile.mu.Unlock()
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
		// Per MS-FSCC 2.4.42.2 (FileRenameInformation for SMB2) / MS-SMB2 2.2.39:
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
			toDir = openFile.Name().ParentHandle
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
		if srcName := openFile.Name(); len(srcName.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot rename root directory", "path", srcName.Path)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Directory rename: break H-leases on every open child file (RH→R
		// strip-H) and wait for each break to drain. After the wait, ANY
		// remaining open child blocks the parent rename with STATUS_ACCESS_DENIED
		// per MS-FSA §2.1.5.15.12 ("FileRenameInformation") (smbtorture rename_dir_openfile: 8 sub-cases all
		// hinge on whether every H-leased child closes-on-break or stays open
		// after ACK). Children without an H-lease are no-op'd by
		// ComputeLeaseBreakTo and stay open → the open-child recheck produces the
		// immediate ACCESS_DENIED for the "no-hleases" case.
		//
		// PRECEDENCE (Fix A, smbtorture rename_dir_openfile): this open-child
		// determination runs BEFORE the destination-parent share-mode gate
		// below. A directory rename whose source has an open child is
		// ACCESS_DENIED, and that status must win over any SHARING_VIOLATION the
		// dst-parent gate would otherwise raise — MS-FSA evaluates the source
		// open-child constraint ahead of the dst-parent implicit-open conflict.
		//
		// Deadlock-safety: the per-child break-WAITs run OUTSIDE renameScanMu (a
		// break can only complete when the child's holder CLOSEs, and CLOSE needs
		// renameScanMu). Only the final authoritative anyOpenChild scan is taken
		// under the mutex, so a child whose holder is mid-CLOSE is observed as
		// either fully present (live → ACCESS_DENIED) or fully gone (→ proceed).
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
			// Each wait below ends on an ACK or CLOSE from a lease holder
			// that may be this very connection, and the client cannot send
			// either until the responses queued behind this rename reach it.
			// Step out of the response order first; the break notifications
			// these waits are waiting on have already been written.
			releaseResponseOrder(ctx)
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
			// Authoritative recheck under the scan mutex (Fix B): serialized vs
			// a concurrent CLOSE removing the last open child from h.files.
			h.renameScanMu.Lock()
			openChild := h.anyOpenChild(openFile.MetadataHandle)
			h.renameScanMu.Unlock()
			if openChild {
				logger.Debug("SET_INFO: dir rename blocked by open child",
					"dir", openFile.Name().Path)
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}

		// Per MS-FSA 2.1.5.15.12 ("FileRenameInformation"): rename takes an implicit open on
		// the destination parent directory with FILE_ADD_FILE|SYNCHRONIZE
		// (FILE_ADD_SUBDIRECTORY for a directory source) and ShareAccess
		// FILE_SHARE_READ|FILE_SHARE_WRITE. Any existing open of that
		// directory that denies write sharing or already holds DELETE access
		// conflicts. Stream renames don't traverse the directory layer and
		// returned earlier above; we're past that branch here.
		//
		// Conflict-gated dst-parent dir-lease pre-break (smbtorture
		// smb2.dirlease.rename_dst_parent, lease.c:7331): when an existing
		// holder on dst-parent denies the implicit open with
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
		//
		// Fix B (race): the strip-H break-WAIT inside
		// breakDstParentDirHandleLeasesForRename runs OUTSIDE renameScanMu
		// (it can only complete when the dst-parent holder CLOSEs, and CLOSE
		// needs the mutex). The authoritative post-break re-scan that decides
		// SHARING_VIOLATION-vs-proceed is taken UNDER the mutex, so a holder
		// that a concurrent CLOSE is mid-removing is observed atomically — no
		// spurious SHARING_VIOLATION.
		h.renameScanMu.Lock()
		conflict := h.checkParentDirRenameConflict(openFile, toDir)
		h.renameScanMu.Unlock()
		if conflict && !bytes.Equal(toDir, openFile.Name().ParentHandle) {
			h.breakDstParentDirHandleLeasesForRename(authCtx, toDir, openFile)
			// Re-check after the strip-H break drained (the holder's break
			// handler may have closed its conflicting open). If the
			// re-check is clean the rename can proceed without surfacing
			// SHARING_VIOLATION — required by dirlease.rename_dst_parent
			// phase-2 (lease.c:7361 expects NT_STATUS_OK after the holder
			// upgrades the lease and the second setinfo runs). Authoritative
			// under the mutex.
			h.renameScanMu.Lock()
			conflict = h.checkParentDirRenameConflict(openFile, toDir)
			h.renameScanMu.Unlock()
		}
		if conflict {
			logger.Debug("SET_INFO: rename blocked by destination-parent sharing violation",
				"path", openFile.Name().Path,
				"toDir", fmt.Sprintf("%x", toDir),
				"fileID", fmt.Sprintf("%x", openFile.FileID))
			return setInfoStatus(types.StatusSharingViolation), nil
		}

		// Pre-rename lease break: per MS-FSA §2.1.5.15.12 ("FileRenameInformation") + Samba
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
		// MS-FSA §2.1.5.15.12 ("FileRenameInformation") — surface STATUS_ACCESS_DENIED. The dst close
		// path (smbtorture v2_rename_target_overwrite stage 3) clears
		// the open and the post-wait recheck then proceeds to the rename.
		isOverwrite := renameInfo.ReplaceIfExists
		metaSvc := h.Registry.GetMetadataService()

		// A destination name that already resolves to the file being renamed
		// is another hard link to it, so there is nothing to move: unlinking
		// either name would drop a link the caller never asked to lose, and
		// Move returns success without touching the store. Everything below
		// would then describe a change that did not happen — the lease
		// breaks, the paired rename notification, and the handle's own name.
		// The link path takes the same shortcut for a link onto a name the
		// file already answers to.
		//
		// Renaming an entry onto itself is excluded. It reaches the same
		// no-op inside Move, but it is the ordinary "rename to the name I
		// already have" request rather than a second link, and the work that
		// request carries is still owed — a client holding a lease on the
		// file is broken for it.
		//
		// The probe is exact-case, matching the GetChild inside Move, so a
		// case-mismatched destination still falls through to the overwrite
		// path below and replaces the entry it found.
		srcName := openFile.Name()
		renamingOntoOwnEntry := toName == srcName.FileName && bytes.Equal(toDir, srcName.ParentHandle)
		if dstHandle, childErr := metaSvc.GetChild(authCtx.Context, toDir, toName); childErr == nil &&
			!renamingOntoOwnEntry && len(openFile.MetadataHandle) > 0 &&
			bytes.Equal(dstHandle, openFile.MetadataHandle) {
			logger.Debug("SET_INFO: rename destination is another link to the source",
				"from", openFile.Name().Path,
				"to", newPath)
			return setInfoStatus(types.StatusSuccess), nil
		}

		// Dispatch the SOURCE file's break ahead of the destination lookup
		// below. Every gate that can still reject this rename has already run,
		// and the source break needs nothing the lookup produces.
		//
		// The ordering matters because the destination lookup is
		// case-insensitive: on a miss it enumerates the whole destination
		// directory. A client that pipelines a request behind the rename gets
		// that request answered while the enumeration is still running, so the
		// break notification the rename owes lands after a response the client
		// has already acted on. smbtorture rename_wait does exactly that — it
		// reads the break out of the following CREATE's I/O and acks it — and
		// acks an all-zero lease key when the break has not arrived yet, which
		// the server then rejects.
		//
		// The destination break stays below: it needs the resolved handle, and
		// re-dispatching the source break there is suppressed as a duplicate.
		if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			if err := h.LeaseManager.BreakLeasesOnRename(
				lock.FileHandle(openFile.MetadataHandle),
				"", // dst not resolved yet; broken below
				openFile.ShareName,
				openFile.LeaseKey,
				false, // isOverwrite: dst handled below
			); err != nil {
				logger.Debug("SET_INFO: rename source lease break dispatch failed", "error", err)
			}
		}

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

			// MS-SMB2 §3.3.5.21 ("Receiving an SMB2 SET_INFO Request") and
			// §3.3.4.7 ("Object Store Indicates a Lease Break"): RENAME breaks Handle leases on
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
						"src", openFile.Name().Path,
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
			// The source break was dispatched above; this waits for the
			// holder to acknowledge it, and the holder may be this very
			// connection — smbtorture lease.rename_wait holds both leases on
			// one connection and reads the break out of the CREATE it
			// pipelined behind this rename. Step out of the response order so
			// that CREATE can be answered and the ACK can arrive.
			releaseResponseOrder(ctx)
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
		// stages 1/2: ACK leaves dst open ⇒ ACCESS_DENIED). The break-WAIT
		// above ran outside renameScanMu; the authoritative open-handle scan
		// is taken under the mutex so a holder mid-CLOSE is observed
		// atomically (Fix B — same race class as the dst-parent gate).
		if isOverwrite && len(dstMetaHandle) > 0 {
			h.renameScanMu.Lock()
			dstStillOpen := h.hasOpenHandleOnFile(dstMetaHandle, openFile.FileID)
			h.renameScanMu.Unlock()
			if dstStillOpen {
				logger.Debug("SET_INFO: rename overwrite blocked by open handle on destination",
					"src", openFile.Name().Path,
					"dst", newPath)
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}

		// Save the pre-rename name for notification and for the post-rename
		// dir-lease break on the source parent, which the update below
		// replaces with toDir.
		oldName := openFile.Name()
		oldPath := oldName.Path
		oldFileName := oldName.FileName
		oldParentPath := GetParentPath(oldPath)
		srcParentHandle := oldName.ParentHandle

		// Pre-overwrite the case-mismatched destination: Move's destination
		// probe is exact-case GetChild(toName), so a destination that exists
		// under a different casing (e.g. on disk "Foo.txt", client said
		// "FOO.TXT") would be missed and Move would silently create a second
		// sibling entry. Remove the matched-case destination upfront so Move
		// inserts the source under the client-requested casing.
		if isOverwrite && dstMatchedName != "" && dstMatchedName != toName {
			removed, _, rmErr := metaSvc.RemoveFile(authCtx, toDir, dstMatchedName)
			if rmErr != nil {
				logger.Debug("SET_INFO: rename overwrite pre-remove failed",
					"name", dstMatchedName, "error", rmErr)
				return setInfoStatus(types.StatusForErr(rmErr)), nil
			}
			// RemoveFile drops the name and the inode but never the bytes; its
			// PayloadID is empty whenever the content must survive.
			if removed != nil {
				h.purgeBlockStorePayload(ctx.Context, toDir, removed.PayloadID, dstMatchedName, "SET_INFO rename overwrite")
			}
		}

		// Move stamps the renamed inode's LastChangeTime. A client holding
		// this handle open must keep observing the ChangeTime it was handed at
		// CREATE, so put the pre-rename value back.
		var clobbered *metadata.File
		var renameWcc *metadata.RenameWcc
		clobbered, renameWcc, err = metaSvc.Move(authCtx, srcParentHandle, oldFileName, toDir, toName)
		if err != nil {
			logger.Debug("SET_INFO: rename failed",
				"from", openFile.Name().Path,
				"to", newPath,
				"error", err)
			return setInfoStatus(types.StatusForErr(err)), nil
		}

		h.restorePreRenameChangeTime(authCtx.Context, openFile.MetadataHandle, renameWcc)

		// The pre-remove above only fires for a case-mismatched destination, so
		// an exact-case ReplaceIfExists overwrite reaches Move's own clobber
		// path instead, and its victim's bytes are released here.
		if clobbered != nil {
			h.purgeBlockStorePayload(ctx.Context, toDir, clobbered.PayloadID, toName, "SET_INFO rename")
		}

		// Move's LastChangeTime stamp is an automatic update, so a timestamp
		// frozen on this handle has to be put back the same way WRITE and
		// truncate put theirs back.
		h.restoreFrozenTimestamps(authCtx, openFile)

		// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): Restore frozen timestamps on parent directories.
		// Move updates both source and destination parent directory timestamps.
		h.restoreParentDirFrozenTimestamps(authCtx, srcParentHandle)
		if !bytes.Equal(toDir, srcParentHandle) {
			h.restoreParentDirFrozenTimestamps(authCtx, toDir)
		}

		// On successful completion of a rename, if the file was marked for
		// delete-on-close, clear that disposition. MS-FSA states no such rule;
		// 2.1.5.15.12 instead fails a rename whose Open.Link.IsDeleted is TRUE.
		// This prevents the renamed file from being deleted when the handle closes.
		openFile.mu.Lock()
		clearedDOC := openFile.DeletePending
		openFile.DeletePending = false
		openFile.mu.Unlock()
		if clearedDOC {
			logger.Debug("SET_INFO: cleared delete-on-close after rename",
				"oldPath", oldPath,
				"newPath", newPath)
		}

		// Notify watchers about the rename using paired notification.
		// Per MS-FSCC 2.7.1 (FILE_NOTIFY_INFORMATION), rename notifications MUST contain both
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
					"from", openFile.Name().Path,
					"to", newPath)
			}
		}

		// Update open file state to reflect the new path.
		// Compute actual resulting path from the destination directory and name,
		// since newPath may be relative when RootDirectory is non-zero.
		actualNewPath := newPath
		if !bytes.Equal(renameInfo.RootDirectory[:], zeroRootDir[:]) {
			// Handle-relative rename: build path from parent path + new name
			parentPath := GetParentPath(openFile.Name().Path)
			if parentPath == "" || parentPath == "/" {
				actualNewPath = toName
			} else {
				actualNewPath = parentPath + "/" + toName
			}
		}
		// The handle lock serializes this against a concurrent rename on the
		// same handle; readers get the triple from the single atomic swap.
		openFile.mu.Lock()
		openFile.SetName(OpenName{Path: actualNewPath, FileName: toName, ParentHandle: toDir})
		openFile.mu.Unlock()
		h.StoreOpenFile(openFile)

		// Per MS-FSA 2.1.5.15.12 ("FileRenameInformation") (smbtorture smb2.dirlease.rename):
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
		// FILE_DISPOSITION_INFORMATION_EX [MS-FSCC] 2.4.12 (FileDispositionInformationEx)
		// DeletePending (1 byte for class 13, 4 bytes flags for class 64)
		if len(buffer) < 1 {
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		// ignoreReadonly stays false for the 1-byte FileDispositionInformation:
		// it carries no flags and so can never waive the read-only refusal below.
		var deletePending, ignoreReadonly bool
		if class == types.FileDispositionInformationEx {
			// FileDispositionInformationEx uses a 4-byte Flags field per MS-FSCC 2.4.12 (FileDispositionInformationEx)
			if len(buffer) < 4 {
				return setInfoStatus(types.StatusInfoLengthMismatch), nil
			}
			dispR := smbenc.NewReader(buffer)
			flags := dispR.ReadUint32()

			// Per MS-FSCC 2.4.12: "if set and the file is not opened with
			// FILE_DELETE_ON_CLOSE, STATUS_NOT_SUPPORTED MUST be returned". That
			// refusal is the whole of what this flag does here — the disposition it
			// would otherwise select rides on the DELETE bit read below, so no
			// separate delete-on-close state is written. The create option is held
			// per-handle as InitialDeleteOnClose.
			if flags&types.FileDispositionOnClose != 0 && !openFile.InitialDeleteOnClose {
				logger.Debug("SET_INFO: FILE_DISPOSITION_ON_CLOSE on a handle not opened delete-on-close",
					"path", openFile.Name().Path)
				return setInfoStatus(types.StatusNotSupported), nil
			}

			deletePending = flags&types.FileDispositionDelete != 0
			ignoreReadonly = flags&types.FileDispositionIgnoreReadonlyAttribute != 0

			// FILE_DISPOSITION_POSIX_SEMANTICS is accepted and not acted on: the
			// link is removed from the namespace at close either way, and keeping
			// the data streams readable through other handles after the unlink is
			// not implemented.
		} else {
			deletePending = buffer[0] != 0
		}

		// Capture pre-state to suppress redundant break dispatches when the
		// disposition is reaffirmed (deletePending stays true).
		openFile.mu.RLock()
		wasDeletePending := openFile.DeletePending
		openFile.mu.RUnlock()

		// Validate we have parent info for deletion
		if delName := openFile.Name(); deletePending && len(delName.ParentHandle) == 0 {
			logger.Debug("SET_INFO: cannot delete root directory", "path", delName.Path)
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Per MS-FSA 2.1.5.15.3 ("FileDispositionInformation"): Setting delete disposition requires DELETE access.
		// Gate consults Open.GrantedAccess (post-DACL intersection at CREATE), not
		// the pre-DACL DesiredAccess — same fix class as #616 (ChangeNotify).
		if deletePending {
			if !hasDeleteAccess(openFile.GrantedAccess) {
				logger.Debug("SET_INFO: delete disposition without DELETE access",
					"path", openFile.Name().Path,
					"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
				return setInfoStatus(types.StatusAccessDenied), nil
			}

			// Per MS-FSCC 2.4.12 FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE
			// "allows files with the READ_ONLY attribute to be deleted anyway";
			// without it the refusal below is a MUST.
			if !openFile.IsDirectory && !ignoreReadonly {
				metaSvc := h.Registry.GetMetadataService()
				file, fileErr := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
				if fileErr == nil {
					attrs := FileAttrToSMBAttributes(&file.FileAttr)
					if attrs&types.FileAttributeReadonly != 0 {
						logger.Debug("SET_INFO: delete disposition on read-only file", "path", openFile.Name().Path)
						return setInfoStatus(types.StatusCannotDelete), nil
					}
				}
			}

			// Per MS-FSA 2.1.5.15.3 step 3.2.1 (and 2.1.5.15.4 for the Ex
			// class): marking a directory that still holds entries for
			// deletion is refused with STATUS_DIRECTORY_NOT_EMPTY. This is
			// the step that reports it — the close acting on the disposition
			// leaves a non-empty directory in place and still succeeds
			// (MS-FSA 2.1.5.5 phase 1), so a client that never sees the
			// refusal here never learns the removal did not happen.
			//
			// A directory that cannot be enumerated is not treated as
			// non-empty: the disposition is allowed through, leaving the close
			// to resolve it.
			if openFile.IsDirectory {
				metaSvc := h.Registry.GetMetadataService()
				// maxBytes is only a page-size hint, not an entry count — a
				// single entry is all this check has to see.
				page, dirErr := metaSvc.ReadDirectory(authCtx, openFile.MetadataHandle, 0, 1)
				if dirErr == nil && len(page.Entries) > 0 {
					logger.Debug("SET_INFO: delete disposition on non-empty directory",
						"path", openFile.Name().Path)
					return setInfoStatus(types.StatusDirectoryNotEmpty), nil
				}
			}
		}

		// Per MS-FSA 2.1.4.12 ("Algorithm to Check for an Oplock Break") / Samba source3/smbd/smb2_setinfo.c
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
					"path", openFile.Name().Path, "error", breakErr)
			}
		}

		// Mark file for deletion on close and record the DOC setter's
		// parent key for unlink parent-key suppression. Written under the
		// handle lock: the delete-pending gates and the CLOSE delete-on-close
		// election read these fields under it.
		openFile.mu.Lock()
		openFile.DeletePending = deletePending
		if deletePending {
			openFile.DeleteOnCloseParentKey = openFile.ParentLeaseKey
			openFile.HasDeleteOnCloseParentKey = openFile.HasParentLeaseKey
		}
		openFile.mu.Unlock()
		h.StoreOpenFile(openFile)

		// Per [MS-FSA] 2.1.5.15.3 step 3.2.3.2 (and 2.1.5.15.4 step 4.3.3.2, which
		// states the same sweep for the Ex class this branch also serves):
		// marking a directory for deletion completes
		// every pending CHANGE_NOTIFY on that directory with
		// STATUS_DELETE_PENDING. The watcher is normally a different handle on
		// the same directory, so this cannot be reached from the close path
		// that answers this handle's own watch.
		//
		// Clearing the disposition drops the marker again. The one-way rule in
		// [MS-FSA] 2.1.1.6 (Per Open, item 21) is scoped to a single Open,
		// whose flag dies with the handle; the marker here is scoped to the
		// directory, so keeping it after the deletion has been called off would
		// leave a live directory permanently unwatchable.
		//
		// Only once NO open still carries the disposition, though: the marker
		// describes the directory, and the write above cleared one handle's
		// view of it. Dropping it while a sibling open still holds the
		// directory delete-pending would put the late-arriving CHANGE_NOTIFYs
		// straight back to waiting on a sweep that has already run.
		if openFile.IsDirectory && h.NotifyRegistry != nil {
			switch {
			case deletePending:
				h.NotifyRegistry.MarkDirectoryDeletePending(openFile.ShareName, openFile.Name().Path)
			case !h.isFileDeletePending(openFile.MetadataHandle):
				h.NotifyRegistry.ClearDeletePendingMark(openFile.ShareName, openFile.Name().Path)
			}
		}

		logger.Debug("SET_INFO: delete disposition set",
			"path", openFile.Name().Path,
			"deletePending", deletePending,
			"class", class)
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileEndOfFileInformation:
		// FILE_END_OF_FILE_INFORMATION [MS-FSCC] 2.4.14 (FileEndOfFileInformation)
		newSize, err := decodeEndOfFileInfo(buffer)
		if err != nil {
			return setInfoStatus(types.StatusInvalidParameter), nil
		}

		// Setting EOF is a size-changing data write. SMB authorizes it from the
		// open handle's GrantedAccess (post-DACL intersection at CREATE), not the
		// file's current POSIX mode — so a handle opened with FILE_WRITE_DATA on a
		// DOS-READONLY file may truncate it. Signal the metadata layer to
		// authorize the size change from the handle rather than re-deny on mode.
		authCtx.WriteAuthorizedByHandle = hasWriteAccess(openFile.GrantedAccess)

		// Break Level II (Read) oplocks held by other clients.
		// Per MS-SMB2 3.3.5.21.2 / MS-FSA 2.1.5.15.5 ("FileEndOfFileInformation"): truncation is a
		// data-modifying operation that invalidates read caches.
		// Required by smbtorture smb2.oplock.batch11/batch12.
		if h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
			lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
			if breakErr := h.LeaseManager.BreakReadLeasesOnWrite(lockFileHandle, openFile.ShareName, openFile.LeaseKey); breakErr != nil {
				logger.Debug("SET_INFO: oplock break on EOF set failed (non-fatal)", "path", openFile.Name().Path, "error", breakErr)
			}
			// MS-SMB2 §3.3.4.7 ("Object Store Indicates a Lease Break"):
			// truncation is a data-modifying op that
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
						"path", openFile.Name().Path,
						"count", purged)
				}
			}
		}

		metaSvc := h.Registry.GetMetadataService()

		// Check for conflicting byte-range locks. MS-FSA does not route a
		// FileEndOfFileInformation set through the lock-conflict algorithm of
		// 2.1.4.10; this reuses it to match Windows, which fails a truncate into
		// a range another session holds locked.
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
				"path", openFile.Name().Path, "newSize", newSize)
			return setInfoStatus(types.StatusFileLockConflict), nil
		}

		setAttrs := &metadata.SetAttrs{
			Size: &newSize,
		}

		// Snapshot the pre-truncate file BEFORE SetFileAttributes prunes
		// FileAttr.Blocks, so the block-store truncate reclaim below can reap
		// RefCount on every dropped block and discard the physical tail bytes.
		// Best-effort: a snapshot failure only forfeits reclaim, not the op.
		preFile, _ := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)

		_, err = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs)
		if err != nil {
			logger.Debug("SET_INFO: failed to set EOF", "path", openFile.Name().Path, "error", err)
			return setInfoStatus(types.StatusForErr(err)), nil
		}

		// Physically discard block data past the new EOF. SetFileAttributes
		// only prunes the metadata block list + size; without this the tail
		// bytes survive and a later re-extend re-exposes them as file content
		// instead of a zero-filled hole (data-integrity / info-leak bug). NFS
		// SETATTR/CREATE-truncate drive the same common helper.
		if rErr := common.ReclaimTruncatedBlocks(authCtx.Context, h.Registry, openFile.MetadataHandle, preFile, newSize); rErr != nil {
			logger.Warn("SET_INFO: block store truncate reclaim failed",
				"path", openFile.Name().Path, "size", newSize, "error", rErr)
		}

		// Restore frozen timestamps after truncation (which updates Mtime/Ctime)
		h.restoreFrozenTimestamps(authCtx, openFile)

		// Samba parity (fileio.c): SET_INFO EndOfFile also flushes the
		// pending delayed-write window.
		flushSmbDelayedWrite(openFile)
		h.StoreOpenFile(openFile)

		// Break parent directory leases on child EOF change (#470:
		// smb2.dirlease.seteof). Per MS-FSA 2.1.5.15.5 ("FileEndOfFileInformation"): size changes
		// are visible in READDIR results, invalidating parent-dir
		// Read + Handle caching. Parent-key suppression (C2) applies.
		h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)

		if h.NotifyRegistry != nil {
			h.notifyOpenFileModified(openFile, FileNotifyChangeSize)
		}

		return setInfoStatus(types.StatusSuccess), nil

	case types.FilePositionInformation:
		// FILE_POSITION_INFORMATION [MS-FSCC] 2.4.40 (FilePositionInformation) (8 bytes)
		// Per MS-FSA §2.1.5.15.10 ("FilePositionInformation"): If InputBufferSize is less than the size of
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
		// Per MS-FSA 2.1.5.15.1 ("FileAllocationInformation"): if Open.GrantedAccess
		// does not contain FILE_WRITE_DATA, the operation MUST be failed with
		// STATUS_ACCESS_DENIED. This class is exempt from the FILE_WRITE_ATTRIBUTES
		// gate above precisely because it carries this check, and the gate runs
		// before the lease break so a handle that may not write cannot invalidate
		// another client's cached read state.
		if !hasAccessRight(openFile.GrantedAccess, uint32(types.FileWriteData)) {
			logger.Debug("SET_INFO: allocation set without FILE_WRITE_DATA",
				"path", openFile.Name().Path,
				"grantedAccess", fmt.Sprintf("0x%x", openFile.GrantedAccess))
			return setInfoStatus(types.StatusAccessDenied), nil
		}

		// Allocation size is not persisted (DittoFS does not preallocate), but
		// per MS-FSA 2.1.5.15.1 ("FileAllocationInformation") and Samba `smbd_smb2_setinfo_lease_break_fsp_check`
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
				logger.Debug("SET_INFO: oplock break on allocation set failed (non-fatal)", "path", openFile.Name().Path, "error", breakErr)
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

			// Per MS-FSA 2.1.5.15.1 ("FileAllocationInformation"): when the requested AllocationSize is
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
					// Allocation-driven truncate is a size-changing data write —
					// authorize it from the open handle's GrantedAccess, not the
					// file's POSIX mode (handle-based SMB write authorization).
					authCtx.WriteAuthorizedByHandle = hasWriteAccess(openFile.GrantedAccess)
					if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{
						Size: &requested,
					}); err != nil {
						logger.Debug("SET_INFO: allocation-driven truncate failed",
							"path", openFile.Name().Path, "error", err)
						return setInfoStatus(types.StatusForErr(err)), nil
					}
					// Discard block data past the new EOF (curFile is the pre-op
					// snapshot). Same reclaim the FileEndOfFileInformation path
					// drives — without it a later re-extend re-exposes the tail
					// bytes as content instead of zeros.
					if rErr := common.ReclaimTruncatedBlocks(authCtx.Context, h.Registry, openFile.MetadataHandle, curFile, requested); rErr != nil {
						logger.Warn("SET_INFO: allocation-driven block truncate reclaim failed",
							"path", openFile.Name().Path, "size", requested, "error", rErr)
					}
					h.restoreFrozenTimestamps(authCtx, openFile)
					flushSmbDelayedWrite(openFile)
					h.StoreOpenFile(openFile)
					h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)
					if h.NotifyRegistry != nil {
						h.notifyOpenFileModified(openFile, FileNotifyChangeSize)
					}
				}
			}
		}
		return setInfoStatus(types.StatusSuccess), nil

	case types.FileModeInformation:
		// FILE_MODE_INFORMATION [MS-FSCC] 2.4.31 (FileModeInformation) (4 bytes). SET adjusts the
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
		// Per MS-FSA 2.1.5.15.8 ("FileModeInformation"): any bit set outside the valid FILE_MODE_*
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
		// FILE_LINK_INFORMATION [MS-FSCC] 2.4.28 (FileLinkInformation) — hard link creation.
		// Wire format mirrors FILE_RENAME_INFORMATION: ReplaceIfExists (1B),
		// Reserved (7B), RootDirectory (8B), FileNameLength (4B), FileName (UTF-16LE).
		return h.handleFileLinkInformation(ctx, authCtx, openFile, buffer)

	case types.FileFullEaInformation: // [MS-FSCC] 2.4.16 (FileFullEaInformation) - Extended attributes
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
				"path", openFile.Name().Path, "error", decErr)
			return setInfoStatus(types.StatusInvalidParameter), nil
		}
		for _, e := range entries {
			if isReservedACLXattrName(e.name) {
				logger.Debug("SET_INFO: FileFullEaInformation reserved name rejected",
					"path", openFile.Name().Path, "name", e.name)
				return setInfoStatus(types.StatusAccessDenied), nil
			}
		}

		// Persist the EA set/delete mutations through the metadata layer.
		// A zero-length value deletes the named EA; a non-empty value upserts
		// it (MS-FSCC §2.4.16 ("FileFullEaInformation")). EA-name matching is
		// case-insensitive per NTFS semantics — MS-FSCC defines the structure but
		// states no matching rule — and the metadata layer resolves them so casing
		// round-trips.
		metaSvc := h.Registry.GetMetadataService()
		setAttrs := &metadata.SetAttrs{EAMutations: eaMutationsFromEntries(entries)}
		if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, setAttrs); err != nil {
			logger.Debug("SET_INFO: FileFullEaInformation persist failed",
				"path", openFile.Name().Path, "error", err)
			return setInfoStatus(types.StatusForErr(err)), nil
		}

		logger.Debug("SET_INFO: FileFullEaInformation persisted",
			"path", openFile.Name().Path, "count", len(entries))
		if h.NotifyRegistry != nil {
			h.notifyOpenFileModified(openFile, FileNotifyChangeEa)
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

// restorePreRenameChangeTime puts back the ChangeTime the renamed inode had
// before Service.Move stamped its own. Move stamps the renamed inode's Ctime,
// but MS-FSA 2.1.5.15.12 note <187> defers that stamp until the handle is
// closed, so a client that renames through a handle it still holds open keeps
// observing the pre-rename ChangeTime. The normative text of 2.1.5.15.12 says
// only that a rename updates LastChangeTime; the note is the half that says
// when it becomes visible. Conformance case smb2.rename.simple_modtime pins it,
// by comparing a CREATE reply's change_time against a post-rename query on the
// same handle.
//
// Both timestamps come from the rename's own transaction rather than from a
// read taken here. That is what keeps the restore from erasing somebody else's
// update: an advance committed before the rename is already in SourcePreCtime,
// so putting it back is a no-op rather than a walk backwards, and the value
// compared against has been through the store, so it still compares equal on
// backends that truncate timestamps on the way in.
//
// The first of those holds on backends whose transaction serialises the read
// against concurrent writers. Under READ COMMITTED with an unlocked read —
// postgres — a write can still land inside the rename's own window, so there the
// erasure is narrowed rather than eliminated (#2324).
//
// There is no permission check on the restore. An explicit timestamp write is
// ownership-gated in the metadata layer while the rename itself is authorized
// on the parent directories, so writing the stamp back as the caller would land
// for an owner and be refused for everyone else: one rename, two observable
// ChangeTimes, chosen by a check the caller never asked for. A read-only share
// cannot reach here, because Move would already have refused.
//
// Only Ctime is restored: Move leaves the renamed inode's Mtime alone, and the
// parent directories' timestamps are handled by
// restoreParentDirFrozenTimestamps.
//
// ponytail: the rule being implemented is handle-scoped — what a handle that
// was already open keeps observing — but this writes the stored timestamp, so
// it stays file-scoped. The conditional restore removes the case that was
// actively wrong, a concurrent advance being walked backwards for everyone; it
// does not make the preserve per-handle, so a second handle on the same file
// still observes the restored value rather than the one the rename stamped.
// Upgrade to a per-OpenFile overlay consulted by QUERY_INFO — the seam
// applyFrozenTimestamps already uses — once that overlay can also say when to
// stop applying: it has to yield to the next real Ctime advance, including one
// made through a different handle, which an OpenFile field cannot see on its
// own. A directory enumeration reports a child's ChangeTime with no OpenFile at
// all, so an overlay does not cover that path either.

func (h *Handler) restorePreRenameChangeTime(ctx context.Context, handle metadata.FileHandle, wcc *metadata.RenameWcc) {
	if wcc == nil {
		return
	}
	if err := h.Registry.GetMetadataService().RestoreChangeTimeIfUnchanged(
		ctx, handle, wcc.SourceCtime, wcc.SourcePreCtime,
	); err != nil {
		logger.Debug("SET_INFO: restoring pre-rename ChangeTime failed", "error", err)
	}
}

// withTimestampHandleAuth returns a copy of authCtx carrying the open handle's
// FILE_WRITE_ATTRIBUTES grant, which authorizes an explicit timestamp write in
// the metadata layer in place of POSIX ownership. A copy, because the caller's
// AuthContext outlives the timestamp write the grant is meant for.

func withTimestampHandleAuth(authCtx *metadata.AuthContext, grantedAccess uint32) *metadata.AuthContext {
	scoped := *authCtx
	scoped.TimestampAuthorizedByHandle = hasAccessRight(grantedAccess, uint32(types.FileWriteAttributes))
	return &scoped
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
		"path", openFile.Name().Path,
		"mtimeFrozen", mtimeFrozen,
		"ctimeFrozen", ctimeFrozen,
		"atimeFrozen", atimeFrozen,
		"frozenMtime", frozenMtime,
		"frozenCtime", frozenCtime,
		"frozenAtime", frozenAtime)

	metaSvc := h.Registry.GetMetadataService()
	// The restore writes explicit timestamps. The handle being restored is the
	// one that froze them, and freezing required FILE_WRITE_ATTRIBUTES on it, so
	// carry that grant through rather than letting the restore succeed or fail
	// on who owns the file.
	if _, err := metaSvc.SetFileAttributes(
		withTimestampHandleAuth(authCtx, openFile.GrantedAccess),
		openFile.MetadataHandle, restoreAttrs); err != nil {
		logger.Debug("restoreFrozenTimestamps: failed", "path", openFile.Name().Path, "error", err)
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
// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): When a timestamp is frozen via SET_INFO with -1 sentinel,
// the timestamp MUST NOT be auto-updated by subsequent operations. The metadata layer
// (createEntry, removeFile, etc.) always updates parent directory timestamps. This
// method iterates open handles to find directory handles matching the given parent
// metadata handle and restores any frozen timestamps.
//
// The restore writes explicit timestamps, which the metadata layer gates on
// ownership, and the child operation's caller need not own the directory — so
// it is authorized by the freezing handle's own FILE_WRITE_ATTRIBUTES grant
// instead, the right SMB says governs a timestamp write. That grant is always
// present: the frozen flags are only ever set by SET_INFO FileBasicInformation,
// which is itself gated on FILE_WRITE_ATTRIBUTES.

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
		// Carry the grant of the handle that froze these values, rather than the
		// identity of whoever drove the child operation.
		if _, err := metaSvc.SetFileAttributes(
			withTimestampHandleAuth(authCtx, openFile.GrantedAccess),
			openFile.MetadataHandle, restoreAttrs); err != nil {
			logger.Debug("restoreParentDirFrozenTimestamps: failed",
				"path", openFile.Name().Path, "error", err)
		} else {
			// IsMtimeFrozen / IsCtimeFrozen / IsAtimeFrozen each take
			// openFile.mu (read); see #606. Cheap because the parent-dir
			// frozen log line is debug-gated.
			logger.Debug("restoreParentDirFrozenTimestamps: restored",
				"path", openFile.Name().Path,
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
