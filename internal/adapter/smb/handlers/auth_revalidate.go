package handlers

import (
	"context"
	"errors"

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
// gate then refuses every command but LOGOFF, CLOSE and LOCK. Trees are
// re-resolved against a user record read here rather than the session's
// snapshot, because permission resolution reads grants and group membership off
// the user object rather than the database: resolving against the snapshot would
// consult exactly the grants that were just revoked. A changed permission
// replaces the tree; one that has dropped to none removes it, so the next
// operation on that tree is refused and a fresh TREE_CONNECT re-decides access.
//
// Nothing here writes to the session's own user field. That field is read
// unlocked on the dispatch path, so publishing a new record from this goroutine
// would be a data race rather than a refresh.
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

	// Sessions that survive, mapped to the record their trees re-resolve
	// against. A surviving guest session maps to a nil record, which is what it
	// authenticated with; a revoked session is absent, so the tree pass leaves
	// it alone.
	surviving := make(map[uint64]*models.User)

	h.SessionManager.RangeSessions(func(sessionID uint64, value any) bool {
		sess, ok := value.(*session.Session)
		if !ok || sess.LoggedOff.Load() {
			return true
		}
		current := sess.User
		if current == nil {
			// Guest and anonymous sessions carry no user record; their access
			// rests on the share default, which the tree pass re-resolves.
			surviving[sessionID] = nil
			return true
		}

		// A directory-resolved principal with no local account is backed by a
		// synthesized record that was never persisted, so it carries no primary
		// key and there is no row to re-read. Its authorization comes from the
		// SID grants the tree pass re-resolves. Looking it up would report the
		// user as missing and retire every AD session on the next unrelated
		// user edit.
		if current.ID == "" {
			surviving[sessionID] = current
			return true
		}

		user, err := userStore.GetUser(ctx, current.Username)
		switch {
		case errors.Is(err, models.ErrUserNotFound):
			logger.Info("SMB session revoked: user deleted",
				"sessionID", sessionID, "username", current.Username)
			h.revokeSession(sess, sessionID)
		case err != nil:
			// A store failure is not evidence that the account went away, and
			// revoking on one would drop every SMB session on a transient
			// outage. The sweep re-runs on the next mutation, and the account
			// is re-checked then.
			logger.Warn("SMB authorization re-check failed, session left intact",
				"sessionID", sessionID, "username", current.Username, "error", err)
		case user == nil || !user.Enabled:
			logger.Info("SMB session revoked: user disabled",
				"sessionID", sessionID, "username", current.Username)
			h.revokeSession(sess, sessionID)
		default:
			surviving[sessionID] = user
		}
		return true
	})

	h.revalidateTrees(ctx, userStore, surviving)
}

// revokeSession retires a session's authorization and completes anything it has
// parked on the server.
//
// The dispatch gate only refuses a request the client sends, but an armed
// CHANGE_NOTIFY is delivered from a timer and consults no session state, so a
// client that arms one and then goes quiet would keep receiving file names from
// a share it has lost. Completing them here is what the expiry path already does
// when it refuses a request.
func (h *Handler) revokeSession(sess *session.Session, sessionID uint64) {
	sess.RevokeAuth()
	h.ExpireSessionNotifies(sessionID)
}

// revalidateTrees re-resolves each surviving session's pinned tree permissions
// against the user records the session pass just read.
func (h *Handler) revalidateTrees(ctx context.Context, userStore models.UserStore, surviving map[uint64]*models.User) {
	type treeUpdate struct {
		treeID     uint32
		sessionID  uint64
		permission models.SharePermission
	}
	var updates []treeUpdate

	h.trees.Range(func(_, value any) bool {
		tree, ok := value.(*TreeConnection)
		if !ok {
			return true
		}
		user, alive := surviving[tree.SessionID]
		if !alive {
			// Either the session was revoked — already refused at dispatch, and
			// its trees go with it on logoff or reconnect — or it is gone.
			return true
		}
		sess, ok := h.GetSession(tree.SessionID)
		if !ok {
			return true
		}
		share, err := h.Registry.GetShare(tree.ShareName)
		if err != nil || share == nil {
			// The share is gone. Its trees are left as they are: a removed share
			// has no permission to re-resolve against, and TREE_CONNECT's own
			// lookup refuses a fresh connect to it.
			return true
		}

		permission, _ := resolveSharePermissionForUser(
			&SMBHandlerContext{Context: ctx},
			sess,
			user,
			share,
			models.ParseSharePermission(share.DefaultPermission),
			userStore,
		)
		permission = capReadOnlyShare(share, permission)
		if permission == tree.Permission {
			return true
		}
		updates = append(updates, treeUpdate{treeID: tree.TreeID, sessionID: tree.SessionID, permission: permission})
		return true
	})

	// Applied outside the Range: sync.Map permits mutation during one, but
	// which entries a walk still visits afterwards is left undefined.
	for _, u := range updates {
		// A permission of none removes the tree rather than being stored on it:
		// downstream read-only checks read none as "not read-only", so pinning
		// it would lift the ceiling instead of closing access.
		if u.permission == models.PermissionNone {
			// Close the opens first, the way TREE_DISCONNECT does. CLOSE is
			// itself a tree-scoped command, so a handle left behind on a
			// removed tree can never be closed by its client, and any byte-range
			// lock it holds would stand against other users until the
			// connection dropped.
			closed := h.CloseAllFilesForTree(ctx, u.treeID, u.sessionID)
			logger.Info("SMB tree removed: share access revoked",
				"treeID", u.treeID, "filesClosed", closed)
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
