package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// RevalidateAuthorization re-runs the authorization decisions that SESSION_SETUP
// and TREE_CONNECT resolved once, and retires the ones that no longer hold.
//
// SMB decides authorization at establishment and stores the answer: the user
// record on the session, the resolved share permission on the tree. Neither is
// re-read per operation, so a control-plane change — disabling a user, deleting
// one, revoking or downgrading a share grant — reaches an established connection
// through nothing. There is no TTL behind either the way the NFSv3 auth cache
// has one, so the staleness is unbounded and the change has to be pushed in.
//
// A session whose user has been deleted or disabled is revoked, and the dispatch
// gate then refuses every command but LOGOFF, CLOSE and LOCK. A session whose
// user is still valid has that record refreshed, because permission resolution
// reads grants and group memberships off the user object rather than the
// database — leaving the snapshot in place would re-resolve against exactly the
// grants that were just revoked.
//
// Trees are then re-resolved against the refreshed records. A changed permission
// replaces the tree; one that has dropped to none removes it, so the next
// operation on that tree is refused and a fresh TREE_CONNECT is what re-decides
// access.
//
// Runs off the request path, from the adapter's auth-cache-invalidate
// subscription.
//
// ponytail: walks every session and every tree on each invalidation rather than
// indexing them by username. Both tables are per-connection state bounded by the
// client count, and the walk runs only on a control-plane mutation, not per
// operation; index them if a deployment ever makes that walk visible.
func (h *Handler) RevalidateAuthorization(ctx context.Context) {
	userStore := h.Registry.GetUserStore()
	if userStore == nil {
		return
	}

	h.SessionManager.RangeSessions(func(sessionID uint64, value any) bool {
		sess, ok := value.(*session.Session)
		if !ok || sess == nil || sess.LoggedOff.Load() {
			return true
		}
		// Guest and anonymous sessions carry no user record, so there is no
		// grant to re-resolve and nothing to revoke.
		current := sess.User
		if current == nil {
			return true
		}

		user, err := userStore.GetUser(ctx, current.Username)
		switch {
		case err != nil || user == nil:
			// A lookup failure is treated as revocation rather than as a
			// reason to keep the session: the store is the authority on
			// whether the account still exists, and holding authorization
			// open across an outage is the failure mode this exists to close.
			logger.Info("SMB session revoked: user record no longer readable",
				"sessionID", sessionID, "username", current.Username, "error", err)
			sess.RevokeAuth()
		case !user.Enabled:
			logger.Info("SMB session revoked: user disabled",
				"sessionID", sessionID, "username", current.Username)
			sess.RevokeAuth()
		default:
			sess.RefreshUser(user)
		}
		return true
	})

	h.revalidateTrees(ctx)
}

// revalidateTrees re-resolves each tree's pinned share permission against the
// session's refreshed user record.
func (h *Handler) revalidateTrees(ctx context.Context) {
	type treeUpdate struct {
		treeID     uint32
		permission models.SharePermission
		remove     bool
	}
	var updates []treeUpdate

	h.trees.Range(func(_, value any) bool {
		tree, ok := value.(*TreeConnection)
		if !ok || tree == nil {
			return true
		}
		sess, ok := h.GetSession(tree.SessionID)
		if !ok || sess == nil {
			return true
		}
		// A revoked session is already refused at dispatch, and its trees go
		// with it when the client logs off or reconnects.
		if sess.AuthRevoked() {
			return true
		}
		share, err := h.Registry.GetShare(tree.ShareName)
		if err != nil || share == nil {
			// The share is gone; TREE_CONNECT's own lookup would refuse it and
			// the share-change subscription owns that teardown.
			return true
		}

		permission, _ := resolveSharePermission(
			&SMBHandlerContext{Context: ctx},
			sess,
			share,
			models.ParseSharePermission(share.DefaultPermission),
			h.Registry.GetUserStore(),
		)
		// Mirror TREE_CONNECT's read-only cap, or a re-resolution could hand
		// back write access the share itself forbids.
		if share.ReadOnly && (permission == models.PermissionReadWrite || permission == models.PermissionAdmin) {
			permission = models.PermissionRead
		}
		if permission == tree.Permission {
			return true
		}
		updates = append(updates, treeUpdate{
			treeID:     tree.TreeID,
			permission: permission,
			remove:     permission == models.PermissionNone,
		})
		return true
	})

	// Applied outside the Range: sync.Map forbids neither, but mutating the
	// table being walked makes which entries the walk still visits undefined.
	for _, u := range updates {
		if u.remove {
			logger.Info("SMB tree removed: share access revoked",
				"treeID", u.treeID)
			h.DeleteTree(u.treeID)
			continue
		}
		tree, ok := h.GetTree(u.treeID)
		if !ok {
			continue
		}
		// TreeConnection is written once at TREE_CONNECT and only read after,
		// so the permission change is published by swapping in a fresh copy
		// rather than by assigning through the pointer every in-flight request
		// is already reading. A request that loaded the old pointer completes
		// against the old permission; the next one sees the new.
		updated := *tree
		updated.Permission = u.permission
		logger.Info("SMB tree permission re-resolved",
			"treeID", u.treeID, "share", updated.ShareName,
			"from", tree.Permission, "to", u.permission)
		h.StoreTree(&updated)
	}
}
