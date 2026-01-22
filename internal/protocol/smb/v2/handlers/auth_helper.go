// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
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
//   - SMB session user → Unix UID/GID (from User.UID/GID fields)
//   - SMB share permission → metadata store permission checks
//
// Identity Resolution:
// For authenticated users, this function uses the UID/GID fields from the User.
// If the user has no UID/GID configured, it falls back to default values (1000/1000).
func BuildAuthContext(ctx *SMBHandlerContext) (*metadata.AuthContext, error) {
	// Authenticated user - delegate to BuildAuthContextFromUser
	if ctx.User != nil {
		return BuildAuthContextFromUser(ctx, ctx.User), nil
	}

	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
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

	return authCtx, nil
}

// getUserIdentity returns the UID/GID for a user.
// Returns the user's configured UID/GID, or defaults if not set.
func getUserIdentity(user *models.User) (uid, gid uint32) {
	uid = defaultUID
	gid = defaultGID

	if user == nil {
		return uid, gid
	}

	if user.UID != nil {
		uid = *user.UID
	} else {
		logger.Debug("User has no UID configured, using default",
			"username", user.Username, "uid", uid)
	}

	if user.GID != nil {
		gid = *user.GID
	} else {
		logger.Debug("User has no GID configured, using default",
			"username", user.Username, "gid", gid)
	}

	return uid, gid
}

// BuildAuthContextFromUser creates an AuthContext from a User.
// This is useful when the handler has direct access to a User object.
//
// Identity Resolution:
// Uses the UID/GID fields from the User. If not set, falls back to defaults.
func BuildAuthContextFromUser(ctx *SMBHandlerContext, user *models.User) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	if user != nil {
		uid, gid := getUserIdentity(user)
		authCtx.Identity.UID = &uid
		authCtx.Identity.GID = &gid
		authCtx.Identity.Username = user.Username
	}

	return authCtx
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
