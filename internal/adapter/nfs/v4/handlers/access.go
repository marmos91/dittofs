package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
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
// Checks access permissions for the current filehandle against a requested bitmask.
// Delegates to MetadataService.CheckAccess for real files; pseudo-fs grants all access.
// No side effects; read-only permission probe returning supported and granted bitmasks.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_STALE, NFS4ERR_BADXDR, NFS4ERR_IO.
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
// It routes through the central metadata permission checker
// (MetadataService.CheckPermissions) exactly like the NFSv3 ACCESS handler,
// so ACLs, DENY ACEs, and SID-based grants are honored identically across
// protocols. The handler only translates protocol access bits to and from the
// canonical metadata.Permission vocabulary; all permission logic lives in the
// metadata layer. All 6 access bits are reported as supported.
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

	handle := metadata.FileHandle(ctx.CurrentFH)
	file, err := metaSvc.GetFile(authCtx.Context, handle)
	if err != nil {
		status := common.MapToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(status),
		}
	}

	// Translate the requested ACCESS4 bits to the canonical permission
	// vocabulary, run the central checker, then translate the granted set
	// back. This is the same path NFSv3 ACCESS uses (see
	// internal/adapter/nfs/v3/handlers/access.go) — the metadata layer owns
	// ACL / DENY-ACE / SID evaluation; the handler does protocol only.
	requestedPerms := nfsAccessToPermissions(accessReq, file.Type)

	grantedPerms, err := metaSvc.CheckPermissions(authCtx, handle, requestedPerms)
	if err != nil {
		status := common.MapToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_ACCESS,
			Data:   encodeStatusOnly(status),
		}
	}

	// Mask back to what the client actually asked for; CheckPermissions only
	// ever returns a subset of the requested generic flags, but the
	// permission<->access translation is not perfectly bijective for
	// directories (LOOKUP and EXECUTE both map to Traverse), so re-AND with
	// the requested ACCESS4 bits.
	granted := permissionsToNFSAccess(grantedPerms, file.Type) & accessReq

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

// nfsAccessToPermissions translates NFSv4 ACCESS4 bits to the canonical
// metadata.Permission vocabulary. NFSv4 ACCESS4 bits (RFC 7530 §6) share the
// same numeric values and semantics as NFSv3 ACCESS bits (RFC 1813 §3.3.4), so
// this mirrors internal/adapter/nfs/v3/handlers/access.go::nfsAccessToPermissions
// to keep cross-protocol enforcement identical.
//
// The translation is context-sensitive on file type: for directories
// ACCESS4_READ means list, ACCESS4_LOOKUP/EXECUTE mean traverse, and
// MODIFY/EXTEND mean write; for files the mapping is direct.
func nfsAccessToPermissions(accessReq uint32, fileType metadata.FileType) metadata.Permission {
	var perms metadata.Permission

	if fileType == metadata.FileTypeDirectory {
		if accessReq&ACCESS4_READ != 0 {
			perms |= metadata.PermissionListDirectory
		}
		if accessReq&ACCESS4_LOOKUP != 0 {
			perms |= metadata.PermissionTraverse
		}
		if accessReq&ACCESS4_EXECUTE != 0 {
			perms |= metadata.PermissionTraverse
		}
		if accessReq&(ACCESS4_MODIFY|ACCESS4_EXTEND) != 0 {
			perms |= metadata.PermissionWrite
		}
	} else {
		if accessReq&ACCESS4_READ != 0 {
			perms |= metadata.PermissionRead
		}
		if accessReq&(ACCESS4_MODIFY|ACCESS4_EXTEND) != 0 {
			perms |= metadata.PermissionWrite
		}
		if accessReq&ACCESS4_EXECUTE != 0 {
			perms |= metadata.PermissionExecute
		}
	}

	if accessReq&ACCESS4_DELETE != 0 {
		perms |= metadata.PermissionDelete
	}

	return perms
}

// permissionsToNFSAccess is the inverse of nfsAccessToPermissions, mirroring
// internal/adapter/nfs/v3/handlers/access.go::permissionsToNFSAccess.
func permissionsToNFSAccess(perms metadata.Permission, fileType metadata.FileType) uint32 {
	var accessRes uint32

	if fileType == metadata.FileTypeDirectory {
		if perms&metadata.PermissionListDirectory != 0 {
			accessRes |= ACCESS4_READ
		}
		if perms&metadata.PermissionTraverse != 0 {
			accessRes |= ACCESS4_LOOKUP | ACCESS4_EXECUTE
		}
		if perms&metadata.PermissionWrite != 0 {
			accessRes |= ACCESS4_MODIFY | ACCESS4_EXTEND
		}
	} else {
		if perms&metadata.PermissionRead != 0 {
			accessRes |= ACCESS4_READ
		}
		if perms&metadata.PermissionWrite != 0 {
			accessRes |= ACCESS4_MODIFY | ACCESS4_EXTEND
		}
		if perms&metadata.PermissionExecute != 0 {
			accessRes |= ACCESS4_EXECUTE
		}
	}

	if perms&metadata.PermissionDelete != 0 {
		accessRes |= ACCESS4_DELETE
	}

	return accessRes
}
