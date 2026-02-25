package handlers

import (
	"bytes"
	goerrors "errors"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
	metaerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// handleRemove implements the REMOVE operation (RFC 7530 Section 16.25).
//
// REMOVE deletes a file or empty directory from its parent directory.
// The current filehandle must reference the parent directory.
//
// For files, REMOVE calls MetadataService.RemoveFile.
// For directories, if RemoveFile returns "is a directory" error,
// it falls back to MetadataService.RemoveDirectory.
//
// Wire format args:
//
//	component4 target (XDR string)
//
// Wire format res (success):
//
//	nfsstat4 status (NFS4_OK)
//	change_info4 cinfo
//
// Wire format res (error):
//
//	nfsstat4 status
func (h *Handler) handleRemove(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle (parent directory)
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
		}
	}

	// Decode REMOVE4args: target (component4 = XDR string)
	target, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate UTF-8 filename
	if status := types.ValidateUTF8Filename(target); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 REMOVE auth context failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_REMOVE,
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
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(status),
		}
	}
	beforeCtime := uint64(parentFile.Ctime.UnixNano())

	// Look up the child entry before removal to get its handle.
	// This is needed to revoke any directory delegations on the child
	// if it's a directory being removed.
	var childFH metadata.FileHandle
	if h.StateManager != nil {
		child, lookupErr := metaSvc.Lookup(authCtx, parentHandle, target)
		if lookupErr == nil {
			fh, encErr := metadata.EncodeFileHandle(child)
			if encErr == nil {
				childFH = fh
			}
		}
	}

	// Try RemoveFile first (works for regular files, symlinks, etc.)
	_, removeErr := metaSvc.RemoveFile(authCtx, parentHandle, target)
	if removeErr != nil {
		// Check if the error indicates this is a directory (ErrIsDirectory)
		var storeErr *metaerrors.StoreError
		if goerrors.As(removeErr, &storeErr) && storeErr.Code == metaerrors.ErrIsDirectory {
			// It's a directory -- try RemoveDirectory instead
			removeErr = metaSvc.RemoveDirectory(authCtx, parentHandle, target)
		}
	}

	if removeErr != nil {
		status := types.MapMetadataErrorToNFS4(removeErr)
		logger.Debug("NFSv4 REMOVE failed",
			"target", target,
			"error", removeErr,
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_REMOVE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Get post-operation parent attributes for change_info
	parentFileAfter, err := metaSvc.GetFile(ctx.Context, parentHandle)
	if err != nil {
		logger.Debug("NFSv4 REMOVE post-op getattr failed", "error", err)
	}
	afterCtime := beforeCtime
	if parentFileAfter != nil {
		afterCtime = uint64(parentFileAfter.Ctime.UnixNano())
	}

	logger.Debug("NFSv4 REMOVE successful",
		"target", target,
		"client", ctx.ClientAddr)

	// Notify directory delegation holders about the removed entry
	if h.StateManager != nil {
		var originClientID uint64
		if ctx.ClientState != nil {
			originClientID = ctx.ClientState.ClientID
		}
		h.StateManager.NotifyDirChange([]byte(parentHandle), state.DirNotification{
			Type:           types.NOTIFY4_REMOVE_ENTRY,
			EntryName:      target,
			OriginClientID: originClientID,
		})

		// If the removed entry was a directory, revoke any directory delegations on it
		if childFH != nil {
			delegs := h.StateManager.GetDelegationsForFile([]byte(childFH))
			for _, deleg := range delegs {
				if deleg.IsDirectory {
					h.StateManager.RecallDirDelegation(deleg, "directory_deleted")
				}
			}
		}
	}

	// Encode REMOVE4resok
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	// change_info4
	encodeChangeInfo4(&buf, true, beforeCtime, afterCtime)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_REMOVE,
		Data:   buf.Bytes(),
	}
}
