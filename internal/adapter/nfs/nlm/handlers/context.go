// Package handlers provides NLM (Network Lock Manager) procedure handlers.
//
// NLM is the advisory file locking protocol used by NFS clients to coordinate
// byte-range locks across the network. This package implements the server-side
// handlers for NLM v4 procedures.
package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
)

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

	// Version is the negotiated NLM protocol version (1, 3, or 4). It selects
	// the byte-range offset/length wire width when decoding requests and
	// encoding responses: v4 uses 64-bit, v1/v3 use 32-bit. macOS NFSv3 lock
	// clients negotiate v1/v3. See types.IsWideVersion.
	Version uint32

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

	// Data contains the raw request bytes for procedures that need direct access.
	// Some procedures (like FREE_ALL) need to decode request data directly
	// rather than receiving pre-decoded structures.
	Data []byte
}

// Credentials returns what the client presented on the RPC, for the lock
// service to resolve into the share's effective identity. A nil UID means the
// call carried no credentials (AUTH_NULL, or an AUTH_UNIX credential that did
// not parse).
//
// Deliberately NOT an identity: these are unresolved, unsquashed and
// client-supplied, and authorizing against them directly is what let a client
// claiming uid 0 take the root bypass.
func (c *NLMHandlerContext) Credentials() auth.Credentials {
	return auth.Credentials{
		UID:        c.UID,
		GID:        c.GID,
		GIDs:       c.GIDs,
		ClientAddr: c.ClientAddr,
	}
}
