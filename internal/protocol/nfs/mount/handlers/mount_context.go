package handlers

import (
	"context"
	"net"
)

// MountHandlerContext is the unified context used by all Mount protocol procedure handlers.
//
// This context contains all the information needed to process a Mount request,
// including client identification and authentication credentials.
//
// Similar to the NFS handlers, all mount procedures were using individual context
// types (MountContext, DumpContext, etc.) that were structurally identical.
// Consolidating into a single type:
//   - Reduces code duplication
//   - Simplifies maintenance
//   - Makes handler signatures more consistent
//   - Reduces mental overhead
//
// All mount handlers use the same fields because they all need to:
//   - Check for cancellation (Context)
//   - Identify the client (ClientAddr)
//   - Know the auth method (AuthFlavor)
//   - Access Unix credentials for some operations (UID, GID, GIDs)
//
// The Mount protocol is called before NFS operations to obtain the initial
// file handle, so it doesn't have a Share field (shares are selected via
// the mount path parameter in the MNT procedure).
type MountHandlerContext struct {
	// Context carries cancellation signals and deadlines.
	// Handlers should check this context to abort operations if:
	//   - The server is shutting down
	//   - The client disconnects
	//   - A timeout occurs
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	// Used for logging, access control, and tracking active mounts.
	ClientAddr string

	// AuthFlavor indicates the RPC authentication method.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the user ID from AUTH_UNIX credentials.
	// Nil if AuthFlavor != AUTH_UNIX or credentials not provided.
	// Used for access control decisions in the MNT procedure.
	UID *uint32

	// GID is the primary group ID from AUTH_UNIX credentials.
	// Nil if AuthFlavor != AUTH_UNIX or credentials not provided.
	GID *uint32

	// GIDs is the list of supplementary group IDs from AUTH_UNIX credentials.
	// Empty if AuthFlavor != AUTH_UNIX or credentials not provided.
	GIDs []uint32

	// KerberosEnabled indicates whether RPCSEC_GSS (Kerberos) authentication
	// is supported by the server. When true, the mount response should
	// advertise AUTH_RPCSEC_GSS (6) in addition to AUTH_UNIX (1).
	KerberosEnabled bool
}

// extractClientIP extracts the IP address from a network address string.
// It handles the common pattern of splitting "IP:port" addresses and falling
// back to the full address if parsing fails.
//
// This is a helper to reduce code duplication across mount handlers, as the
// same IP extraction logic was repeated in multiple files.
//
// Parameters:
//   - addr: Network address in "IP:port" format (e.g., "192.168.1.100:1234")
//
// Returns:
//   - string: The IP address without the port, or the full address if parsing fails
//
// Example:
//
//	extractClientIP("192.168.1.100:1234") // Returns "192.168.1.100"
//	extractClientIP("192.168.1.100")      // Returns "192.168.1.100" (no port)
func extractClientIP(addr string) string {
	clientIP, _, err := net.SplitHostPort(addr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		return addr
	}
	return clientIP
}

// isContextCancelled checks if the context has been cancelled.
// This is a convenience helper to simplify the common pattern of checking
// for context cancellation at the start of handler functions.
//
// Returns true if the context is cancelled, false otherwise.
//
// Example usage in a handler:
//
//	if isContextCancelled(ctx) {
//	    logger.Debug("Operation cancelled", "client", ctx.ClientAddr, "error", ctx.Context.Err())
//	    return &Response{Status: ErrorStatus}, ctx.Context.Err()
//	}
func (c *MountHandlerContext) isContextCancelled() bool {
	select {
	case <-c.Context.Done():
		return true
	default:
		return false
	}
}
