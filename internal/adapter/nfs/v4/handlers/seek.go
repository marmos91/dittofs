package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// SEEK4args / SEEK4res (RFC 7862 Section 15.11):
//
//	struct SEEK4args {
//	    stateid4        sa_stateid;
//	    offset4         sa_offset;
//	    data_content4   sa_what;     // NFS4_CONTENT_DATA | NFS4_CONTENT_HOLE
//	};
//	struct seek_res4 {
//	    bool            sr_eof;
//	    offset4         sr_offset;
//	};
//	union SEEK4res switch (nfsstat4 sa_status) {
//	 case NFS4_OK: seek_res4 resok4;
//	 default:      void;
//	};
//
// SEEK reports the next data (SEEK_DATA) or hole (SEEK_HOLE) boundary at or
// after sa_offset. The hole map is derived from the file's content-addressed
// block list (a byte range with no covering block ref is a hole). Per
// RFC 7862, sa_offset >= file size returns NFS4ERR_NXIO, and SEEK_DATA past the
// last data extent also returns NFS4ERR_NXIO (no more data). SEEK_HOLE always
// finds a (virtual) hole at EOF.
func (h *Handler) handleSeek(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return seekErr(status)
	}
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return seekErr(types.NFS4ERR_ISDIR)
	}

	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return seekErr(types.NFS4ERR_BADXDR)
	}
	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return seekErr(types.NFS4ERR_BADXDR)
	}
	what, err := xdr.DecodeUint32(reader)
	if err != nil {
		return seekErr(types.NFS4ERR_BADXDR)
	}
	if what != types.NFS4_CONTENT_DATA && what != types.NFS4_CONTENT_HOLE {
		return seekErr(types.NFS4ERR_INVAL)
	}

	// SEEK is a read-family operation: validate the stateid for read access
	// (special stateids permitted), mirroring READ.
	if openState, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH, state.StateidOpRead); stateErr != nil {
		st := mapStateError(stateErr)
		logger.Debug("NFSv4.2 SEEK stateid validation failed", "error", stateErr, "nfs_status", st, "client", ctx.ClientAddr)
		return seekErr(st)
	} else if openState != nil && openState.ShareAccess&types.OPEN4_SHARE_ACCESS_READ == 0 {
		return seekErr(types.NFS4ERR_OPENMODE)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return seekErr(nfs4StatusForAuthError(err))
	}
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return seekErr(types.NFS4ERR_SERVERFAULT)
	}

	file, err := metaSvc.GetFile(authCtx.Context, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		return seekErr(common.MapToNFS4(err))
	}
	if file.Type != metadata.FileTypeRegular {
		return seekErr(types.NFS4ERR_ISDIR)
	}

	// sa_offset at or beyond EOF: no data and no in-file hole remain.
	if offset >= file.Size {
		return seekErr(types.NFS4ERR_NXIO)
	}

	var (
		nextOffset uint64
		found      bool
	)
	if what == types.NFS4_CONTENT_DATA {
		nextOffset, found = block.NextDataOffset(file.Blocks, file.Size, offset)
	} else {
		nextOffset, found = block.NextHoleOffset(file.Blocks, file.Size, offset)
	}
	if !found {
		// SEEK_DATA found no further data — the tail is a hole. NFS4ERR_NXIO is
		// the RFC-mandated "no matching content" status.
		return seekErr(types.NFS4ERR_NXIO)
	}

	// sr_eof is set when the reported offset coincides with EOF (the boundary
	// is the end of the file). For SEEK_HOLE this is the common "hole at EOF"
	// case; SEEK_DATA never returns EOF here because data offsets are < size.
	eof := nextOffset >= file.Size

	logger.Debug("NFSv4.2 SEEK", "what", what, "from", offset, "next", nextOffset,
		"eof", eof, "size", file.Size, "client", ctx.ClientAddr)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteBool(&buf, eof)
	_ = xdr.WriteUint64(&buf, nextOffset)
	return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_SEEK, Data: buf.Bytes()}
}

// seekErr builds a SEEK error result (status only).
func seekErr(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_SEEK,
		Data:   encodeStatusOnly(status),
	}
}
