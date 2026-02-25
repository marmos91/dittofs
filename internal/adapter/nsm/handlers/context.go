// Package handlers provides NSM (Network Status Monitor) protocol handlers.
//
// NSM is the crash recovery protocol used by NLM (Network Lock Manager) clients
// to detect server crashes and reclaim locks. This package implements the
// server-side handlers for NSM procedures.
package handlers

import "context"

// NSMHandlerContext contains context for NSM procedure handlers.
//
// This context is passed to all NSM procedure handlers and contains
// information about the requesting client and the request context.
type NSMHandlerContext struct {
	// Context is the Go context for cancellation and timeouts.
	// Handlers should check ctx.Done() during long operations.
	Context context.Context

	// ClientAddr is the remote client IP address.
	// Used for logging and registration tracking.
	ClientAddr string

	// ClientName is the client hostname (from RPC auth or request).
	// May be empty if not provided in the request.
	ClientName string

	// AuthFlavor is the RPC authentication flavor (AUTH_NULL, AUTH_UNIX, etc.).
	AuthFlavor uint32

	// UID is the user ID from AUTH_UNIX credentials, if present.
	UID *uint32

	// GID is the primary group ID from AUTH_UNIX credentials, if present.
	GID *uint32

	// GIDs contains supplementary group IDs from AUTH_UNIX, if present.
	GIDs []uint32
}
