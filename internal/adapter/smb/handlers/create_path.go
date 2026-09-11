package handlers

import (
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// CREATE path resolution: pipe opens, root opens, path walking, and the
// new/overwrite file open arms.
func (h *Handler) handlePipeCreate(ctx *SMBHandlerContext, req *CreateRequest, tree *TreeConnection) (*CreateResponse, error) {
	// Normalize pipe name (remove leading/backslashes and "pipe\" prefix)
	pipeName := normalizeCreatePath(req.FileName)
	pipeName = strings.TrimPrefix(pipeName, "pipe/")
	pipeName = strings.ToLower(pipeName)

	logger.Debug("CREATE on IPC$ named pipe",
		"originalName", req.FileName,
		"normalizedName", pipeName)

	// Check if this is a supported pipe
	if !rpc.IsSupportedPipe(pipeName) {
		logger.Debug("CREATE: unsupported pipe", "pipeName", pipeName)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameNotFound}}, nil
	}

	// Update pipe manager with cached share list.
	// Cache is invalidated via Runtime.OnShareChange() callback.
	if shares := h.getCachedShares(); shares != nil {
		h.PipeManager.SetShares(shares)
	}

	// Generate file ID for the pipe
	smbFileID := h.GenerateFileID()

	// Create pipe state
	h.PipeManager.CreatePipe(smbFileID, pipeName)

	// Store open file entry for the pipe
	openFile := (&OpenFile{
		FileID:        smbFileID,
		TreeID:        ctx.TreeID,
		SessionID:     ctx.SessionID,
		ShareName:     tree.ShareName,
		OpenTime:      time.Now(),
		DesiredAccess: req.DesiredAccess,
		// Pipes have no DACL; the granted set is the resolved request.
		GrantedAccess: resolveAccessFlags(req.DesiredAccess),
		IsDirectory:   false,
		IsPipe:        true,
		PipeName:      pipeName,
	}).WithName(OpenName{Path: req.FileName})
	h.StoreOpenFile(openFile)

	logger.Debug("CREATE pipe successful",
		"fileID", fmt.Sprintf("%x", smbFileID),
		"pipeName", pipeName)

	// Build success response
	now := time.Now()
	return &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     0,
		CreateAction:    types.FileOpened,
		CreationTime:    now,
		LastAccessTime:  now,
		LastWriteTime:   now,
		ChangeTime:      now,
		AllocationSize:  0,
		EndOfFile:       0,
		FileAttributes:  types.FileAttributeNormal,
		FileID:          smbFileID,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// handleOpenRootCreate handles opening the root directory of a share.

func (h *Handler) handleOpenRootCreate(
	ctx *SMBHandlerContext,
	req *CreateRequest,
	authCtx *metadata.AuthContext,
	rootHandle metadata.FileHandle,
	tree *TreeConnection,
) (*CreateResponse, error) {
	// Root can only be opened with FILE_OPEN disposition
	if req.CreateDisposition != types.FileOpen && req.CreateDisposition != types.FileOpenIf {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameCollision}}, nil
	}

	// Get root file attributes
	metaSvc := h.Registry.GetMetadataService()
	rootFile, err := metaSvc.GetFile(authCtx.Context, rootHandle)
	if err != nil {
		logger.Warn("CREATE: failed to get root file", "error", err)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameNotFound}}, nil
	}

	// Store open file
	smbFileID := h.GenerateFileID()
	// Share-root open: report the DACL-evaluated per-bit granted mask per
	// MS-SMB2 §3.3.5.9 paragraph 8. CheckFileAccess returns the granted
	// intersection on both arms (allow AND ErrAccessDenied). Share-root
	// access has already been authorised upstream (share-level permission via
	// ResolveSharePermission at mount + Tree Connect ACL); a partial-deny here
	// therefore reflects a narrower DACL than the share-level grant, not a fatal denial, so we
	// log it and continue with the (possibly narrowed) mask rather than
	// overstate rights by re-resolving DesiredAccess.
	var grantedAccess uint32
	if metaSvc := h.Registry.GetMetadataService(); metaSvc != nil {
		g, err := metaSvc.CheckFileAccess(rootFile, authCtx, req.DesiredAccess)
		if err != nil {
			logger.Debug("CREATE: share-root CheckFileAccess returned narrowed mask",
				"share", tree.ShareName,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
				"granted", fmt.Sprintf("0x%x", g),
				"error", err)
		}
		grantedAccess = g
	} else {
		// No metadata service available: fall back to the resolved mask.
		grantedAccess = resolveAccessFlags(req.DesiredAccess)
	}
	// Grant a directory lease when the open requests one, mirroring the
	// fresh directory-open path. The root is always a directory, so this
	// only ever grants a directory lease (never a traditional oplock).
	// Without a granted lease the client cannot cache the root directory's
	// identity at mount, so a later stat re-fetches the real inode number
	// and, under serverino, the mismatch surfaces as a stale-handle error.
	var grantedOplock uint8
	var leaseResponse *LeaseResponseContext
	if req.OplockLevel == OplockLevelLease && h.LeaseManager != nil {
		if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
			var newLeaseKey [16]byte
			if parsed, decErr := DecodeLeaseCreateContext(leaseCtx.Data); decErr == nil && parsed != nil {
				newLeaseKey = parsed.LeaseKey
			}
			disallowWriteLease := h.disallowWriteLeaseForFile(
				authCtx.Context, rootHandle, newLeaseKey, smbFileID, connClientGUID(ctx),
			)
			statOpenLease := isStatOnlyOpen(req.DesiredAccess) &&
				!isDestructiveDisposition(req.CreateDisposition)
			var leaseErr error
			leaseResponse, leaseErr = ProcessLeaseCreateContext(
				authCtx.Context,
				h.LeaseManager,
				leaseCtx.Data,
				lock.FileHandle(rootHandle),
				ctx.SessionID,
				connClientGUID(ctx),
				fmt.Sprintf("smb:%d", ctx.SessionID),
				tree.ShareName,
				true, // the share root is always a directory
				disallowWriteLease,
				statOpenLease,
			)
			if leaseErr != nil {
				logger.Debug("CREATE: share-root lease context processing failed", "error", leaseErr)
			}
			if leaseResponse != nil {
				grantedOplock = OplockLevelLease
			}
		}
	}

	openFile := &OpenFile{
		FileID:         smbFileID,
		TreeID:         ctx.TreeID,
		SessionID:      ctx.SessionID,
		ShareName:      tree.ShareName,
		OpenTime:       time.Now(),
		DesiredAccess:  req.DesiredAccess,
		GrantedAccess:  grantedAccess,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
		OplockLevel:    grantedOplock,
	}
	if leaseResponse != nil && leaseResponse.LeaseState != lock.LeaseStateNone {
		openFile.LeaseKey = leaseResponse.LeaseKey
	}
	// Record the RqLs parent-lease-key linkage so break coordination on this
	// handle can apply the parent-key suppression rule, matching the non-root
	// directory-open path.
	if leaseResponse != nil && leaseResponse.HasParent {
		openFile.ParentLeaseKey = leaseResponse.ParentLeaseKey
		openFile.HasParentLeaseKey = true
	}
	// Snapshot opener identity so handle-bound ops survive re-auth (#772).
	h.CaptureOpenerIdentity(ctx, openFile)
	h.StoreOpenFile(openFile)

	creation, access, write, change := FileAttrToSMBTimes(&rootFile.FileAttr)

	resp := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     grantedOplock,
		CreateAction:    types.FileOpened,
		CreationTime:    creation,
		LastAccessTime:  access,
		LastWriteTime:   write,
		ChangeTime:      change,
		AllocationSize:  0,
		EndOfFile:       0,
		FileAttributes:  types.FileAttributeDirectory,
		FileID:          smbFileID,
	}
	if leaseResponse != nil {
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: LeaseContextTagResponse,
			Data: leaseResponse.Encode(),
		})
	}
	// Answer the on-disk-id (QFid) request with the root's stable file ID, the
	// same value FILE_ALL reports as the inode number. A serverino client that
	// asks for the on-disk id at mount (instead of a separate query) uses it as
	// the root inode identity; without this response it derives a fabricated
	// number that every later stat then contradicts, yielding a stale handle.
	if FindCreateContext(req.CreateContexts, "QFid") != nil {
		qfidFileID := h.baseFileUUID(authCtx, nil, "", rootFile.ID)
		qfidResp := make([]byte, 32)
		copy(qfidResp[0:16], qfidFileID[:16])
		copy(qfidResp[16:32], h.ServerGUID[:])
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: "QFid",
			Data: qfidResp,
		})
	}
	return resp, nil
}

// walkPath walks a path from a starting handle, returning the final handle.

func (h *Handler) walkPath(
	authCtx *metadata.AuthContext,
	startHandle metadata.FileHandle,
	pathStr string,
) (metadata.FileHandle, error) {
	currentHandle := startHandle
	metaSvc := h.Registry.GetMetadataService()

	// Split path into components
	parts := strings.Split(pathStr, "/")
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			// Navigate to parent directory using Lookup which handles ".." natively
			parentFile, err := metaSvc.Lookup(authCtx, currentHandle, "..")
			if err != nil {
				return nil, fmt.Errorf("walkPath: lookup parent '..': %w", err)
			}
			currentHandle, err = metadata.EncodeFileHandle(parentFile)
			if err != nil {
				return nil, fmt.Errorf("encode parent handle: %w", err)
			}
			continue
		}

		file, _, lookupErr := h.lookupCaseInsensitive(authCtx, metaSvc, currentHandle, part)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if file == nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("child not found: %s", part),
			}
		}

		if file.Type != metadata.FileTypeDirectory {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: fmt.Sprintf("%s is not a directory", part),
			}
		}

		var encErr error
		currentHandle, encErr = metadata.EncodeFileHandle(file)
		if encErr != nil {
			return nil, encErr
		}
	}

	return currentHandle, nil
}

// createNewFile creates a new file or directory in the metadata store.

func (h *Handler) createNewFile(
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	parentFile *metadata.File,
	name string,
	req *CreateRequest,
	isDirectory bool,
) (*metadata.File, metadata.FileHandle, error) {
	// Build file attributes
	fileAttr := &metadata.FileAttr{
		Mode:   SMBModeFromAttrs(req.FileAttributes, isDirectory),
		Hidden: req.FileAttributes&types.FileAttributeHidden != 0,
	}

	// Set owner from auth context
	if authCtx.Identity.UID != nil {
		fileAttr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		fileAttr.GID = *authCtx.Identity.GID
	}

	if isDirectory {
		fileAttr.Type = metadata.FileTypeDirectory
	} else {
		fileAttr.Type = metadata.FileTypeRegular
		fileAttr.Size = 0
	}

	// Inherit compression state from parent directory.
	// Per MS-FSA 2.1.5.1.1: if the parent directory has FILE_ATTRIBUTE_COMPRESSED,
	// the new file/directory inherits the compression attribute.
	// Per MS-SMB2 2.2.13: FILE_NO_COMPRESSION in CreateOptions suppresses inheritance.
	metaSvc := h.Registry.GetMetadataService()
	if req.CreateOptions&types.FileNoCompression == 0 {
		if parentFile != nil && parentFile.Mode&modeDOSCompressed != 0 {
			fileAttr.Mode |= modeDOSCompressed
		}
	}

	// Create appropriate file type based on fileAttr.Type
	var file *metadata.File
	var err error
	if isDirectory {
		file, _, err = metaSvc.CreateDirectory(authCtx, parentHandle, name, fileAttr)
	} else {
		file, _, err = metaSvc.CreateFile(authCtx, parentHandle, name, fileAttr)
	}

	if err != nil {
		return nil, nil, err
	}

	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, nil, err
	}

	return file, fileHandle, nil
}

// overwriteFile truncates an existing file for OVERWRITE/SUPERSEDE operations.

func (h *Handler) overwriteFile(
	authCtx *metadata.AuthContext,
	existingFile *metadata.File,
	req *CreateRequest,
) (*metadata.File, metadata.FileHandle, error) {
	fileHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		return nil, nil, err
	}

	// Truncate to zero size and apply requested attributes
	zeroSize := uint64(0)
	setAttrs := &metadata.SetAttrs{
		Size: &zeroSize,
	}

	// Per MS-FSA 2.1.5.1.2 ("Open of an Existing File"): OVERWRITE/SUPERSEDE forces FILE_ATTRIBUTE_ARCHIVE
	// on the post-overwrite metadata regardless of what the client sent — the
	// data is "needs backup" again. Apply the requested attributes plus ARCHIVE,
	// and preserve modeDOSCompressed (controlled only via FSCTL_SET_COMPRESSION).
	attrs := req.FileAttributes | types.FileAttributeArchive
	mode := SMBModeFromAttrs(attrs, existingFile.Type == metadata.FileTypeDirectory)
	mode |= existingFile.Mode & modeDOSCompressed
	setAttrs.Mode = &mode
	hiddenVal := req.FileAttributes&types.FileAttributeHidden != 0
	setAttrs.Hidden = &hiddenVal

	metaSvc := h.Registry.GetMetadataService()
	_, err = metaSvc.SetFileAttributes(authCtx, fileHandle, setAttrs)
	if err != nil {
		return nil, nil, err
	}

	// Get updated file
	updatedFile, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		return nil, nil, err
	}

	return updatedFile, fileHandle, nil
}

// updateBaseObjectCtime updates the ChangeTime of the base file or directory
// that hosts an ADS. Per MS-FSA / NTFS semantics, creating or modifying an
// alternate data stream propagates a ChangeTime update to the base object.

func (h *Handler) updateBaseObjectCtime(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.Service,
	parentHandle metadata.FileHandle,
	baseObjectName string,
) {
	baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseObjectName)
	if baseFile == nil {
		return
	}
	baseHandle, err := metadata.EncodeFileHandle(baseFile)
	if err != nil {
		return
	}
	now := time.Now()
	if _, updateErr := metaSvc.SetFileAttributes(authCtx, baseHandle, &metadata.SetAttrs{Ctime: &now}); updateErr != nil {
		logger.Debug("updateBaseObjectCtime: failed",
			"baseObject", baseObjectName, "error", updateErr)
	}
}

// updateBaseObjectTimestampsForADSWrite updates the ChangeTime and LastWriteTime
// of the base file or directory that hosts an ADS after a WRITE to the stream.
// Per MS-FSA / NTFS semantics, data writes to an alternate data stream propagate
// Mtime and Ctime changes to the base object, unless the corresponding timestamp
// is frozen on the ADS handle.
//
// parentHandle is passed in rather than read off openFile so it stays the same
// directory the caller derived baseObjectName from: SET_INFO rename can move
// the handle to another parent while the write is in flight.

func (h *Handler) updateBaseObjectTimestampsForADSWrite(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.Service,
	openFile *OpenFile,
	parentHandle metadata.FileHandle,
	baseObjectName string,
) {
	baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseObjectName)
	if baseFile == nil {
		return
	}
	baseHandle, err := metadata.EncodeFileHandle(baseFile)
	if err != nil {
		return
	}
	now := time.Now()
	setAttrs := &metadata.SetAttrs{}
	// Snapshot the freeze flags under the per-OpenFile read lock so we
	// observe a consistent view against a concurrent SET_INFO freeze/thaw
	// (#606).
	openFile.mu.RLock()
	ctimeFrozen := openFile.CtimeFrozen
	mtimeFrozen := openFile.MtimeFrozen
	openFile.mu.RUnlock()
	if !ctimeFrozen {
		setAttrs.Ctime = &now
	}
	if !mtimeFrozen {
		setAttrs.Mtime = &now
	}
	if setAttrs.Ctime == nil && setAttrs.Mtime == nil {
		return
	}
	// If only one timestamp is frozen, metadata's SetFileAttributes will
	// auto-bump Ctime to NOW because modified=true and attrs.Ctime==nil
	// (file_modify.go: `if modified { if attrs.Ctime == nil { file.Ctime = now }}`).
	// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"), the freeze sentinel applies to the underlying
	// object, so an ADS write must not bump the base's frozen ChangeTime
	// (WPTS FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime).
	// Pin Ctime to the base's current value when the ADS handle has Ctime frozen.
	if ctimeFrozen && setAttrs.Ctime == nil {
		baseCtime := baseFile.Ctime
		setAttrs.Ctime = &baseCtime
	}
	_, _ = metaSvc.SetFileAttributes(authCtx, baseHandle, setAttrs)
}

// isStatOnlyOpen returns true when DesiredAccess contains only stat-open bits:
// FILE_READ_ATTRIBUTES, FILE_WRITE_ATTRIBUTES, READ_CONTROL, SYNCHRONIZE —
// in any combination, but with no other (data/delete/dac/owner) bits set.
// At least one stat bit must be present.
//
// Mirrors Samba `is_lease_stat_open` (source3/smbd/open.c):
//
//	SEC_STD_SYNCHRONIZE | SEC_STD_READ_CONTROL |
//	FILE_READ_ATTRIBUTES | FILE_WRITE_ATTRIBUTES
//
// READ_CONTROL is included per smb2.lease.statopen4 test 8 which
// requires READ_CONTROL-only opens to NOT break leases. Samba's
// is_lease_stat_open includes SEC_STD_READ_CONTROL as well.
