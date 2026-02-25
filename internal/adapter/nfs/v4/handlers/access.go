package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// NFSv4 ACCESS bit constants per RFC 7530 Section 6.
const (
	ACCESS4_READ    = 0x01
	ACCESS4_LOOKUP  = 0x02
	ACCESS4_MODIFY  = 0x04
	ACCESS4_EXTEND  = 0x08
	ACCESS4_DELETE  = 0x10
	ACCESS4_EXECUTE = 0x20
)

// handleAccess implements the ACCESS operation (RFC 7530 Section 16.1).
//
// ACCESS checks the access permissions for the current filehandle.
// For pseudo-fs handles, all access is granted since pseudo-fs directories
// are always accessible.
//
// Wire format args: access (uint32 bitmask)
// Wire format res:  nfsstat4 (uint32) + supported (uint32) + access (uint32)
func (h *Handler) handleAccess(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(status),
		}
	}

	// Read requested access mask
	accessReq, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Check if current FH is a pseudo-fs handle
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		// Pseudo-fs directories are always accessible: grant all requested bits
		var buf bytes.Buffer
		_ = xdr.WriteUint32(&buf, types.NFS4_OK)
		_ = xdr.WriteUint32(&buf, ACCESS4_READ|ACCESS4_LOOKUP|ACCESS4_MODIFY|ACCESS4_EXTEND|ACCESS4_DELETE|ACCESS4_EXECUTE) // supported
		_ = xdr.WriteUint32(&buf, accessReq)                                                                                // access granted

		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_ACCESS,
			Data:   buf.Bytes(),
		}
	}

	// Real filesystem handle -- check permissions from metadata service
	return h.accessRealFS(ctx, accessReq)
}

// accessRealFS handles ACCESS for real filesystem files.
//
// It checks Unix permission bits against the effective UID/GID from
// the auth context. All 6 access bits are reported as supported.
func (h *Handler) accessRealFS(ctx *types.CompoundContext, accessReq uint32) *types.CompoundResult {
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	file, err := metaSvc.GetFile(authCtx.Context, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(status),
		}
	}

	// Check Unix permission bits against UID/GID
	granted := checkAccessBits(accessReq, file, authCtx)

	// Report all ACCESS4 bits the server can evaluate
	supported := uint32(ACCESS4_READ | ACCESS4_LOOKUP | ACCESS4_MODIFY |
		ACCESS4_EXTEND | ACCESS4_DELETE | ACCESS4_EXECUTE)

	// Debug log the access check result
	var uid, gid uint32
	if authCtx.Identity != nil {
		if authCtx.Identity.UID != nil {
			uid = *authCtx.Identity.UID
		}
		if authCtx.Identity.GID != nil {
			gid = *authCtx.Identity.GID
		}
	}
	logger.Debug("NFSv4 ACCESS check",
		"file_path", file.Path,
		"file_mode", fmt.Sprintf("0%o", file.Mode),
		"file_uid", file.UID,
		"file_gid", file.GID,
		"file_type", file.Type,
		"auth_uid", uid,
		"auth_gid", gid,
		"requested", fmt.Sprintf("0x%02x", accessReq),
		"granted", fmt.Sprintf("0x%02x", granted),
		"client", ctx.ClientAddr)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteUint32(&buf, supported) // supported
	_ = xdr.WriteUint32(&buf, granted)   // access

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_ACCESS,
		Data:   buf.Bytes(),
	}
}

// checkAccessBits checks requested ACCESS bits against file Unix permissions.
//
// Root (UID 0) gets all access. For other users, it checks the appropriate
// owner/group/other permission bits based on the effective UID/GID.
func checkAccessBits(requested uint32, file *metadata.File, authCtx *metadata.AuthContext) uint32 {
	var granted uint32

	// Determine effective UID/GID
	uid := ^uint32(0) // sentinel: invalid UID
	gid := ^uint32(0)
	var gids []uint32

	if authCtx.Identity != nil {
		if authCtx.Identity.UID != nil {
			uid = *authCtx.Identity.UID
		}
		if authCtx.Identity.GID != nil {
			gid = *authCtx.Identity.GID
		}
		gids = authCtx.Identity.GIDs
	}

	// Root gets everything
	if uid == 0 {
		return requested
	}

	// Determine which permission triad applies
	mode := file.Mode & 0o7777
	var readBit, writeBit, execBit bool

	if uid == file.UID {
		// Owner bits
		readBit = mode&0o400 != 0
		writeBit = mode&0o200 != 0
		execBit = mode&0o100 != 0
	} else if isInGroup(gid, gids, file.GID) {
		// Group bits
		readBit = mode&0o040 != 0
		writeBit = mode&0o020 != 0
		execBit = mode&0o010 != 0
	} else {
		// Other bits
		readBit = mode&0o004 != 0
		writeBit = mode&0o002 != 0
		execBit = mode&0o001 != 0
	}

	// Map permission bits to ACCESS4 bits
	if requested&ACCESS4_READ != 0 && readBit {
		granted |= ACCESS4_READ
	}
	if requested&ACCESS4_LOOKUP != 0 {
		// For directories: execute bit; for files: execute bit
		if execBit {
			granted |= ACCESS4_LOOKUP
		}
	}
	if requested&ACCESS4_MODIFY != 0 && writeBit {
		granted |= ACCESS4_MODIFY
	}
	if requested&ACCESS4_EXTEND != 0 && writeBit {
		granted |= ACCESS4_EXTEND
	}
	if requested&ACCESS4_DELETE != 0 && writeBit {
		// Simplified: grant DELETE if write permission on current
		granted |= ACCESS4_DELETE
	}
	if requested&ACCESS4_EXECUTE != 0 && execBit {
		granted |= ACCESS4_EXECUTE
	}

	return granted
}

// isInGroup checks if the given gid or any supplementary gids match the target group.
func isInGroup(gid uint32, gids []uint32, targetGID uint32) bool {
	if gid == targetGID {
		return true
	}
	for _, g := range gids {
		if g == targetGID {
			return true
		}
	}
	return false
}
