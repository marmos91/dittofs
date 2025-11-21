package handlers

import "context"

// NFSHandlerContext is the unified context used by all NFS v3 procedure handlers.
//
// This context contains all the information needed to process an NFS request,
// including client identification, authentication credentials, and the share being accessed.
//
// Previously, each handler had its own context type (ReadContext, WriteContext, etc.),
// but they were all structurally identical. Consolidating into a single type:
//   - Reduces code duplication (eliminated 16+ duplicate struct definitions)
//   - Simplifies maintenance (one place to add new fields)
//   - Makes handler signatures more consistent
//   - Reduces mental overhead when reading handler code
//
// All handlers use the same fields because they all need to:
//   - Check for cancellation (Context)
//   - Identify the client (ClientAddr)
//   - Know the auth method (AuthFlavor)
//   - Perform permission checks (UID, GID, GIDs)
//   - Route to the correct share (Share)
//
// Not all handlers strictly need all fields (e.g., NULL doesn't need UID/GID),
// but the memory overhead is negligible and the simplification is significant.
type NFSHandlerContext struct {
	// Context carries cancellation signals and deadlines.
	// Handlers should check this context to abort operations if:
	//   - The server is shutting down
	//   - The client disconnects
	//   - A timeout occurs
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	// Used for logging and access control decisions.
	ClientAddr string

	// Share is the name of the share being accessed (e.g., "/export").
	// Extracted at the connection layer from the file handle.
	// Used for routing operations to the correct metadata and content stores.
	Share string

	// AuthFlavor indicates the RPC authentication method.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	//   - 2: AUTH_SHORT (short-hand authentication)
	//   - 3: AUTH_DH (Diffie-Hellman authentication)
	AuthFlavor uint32

	// UID is the user ID from AUTH_UNIX credentials.
	// Nil if AuthFlavor != AUTH_UNIX or credentials not provided.
	// Used for permission checks via the metadata store.
	UID *uint32

	// GID is the primary group ID from AUTH_UNIX credentials.
	// Nil if AuthFlavor != AUTH_UNIX or credentials not provided.
	// Used for permission checks via the metadata store.
	GID *uint32

	// GIDs is the list of supplementary group IDs from AUTH_UNIX credentials.
	// Empty if AuthFlavor != AUTH_UNIX or credentials not provided.
	// Used for group-based permission checks via the metadata store.
	GIDs []uint32
}

// GetContext returns the Go context for cancellation handling.
func (c *NFSHandlerContext) GetContext() context.Context {
	return c.Context
}

// GetClientAddr returns the client's network address.
func (c *NFSHandlerContext) GetClientAddr() string {
	return c.ClientAddr
}

// GetAuthFlavor returns the RPC authentication flavor.
func (c *NFSHandlerContext) GetAuthFlavor() uint32 {
	return c.AuthFlavor
}

// GetUID returns the Unix user ID from AUTH_UNIX credentials.
func (c *NFSHandlerContext) GetUID() *uint32 {
	return c.UID
}

// GetGID returns the Unix primary group ID from AUTH_UNIX credentials.
func (c *NFSHandlerContext) GetGID() *uint32 {
	return c.GID
}

// GetGIDs returns the Unix supplementary group IDs from AUTH_UNIX credentials.
func (c *NFSHandlerContext) GetGIDs() []uint32 {
	return c.GIDs
}

// GetShare returns the share name being accessed.
func (c *NFSHandlerContext) GetShare() string {
	return c.Share
}
