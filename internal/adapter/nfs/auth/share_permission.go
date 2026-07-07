package auth

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ErrShareAccessDenied is returned when a UID does not have permission to
// access a share (default permission is "none" for an unknown UID, or the
// resolved permission for a known user is "none").
var ErrShareAccessDenied = errors.New("share access denied")

// sidUnixSharePermissionResolver is the subset of the control-plane store that
// resolves a share permission from an NFS login's numeric UID + GIDs against
// direct AD/SID grants (#1528). The concrete store implements it; a store that
// does not simply skips the direct-AD-grant path.
type sidUnixSharePermissionResolver interface {
	ResolveSharePermissionForUnixIDs(ctx context.Context, uid uint32, gids []uint32, shareName string) (models.SharePermission, error)
}

// SharePermissionResult is the outcome of export-squash permission resolution.
type SharePermissionResult struct {
	// ReadOnly is true when the effective access is read-only — either the
	// share itself is read-only, or the resolved permission is "read".
	ReadOnly bool

	// Username is the resolved DittoFS username (or "root"), for logging and
	// auditing. Empty when no user could be resolved.
	Username string
}

// ResolveSharePermission applies the export-squash permission policy for an NFS
// request, independent of protocol version. It is the single source of truth
// shared by the NFSv3 and NFSv4 auth-context builders.
//
// The policy:
//   - No identity store or share → no policy information available; allow with
//     share defaults (read-write, honouring share.ReadOnly).
//   - No UID (AUTH_NULL / AUTH_NONE anonymous caller) → treated as guest, NOT
//     bypassed: denied when default_permission is "none", otherwise granted with
//     read-only coerced when the default permission is "read".
//   - Root (UID 0) under a squash mode that keeps root privileged (none,
//     root_to_admin, or all_to_admin) → admin, username "root". An empty/unset
//     squash normalizes to root_to_guest (the default) and does NOT promote root.
//   - Unknown UID → guest: denied when default_permission is "none", otherwise
//     granted with read-only coerced when the default permission is "read".
//   - Known user → their resolved share permission (falling back to the share
//     default on resolution error); denied when "none", read-only coerced when
//     "read".
//
// Returns ErrShareAccessDenied when access must be denied.
func ResolveSharePermission(
	ctx context.Context,
	identityStore models.IdentityStore,
	share *runtime.Share,
	shareName string,
	clientAddr string,
	uid *uint32,
	gids []uint32,
) (SharePermissionResult, error) {
	var result SharePermissionResult

	// No identity store or share - no per-user policy is resolvable, so allow
	// with share defaults. Still honour the share-level read-only flag when a
	// share is present: an unavailable identity store must never silently make a
	// read-only export writable. (A nil UID is NOT short-circuited here: an
	// anonymous AUTH_NULL caller is gated by default_permission below.)
	if identityStore == nil || share == nil {
		if share != nil {
			result.ReadOnly = share.ReadOnly
		}
		return result, nil
	}

	defaultPerm := models.ParseSharePermission(share.DefaultPermission)

	// AUTH_NULL / AUTH_NONE anonymous caller (no UID asserted). This must NOT
	// bypass the export permission model: gate it through the same guest policy
	// as an unknown UID so default_permission=none denies it and read-only
	// exports / default_permission=read coerce it to read-only.
	if uid == nil {
		if defaultPerm == models.PermissionNone {
			logger.DebugCtx(ctx, "Share access denied (anonymous AUTH_NULL caller, default permission is none)",
				"share", shareName, "client", clientAddr)
			return SharePermissionResult{}, ErrShareAccessDenied
		}
		result.ReadOnly = share.ReadOnly || defaultPerm == models.PermissionRead
		logger.DebugCtx(ctx, "Anonymous (AUTH_NULL) guest access granted",
			"share", shareName, "permission", defaultPerm, "readOnly", result.ReadOnly, "client", clientAddr)
		return result, nil
	}

	// Check if caller is root (UID 0) and squash mode doesn't map root away.
	// Root has full admin access only when squash mode is: none, root_to_admin,
	// or all_to_admin. An empty/unset squash normalizes to DefaultSquashMode
	// (root_to_guest), which does NOT promote root.
	isCallerRoot := *uid == 0
	effSquash := share.Squash.OrDefault()
	rootHasAdmin := effSquash == models.SquashNone ||
		effSquash == models.SquashRootToAdmin ||
		effSquash == models.SquashAllToAdmin
	if isCallerRoot && rootHasAdmin {
		result.ReadOnly = share.ReadOnly
		result.Username = "root"
		return result, nil
	}

	// Direct AD/SID grants (#1528): match the login's UID + group GIDs against
	// the share's SID grants. This authorizes an AD principal that has no local
	// DittoFS account — the NFS analogue of the SMB PAC-SID path (NFS carries no
	// SID, so grants are matched on the numeric id the login resolved to).
	sidPerm := models.PermissionNone
	if r, ok := identityStore.(sidUnixSharePermissionResolver); ok {
		if p, sErr := r.ResolveSharePermissionForUnixIDs(ctx, *uid, gids, shareName); sErr != nil {
			logger.DebugCtx(ctx, "SID permission resolution failed, ignoring",
				"share", shareName, "uid", *uid, "error", sErr)
		} else {
			sidPerm = p
		}
	}

	// Try reverse lookup: find user by UID.
	user, err := identityStore.GetUserByUID(ctx, *uid)
	if err != nil || user == nil {
		logger.DebugCtx(ctx, "NFS UID reverse lookup failed, treating as guest",
			"share", shareName, "uid", *uid, "client", clientAddr, "error", err)

		// Guest access: the higher of the share default and any direct AD/SID
		// grant. A SID grant lets an AD principal with no local account through
		// even when default_permission is none.
		guestPerm := defaultPerm
		if sidPerm.Level() > guestPerm.Level() {
			guestPerm = sidPerm
		}
		if guestPerm == models.PermissionNone {
			logger.DebugCtx(ctx, "Share access denied (unknown UID, no default or SID grant)",
				"share", shareName, "uid", *uid)
			return SharePermissionResult{}, ErrShareAccessDenied
		}

		result.ReadOnly = share.ReadOnly || guestPerm == models.PermissionRead
		logger.DebugCtx(ctx, "Access granted (guest/AD-SID)", "share", shareName, "permission", guestPerm, "readOnly", result.ReadOnly)
		return result, nil
	}

	// User found - resolve their permission.
	logger.DebugCtx(ctx, "NFS UID reverse lookup succeeded",
		"share", shareName, "uid", *uid, "username", user.Username, "client", clientAddr)

	perm, permErr := identityStore.ResolveSharePermission(ctx, user, shareName)
	if permErr != nil {
		logger.DebugCtx(ctx, "Permission resolution failed, using default",
			"share", shareName, "user", user.Username, "error", permErr, "default", defaultPerm)
		perm = defaultPerm
	}
	// A direct AD/SID grant can elevate a local user's access (additive, like
	// group membership).
	if sidPerm.Level() > perm.Level() {
		perm = sidPerm
	}

	if perm == models.PermissionNone {
		logger.DebugCtx(ctx, "Share access denied", "share", shareName, "user", user.Username)
		return SharePermissionResult{}, ErrShareAccessDenied
	}

	result.ReadOnly = share.ReadOnly || perm == models.PermissionRead
	result.Username = user.Username
	logger.DebugCtx(ctx, "User permission resolved", "share", shareName, "user", user.Username, "permission", perm, "readOnly", result.ReadOnly)

	return result, nil
}
