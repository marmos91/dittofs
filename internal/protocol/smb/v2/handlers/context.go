// Package handlers provides SMB2 command handlers and session management.
package handlers

import "context"

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

	// Username for authenticated sessions (Phase 2+)
	Username string
	Domain   string
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
