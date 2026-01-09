// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BuildAuthContext creates a metadata.AuthContext from SMB handler context.
//
// This bridges the SMB authentication model to the protocol-agnostic
// metadata store authentication context. It maps:
//   - SMB session user → Unix UID/GID
//   - SMB share permission → metadata store permission checks
//
// For authenticated users, UID/GID come from the identity.User.
// For guest sessions, a default guest identity is used.
func BuildAuthContext(ctx *SMBHandlerContext, _ *registry.Registry) (*metadata.AuthContext, error) {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	// Build identity from SMB session user
	if ctx.User != nil {
		// Authenticated user - use their Unix identity
		authCtx.Identity.UID = &ctx.User.UID
		authCtx.Identity.GID = &ctx.User.GID
		// Note: Supplementary groups would need to be resolved from UserStore
		// For now, we only use primary UID/GID
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
func BuildAuthContextFromUser(ctx *SMBHandlerContext, user *identity.User) *metadata.AuthContext {
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context,
		ClientAddr: ctx.ClientAddr,
		Identity:   &metadata.Identity{},
	}

	if user != nil {
		authCtx.Identity.UID = &user.UID
		authCtx.Identity.GID = &user.GID
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
