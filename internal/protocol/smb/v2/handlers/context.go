// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"context"

	"github.com/marmos91/dittofs/pkg/identity"
)

// SMBHandlerContext carries request context through handlers
type SMBHandlerContext struct {
	// Context for cancellation and deadlines
	Context context.Context

	// ClientAddr is the remote address of the client
	ClientAddr string

	// SessionID from the request (0 before SESSION_SETUP completes)
	SessionID uint64

	// TreeID from the request (0 before TREE_CONNECT)
	TreeID uint32

	// MessageID from the request header
	MessageID uint64

	// ShareName resolved from TreeID (populated after TREE_CONNECT)
	ShareName string

	// IsGuest indicates guest/anonymous session
	IsGuest bool

	// Username for authenticated sessions
	Username string
	Domain   string

	// User is the authenticated DittoFS user (nil for guest sessions)
	// This is set from the session during request handling.
	User *identity.User

	// Permission is the user's permission level for the current share
	// This is resolved during TREE_CONNECT and used for access control.
	Permission identity.SharePermission
}

// NewSMBHandlerContext creates a new context from request parameters
func NewSMBHandlerContext(ctx context.Context, clientAddr string, sessionID uint64, treeID uint32, messageID uint64) *SMBHandlerContext {
	return &SMBHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		SessionID:  sessionID,
		TreeID:     treeID,
		MessageID:  messageID,
	}
}

// WithUser returns a copy of the context with user identity populated
func (c *SMBHandlerContext) WithUser(user *identity.User, permission identity.SharePermission) *SMBHandlerContext {
	newCtx := *c
	newCtx.User = user
	newCtx.Permission = permission
	if user != nil {
		newCtx.Username = user.Username
		newCtx.IsGuest = false
	}
	return &newCtx
}
