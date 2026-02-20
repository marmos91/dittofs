package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleOpen implements the OPEN operation (RFC 7530 Section 16.16).
//
// OPEN creates regular files (OPEN4_CREATE + CLAIM_NULL) and opens existing
// files (OPEN4_NOCREATE + CLAIM_NULL). It creates tracked state via
// StateManager.OpenFile and returns a real stateid.
//
// Delegation handling (Plan 11-03):
//   - CLAIM_NULL: checks for delegation conflicts (returns NFS4ERR_DELAY if
//     conflicting delegation from another client), then tries to grant a
//     delegation if conditions are met (exclusive access, callback path up)
//   - CLAIM_DELEGATE_CUR: opens file using existing delegation stateid
//   - CLAIM_DELEGATE_PREV: NFS4ERR_NOTSUPP (requires persistent delegation state)
//
// Grace period handling:
//   - CLAIM_NULL: Blocked with NFS4ERR_GRACE during grace period
//   - CLAIM_PREVIOUS: Allowed during grace for state reclaim; blocked with
//     NFS4ERR_NO_GRACE outside grace period
//
// Wire format args (OPEN4args):
//
//	uint32   seqid          (open-owner sequence number)
//	uint32   share_access   (OPEN4_SHARE_ACCESS_READ/WRITE/BOTH)
//	uint32   share_deny     (OPEN4_SHARE_DENY_NONE/READ/WRITE/BOTH)
//	open_owner4:
//	  uint64   clientid
//	  opaque   owner<>
//	openflag4:
//	  uint32   opentype     (OPEN4_NOCREATE or OPEN4_CREATE)
//	  [if CREATE: createhow4]
//	open_claim4:
//	  uint32   claim_type   (CLAIM_NULL, CLAIM_PREVIOUS, etc.)
//	  [if CLAIM_NULL: component4 filename]
//	  [if CLAIM_PREVIOUS: uint32 delegation_type]
//
// Wire format res (success - OPEN4resok):
//
//	nfsstat4    status       (NFS4_OK)
//	stateid4    stateid      (tracked stateid from StateManager)
//	change_info4 cinfo       (parent directory change)
//	uint32      rflags       (OPEN4_RESULT_CONFIRM if new owner)
//	bitmap4     attrset      (empty - no attrs set by server)
//	open_delegation4:
//	  uint32    delegation_type (OPEN_DELEGATE_NONE / READ / WRITE)
func (h *Handler) handleOpen(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle (parent directory for CLAIM_NULL)
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return openError(status)
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return openError(types.NFS4ERR_ROFS)
	}

	// ========================================================================
	// Decode OPEN4args
	// ========================================================================

	// 1. seqid (uint32) - open-owner sequence number
	seqid, err := xdr.DecodeUint32(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}
	logger.Debug("NFSv4 OPEN", "seqid", seqid, "client", ctx.ClientAddr)

	// 2. share_access (uint32)
	shareAccess, err := xdr.DecodeUint32(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	// 3. share_deny (uint32)
	shareDeny, err := xdr.DecodeUint32(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}
	logger.Debug("NFSv4 OPEN share", "access", shareAccess, "deny", shareDeny)

	// 4. open_owner4: clientid (uint64) + owner (XDR opaque)
	clientID, err := xdr.DecodeUint64(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}
	ownerData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	// 5. openflag4: opentype (uint32)
	openType, err := xdr.DecodeUint32(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	var createMode uint32
	var createAttrs *metadata.SetAttrs

	if openType == types.OPEN4_CREATE {
		// Decode createhow4: createmode (uint32)
		createMode, err = xdr.DecodeUint32(reader)
		if err != nil {
			return openError(types.NFS4ERR_BADXDR)
		}

		switch createMode {
		case types.UNCHECKED4, types.GUARDED4:
			// Decode createattrs (fattr4 = bitmap4 + opaque)
			setAttrs, _, fattr4Err := attrs.DecodeFattr4ToSetAttrs(reader)
			if fattr4Err != nil {
				if nfsErr, ok := fattr4Err.(attrs.NFS4StatusError); ok {
					return openError(nfsErr.NFS4Status())
				}
				return openError(types.NFS4ERR_BADXDR)
			}
			createAttrs = setAttrs
		case types.EXCLUSIVE4:
			// Decode verifier (8 bytes) -- treat as GUARDED4
			var verifier [8]byte
			if _, err := io.ReadFull(reader, verifier[:]); err != nil {
				return openError(types.NFS4ERR_BADXDR)
			}
			createMode = types.GUARDED4
		default:
			return openError(types.NFS4ERR_INVAL)
		}
	}

	// 6. open_claim4: claim_type (uint32)
	claimType, err := xdr.DecodeUint32(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	// ========================================================================
	// Dispatch by claim type
	// ========================================================================

	switch claimType {
	case types.CLAIM_NULL:
		// New open: check grace period BEFORE file creation/lookup
		if graceErr := h.StateManager.CheckGraceForNewState(); graceErr != nil {
			nfsStatus := mapStateError(graceErr)
			logger.Debug("NFSv4 OPEN blocked by grace period",
				"claim_type", "CLAIM_NULL",
				"client", ctx.ClientAddr)
			return openError(nfsStatus)
		}

		return h.handleOpenClaimNull(ctx, reader, seqid, shareAccess, shareDeny,
			clientID, ownerData, openType, createMode, claimType, createAttrs)

	case types.CLAIM_PREVIOUS:
		return h.handleOpenClaimPrevious(ctx, reader, seqid, shareAccess, shareDeny,
			clientID, ownerData, claimType)

	case types.CLAIM_DELEGATE_CUR:
		return h.handleOpenClaimDelegateCur(ctx, reader, seqid, shareAccess, shareDeny,
			clientID, ownerData)

	case types.CLAIM_DELEGATE_PREV:
		// CLAIM_DELEGATE_PREV: reclaiming delegations after server restart.
		// Requires persistent delegation state which is out of scope for Phase 11.
		// Must consume the component4 arg to prevent XDR desync.
		xdr.DecodeString(reader) //nolint:errcheck
		return openError(types.NFS4ERR_NOTSUPP)

	default:
		return openError(types.NFS4ERR_INVAL)
	}
}

// handleOpenClaimNull handles the CLAIM_NULL path for OPEN.
// This is the primary code path for new file opens and creates.
func (h *Handler) handleOpenClaimNull(
	ctx *types.CompoundContext,
	reader io.Reader,
	seqid, shareAccess, shareDeny uint32,
	clientID uint64, ownerData []byte,
	openType, createMode, claimType uint32,
	createAttrs *metadata.SetAttrs,
) *types.CompoundResult {
	// CLAIM_NULL: decode filename (component4 = XDR string)
	filename, err := xdr.DecodeString(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	if status := types.ValidateUTF8Filename(filename); status != types.NFS4_OK {
		return openError(status)
	}

	// ========================================================================
	// Build auth context and get services
	// ========================================================================

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 OPEN auth context failed",
			"error", err,
			"client", ctx.ClientAddr)
		return openError(types.NFS4ERR_SERVERFAULT)
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return openError(types.NFS4ERR_SERVERFAULT)
	}

	parentHandle := metadata.FileHandle(ctx.CurrentFH)

	// Get pre-operation parent attributes for change_info
	parentFile, err := metaSvc.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return openError(types.MapMetadataErrorToNFS4(err))
	}
	beforeCtime := uint64(parentFile.Ctime.UnixNano())

	// ========================================================================
	// Execute OPEN logic
	// ========================================================================

	var fileHandle metadata.FileHandle
	var created bool

	if openType == types.OPEN4_NOCREATE {
		// Open existing file
		child, lookupErr := metaSvc.Lookup(authCtx, parentHandle, filename)
		if lookupErr != nil {
			return openError(types.MapMetadataErrorToNFS4(lookupErr))
		}
		fh, encErr := metadata.EncodeFileHandle(child)
		if encErr != nil {
			return openError(types.NFS4ERR_SERVERFAULT)
		}
		// Check access permissions for the requested share_access
		if accessErr := checkOpenAccess(metaSvc, authCtx, fh, shareAccess); accessErr != nil {
			return openError(types.MapMetadataErrorToNFS4(accessErr))
		}
		fileHandle = fh
	} else {
		// OPEN4_CREATE: create or open existing
		child, lookupErr := metaSvc.Lookup(authCtx, parentHandle, filename)
		if lookupErr == nil {
			// File exists
			if createMode == types.GUARDED4 {
				return openError(types.NFS4ERR_EXIST)
			}
			// UNCHECKED4: open existing -- no error
			fh, encErr := metadata.EncodeFileHandle(child)
			if encErr != nil {
				return openError(types.NFS4ERR_SERVERFAULT)
			}
			// Check access permissions for the requested share_access
			if accessErr := checkOpenAccess(metaSvc, authCtx, fh, shareAccess); accessErr != nil {
				return openError(types.MapMetadataErrorToNFS4(accessErr))
			}
			fileHandle = fh
		} else {
			// File doesn't exist: create it
			uid, gid := effectiveUIDGID(authCtx)
			fileMode := uint32(0o644) // Default mode
			if createAttrs != nil && createAttrs.Mode != nil {
				fileMode = *createAttrs.Mode
			}
			newFile, createErr := metaSvc.CreateFile(authCtx, parentHandle, filename, &metadata.FileAttr{
				Mode: fileMode,
				UID:  uid,
				GID:  gid,
			})
			if createErr != nil {
				return openError(types.MapMetadataErrorToNFS4(createErr))
			}
			fh, encErr := metadata.EncodeFileHandle(newFile)
			if encErr != nil {
				return openError(types.NFS4ERR_SERVERFAULT)
			}
			fileHandle = fh
			created = true
		}
	}

	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Get post-operation parent attributes for change_info
	afterCtime := beforeCtime
	if created {
		parentFileAfter, getErr := metaSvc.GetFile(ctx.Context, parentHandle)
		if getErr == nil {
			afterCtime = uint64(parentFileAfter.Ctime.UnixNano())
		}
	}

	// ========================================================================
	// Check delegation conflicts BEFORE creating state
	// ========================================================================
	// If another client holds a conflicting delegation, trigger async CB_RECALL
	// and return NFS4ERR_DELAY so the client retries the entire OPEN.

	conflict, _ := h.StateManager.CheckDelegationConflict([]byte(fileHandle), clientID, shareAccess)
	if conflict {
		logger.Debug("NFSv4 OPEN delegation conflict, returning NFS4ERR_DELAY",
			"file", filename,
			"client_id", clientID,
			"client", ctx.ClientAddr)
		return openError(types.NFS4ERR_DELAY)
	}

	// ========================================================================
	// Create tracked state via StateManager
	// ========================================================================

	openResult, stateErr := h.StateManager.OpenFile(
		clientID, ownerData, seqid,
		[]byte(fileHandle),
		shareAccess, shareDeny,
		claimType,
	)
	if stateErr != nil {
		return openError(mapStateError(stateErr))
	}

	if openResult.IsReplay {
		return &types.CompoundResult{
			Status: openResult.CachedStatus,
			OpCode: types.OP_OPEN,
			Data:   openResult.CachedData,
		}
	}

	// ========================================================================
	// Try to grant a delegation
	// ========================================================================

	delegType, shouldGrant := h.StateManager.ShouldGrantDelegation(clientID, []byte(fileHandle), shareAccess)
	var deleg *state.DelegationState
	if shouldGrant {
		deleg = h.StateManager.GrantDelegation(clientID, []byte(fileHandle), delegType)
	}

	logger.Debug("NFSv4 OPEN successful",
		"file", filename,
		"created", created,
		"openType", openType,
		"createMode", createMode,
		"stateid_seqid", openResult.Stateid.Seqid,
		"rflags", openResult.RFlags,
		"delegation", delegType,
		"client", ctx.ClientAddr)

	// ========================================================================
	// Encode OPEN4resok
	// ========================================================================

	return h.encodeOpenResult(clientID, ownerData, &openResult.Stateid, openResult.RFlags,
		beforeCtime, afterCtime, deleg)
}

// handleOpenClaimPrevious handles the CLAIM_PREVIOUS path for OPEN.
// This is the reclaim code path used during the grace period after server restart.
//
// Per RFC 7530 Section 16.16.4, CLAIM_PREVIOUS args:
//
//	uint32  delegate_type  (delegation type being reclaimed, or OPEN_DELEGATE_NONE)
//
// The client is requesting to reclaim previously-held state. The StateManager
// handles grace period checking internally: allowed during grace, returns
// NFS4ERR_NO_GRACE outside grace.
func (h *Handler) handleOpenClaimPrevious(
	ctx *types.CompoundContext,
	reader io.Reader,
	seqid, shareAccess, shareDeny uint32,
	clientID uint64, ownerData []byte,
	claimType uint32,
) *types.CompoundResult {
	// Decode CLAIM_PREVIOUS args: delegate_type (uint32)
	_, err := xdr.DecodeUint32(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	// For CLAIM_PREVIOUS, the currentFH is the file being reclaimed.
	fileHandle := make([]byte, len(ctx.CurrentFH))
	copy(fileHandle, ctx.CurrentFH)

	logger.Debug("NFSv4 OPEN CLAIM_PREVIOUS",
		"seqid", seqid,
		"share_access", shareAccess,
		"client_id", clientID,
		"client", ctx.ClientAddr)

	// StateManager.OpenFile checks grace period for CLAIM_PREVIOUS:
	//   - During grace: allowed, notifies grace period of reclaim
	//   - Outside grace: returns NFS4ERR_NO_GRACE
	openResult, stateErr := h.StateManager.OpenFile(
		clientID, ownerData, seqid,
		fileHandle,
		shareAccess, shareDeny,
		claimType,
	)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 OPEN CLAIM_PREVIOUS failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return openError(nfsStatus)
	}

	if openResult.IsReplay {
		return &types.CompoundResult{
			Status: openResult.CachedStatus,
			OpCode: types.OP_OPEN,
			Data:   openResult.CachedData,
		}
	}

	logger.Debug("NFSv4 OPEN CLAIM_PREVIOUS successful",
		"stateid_seqid", openResult.Stateid.Seqid,
		"rflags", openResult.RFlags,
		"client", ctx.ClientAddr)

	// Use dummy change_info (reclaim doesn't create new files)
	return h.encodeOpenResult(clientID, ownerData, &openResult.Stateid, openResult.RFlags,
		0, 0, nil)
}

// encodeOpenResult encodes the OPEN4resok response shared by all claim paths.
//
// The deleg parameter controls the open_delegation4 response:
//   - nil: OPEN_DELEGATE_NONE
//   - non-nil: full delegation encoding (stateid, recall, ACE, space limit)
func (h *Handler) encodeOpenResult(
	clientID uint64, ownerData []byte,
	stateid *types.Stateid4, rflags uint32,
	beforeCtime, afterCtime uint64,
	deleg *state.DelegationState,
) *types.CompoundResult {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	types.EncodeStateid4(&buf, stateid)
	encodeChangeInfo4(&buf, true, beforeCtime, afterCtime)
	_ = xdr.WriteUint32(&buf, rflags)
	_ = xdr.WriteUint32(&buf, 0) // attrset: empty bitmap
	state.EncodeDelegation(&buf, deleg)

	// Cache the result for replay detection
	h.StateManager.CacheOpenResult(clientID, ownerData, types.NFS4_OK, buf.Bytes())

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_OPEN,
		Data:   buf.Bytes(),
	}
}

// handleOpenConfirm implements the OPEN_CONFIRM operation (RFC 7530 Section 16.20).
//
// OPEN_CONFIRM promotes an unconfirmed open-owner to confirmed status via
// StateManager.ConfirmOpen. The stateid seqid is incremented on success.
//
// Wire format args:
//
//	stateid4  open_stateid
//	uint32    seqid
//
// Wire format res (success):
//
//	nfsstat4  status (NFS4_OK)
//	stateid4  open_stateid (seqid incremented)
func (h *Handler) handleOpenConfirm(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_OPEN_CONFIRM,
			Data:   encodeStatusOnly(status),
		}
	}

	// Decode OPEN_CONFIRM4args: stateid4 + seqid
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_OPEN_CONFIRM,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	confirmSeqid, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_OPEN_CONFIRM,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 OPEN_CONFIRM",
		"stateid_seqid", stateid.Seqid,
		"confirm_seqid", confirmSeqid,
		"client", ctx.ClientAddr)

	// Delegate to StateManager
	resultStateid, stateErr := h.StateManager.ConfirmOpen(stateid, confirmSeqid)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 OPEN_CONFIRM failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_OPEN_CONFIRM,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	types.EncodeStateid4(&buf, resultStateid)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_OPEN_CONFIRM,
		Data:   buf.Bytes(),
	}
}

// handleOpenClaimDelegateCur handles the CLAIM_DELEGATE_CUR path for OPEN.
//
// This is used when a client already holds a delegation and wants to open
// the file with CLAIM_DELEGATE_CUR, providing its delegation stateid.
//
// Per RFC 7530 Section 16.16.4, CLAIM_DELEGATE_CUR args:
//
//	stateid4    delegate_stateid  (the delegation stateid)
//	component4  file              (filename)
//
// The handler validates the delegation stateid, then proceeds like CLAIM_NULL
// but skips delegation conflict check and delegation grant (client already
// holds the delegation).
func (h *Handler) handleOpenClaimDelegateCur(
	ctx *types.CompoundContext,
	reader io.Reader,
	seqid, shareAccess, shareDeny uint32,
	clientID uint64, ownerData []byte,
) *types.CompoundResult {
	// Decode CLAIM_DELEGATE_CUR args: stateid4 + component4
	delegStateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	filename, err := xdr.DecodeString(reader)
	if err != nil {
		return openError(types.NFS4ERR_BADXDR)
	}

	// Validate the delegation stateid
	_, delegErr := h.StateManager.ValidateDelegationStateid(delegStateid)
	if delegErr != nil {
		nfsStatus := mapStateError(delegErr)
		logger.Debug("NFSv4 OPEN CLAIM_DELEGATE_CUR: invalid delegation stateid",
			"error", delegErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return openError(nfsStatus)
	}

	if status := types.ValidateUTF8Filename(filename); status != types.NFS4_OK {
		return openError(status)
	}

	logger.Debug("NFSv4 OPEN CLAIM_DELEGATE_CUR",
		"file", filename,
		"seqid", seqid,
		"client_id", clientID,
		"client", ctx.ClientAddr)

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return openError(types.NFS4ERR_SERVERFAULT)
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return openError(types.NFS4ERR_SERVERFAULT)
	}

	parentHandle := metadata.FileHandle(ctx.CurrentFH)

	// Get pre-operation parent attributes for change_info
	parentFile, err := metaSvc.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return openError(types.MapMetadataErrorToNFS4(err))
	}
	beforeCtime := uint64(parentFile.Ctime.UnixNano())

	// Lookup the file (delegation holder opening an existing file)
	child, lookupErr := metaSvc.Lookup(authCtx, parentHandle, filename)
	if lookupErr != nil {
		return openError(types.MapMetadataErrorToNFS4(lookupErr))
	}

	fileHandle, encErr := metadata.EncodeFileHandle(child)
	if encErr != nil {
		return openError(types.NFS4ERR_SERVERFAULT)
	}

	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Create tracked state via StateManager (use CLAIM_NULL for state tracking).
	// The client already has a delegation, so no conflict check or grant needed.
	openResult, stateErr := h.StateManager.OpenFile(
		clientID, ownerData, seqid,
		[]byte(fileHandle),
		shareAccess, shareDeny,
		types.CLAIM_NULL,
	)
	if stateErr != nil {
		return openError(mapStateError(stateErr))
	}

	if openResult.IsReplay {
		return &types.CompoundResult{
			Status: openResult.CachedStatus,
			OpCode: types.OP_OPEN,
			Data:   openResult.CachedData,
		}
	}

	logger.Debug("NFSv4 OPEN CLAIM_DELEGATE_CUR successful",
		"file", filename,
		"stateid_seqid", openResult.Stateid.Seqid,
		"client", ctx.ClientAddr)

	// No delegation grant (client already has one)
	return h.encodeOpenResult(clientID, ownerData, &openResult.Stateid, openResult.RFlags,
		beforeCtime, beforeCtime, nil)
}

// openError builds an OPEN error result for the given NFS4 status code.
func openError(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_OPEN,
		Data:   encodeStatusOnly(status),
	}
}

// checkOpenAccess verifies that the caller has appropriate file-level permissions
// for the requested share_access mode. This is required by NFSv4 OPEN to enforce
// POSIX access control -- without it, any user can open any file regardless of mode bits.
func checkOpenAccess(metaSvc *metadata.MetadataService, authCtx *metadata.AuthContext, handle metadata.FileHandle, shareAccess uint32) error {
	var requiredPerm metadata.Permission
	if shareAccess&types.OPEN4_SHARE_ACCESS_READ != 0 {
		requiredPerm |= metadata.PermissionRead
	}
	if shareAccess&types.OPEN4_SHARE_ACCESS_WRITE != 0 {
		requiredPerm |= metadata.PermissionWrite
	}
	if requiredPerm == 0 {
		return nil
	}

	granted, err := metaSvc.CheckPermissions(authCtx, handle, requiredPerm)
	if err != nil {
		return err
	}

	if granted&requiredPerm != requiredPerm {
		return &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "open access denied",
		}
	}
	return nil
}

// effectiveUIDGID extracts the UID and GID from the auth context identity,
// defaulting to 0 (root) if the identity or its fields are nil.
func effectiveUIDGID(authCtx *metadata.AuthContext) (uint32, uint32) {
	var uid, gid uint32
	if authCtx.Identity != nil {
		if authCtx.Identity.UID != nil {
			uid = *authCtx.Identity.UID
		}
		if authCtx.Identity.GID != nil {
			gid = *authCtx.Identity.GID
		}
	}
	return uid, gid
}
