// Package handlers provides NLM (Network Lock Manager) procedure handlers.
//
// NLM is the advisory file locking protocol used by NFS clients to coordinate
// byte-range locks across the network. This package implements the server-side
// handlers for NLM v4 procedures.
package handlers

import "context"

// NLMHandlerContext contains context for NLM procedure handlers.
//
// This context is created for each NLM RPC call and passed through to the
// handler methods. It contains the Go context for cancellation/timeout,
// client identification for logging/metrics, and authentication information
// from the RPC call.
type NLMHandlerContext struct {
	// Context is the Go context for cancellation/timeout.
	// Handlers should check this context for cancellation before
	// performing expensive operations.
	Context context.Context

	// ClientAddr is the remote address of the NLM client.
	// Used for logging, metrics, and owner identification.
	ClientAddr string

	// AuthFlavor is the RPC authentication flavor (AUTH_UNIX, AUTH_NULL).
	// Most NLM clients use AUTH_UNIX.
	AuthFlavor uint32

	// UID is the Unix user ID (from AUTH_UNIX, nil if not available).
	// Used for permission checking on lock operations.
	UID *uint32

	// GID is the Unix primary group ID (from AUTH_UNIX, nil if not available).
	GID *uint32

	// GIDs is the list of supplementary group IDs.
	GIDs []uint32
}
