package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// authStatusError wraps a buildV4AuthContext failure with the NFS4 status the
// COMPOUND operation should return. Without it every auth failure collapsed to
// NFS4ERR_SERVERFAULT, which mislabels an authorization denial (a krb5 export
// policy rejection or a default_permission=none denial) as an internal error.
type authStatusError struct {
	status uint32
	err    error
}

func (e *authStatusError) Error() string { return e.err.Error() }

func (e *authStatusError) Unwrap() error { return e.err }

// nfs4StatusForAuthError maps a buildV4AuthContext error to the NFS4 status a
// handler should return. Typed authStatusError values carry their own status
// (NFS4ERR_WRONGSEC for an export auth-flavor rejection, NFS4ERR_ACCESS for a
// share-permission denial); anything else is a genuine internal fault.
func nfs4StatusForAuthError(err error) uint32 {
	var ae *authStatusError
	if errors.As(err, &ae) {
		return ae.status
	}
	return types.NFS4ERR_SERVERFAULT
}

// buildV4AuthContext creates an AuthContext for NFSv4 real-FS operations.
//
// It extracts the share name from the file handle, builds an identity from
// the CompoundContext credentials, applies identity mapping rules, resolves
// permissions, and returns the effective AuthContext.
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - string: The share name extracted from the handle
//   - error: If handle decoding, identity mapping, or permission resolution fails
func (h *Handler) buildV4AuthContext(ctx *types.CompoundContext, handle []byte) (*metadata.AuthContext, string, error) {
	// Decode file handle to extract share name
	shareName, _, err := metadata.DecodeFileHandle(metadata.FileHandle(handle))
	if err != nil {
		return nil, "", fmt.Errorf("decode file handle: %w", err)
	}

	// Map auth flavor to auth method string. RPCSEC_GSS (Kerberos) was
	// previously mislabeled "anonymous", which both confused audit logs and
	// blocked any per-share auth-flavor policy from telling krb5 apart from a
	// truly anonymous request.
	authMethod := "anonymous"
	switch ctx.AuthFlavor {
	case rpc.AuthUnix:
		authMethod = "unix"
	case rpc.AuthRPCSECGSS:
		authMethod = "kerberos"
	}

	// Build identity from Unix credentials (before mapping)
	originalIdentity := &metadata.Identity{
		UID:  ctx.UID,
		GID:  ctx.GID,
		GIDs: ctx.GIDs,
	}
	// Carry the Windows half of a GSS-resolved identity across. The numeric
	// triple above is all the compound context holds, so a Kerberos principal's
	// SID and group SIDs would otherwise be lost before ACL evaluation matches
	// ACEs against them.
	if gssIdentity := gss.IdentityFromContext(ctx.Context); gssIdentity != nil {
		originalIdentity.SID = gssIdentity.SID
		originalIdentity.GroupSIDs = gssIdentity.GroupSIDs
	}

	// Set username from UID if available (for logging/auditing)
	if originalIdentity.UID != nil {
		originalIdentity.Username = fmt.Sprintf("uid:%d", *originalIdentity.UID)
	}

	if h.Registry == nil {
		// No registry available -- return a basic auth context
		return &metadata.AuthContext{
			Context:    ctx.Context,
			ClientAddr: ctx.ClientAddr,
			AuthMethod: authMethod,
			Identity:   originalIdentity,
		}, shareName, nil
	}

	// Enforce the share's netgroup client allowlist. NFSv4 has no MOUNT
	// protocol, so the check the v3 MOUNT handler performs never runs for a v4
	// client: without it here a netgroup-restricted share is reachable from any
	// address over PUTROOTFH/PUTFH/LOOKUP. This gates every operation that
	// builds an auth context. Operations that act on the current filehandle
	// without one are gated where that handle enters the compound instead, by
	// shareEntryStatus.
	if err := h.checkNetgroupAccess(ctx, shareName); err != nil {
		return nil, "", err
	}

	// Get share for the export-squash permission policy. On error GetShare
	// returns a nil share; ResolveSharePermission treats a nil share as "no
	// policy info" (allow with defaults) and the ApplyIdentityMapping step
	// below still fails closed if the share is genuinely gone.
	share, _ := h.Registry.GetShare(shareName)

	// Enforce the per-share export access policy — which auth flavors this
	// export accepts and the GSS protection floor. NFSv4.1 has no MOUNT call, so
	// the checks the MOUNT handler applies never ran on v4: a share that requires
	// Kerberos was usable over AUTH_SYS on v4.1, silently bypassing the
	// requirement. This is the same decision MOUNT and the v3 operation path
	// apply, so the three cannot drift. It runs at the first real-FS op that
	// resolves the share handle, and the refusal surfaces as NFS4ERR_WRONGSEC so
	// the client retries with the correct flavor (SECINFO).
	//
	// The netgroup allowlist is checked above with its own status, so no netgroup
	// lookup is handed to the policy here.
	if share != nil {
		if accessErr := auth.CheckExportAccess(ctx.Context, share, ctx.AuthFlavor, nil, nil); accessErr != nil {
			return nil, "", &authStatusError{status: types.NFS4ERR_WRONGSEC, err: accessErr}
		}
	}

	// Apply the export-squash permission policy (default_permission=none denial,
	// root→admin promotion, guest/known-user read-only coercion). This is the
	// SAME policy the v3 path applies via auth.ResolveSharePermission; v4
	// previously skipped it, so default_permission=none did not deny unknown
	// UIDs and read-only coercion never fired.
	permGIDs := append([]uint32(nil), ctx.GIDs...)
	if ctx.GID != nil {
		permGIDs = append(permGIDs, *ctx.GID)
	}
	permResult, err := auth.ResolveSharePermission(
		ctx.Context, h.Registry.GetIdentityStore(), share, shareName, ctx.ClientAddr, ctx.UID, permGIDs)
	if err != nil {
		// A share-permission denial (e.g. default_permission=none for an
		// unmapped principal — the krb5 machine-principal-maps-to-nobody case)
		// is an authorization decision, not an internal fault: surface it as
		// NFS4ERR_ACCESS so the client sees "permission denied", not a server
		// error.
		if errors.Is(err, auth.ErrShareAccessDenied) {
			return nil, "", &authStatusError{status: types.NFS4ERR_ACCESS, err: err}
		}
		return nil, "", err
	}
	if permResult.Username != "" {
		originalIdentity.Username = permResult.Username
	}

	// Apply share-level identity mapping (all_squash, root_squash).
	//
	// Fail closed on mapping failure (parity with the v3 path,
	// BuildAuthContextWithMapping). A mapping failure means the share could
	// not be resolved (e.g. a stale handle for a deleted/renamed share); the
	// previous behaviour of falling back to the original, UNMAPPED identity
	// silently bypassed squash rules (a root client would have stayed root).
	effectiveIdentity, err := h.Registry.ApplyIdentityMapping(shareName, originalIdentity)
	if err != nil {
		logger.Debug("NFSv4 identity mapping failed",
			"share", shareName,
			"error", err,
			"client", ctx.ClientAddr)
		return nil, "", fmt.Errorf("apply identity mapping: %w", err)
	}

	// Create auth context with the effective (mapped) identity. LockClientID
	// keys the lock-layer client identity the metadata layer's originator
	// exclusion compares (see OnDirChange): without it, the holder's own
	// mutations recall the holder's own directory delegation instead of
	// notifying it — the recall-vs-notify division of RFC 7530 collapses to
	// recall-only.
	authCtx := &metadata.AuthContext{
		Context:       ctx.Context,
		ClientAddr:    ctx.ClientAddr,
		AuthMethod:    authMethod,
		Identity:      effectiveIdentity,
		ShareReadOnly: permResult.ReadOnly,
		LockClientID:  state.NFSLockClientIdentity(ctx.EffectiveClientID(0)),
	}

	return authCtx, shareName, nil
}

// shareEntryStatus reports the NFSv4 status a compound must answer with when a
// share's file handle is about to enter the current filehandle, or NFS4_OK when
// it may. A disabled share answers NFS4ERR_STALE (clients reacquire fresh
// handles after a restore plus an explicit re-enable); a client outside the
// share's netgroup allowlist answers NFS4ERR_ACCESS.
//
// It exists because a compound may enter a share's handle and then run only
// operations that make no metadata call of their own, so no auth context is
// ever built for it. There are exactly two places a share handle enters the
// compound: PUTFH, and a LOOKUP that crosses an export junction out of the
// pseudo-fs. Both call this, so the two cannot drift apart.
//
// It deliberately covers only what can be decided from the handle and the
// peer: the share's enabled state and its netgroup client allowlist. The
// export auth-flavor policy (AllowAuthSys, RequireKerberos, MinKerberosLevel)
// needs the request's auth flavor, so it is applied by buildV4AuthContext, and
// the operations that act on the current filehandle without a metadata call of
// their own -- LOCK, LOCKT, LOCKU, GET_DIR_DELEGATION -- call
// currentFHAccessStatus to reach it rather than duplicating the check here.
// Returning NFS4ERR_WRONGSEC from PUTFH would send a client to SECINFO rather
// than to a stronger flavor, which is why the flavor gate is not folded in.
func (h *Handler) shareEntryStatus(ctx *types.CompoundContext, shareName string) uint32 {
	if h.Registry == nil {
		return types.NFS4_OK
	}

	if share, err := h.Registry.GetShare(shareName); err == nil && share != nil && !share.Enabled {
		logger.Warn("NFSv4 refused a handle for a disabled share",
			"share", share.Name, "client", ctx.ClientAddr)
		return types.NFS4ERR_STALE
	}

	if ngErr := h.checkNetgroupAccess(ctx, shareName); ngErr != nil {
		return nfs4StatusForAuthError(ngErr)
	}

	return types.NFS4_OK
}

// currentFHAccessStatus applies the export access policy to the compound's
// current filehandle, reporting the NFS4 status to answer with -- NFS4_OK when
// the operation may proceed.
//
// LOCK, LOCKT, LOCKU and GET_DIR_DELEGATION mutate or observe state through
// the StateManager without making a metadata call, so unlike every other
// real-FS operation they never reached buildV4AuthContext and so never reached
// its export auth-flavor check: a share that requires Kerberos, or disallows
// AUTH_SYS, stayed reachable over AUTH_SYS through them. Calling this routes
// them through the same gate as the rest of the protocol, and the refusal
// carries the status that gate already maps (NFS4ERR_WRONGSEC for a flavor
// rejection, NFS4ERR_ACCESS for a share permission denial) instead of a
// second, divergent answer.
//
// Call it immediately before the operation touches state, not on entry: a
// malformed request must still answer NFS4ERR_BADXDR and a request with no
// current filehandle NFS4ERR_NOFILEHANDLE, and a policy refusal ahead of those
// preconditions would report the wrong thing to a client that never got far
// enough to be refused.
//
// A pseudo-fs handle names no share and carries no export policy, so it is
// allowed through: the callers reject it separately with NFS4ERR_INVAL.
func (h *Handler) currentFHAccessStatus(ctx *types.CompoundContext) uint32 {
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return types.NFS4_OK
	}

	if _, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH); err != nil {
		return nfs4StatusForAuthError(err)
	}
	return types.NFS4_OK
}

// refuseCurrentFHAccess returns the status-only refusal for op when the export
// access policy forbids the compound's current filehandle, or nil when the
// operation may proceed. See currentFHAccessStatus for where it belongs.
func (h *Handler) refuseCurrentFHAccess(ctx *types.CompoundContext, op uint32) *types.CompoundResult {
	status := h.currentFHAccessStatus(ctx)
	if status == types.NFS4_OK {
		return nil
	}
	return &types.CompoundResult{
		Status: status,
		OpCode: op,
		Data:   encodeStatusOnly(status),
	}
}

// checkNetgroupAccess returns NFS4ERR_ACCESS unless the compound's peer is in
// the netgroup that shareName is restricted to.
//
// It fails closed the same way the v3 MOUNT handler does: a lookup error and a
// no-match both deny. A peer address that cannot be parsed yields a nil IP,
// which matches no netgroup member and so denies any share that has a netgroup,
// while leaving shares without one (empty allowlist = allow all) unaffected.
func (h *Handler) checkNetgroupAccess(ctx *types.CompoundContext, shareName string) error {
	host := ctx.ClientAddr
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	clientIP := net.ParseIP(host)

	allowed, err := h.Registry.CheckNetgroupAccess(ctx.Context, shareName, clientIP)
	if err != nil {
		logger.Warn("NFSv4 netgroup access check failed, denying",
			"share", shareName, "client", ctx.ClientAddr, "error", err)
		return &authStatusError{
			status: types.NFS4ERR_ACCESS,
			err:    fmt.Errorf("netgroup access check for share %q: %w", shareName, err),
		}
	}
	if !allowed {
		logger.Warn("NFSv4 access denied: client not in the share's netgroup",
			"share", shareName, "client", ctx.ClientAddr)
		return &authStatusError{
			status: types.NFS4ERR_ACCESS,
			err:    fmt.Errorf("client %q is not in the netgroup allowed for share %q", ctx.ClientAddr, shareName),
		}
	}
	return nil
}

// checkReadPermission gates a data read on the caller's read permission for
// handle, reporting the NFS4 status to answer with — NFS4_OK when the read may
// proceed.
//
// The read-family operations all accept the anonymous (all-zero) and
// READ-bypass (all-one) special stateids, which carry no open state and so
// never went through OPEN's permission check. The share-access check on a real
// open stateid does not cover it either: that constrains how the file was
// opened, not who the caller is.
//
// The gate must stay unconditional rather than narrowing to the special-stateid
// case. A non-nil open state says the file was opened with the right mode; it
// says nothing about which client is presenting the stateid, so skipping the
// check whenever one exists would reopen the same hole through another door.
func checkReadPermission(
	metaSvc *metadata.Service,
	ctx *types.CompoundContext,
	authCtx *metadata.AuthContext,
	handle metadata.FileHandle,
	file *metadata.File,
	op uint32,
) uint32 {
	if err := metaSvc.CheckReadPermissionFile(authCtx, handle, file); err != nil {
		status := types.StatusForErr(err)
		logger.Debug("NFSv4 read denied",
			"op", types.OpName(op),
			"nfs_status", status,
			"error", err,
			"client", ctx.ClientAddr)
		return status
	}
	return types.NFS4_OK
}

// resolveBlockStore resolves the per-share block store for ctx.CurrentFH,
// taking the write path when forWrite is set. On failure it returns the
// SERVERFAULT result the caller returns unchanged, tagged with op.
//
// The nil-Registry guard lives here rather than in common.ResolveFor* so the
// NFSv4-specific concern stays out of the shared resolver.
func (h *Handler) resolveBlockStore(ctx *types.CompoundContext, op uint32, forWrite bool) (*engine.Store, *types.CompoundResult) {
	serverFault := func() *types.CompoundResult {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: op,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}
	if h.Registry == nil {
		logger.Debug("NFSv4 no registry configured", "op", types.OpName(op), "client", ctx.ClientAddr)
		return nil, serverFault()
	}
	resolve := common.ResolveForRead
	if forWrite {
		resolve = common.ResolveForWrite
	}
	blockStore, err := resolve(ctx.Context, h.Registry, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		logger.Debug("NFSv4 block store resolve failed", "op", types.OpName(op), "error", err, "client", ctx.ClientAddr)
		return nil, serverFault()
	}
	return blockStore, nil
}

// getMetadataServiceForCtx returns the MetadataService from the handler's registry.
// Returns an error if the registry is nil.
func getMetadataServiceForCtx(h *Handler) (*metadata.Service, error) {
	if h.Registry == nil {
		return nil, fmt.Errorf("no registry configured")
	}
	return h.Registry.GetMetadataService(), nil
}

// encodeChangeInfo4 encodes a change_info4 structure into the buffer.
//
// change_info4 is used by CREATE, REMOVE, RENAME, and other operations
// to report directory change information.
//
// Wire format:
//
//	bool    atomic;      (true if before/after are atomic)
//	uint64  changeid_before;
//	uint64  changeid_after;
func encodeChangeInfo4(buf *bytes.Buffer, atomic bool, before, after uint64) {
	_ = xdr.WriteBool(buf, atomic)
	_ = xdr.WriteUint64(buf, before)
	_ = xdr.WriteUint64(buf, after)
}

// regularFileStatus reports the status an operation defined only over regular
// files must return for the type of object its current filehandle designates:
// NFS4_OK for a regular file, NFS4ERR_ISDIR for a directory and NFS4ERR_INVAL
// for every other type. COMMIT (RFC 7530 Section 16.5.4), LOCK, LOCKT and LOCKU
// (Section 16.10.4) and READ and WRITE (Sections 16.22.4 and 16.36.4) all state
// the rule in the same words.
func regularFileStatus(fileType metadata.FileType) uint32 {
	switch fileType {
	case metadata.FileTypeRegular:
		return types.NFS4_OK
	case metadata.FileTypeDirectory:
		return types.NFS4ERR_ISDIR
	default:
		return types.NFS4ERR_INVAL
	}
}

// directoryStatus reports the status LOOKUP and LOOKUPP must return for the
// type of object their current filehandle designates. RFC 7530 Section 16.15.4:
// a symbolic link is reported as NFS4ERR_SYMLINK so the client knows to resolve
// it, and every other non-directory type as NFS4ERR_NOTDIR.
func directoryStatus(fileType metadata.FileType) uint32 {
	switch fileType {
	case metadata.FileTypeDirectory:
		return types.NFS4_OK
	case metadata.FileTypeSymlink:
		return types.NFS4ERR_SYMLINK
	default:
		return types.NFS4ERR_NOTDIR
	}
}

// fileTypeForHandle resolves the object type of a real-filesystem filehandle
// for the operations that gate on it but never load the file otherwise. The
// second return is NFS4_OK when the type is usable and the status to report
// when the handle could not be resolved.
func (h *Handler) fileTypeForHandle(ctx *types.CompoundContext, handle []byte) (metadata.FileType, uint32) {
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return 0, types.NFS4ERR_SERVERFAULT
	}
	// GetFileForRead: handle-addressed, File.Path unused -- skip derivePath.
	file, err := metaSvc.GetFileForRead(ctx.Context, metadata.FileHandle(handle))
	if err != nil {
		return 0, types.StatusForErr(err)
	}
	return file.Type, types.NFS4_OK
}
