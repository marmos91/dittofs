// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Default UID/GID used when user has no UID/GID configured.
const (
	defaultUID = uint32(1000)
	defaultGID = uint32(1000)
)

// BuildAuthContext creates a metadata.AuthContext from SMB handler context.
//
// This bridges the SMB authentication model to the protocol-agnostic
// metadata store authentication context. It maps:
//   - SMB session user → Unix UID (from User.UID field)
//   - SMB session user → Unix GID (from user's Group membership)
//   - SMB share permission → metadata store permission checks
//
// Identity Resolution:
// For authenticated users, UID comes from the User model and GID comes from
// the user's group membership (lowest GID is used for best permission matching).
// If not configured, falls back to default values (1000/1000).
func BuildAuthContext(ctx *SMBHandlerContext) (*metadata.AuthContext, error) {
	// Authenticated user - delegate to BuildAuthContextFromUser
	if ctx.User != nil {
		return BuildAuthContextFromUser(ctx, ctx.User), nil
	}

	authCtx := &metadata.AuthContext{
		Context:                ctx.Context,
		ClientAddr:             ctx.ClientAddr,
		LockClientID:           fmt.Sprintf("smb:%d", ctx.SessionID),
		Identity:               &metadata.Identity{},
		BypassTraverseChecking: true,
	}

	if ctx.IsGuest {
		// Guest session - use nobody/nogroup
		guestUID := uint32(65534) // nobody
		guestGID := uint32(65534) // nogroup
		authCtx.Identity.UID = &guestUID
		authCtx.Identity.GID = &guestGID
	} else {
		// Anonymous/null session - use root (for now)
		// This allows basic operations but should be restricted by share permissions
		rootUID := uint32(0)
		rootGID := uint32(0)
		authCtx.Identity.UID = &rootUID
		authCtx.Identity.GID = &rootGID
	}

	// Set share-level permission flags for guest/anonymous sessions
	authCtx.ShareWritable = HasWritePermission(ctx)
	authCtx.ShareReadOnly = ctx.Permission == models.PermissionRead

	return authCtx, nil
}

// getUserIdentity returns the UID/GID for a user.
// UID comes from user.UID field.
// GID comes from the user's group membership (lowest GID for root-level access).
// Falls back to defaults if not configured.
func getUserIdentity(user *models.User) (uid, gid uint32) {
	uid = defaultUID
	gid = defaultGID

	if user == nil {
		return uid, gid
	}

	// Get UID from user
	if user.UID != nil {
		uid = *user.UID
	} else {
		logger.Debug("User has no UID configured, using default",
			"username", user.Username, "uid", uid)
	}

	// Get GID from user's primary group (first group with a GID).
	// This follows Unix semantics where the primary group is used for new file creation.
	gidFound := false
	for _, group := range user.Groups {
		if group.GID != nil {
			gid = *group.GID
			gidFound = true
			break
		}
	}

	if !gidFound {
		logger.Debug("User has no group with GID configured, using default",
			"username", user.Username, "gid", gid)
	}

	return uid, gid
}

// BuildAuthContextFromUser creates an AuthContext from a User.
// This is useful when the handler has direct access to a User object.
//
// Identity Resolution:
// UID comes from User.UID, GID comes from user's group membership.
// Falls back to defaults (1000/1000) if not configured.
//
// Share Permission:
// ShareWritable is set based on the SMB context's share permission.
// This allows users with share-level write permission to bypass file-level
// Unix permission checks.
func BuildAuthContextFromUser(ctx *SMBHandlerContext, user *models.User) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:                ctx.Context,
		ClientAddr:             ctx.ClientAddr,
		LockClientID:           fmt.Sprintf("smb:%d", ctx.SessionID),
		Identity:               &metadata.Identity{},
		BypassTraverseChecking: true,
	}

	if user != nil {
		uid, gid := getUserIdentity(user)
		authCtx.Identity.UID = &uid
		authCtx.Identity.GID = &gid
		authCtx.Identity.Username = user.Username
		if user.SID != "" {
			sid := user.SID
			authCtx.Identity.SID = &sid
		}
		if len(user.GroupSIDs) > 0 {
			authCtx.Identity.GroupSIDs = append([]string(nil), user.GroupSIDs...)
		}
	}

	// Set share-level permission flags
	// ShareWritable allows bypassing file-level permission checks for write operations
	authCtx.ShareWritable = HasWritePermission(ctx)
	authCtx.ShareReadOnly = ctx.Permission == models.PermissionRead

	return authCtx
}

// primeAuthContextFromOpenFile hand-offs the open's recorded session/tree
// identity onto ctx BEFORE BuildAuthContext is called (refs #603). Follow-up
// operations CREATE / READ / WRITE / QUERY_DIRECTORY (the four current
// callers of this helper) arrive keyed only by FileID — the SMB2 dispatcher
// has no user state to prefill ctx.User with. Without this hand-off
// BuildAuthContext takes the ctx.User==nil arm and synthesises a UID-0
// "anonymous/root" identity, which then trips the UID-0 root-bypass at the
// top of metadata permission checks (e.g. ABE filterByAccess) and silently
// grants root.
//
// We also realign ctx.TreeID / ctx.SessionID onto the IDs the open was
// created against. Downstream gates (notably treeHasAccessBasedEnumeration
// in QueryDirectory) read ctx.TreeID directly; if the dispatcher left a
// stale or zero TreeID on ctx, ABE would be decided against the wrong tree.
//
// The sess.User nil-guard on the User assignment is load-bearing: GetSession(0)
// returns the manager's seeded anonymous pre-auth session with User=nil, and
// test fixtures often pre-populate ctx.User without registering a parallel
// session. In both cases clobbering with nil would re-introduce the bug.
// IsGuest, by contrast, MUST be propagated even when sess.User==nil: guest
// sessions are created with User=nil and IsGuest=true (see
// session.NewSession), and the BuildAuthContext guest arm is what maps them
// to UID/GID 65534 instead of root.
func (h *Handler) primeAuthContextFromOpenFile(ctx *SMBHandlerContext, openFile *OpenFile) {
	h.primeAuthContext(ctx, openFile.TreeID, openFile.SessionID)
}

// primeAuthContext is the same as primeAuthContextFromOpenFile but takes raw
// tree/session IDs. CREATE uses this with the dispatcher-provided ctx.TreeID /
// ctx.SessionID because there is no OpenFile yet.
func (h *Handler) primeAuthContext(ctx *SMBHandlerContext, treeID uint32, sessionID uint64) {
	ctx.TreeID = treeID
	ctx.SessionID = sessionID
	if tree, ok := h.GetTree(treeID); ok {
		ctx.ShareName = tree.ShareName
		ctx.Permission = tree.Permission
	}
	if sess, ok := h.GetSession(sessionID); ok && sess != nil {
		// Propagate guest-ness independent of User: guest sessions seed
		// User=nil + IsGuest=true and BuildAuthContext relies on IsGuest
		// to pick the nobody/nogroup (65534) arm instead of root.
		ctx.IsGuest = sess.IsGuest
		if sess.User != nil {
			ctx.User = sess.User
		}
	}
}

// HasWritePermission checks if the SMB context has write permission for the share.
func HasWritePermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == models.PermissionReadWrite || ctx.Permission == models.PermissionAdmin
}

// HasReadPermission checks if the SMB context has read permission for the share.
func HasReadPermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == models.PermissionRead ||
		ctx.Permission == models.PermissionReadWrite ||
		ctx.Permission == models.PermissionAdmin
}

// HasAdminPermission checks if the SMB context has admin permission for the share.
func HasAdminPermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == models.PermissionAdmin
}
