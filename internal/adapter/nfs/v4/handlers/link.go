package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleLink implements the LINK operation (RFC 7530 Section 16.9).
//
// LINK creates a hard link to an existing file. It uses the two-filehandle
// pattern: SavedFH references the source file (the object to link) and
// CurrentFH references the target directory (where the new link name is created).
//
// Wire format args:
//
//	component4 newname (XDR string)
//
// Wire format res (success):
//
//	nfsstat4 status (NFS4_OK)
//	change_info4 cinfo
//
// Wire format res (error):
//
//	nfsstat4 status
func (h *Handler) handleLink(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle (target directory)
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Require saved filehandle (source file to link)
	if status := types.RequireSavedFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
		}
	}

	// Decode LINK4args: newname (component4 = XDR string)
	newName, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate UTF-8 filename
	if status := types.ValidateUTF8Filename(newName); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Cross-share check: SavedFH and CurrentFH must be from the same share
	savedShareName, _, savedErr := metadata.DecodeFileHandle(metadata.FileHandle(ctx.SavedFH))
	currentShareName, _, currentErr := metadata.DecodeFileHandle(metadata.FileHandle(ctx.CurrentFH))
	if savedErr != nil || currentErr != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADHANDLE,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADHANDLE),
		}
	}
	if savedShareName != currentShareName {
		return &types.CompoundResult{
			Status: types.NFS4ERR_XDEV,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(types.NFS4ERR_XDEV),
		}
	}

	// Build auth context from CurrentFH (target directory)
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 LINK auth context failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	dirHandle := metadata.FileHandle(ctx.CurrentFH)
	sourceHandle := metadata.FileHandle(ctx.SavedFH)

	// Get pre-operation target directory attributes for change_info4
	dirFile, err := metaSvc.GetFile(ctx.Context, dirHandle)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(status),
		}
	}
	beforeCtime := uint64(dirFile.Ctime.UnixNano())

	// Create the hard link: dirHandle (target dir) + newName + sourceHandle (source file)
	linkErr := metaSvc.CreateHardLink(authCtx, dirHandle, newName, sourceHandle)
	if linkErr != nil {
		status := types.MapMetadataErrorToNFS4(linkErr)
		logger.Debug("NFSv4 LINK failed",
			"newname", newName,
			"error", linkErr,
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LINK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Get post-operation target directory attributes for change_info4
	dirFileAfter, err := metaSvc.GetFile(ctx.Context, dirHandle)
	afterCtime := beforeCtime
	if err != nil {
		logger.Debug("NFSv4 LINK post-op getattr failed", "error", err)
	} else if dirFileAfter != nil {
		afterCtime = uint64(dirFileAfter.Ctime.UnixNano())
	}

	logger.Debug("NFSv4 LINK successful",
		"newname", newName,
		"client", ctx.ClientAddr)

	// Notify directory delegation holders about the new link entry
	if h.StateManager != nil {
		var originClientID uint64
		if ctx.ClientState != nil {
			originClientID = ctx.ClientState.ClientID
		}
		h.StateManager.NotifyDirChange(ctx.CurrentFH, state.DirNotification{
			Type:           types.NOTIFY4_ADD_ENTRY,
			EntryName:      newName,
			OriginClientID: originClientID,
		})
	}

	// Encode LINK4resok
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	// change_info4 for the target directory
	encodeChangeInfo4(&buf, true, beforeCtime, afterCtime)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LINK,
		Data:   buf.Bytes(),
	}
}
