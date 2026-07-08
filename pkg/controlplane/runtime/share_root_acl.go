package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// smbDefaultUID mirrors the default UID the SMB adapter assigns to a user that
// has no explicit UID (internal/adapter/smb/handlers/auth_helper.go). A grant
// projected for such a user must use the same effective id so the resulting ACE
// matches the AuthContext the adapter builds at request time. Users that share
// this fallback are indistinguishable at the filesystem layer — assign explicit
// UIDs for per-user isolation.
const smbDefaultUID uint32 = 1000

// grantLevelFor maps a stored control-plane permission string to the acl
// package's protocol-agnostic grant level.
func grantLevelFor(permission string) acl.GrantLevel {
	switch models.SharePermission(permission) {
	case models.PermissionRead:
		return acl.GrantRead
	case models.PermissionReadWrite:
		return acl.GrantReadWrite
	case models.PermissionAdmin:
		return acl.GrantAdmin
	default:
		return acl.GrantNone
	}
}

// ShareRootGrantACL builds the allow-only ACL that projects the share's full
// current set of control-plane grants (user, group, and direct AD/SID grants,
// plus the default permission) into Windows ACE form, using the level→mask
// mapping in the acl package. It is the single source of truth for both the
// root-directory reconcile (ReconcileShareRootACL) and the SMB Security-tab
// projection (#1608), which merges these ACEs into each file/dir descriptor so
// Windows admins can see the AD principals that govern access.
//
// It returns (nil, nil) for a partially-wired runtime (no control-plane store or
// shares service) so callers can treat share-grant projection as best-effort.
// A dangling grant (user/group deleted out from under it) is skipped; a
// transient store error aborts so a partial ACL is never produced.
func (r *Runtime) ShareRootGrantACL(ctx context.Context, shareName string) (*acl.ACL, error) {
	if r.store == nil || r.sharesSvc == nil {
		return nil, nil
	}

	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, fmt.Errorf("share root grant ACL for %q: %w", shareName, err)
	}

	userPerms, err := r.store.GetShareUserPermissions(ctx, shareName)
	if err != nil {
		return nil, fmt.Errorf("share root grant ACL for %q: list user permissions: %w", shareName, err)
	}
	groupPerms, err := r.store.GetShareGroupPermissions(ctx, shareName)
	if err != nil {
		return nil, fmt.Errorf("share root grant ACL for %q: list group permissions: %w", shareName, err)
	}

	grants := make([]acl.RootGrant, 0, len(userPerms)+len(groupPerms))
	for _, p := range userPerms {
		level := grantLevelFor(p.Permission)
		if level == acl.GrantNone {
			continue
		}
		user, err := r.store.GetUserByID(ctx, p.UserID)
		if err != nil {
			if errors.Is(err, models.ErrUserNotFound) {
				// Dangling grant (user removed out from under it): skip
				// rather than abort the whole projection.
				continue
			}
			// Transient error (DB hiccup, context deadline): abort before
			// building a partial ACL that would drop valid grantees.
			return nil, fmt.Errorf("share root grant ACL for %q: get user %q: %w", shareName, p.UserID, err)
		}
		uid := smbDefaultUID
		if user.UID != nil {
			uid = *user.UID
		}
		grants = append(grants, acl.RootGrant{ID: uid, Level: level})
	}
	for _, p := range groupPerms {
		level := grantLevelFor(p.Permission)
		if level == acl.GrantNone {
			continue
		}
		group, err := r.store.GetGroupByID(ctx, p.GroupID)
		if err != nil {
			if errors.Is(err, models.ErrGroupNotFound) {
				// Dangling grant: skip rather than abort.
				continue
			}
			// Transient error: abort before building a partial ACL (see the
			// user-grant path above for the rationale).
			return nil, fmt.Errorf("share root grant ACL for %q: get group %q: %w", shareName, p.GroupID, err)
		}
		if group.GID == nil {
			// A group without an assigned GID cannot be projected onto a
			// numeric ACE; the share-level grant still applies at mount.
			continue
		}
		grants = append(grants, acl.RootGrant{ID: *group.GID, IsGroup: true, Level: level})
	}

	// Direct AD/SID grants (#1528): project a "sid:<SID>" ACE (matched against a
	// login's PAC SIDs on the SMB path) and, when the grant carries a resolved
	// Unix id, an additional "{id}@localdomain" numeric ACE so NFS — which has no
	// SID on the wire — matches by UID/GID. No local object to resolve.
	sidPerms, err := r.store.GetShareSIDPermissions(ctx, shareName)
	if err != nil {
		return nil, fmt.Errorf("share root grant ACL for %q: list SID permissions: %w", shareName, err)
	}
	for _, p := range sidPerms {
		level := grantLevelFor(p.Permission)
		if level == acl.GrantNone {
			continue
		}
		grants = append(grants, acl.RootGrant{ID: p.UnixID, SID: p.SID, IsGroup: p.IsGroup, Level: level})
	}

	return acl.BuildShareRootACL(grantLevelFor(share.DefaultPermission), grants), nil
}

// ReconcileShareRootACL rebuilds the share root directory's ACL from the full
// current set of control-plane share-permission grants, so the filesystem
// permission layer agrees with the share-level access control plane. Without
// this, a user granted read-write at the share level is still denied by the
// root directory's POSIX mode bits (owner uid 0, mode 0755).
//
// It recomputes the entire ACL from current state, so it is idempotent and
// correct after a grant, a revoke, or a default-permission change — a revoke is
// just a rebuild without that grant. Callers treat failures as non-fatal: the
// control-plane permission record is the source of truth and a subsequent
// reconcile self-heals the projection.
func (r *Runtime) ReconcileShareRootACL(ctx context.Context, shareName string) error {
	// Defensive no-op for partially-wired runtimes (e.g. test harnesses that
	// construct a Runtime without a control-plane store or metadata service).
	// Reconciliation is best-effort, so a missing dependency is not an error.
	if r.store == nil || r.metadataService == nil || r.sharesSvc == nil {
		return nil
	}

	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return fmt.Errorf("reconcile root ACL for %q: %w", shareName, err)
	}

	dacl, err := r.ShareRootGrantACL(ctx, shareName)
	if err != nil {
		return fmt.Errorf("reconcile root ACL for %q: %w", shareName, err)
	}
	if dacl == nil {
		// Partially-wired runtime — nothing to project.
		return nil
	}

	// The root directory is owned by uid 0; only a superuser context may
	// rewrite its ACL. The system context bypasses permission checks.
	sysCtx := metadata.NewSystemAuthContext(ctx)
	if _, err := r.metadataService.SetFileAttributes(sysCtx, share.RootHandle, &metadata.SetAttrs{ACL: dacl}); err != nil {
		return fmt.Errorf("reconcile root ACL for %q: set attributes: %w", shareName, err)
	}

	// A grant/revoke or default-permission change just altered who may access
	// the share. Drop any cached per-identity authorization so active clients
	// pick up the new policy without a server restart.
	r.InvalidateAuthCache()
	return nil
}
