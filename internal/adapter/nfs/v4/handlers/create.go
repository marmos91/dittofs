package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleCreate implements the CREATE operation (RFC 7530 Section 16.4).
//
// NFSv4 CREATE creates non-regular file objects: directories, symlinks,
// block/character devices, sockets, and FIFOs. Regular files are created
// via the OPEN operation with OPEN4_CREATE flag (Plan 07-03).
//
// Wire format args:
//
//	createtype4 objtype (uint32)
//	  - For NF4LNK: linkdata (XDR string)
//	  - For NF4BLK/NF4CHR: specdata1 (uint32), specdata2 (uint32)
//	  - For NF4DIR/NF4SOCK/NF4FIFO: no type-specific data
//	component4 objname (XDR string)
//	fattr4 createattrs (bitmap + opaque attr values)
//
// Wire format res (success):
//
//	nfsstat4 status (NFS4_OK)
//	change_info4 cinfo
//	bitmap4 attrset
//
// Wire format res (error):
//
//	nfsstat4 status
func (h *Handler) handleCreate(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle (parent directory)
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
		}
	}

	// Decode CREATE4args: objtype (createtype4 = uint32)
	objType, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Decode type-specific data
	var linkTarget string
	var specMajor, specMinor uint32

	switch objType {
	case types.NF4LNK:
		// Symlink: decode linkdata (XDR string)
		linkTarget, err = xdr.DecodeString(reader)
		if err != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_CREATE,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

	case types.NF4BLK, types.NF4CHR:
		// Block/character device: decode specdata (two uint32s)
		specMajor, err = xdr.DecodeUint32(reader) // specdata1 (major)
		if err != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_CREATE,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}
		specMinor, err = xdr.DecodeUint32(reader) // specdata2 (minor)
		if err != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_CREATE,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

	case types.NF4SOCK, types.NF4FIFO:
		// Socket/FIFO: no type-specific data

	case types.NF4DIR:
		// Directory: no type-specific data

	case types.NF4REG:
		// Regular files must be created via OPEN, not CREATE
		// Consume remaining args before returning error
		_, _ = xdr.DecodeString(reader)
		skipFattr4(reader)

		return &types.CompoundResult{
			Status: types.NFS4ERR_BADTYPE,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADTYPE),
		}

	default:
		// Unknown type
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADTYPE,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADTYPE),
		}
	}

	// Decode objname (component4 = XDR string)
	objName, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate UTF-8 filename
	if status := types.ValidateUTF8Filename(objName); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Decode createattrs (fattr4): bitmap + opaque attr values
	// Parse the attributes to apply mode/owner/group to the new entry
	setAttrs, _, fattr4Err := attrs.DecodeFattr4ToSetAttrs(reader)
	if fattr4Err != nil {
		// Check for typed NFS4 error (e.g., ATTRNOTSUPP, BADOWNER)
		if nfsErr, ok := fattr4Err.(attrs.NFS4StatusError); ok {
			status := nfsErr.NFS4Status()
			return &types.CompoundResult{
				Status: status,
				OpCode: types.OP_CREATE,
				Data:   encodeStatusOnly(status),
			}
		}
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 CREATE auth context failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	parentHandle := metadata.FileHandle(ctx.CurrentFH)

	// Get pre-operation parent attributes for change_info
	parentFile, err := metaSvc.GetFile(ctx.Context, parentHandle)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(status),
		}
	}
	beforeCtime := uint64(parentFile.Ctime.UnixNano())

	// Build default attributes from auth context
	defaultUID := uint32(0)
	defaultGID := uint32(0)
	if authCtx.Identity.UID != nil {
		defaultUID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		defaultGID = *authCtx.Identity.GID
	}

	// Perform the create operation
	var newFile *metadata.File
	var createErr error

	switch objType {
	case types.NF4DIR:
		dirAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
			UID:  defaultUID,
			GID:  defaultGID,
		}
		// Apply createattrs if provided
		if setAttrs.Mode != nil {
			dirAttr.Mode = *setAttrs.Mode
		}
		if setAttrs.UID != nil {
			dirAttr.UID = *setAttrs.UID
		}
		if setAttrs.GID != nil {
			dirAttr.GID = *setAttrs.GID
		}
		newFile, createErr = metaSvc.CreateDirectory(authCtx, parentHandle, objName, dirAttr)

	case types.NF4LNK:
		symlinkAttr := &metadata.FileAttr{
			Type: metadata.FileTypeSymlink,
			Mode: 0o777,
			UID:  defaultUID,
			GID:  defaultGID,
		}
		if setAttrs.Mode != nil {
			symlinkAttr.Mode = *setAttrs.Mode
		}
		if setAttrs.UID != nil {
			symlinkAttr.UID = *setAttrs.UID
		}
		if setAttrs.GID != nil {
			symlinkAttr.GID = *setAttrs.GID
		}
		newFile, createErr = metaSvc.CreateSymlink(authCtx, parentHandle, objName, linkTarget, symlinkAttr)

	case types.NF4BLK:
		blkAttr := &metadata.FileAttr{
			Type: metadata.FileTypeBlockDevice,
			Mode: 0o644,
			UID:  defaultUID,
			GID:  defaultGID,
		}
		if setAttrs.Mode != nil {
			blkAttr.Mode = *setAttrs.Mode
		}
		if setAttrs.UID != nil {
			blkAttr.UID = *setAttrs.UID
		}
		if setAttrs.GID != nil {
			blkAttr.GID = *setAttrs.GID
		}
		newFile, createErr = metaSvc.CreateSpecialFile(authCtx, parentHandle, objName,
			metadata.FileTypeBlockDevice, blkAttr, specMajor, specMinor)

	case types.NF4CHR:
		chrAttr := &metadata.FileAttr{
			Type: metadata.FileTypeCharDevice,
			Mode: 0o644,
			UID:  defaultUID,
			GID:  defaultGID,
		}
		if setAttrs.Mode != nil {
			chrAttr.Mode = *setAttrs.Mode
		}
		if setAttrs.UID != nil {
			chrAttr.UID = *setAttrs.UID
		}
		if setAttrs.GID != nil {
			chrAttr.GID = *setAttrs.GID
		}
		newFile, createErr = metaSvc.CreateSpecialFile(authCtx, parentHandle, objName,
			metadata.FileTypeCharDevice, chrAttr, specMajor, specMinor)

	case types.NF4SOCK:
		sockAttr := &metadata.FileAttr{
			Type: metadata.FileTypeSocket,
			Mode: 0o644,
			UID:  defaultUID,
			GID:  defaultGID,
		}
		if setAttrs.Mode != nil {
			sockAttr.Mode = *setAttrs.Mode
		}
		if setAttrs.UID != nil {
			sockAttr.UID = *setAttrs.UID
		}
		if setAttrs.GID != nil {
			sockAttr.GID = *setAttrs.GID
		}
		newFile, createErr = metaSvc.CreateSpecialFile(authCtx, parentHandle, objName,
			metadata.FileTypeSocket, sockAttr, 0, 0)

	case types.NF4FIFO:
		fifoAttr := &metadata.FileAttr{
			Type: metadata.FileTypeFIFO,
			Mode: 0o644,
			UID:  defaultUID,
			GID:  defaultGID,
		}
		if setAttrs.Mode != nil {
			fifoAttr.Mode = *setAttrs.Mode
		}
		if setAttrs.UID != nil {
			fifoAttr.UID = *setAttrs.UID
		}
		if setAttrs.GID != nil {
			fifoAttr.GID = *setAttrs.GID
		}
		newFile, createErr = metaSvc.CreateSpecialFile(authCtx, parentHandle, objName,
			metadata.FileTypeFIFO, fifoAttr, 0, 0)
	}

	if createErr != nil {
		status := types.MapMetadataErrorToNFS4(createErr)
		logger.Debug("NFSv4 CREATE failed",
			"name", objName,
			"type", objType,
			"error", createErr,
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Get post-operation parent attributes for change_info
	parentFileAfter, err := metaSvc.GetFile(ctx.Context, parentHandle)
	if err != nil {
		// Non-fatal: we already created the entry, encode with best-effort
		logger.Debug("NFSv4 CREATE post-op getattr failed", "error", err)
	}
	afterCtime := beforeCtime
	if parentFileAfter != nil {
		afterCtime = uint64(parentFileAfter.Ctime.UnixNano())
	}

	// Set ctx.CurrentFH to the new entry's handle (copy-on-set)
	newHandle, err := metadata.EncodeFileHandle(newFile)
	if err != nil {
		logger.Debug("NFSv4 CREATE encode handle failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_CREATE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	ctx.CurrentFH = make([]byte, len(newHandle))
	copy(ctx.CurrentFH, newHandle)

	logger.Debug("NFSv4 CREATE successful",
		"name", objName,
		"type", objType,
		"handle", string(newHandle),
		"client", ctx.ClientAddr)

	// Notify directory delegation holders about the new entry
	if h.StateManager != nil {
		var originClientID uint64
		if ctx.ClientState != nil {
			originClientID = ctx.ClientState.ClientID
		}
		h.StateManager.NotifyDirChange([]byte(parentHandle), state.DirNotification{
			Type:           types.NOTIFY4_ADD_ENTRY,
			EntryName:      objName,
			OriginClientID: originClientID,
		})
	}

	// Encode CREATE4resok
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	// change_info4
	encodeChangeInfo4(&buf, true, beforeCtime, afterCtime)
	// attrset: report back which createattrs were applied
	// Build bitmap of attrs we actually applied (mode, owner, owner_group)
	var appliedBitmap []uint32
	if setAttrs.Mode != nil {
		attrs.SetBit(&appliedBitmap, attrs.FATTR4_MODE)
	}
	if setAttrs.UID != nil {
		attrs.SetBit(&appliedBitmap, attrs.FATTR4_OWNER)
	}
	if setAttrs.GID != nil {
		attrs.SetBit(&appliedBitmap, attrs.FATTR4_OWNER_GROUP)
	}
	_ = attrs.EncodeBitmap4(&buf, appliedBitmap)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_CREATE,
		Data:   buf.Bytes(),
	}
}

// skipFattr4 consumes an fattr4 structure from the reader without parsing it.
// fattr4 = bitmap4 + opaque attrvals.
func skipFattr4(reader io.Reader) {
	// Read bitmap4 (array of uint32s)
	bitmapLen, err := xdr.DecodeUint32(reader)
	if err != nil {
		return
	}
	for i := uint32(0); i < bitmapLen; i++ {
		_, _ = xdr.DecodeUint32(reader)
	}
	// Read opaque attrvals
	attrLen, err := xdr.DecodeUint32(reader)
	if err != nil {
		return
	}
	// Skip attrLen bytes (with XDR padding)
	padded := attrLen
	if padded%4 != 0 {
		padded += 4 - (padded % 4)
	}
	skip := make([]byte, padded)
	_, _ = io.ReadFull(reader, skip)
}
