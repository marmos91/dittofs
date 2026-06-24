package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CLONE4args / CLONE4res (RFC 7862 Section 15.13, operation 71):
//
//	struct CLONE4args {
//	    stateid4 cl_src_stateid;
//	    stateid4 cl_dst_stateid;
//	    offset4  cl_src_offset;
//	    offset4  cl_dst_offset;
//	    length4  cl_count;
//	};
//	union CLONE4res switch (nfsstat4 cl_status) {
//	 case NFS4_OK: void;
//	 default:      void;
//	};
//
// CLONE makes the destination file (CURRENT_FH) reference the same content as a
// range of the source file (SAVED_FH) — a reflink. DittoFS's block store is
// content-addressed with dedup, so a whole-file clone is a PURE METADATA op:
// the destination inherits the source's BlockRef list and the CAS RefCount is
// bumped per unique hash. No data is read or written, even on S3 — O(1).
// Copy-on-write is intrinsic: a later WRITE to either file produces new CAS
// blocks under a new hash, leaving the other file's content untouched. This is
// the same engine.CopyPayload refcount path the SMB server-side-copy IOCTLs
// build on (CLAUDE.md: one clone primitive, both protocols — common.CloneWholeFile).
//
// SAVED_FH is the source and CURRENT_FH is the destination, exactly like COPY
// (RFC 7862 Section 15.2): the client issues SAVEFH on the source before PUTFH
// on the destination. Offsets and cl_count must align to FATTR4_CLONE_BLKSIZE
// (advertised as 1 — byte-granular — so no alignment constraint applies; the
// guard remains for future non-1 block sizes). cl_count == 0 means "from
// cl_src_offset to the end of the source file".
//
// DittoFS serves whole-file clones (src_offset==0, dst_offset==0, count of 0 or
// the source size) — the dominant `cp --reflink` path. Offset/partial sub-range
// clones return NFS4ERR_NOTSUPP (RFC 7862 15.13 permits declining unsupported
// clones); a true range reflink would need block-boundary splicing that the
// content-defined FastCDC chunking does not give for free.
func (h *Handler) handleClone(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// CURRENT_FH is the destination, SAVED_FH is the source. Both must be set
	// (the client PUTFHs the source, SAVEFHs it, then PUTFHs the destination).
	// A missing handle on either side is NFS4ERR_NOFILEHANDLE (RFC 7862 15.13);
	// note RequireSavedFH returns NFS4ERR_RESTOREFH, which is RESTOREFH-specific
	// and wrong here, so check SavedFH directly.
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return cloneErr(status)
	}
	if ctx.SavedFH == nil {
		return cloneErr(types.NFS4ERR_NOFILEHANDLE)
	}
	// Neither side may be the read-only pseudo-filesystem.
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) || pseudofs.IsPseudoFSHandle(ctx.SavedFH) {
		return cloneErr(types.NFS4ERR_ROFS)
	}

	srcStateid, dstStateid, srcOffset, dstOffset, count, st := decodeCloneArgs(reader)
	if st != types.NFS4_OK {
		return cloneErr(st)
	}

	// Alignment: src/dst offsets and (unless whole-file) count must be multiples
	// of the advertised clone block size, else NFS4ERR_INVAL (RFC 7862 15.13).
	if blk := uint64(attrs.CloneBlockSize); blk > 1 {
		if srcOffset%blk != 0 || dstOffset%blk != 0 || (count != 0 && count%blk != 0) {
			return cloneErr(types.NFS4ERR_INVAL)
		}
	}

	// Validate the source stateid for READ and the destination for WRITE. Special
	// stateids pass; a real open must carry the matching share-access bit.
	if openState, err := h.StateManager.ValidateStateid(srcStateid, ctx.SavedFH, state.StateidOpRead); err != nil {
		s := mapStateError(err)
		logger.Debug("NFSv4.2 CLONE src stateid validation failed", "error", err, "nfs_status", s, "client", ctx.ClientAddr)
		return cloneErr(s)
	} else if openState != nil && openState.ShareAccess&types.OPEN4_SHARE_ACCESS_READ == 0 {
		return cloneErr(types.NFS4ERR_OPENMODE)
	}
	if openState, err := h.StateManager.ValidateStateid(dstStateid, ctx.CurrentFH, state.StateidOpWrite); err != nil {
		s := mapStateError(err)
		logger.Debug("NFSv4.2 CLONE dst stateid validation failed", "error", err, "nfs_status", s, "client", ctx.ClientAddr)
		return cloneErr(s)
	} else if openState != nil && openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
		return cloneErr(types.NFS4ERR_OPENMODE)
	}

	// Build auth contexts for both sides: the destination gates the content
	// mutation (write), the source gates the read.
	dstAuth, dstShare, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return cloneErr(nfs4StatusForAuthError(err))
	}
	srcAuth, srcShare, err := h.buildV4AuthContext(ctx, ctx.SavedFH)
	if err != nil {
		return cloneErr(nfs4StatusForAuthError(err))
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return cloneErr(types.NFS4ERR_SERVERFAULT)
	}

	srcHandle := metadata.FileHandle(ctx.SavedFH)
	dstHandle := metadata.FileHandle(ctx.CurrentFH)

	// Resolve and type-check both files: CLONE is only defined for regular
	// files (NFS4ERR_ISDIR for directories, NFS4ERR_WRONG_TYPE otherwise).
	srcFile, err := metaSvc.GetFile(srcAuth.Context, srcHandle)
	if err != nil {
		return cloneErr(common.MapToNFS4(err))
	}
	dstFile, err := metaSvc.GetFile(dstAuth.Context, dstHandle)
	if err != nil {
		return cloneErr(common.MapToNFS4(err))
	}
	if st := cloneRequireRegularFile(srcFile); st != types.NFS4_OK {
		return cloneErr(st)
	}
	if st := cloneRequireRegularFile(dstFile); st != types.NFS4_OK {
		return cloneErr(st)
	}

	// Content-addressed dedup is per-share, so source and destination must live
	// in the same share (same block store). NFSv4 has no NFS4ERR_XDEV; RFC 7862
	// directs cross-filesystem CLONE to NFS4ERR_INVAL.
	if srcShare != dstShare {
		logger.Debug("NFSv4.2 CLONE across shares rejected", "src", srcShare, "dst", dstShare, "client", ctx.ClientAddr)
		return cloneErr(types.NFS4ERR_INVAL)
	}

	// Enforce permissions: READ on the source, WRITE on the destination. The
	// stateid validation above accepts special stateids without an OPEN, so it
	// does NOT cover POSIX/ACL or read-only-share enforcement on its own —
	// CheckPermissions does (this mirrors how ALLOCATE/DEALLOCATE re-check via
	// the Service even after stateid validation). CheckPermissions also rejects
	// a write to a read-only export.
	if _, err := metaSvc.CheckPermissions(srcAuth, srcHandle, metadata.PermissionRead); err != nil {
		return cloneErr(common.MapToNFS4(err))
	}
	if _, err := metaSvc.CheckPermissions(dstAuth, dstHandle, metadata.PermissionWrite); err != nil {
		return cloneErr(common.MapToNFS4(err))
	}

	// Self-clone (source and destination are the same file) is a no-op: the
	// content is already identical. Short-circuit BEFORE touching the block
	// store so we never feed CopyPayload srcPayloadID == dstPayloadID, which
	// would inflate the shared payload's RefCount with no offsetting reference.
	if bytes.Equal(srcHandle, dstHandle) {
		logger.Debug("NFSv4.2 CLONE self-clone no-op", "client", ctx.ClientAddr)
		return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_CLONE, Data: encodeStatusOnly(types.NFS4_OK)}
	}

	// DittoFS clones whole files as a pure-metadata O(1) reflink (share the
	// source BlockRef list, bump CAS RefCount per unique hash). A request is
	// "whole file" when it covers the entire source from offset 0 into the
	// destination at offset 0: src_offset==0, dst_offset==0, and count is 0
	// ("to EOF") or exactly the source size. RFC 7862 Section 15.13 permits a
	// server to return NFS4ERR_NOTSUPP for clone requests it cannot satisfy;
	// partial/offset sub-range clones fall in that bucket here (the dominant
	// `cp --reflink` path is always whole-file). Validate offsets/count fit
	// before deciding so a malformed sub-request still gets NFS4ERR_INVAL.
	if srcOffset > srcFile.Size || (count != 0 && srcOffset+count > srcFile.Size) {
		return cloneErr(types.NFS4ERR_INVAL)
	}
	wholeFile := srcOffset == 0 && dstOffset == 0 && (count == 0 || count == srcFile.Size)
	if !wholeFile {
		logger.Debug("NFSv4.2 CLONE sub-range not supported",
			"srcOffset", srcOffset, "dstOffset", dstOffset, "count", count, "client", ctx.ClientAddr)
		return cloneErr(types.NFS4ERR_NOTSUPP)
	}

	blockStore, err := common.ResolveForWrite(ctx.Context, h.Registry, dstHandle)
	if err != nil {
		logger.Error("NFSv4.2 CLONE: cannot resolve block store", "share", dstShare, "error", err)
		return cloneErr(types.NFS4ERR_SERVERFAULT)
	}
	store, err := metaSvc.GetStoreForShare(dstShare)
	if err != nil {
		logger.Error("NFSv4.2 CLONE: cannot resolve metadata store", "share", dstShare, "error", err)
		return cloneErr(types.NFS4ERR_SERVERFAULT)
	}

	// Pass the source blocks and size already fetched above rather than letting
	// CloneWholeFile re-read them — one fewer metadata round-trip and no TOCTOU
	// window on the source size between the whole-file decision and the clone.
	if err := common.CloneWholeFile(
		ctx.Context, blockStore, store, nil,
		dstHandle,
		srcFile.PayloadID, dstFile.PayloadID,
		srcFile.Blocks, srcFile.Size,
	); err != nil {
		logger.Debug("NFSv4.2 CLONE failed", "error", err, "client", ctx.ClientAddr)
		return cloneErr(common.MapToNFS4(err))
	}

	logger.Debug("NFSv4.2 CLONE",
		"srcOffset", srcOffset, "dstOffset", dstOffset, "count", count,
		"share", dstShare, "client", ctx.ClientAddr)
	return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_CLONE, Data: encodeStatusOnly(types.NFS4_OK)}
}

// decodeCloneArgs decodes CLONE4args: cl_src_stateid, cl_dst_stateid,
// cl_src_offset, cl_dst_offset, cl_count (RFC 7862 Section 15.13). Returns
// NFS4ERR_BADXDR on a malformed stream and NFS4ERR_INVAL when src/dst range
// arithmetic overflows uint64.
func decodeCloneArgs(reader io.Reader) (srcStateid, dstStateid *types.Stateid4, srcOffset, dstOffset, count uint64, st uint32) {
	src, err := types.DecodeStateid4(reader)
	if err != nil {
		return nil, nil, 0, 0, 0, types.NFS4ERR_BADXDR
	}
	dst, err := types.DecodeStateid4(reader)
	if err != nil {
		return nil, nil, 0, 0, 0, types.NFS4ERR_BADXDR
	}
	so, err := xdr.DecodeUint64(reader)
	if err != nil {
		return nil, nil, 0, 0, 0, types.NFS4ERR_BADXDR
	}
	do, err := xdr.DecodeUint64(reader)
	if err != nil {
		return nil, nil, 0, 0, 0, types.NFS4ERR_BADXDR
	}
	c, err := xdr.DecodeUint64(reader)
	if err != nil {
		return nil, nil, 0, 0, 0, types.NFS4ERR_BADXDR
	}
	// Guard against offset+count overflow on either side.
	if c > 0 && (so > ^uint64(0)-c || do > ^uint64(0)-c) {
		return nil, nil, 0, 0, 0, types.NFS4ERR_INVAL
	}
	return src, dst, so, do, c, types.NFS4_OK
}

// cloneRequireRegularFile maps a non-regular-file source/destination to the
// CLONE-specific error: NFS4ERR_ISDIR for directories, NFS4ERR_WRONG_TYPE for
// anything else (RFC 7862 Section 15.13 ERRORS).
func cloneRequireRegularFile(file *metadata.File) uint32 {
	switch file.Type {
	case metadata.FileTypeRegular:
		return types.NFS4_OK
	case metadata.FileTypeDirectory:
		return types.NFS4ERR_ISDIR
	default:
		return types.NFS4ERR_WRONG_TYPE
	}
}

// cloneErr builds a CLONE error result (status only).
func cloneErr(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_CLONE,
		Data:   encodeStatusOnly(status),
	}
}
