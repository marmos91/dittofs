// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/registry"
)

// Default UID/GID used when no ShareIdentityMapping is configured for a user.
const (
	defaultUID = uint32(1000)
	defaultGID = uint32(1000)
)

// BuildAuthContext creates a metadata.AuthContext from SMB handler context.
//
// This bridges the SMB authentication model to the protocol-agnostic
// metadata store authentication context. It maps:
//   - SMB session user → Unix UID/GID (via ShareIdentityMapping)
//   - SMB share permission → metadata store permission checks
//
// Identity Resolution:
// For authenticated users, this function looks up the ShareIdentityMapping
// in the IdentityStore to resolve per-share UID/GID. If no mapping exists,
// it falls back to default values (1000/1000).
func BuildAuthContext(ctx *SMBHandlerContext, reg *registry.Registry) (*metadata.AuthContext, error) {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	// Build identity from SMB session
	if ctx.User != nil {
		// Authenticated user - look up ShareIdentityMapping
		uid, gid := resolveUserIdentity(reg, ctx.User.Username, ctx.ShareName)
		authCtx.Identity.UID = &uid
		authCtx.Identity.GID = &gid
		authCtx.Identity.Username = ctx.User.Username
	} else if ctx.IsGuest {
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

// resolveUserIdentity looks up the ShareIdentityMapping for a user on a share.
// Returns the UID/GID from the mapping, or defaults if no mapping exists.
func resolveUserIdentity(reg *registry.Registry, username, shareName string) (uid, gid uint32) {
	// Default values if no mapping found
	uid = defaultUID
	gid = defaultGID

	if reg == nil || username == "" || shareName == "" {
		return uid, gid
	}

	identityStore := reg.GetIdentityStore()
	if identityStore == nil {
		logger.Debug("No identity store configured, using default UID/GID",
			"username", username, "share", shareName, "uid", uid, "gid", gid)
		return uid, gid
	}

	mapping, err := identityStore.GetShareIdentityMapping(username, shareName)
	if err != nil {
		logger.Debug("Failed to get ShareIdentityMapping, using defaults",
			"username", username, "share", shareName, "error", err)
		return uid, gid
	}

	if mapping != nil {
		uid = mapping.UID
		gid = mapping.GID
		logger.Debug("Resolved ShareIdentityMapping",
			"username", username, "share", shareName, "uid", uid, "gid", gid)
	} else {
		logger.Debug("No ShareIdentityMapping found, using defaults",
			"username", username, "share", shareName, "uid", uid, "gid", gid)
	}

	return uid, gid
}

// BuildAuthContextFromUser creates an AuthContext from a User.
// This is useful when the handler has direct access to a User object.
//
// Identity Resolution:
// For authenticated users, this function looks up the ShareIdentityMapping
// in the IdentityStore to resolve per-share UID/GID. If no mapping exists,
// it falls back to default values (1000/1000).
func BuildAuthContextFromUser(ctx *SMBHandlerContext, user *identity.User, reg *registry.Registry) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	if user != nil {
		// Look up ShareIdentityMapping for the user on this share
		uid, gid := resolveUserIdentity(reg, user.Username, ctx.ShareName)
		authCtx.Identity.UID = &uid
		authCtx.Identity.GID = &gid
		authCtx.Identity.Username = user.Username
	}

	return authCtx
}

// HasWritePermission checks if the SMB context has write permission for the share.
func HasWritePermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == identity.PermissionReadWrite || ctx.Permission == identity.PermissionAdmin
}

// HasReadPermission checks if the SMB context has read permission for the share.
func HasReadPermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == identity.PermissionRead ||
		ctx.Permission == identity.PermissionReadWrite ||
		ctx.Permission == identity.PermissionAdmin
}

// HasAdminPermission checks if the SMB context has admin permission for the share.
func HasAdminPermission(ctx *SMBHandlerContext) bool {
	return ctx.Permission == identity.PermissionAdmin
}
