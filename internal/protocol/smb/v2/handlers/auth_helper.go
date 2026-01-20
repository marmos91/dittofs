// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/registry"
)

// BuildAuthContext creates a metadata.AuthContext from SMB handler context.
//
// This bridges the SMB authentication model to the protocol-agnostic
// metadata store authentication context. It maps:
//   - SMB session user → Unix UID/GID (via ShareIdentityMapping)
//   - SMB share permission → metadata store permission checks
//
// NOTE: In the new identity model, users don't have global UID/GID.
// Instead, Unix identity is resolved per-share via ShareIdentityMapping.
// Until IdentityStore integration is complete, authenticated users use
// a default UID/GID (1000/1000).
//
// TODO: Integrate with IdentityStore to resolve ShareIdentityMapping for the current share.
func BuildAuthContext(ctx *SMBHandlerContext, _ *registry.Registry) (*metadata.AuthContext, error) {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	// Build identity from SMB session
	// NOTE: Users no longer have direct UID/GID fields.
	// Unix identity is now per-share (via ShareIdentityMapping).
	// For now, we use default values until IdentityStore integration is complete.
	if ctx.User != nil {
		// Authenticated user - use default Unix identity
		// TODO: Look up ShareIdentityMapping for ctx.ShareName to get proper UID/GID
		defaultUID := uint32(1000)
		defaultGID := uint32(1000)
		authCtx.Identity.UID = &defaultUID
		authCtx.Identity.GID = &defaultGID
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

// BuildAuthContextFromUser creates an AuthContext from a User.
// This is useful when the handler has direct access to a User object.
//
// NOTE: In the new identity model, users don't have global UID/GID.
// This function uses default values until ShareIdentityMapping integration is complete.
func BuildAuthContextFromUser(ctx *SMBHandlerContext, user *identity.User) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	if user != nil {
		// Users no longer have direct UID/GID - use defaults
		// TODO: Look up ShareIdentityMapping for ctx.ShareName
		defaultUID := uint32(1000)
		defaultGID := uint32(1000)
		authCtx.Identity.UID = &defaultUID
		authCtx.Identity.GID = &defaultGID
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
