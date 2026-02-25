package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleRename implements the RENAME operation (RFC 7530 Section 16.26).
//
// RENAME moves or renames a file or directory. It uses the two-filehandle
// pattern: SavedFH references the source directory and CurrentFH references
// the target directory.
//
// Wire format args:
//
//	component4 oldname (XDR string)
//	component4 newname (XDR string)
//
// Wire format res (success):
//
//	nfsstat4 status (NFS4_OK)
//	change_info4 source_cinfo
//	change_info4 target_cinfo
//
// Wire format res (error):
//
//	nfsstat4 status
func (h *Handler) handleRename(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle (target directory)
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}

	// Require saved filehandle (source directory)
	if status := types.RequireSavedFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs is read-only -- check BOTH handles
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) || pseudofs.IsPseudoFSHandle(ctx.SavedFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
		}
	}

	// Decode RENAME4args: oldname (component4 = XDR string)
	oldName, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Decode RENAME4args: newname (component4 = XDR string)
	newName, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate both names
	if status := types.ValidateUTF8Filename(oldName); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}
	if status := types.ValidateUTF8Filename(newName); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}

	// Cross-share check: SavedFH and CurrentFH must be from the same share
	savedShareName, _, savedErr := metadata.DecodeFileHandle(metadata.FileHandle(ctx.SavedFH))
	currentShareName, _, currentErr := metadata.DecodeFileHandle(metadata.FileHandle(ctx.CurrentFH))
	if savedErr != nil || currentErr != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADHANDLE,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_BADHANDLE),
		}
	}
	if savedShareName != currentShareName {
		return &types.CompoundResult{
			Status: types.NFS4ERR_XDEV,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_XDEV),
		}
	}

	// Build auth context from CurrentFH (target directory)
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 RENAME auth context failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	srcDirHandle := metadata.FileHandle(ctx.SavedFH)
	tgtDirHandle := metadata.FileHandle(ctx.CurrentFH)

	// Get pre-operation attributes for both directories (for change_info4)
	srcDirFile, err := metaSvc.GetFile(ctx.Context, srcDirHandle)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}
	srcBeforeCtime := uint64(srcDirFile.Ctime.UnixNano())

	tgtDirFile, err := metaSvc.GetFile(ctx.Context, tgtDirHandle)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}
	tgtBeforeCtime := uint64(tgtDirFile.Ctime.UnixNano())

	// Perform the rename: Move(fromDir, fromName, toDir, toName)
	renameErr := metaSvc.Move(authCtx, srcDirHandle, oldName, tgtDirHandle, newName)
	if renameErr != nil {
		status := types.MapMetadataErrorToNFS4(renameErr)
		logger.Debug("NFSv4 RENAME failed",
			"oldname", oldName,
			"newname", newName,
			"error", renameErr,
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RENAME,
			Data:   encodeStatusOnly(status),
		}
	}

	// Get post-operation attributes for both directories
	srcAfterCtime := srcBeforeCtime
	srcDirFileAfter, err := metaSvc.GetFile(ctx.Context, srcDirHandle)
	if err != nil {
		logger.Debug("NFSv4 RENAME post-op src getattr failed", "error", err)
	} else if srcDirFileAfter != nil {
		srcAfterCtime = uint64(srcDirFileAfter.Ctime.UnixNano())
	}

	tgtAfterCtime := tgtBeforeCtime
	tgtDirFileAfter, err := metaSvc.GetFile(ctx.Context, tgtDirHandle)
	if err != nil {
		logger.Debug("NFSv4 RENAME post-op tgt getattr failed", "error", err)
	} else if tgtDirFileAfter != nil {
		tgtAfterCtime = uint64(tgtDirFileAfter.Ctime.UnixNano())
	}

	logger.Debug("NFSv4 RENAME successful",
		"oldname", oldName,
		"newname", newName,
		"client", ctx.ClientAddr)

	// Notify directory delegation holders about the rename
	if h.StateManager != nil {
		var originClientID uint64
		if ctx.ClientState != nil {
			originClientID = ctx.ClientState.ClientID
		}
		if bytes.Equal(ctx.SavedFH, ctx.CurrentFH) {
			// Same-directory rename: single RENAME notification
			h.StateManager.NotifyDirChange(ctx.SavedFH, state.DirNotification{
				Type:           types.NOTIFY4_RENAME_ENTRY,
				EntryName:      oldName,
				NewName:        newName,
				OriginClientID: originClientID,
			})
		} else {
			// Cross-directory rename: REMOVE from source, ADD to destination
			h.StateManager.NotifyDirChange(ctx.SavedFH, state.DirNotification{
				Type:           types.NOTIFY4_REMOVE_ENTRY,
				EntryName:      oldName,
				OriginClientID: originClientID,
			})
			h.StateManager.NotifyDirChange(ctx.CurrentFH, state.DirNotification{
				Type:           types.NOTIFY4_ADD_ENTRY,
				EntryName:      newName,
				OriginClientID: originClientID,
			})
		}
	}

	// Encode RENAME4resok
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	// source_cinfo (change_info4 for source directory)
	encodeChangeInfo4(&buf, true, srcBeforeCtime, srcAfterCtime)
	// target_cinfo (change_info4 for target directory)
	encodeChangeInfo4(&buf, true, tgtBeforeCtime, tgtAfterCtime)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_RENAME,
		Data:   buf.Bytes(),
	}
}
