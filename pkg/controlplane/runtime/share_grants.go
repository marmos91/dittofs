package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// shareGrantStore decorates a control-plane store so that every share-permission
// mutation completes itself: the grant row is written, then the share root
// directory's ACL is reprojected from the new grant set and every adapter's
// cached per-identity authorization is dropped.
//
// The completion rides the write rather than each HTTP handler because grants
// are written from more than one route — the share permission endpoints and the
// share_permissions field on user create/update. A handler that writes a grant
// and omits the completion leaves the share's on-disk ACL dependent on which
// endpoint the administrator used, and the projection is what lets a grantee
// traverse a root directory owned by uid 0 with mode 0755.
//
// The two halves of the completion have different standing. The ACL is a
// projection and its reconcile is best-effort — a failure is logged, not
// surfaced, because the control-plane permission record is authoritative and a
// later reconcile self-heals it. The invalidation is a consequence of the row
// that was just written and therefore fires unconditionally, including when the
// projection failed: a revoke that returned 204 having notified nobody leaves an
// SMB connection holding the grant for its whole lifetime.
//
// Only a write that succeeded runs the completion, so a rejected mutation
// neither reprojects nor flushes.
type shareGrantStore struct {
	store.Store
	rt *Runtime
}

// ShareGrantStore wraps s so its share-permission mutations also reproject the
// share root ACL and invalidate the adapters' auth caches. Every other method
// passes through unchanged, and the wrapper satisfies store.Store, so callers
// that only read are unaffected.
func (r *Runtime) ShareGrantStore(s store.Store) store.Store {
	return &shareGrantStore{Store: s, rt: r}
}

// grantsChanged runs the completion for a share whose grant set just changed.
func (g *shareGrantStore) grantsChanged(ctx context.Context, shareName string) {
	if err := g.rt.ReconcileShareRootACL(ctx, shareName); err != nil {
		logger.Warn("Failed to reconcile share root ACL", "share", shareName, "error", err)
	}
	g.rt.InvalidateAuthCache()
}

func (g *shareGrantStore) SetUserSharePermission(ctx context.Context, perm *models.UserSharePermission) error {
	if err := g.Store.SetUserSharePermission(ctx, perm); err != nil {
		return err
	}
	g.grantsChanged(ctx, perm.ShareName)
	return nil
}

func (g *shareGrantStore) DeleteUserSharePermission(ctx context.Context, username, shareName string) error {
	if err := g.Store.DeleteUserSharePermission(ctx, username, shareName); err != nil {
		return err
	}
	g.grantsChanged(ctx, shareName)
	return nil
}

func (g *shareGrantStore) SetGroupSharePermission(ctx context.Context, perm *models.GroupSharePermission) error {
	if err := g.Store.SetGroupSharePermission(ctx, perm); err != nil {
		return err
	}
	g.grantsChanged(ctx, perm.ShareName)
	return nil
}

func (g *shareGrantStore) DeleteGroupSharePermission(ctx context.Context, groupName, shareName string) error {
	if err := g.Store.DeleteGroupSharePermission(ctx, groupName, shareName); err != nil {
		return err
	}
	g.grantsChanged(ctx, shareName)
	return nil
}

func (g *shareGrantStore) SetSIDSharePermission(ctx context.Context, perm *models.SIDSharePermission) error {
	if err := g.Store.SetSIDSharePermission(ctx, perm); err != nil {
		return err
	}
	g.grantsChanged(ctx, perm.ShareName)
	return nil
}

func (g *shareGrantStore) DeleteSIDSharePermission(ctx context.Context, sid, shareName string) error {
	if err := g.Store.DeleteSIDSharePermission(ctx, sid, shareName); err != nil {
		return err
	}
	g.grantsChanged(ctx, shareName)
	return nil
}

// DeleteSIDSharePermissionsByDisplayName completes like the other mutations.
// A revoke that drops both a local grant and the direct AD/SID grant for the
// same principal name therefore completes twice; the projection is a full
// rebuild from current state and the invalidation is a broadcast, so the second
// pass is redundant rather than wrong.
//
// ponytail: two root-ACL writes and two cache flushes per name-based revoke;
// coalesce per request only if revoke latency or invalidation churn shows up.
func (g *shareGrantStore) DeleteSIDSharePermissionsByDisplayName(ctx context.Context, shareName, displayName string, isGroup bool) error {
	if err := g.Store.DeleteSIDSharePermissionsByDisplayName(ctx, shareName, displayName, isGroup); err != nil {
		return err
	}
	g.grantsChanged(ctx, shareName)
	return nil
}

// UpdateUser completes an identity change the same way a grant write does. A
// user's grants project onto a share root as ACEs keyed on the user's Unix id,
// so moving that id orphans every ACE built from the old one: the grant row is
// untouched by the update, no grant write fires, and the grantee is left
// holding a live grant with no ACE on a root owned by uid 0 mode 0755. The
// reprojection belongs here rather than in the user handler so every caller of
// the identity mutation gets it, exactly as the grant writes do.
//
// Only a persisted change reprojects, and only when the id actually moved — a
// profile edit that leaves UID alone changes no projected key.
func (g *shareGrantStore) UpdateUser(ctx context.Context, user *models.User) error {
	prev, err := g.GetUserByID(ctx, user.ID)
	if err != nil {
		return err
	}
	if err := g.Store.UpdateUser(ctx, user); err != nil {
		return err
	}
	if sameUnixID(prev.UID, user.UID) {
		return nil
	}
	perms, err := g.GetUserSharePermissions(ctx, user.Username)
	if err != nil {
		logger.Warn("Failed to list share grants after a uid change", "user", user.Username, "error", err)
		return nil
	}
	for _, p := range perms {
		g.grantsChanged(ctx, p.ShareName)
	}
	return nil
}

// UpdateGroup is UpdateUser for a group's GID: a group grant projects under the
// group's Unix id just as a user grant does.
func (g *shareGrantStore) UpdateGroup(ctx context.Context, group *models.Group) error {
	prev, err := g.GetGroupByID(ctx, group.ID)
	if err != nil {
		return err
	}
	if err := g.Store.UpdateGroup(ctx, group); err != nil {
		return err
	}
	if sameUnixID(prev.GID, group.GID) {
		return nil
	}
	perms, err := g.GetGroupSharePermissions(ctx, group.Name)
	if err != nil {
		logger.Warn("Failed to list share grants after a gid change", "group", group.Name, "error", err)
		return nil
	}
	for _, p := range perms {
		g.grantsChanged(ctx, p.ShareName)
	}
	return nil
}

// sameUnixID reports whether two optional Unix ids are the same id. Both nil is
// the same id; one nil is a move to or from the id-less state, which changes
// the projected key (the projection falls back to a default id).
func sameUnixID(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
