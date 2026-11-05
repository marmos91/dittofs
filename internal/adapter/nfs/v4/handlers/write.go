package handlers

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/cubbit/dittofs/internal/adapter/nfs/v4/state"
	"github.com/cubbit/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/cubbit/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/metadata"
)

// serverBootVerifier is an 8-byte verifier derived from server boot
// time. Clients compare it across WRITE and COMMIT responses to detect
// server restarts, at which point they re-send unstable writes.
//
// The restore path calls BumpBootVerifier() on successful in-place
// restore. NFSv4 clients whose next request lands post-swap see a new
// verifier, enter reclaim grace, and fail reclaim with
// NFS4ERR_RECLAIM_BAD — forcing fresh OPENs against the restored
// metadata state.
//
// The value is stored in an atomic.Pointer so BumpBootVerifier can
// safely swap it concurrently with in-flight WRITE/COMMIT handlers.
var serverBootVerifier atomic.Pointer[[8]byte]

func init() {
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(time.Now().UnixNano()))
	serverBootVerifier.Store(&v)
}

// BumpBootVerifier replaces the current verifier with a fresh
// time-derived value. Exported for storebackups.Service.RunRestore to
// invoke after a successful metadata swap.
//
// Safe to call concurrently with read-side handlers; the atomic
// pointer swap is lock-free.
func BumpBootVerifier() {
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(time.Now().UnixNano()))
	serverBootVerifier.Store(&v)
}

// bootVerifierBytes returns a copy of the current verifier so the
// caller can safely embed it in a response buffer without aliasing
// the atomic's pointer.
func bootVerifierBytes() [8]byte {
	return *serverBootVerifier.Load()
}

// handleWrite implements the WRITE operation (RFC 7530 Section 16.36).
// Runs MetadataService.PrepareWrite, so a refused write answers its own status;
// a zero-count write then succeeds, and any other answers NFS4ERR_IO, since no
// content store is wired (common.ErrNoContentStore).
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_ISDIR, NFS4ERR_FBIG, NFS4ERR_NOSPC, NFS4ERR_IO.
func (h *Handler) handleWrite(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return writeErr(status)
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return writeErr(types.NFS4ERR_ROFS)
	}

	// Decode WRITE4args
	stateid, argStatus := types.DecodeStateidArg(ctx, reader)
	if argStatus != types.NFS4_OK {
		return writeErr(argStatus)
	}

	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return writeErr(types.NFS4ERR_BADXDR)
	}

	stable, err := xdr.DecodeUint32(reader)
	if err != nil {
		return writeErr(types.NFS4ERR_BADXDR)
	}

	// stable_how4 is an enum of exactly three values (RFC 7530 Section 16.36.2).
	// Anything else is an undefined enum on the wire, and the reply's committed
	// field is drawn from the same enum -- echoing an out-of-range value back
	// would put an invalid stable_how4 in a successful WRITE4resok. knfsd
	// rejects it at the same point, as bad XDR rather than as a bad argument.
	if stable > types.FILE_SYNC4 {
		logger.Debug("NFSv4 WRITE rejected: stable_how4 out of range",
			"stable", stable,
			"client", ctx.ClientAddr)
		return writeErr(types.NFS4ERR_BADXDR)
	}

	data, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return writeErr(types.NFS4ERR_BADXDR)
	}

	// Validate stateid via StateManager for a write-family operation.
	// Both special stateids are accepted here and behave identically: RFC 7530
	// Section 16.36.4 says a WRITE with the READ-bypass (all-ones) stateid "is
	// treated exactly the same as if the anonymous stateid were used", so
	// neither may bypass a share reservation — ValidateStateid answers
	// NFS4ERR_LOCKED when an open on this file denies writing. Real stateids
	// are validated for correctness (seqid, epoch, filehandle match); implicit
	// lease renewal happens inside ValidateStateid for real stateids.
	openState, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH, state.StateidOpWrite, ctx.SessionClientID)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 WRITE stateid validation failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return writeErr(nfsStatus)
	}

	// Check that the open state includes WRITE access (OPEN4_SHARE_ACCESS_WRITE or BOTH).
	// Special stateids (openState == nil) bypass this check.
	if openState != nil {
		if openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
			logger.Debug("NFSv4 WRITE rejected: read-only open",
				"share_access", openState.ShareAccess,
				"client", ctx.ClientAddr)
			return writeErr(types.NFS4ERR_OPENMODE)
		}
	}

	logger.Debug("NFSv4 WRITE",
		"offset", offset,
		"count", len(data),
		"stable", stable,
		"stateid_seqid", stateid.Seqid,
		"client", ctx.ClientAddr)

	// Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 WRITE auth context failed", "error", err, "client", ctx.ClientAddr)
		st := nfs4StatusForAuthError(err)
		return writeErr(st)
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return writeErr(types.NFS4ERR_SERVERFAULT)
	}
	newSize := offset + uint64(len(data))
	if newSize < offset {
		return writeErr(types.NFS4ERR_FBIG)
	}
	// PrepareWrite validates the target (type, permissions, read-only share)
	// and modifies no metadata, so a refused write still answers with its own
	// status.
	if _, err := metaSvc.PrepareWrite(authCtx, metadata.FileHandle(ctx.CurrentFH), newSize); err != nil {
		return writeErr(types.StatusForErr(err))
	}

	// RFC 7530 Section 16.36.5: a WRITE with count 0 must not update
	// time_modify. It writes nothing, so it needs no content store and any
	// committed level is truthful; report the one the client asked for.
	if len(data) == 0 {
		return encodeWrite4resok(0, stable)
	}

	// A write stores content, and no content store is wired.
	return noContentStoreResult(types.OP_WRITE)
}

// encodeWrite4resok encodes a successful WRITE4 response: the byte count, the
// stability level actually achieved, and the server boot verifier.
func encodeWrite4resok(count, committed uint32) *types.CompoundResult {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteUint32(&buf, count)
	_ = xdr.WriteUint32(&buf, committed)

	// writeverf: 8-byte server boot verifier (fixed-length, NOT XDR opaque)
	verf := bootVerifierBytes()
	buf.Write(verf[:])

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_WRITE,
		Data:   buf.Bytes(),
	}
}

// writeErr builds a WRITE error result (status only).
func writeErr(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_WRITE,
		Data:   encodeStatusOnly(status),
	}
}
