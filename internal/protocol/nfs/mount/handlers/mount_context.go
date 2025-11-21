package handlers

import "context"

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
}
