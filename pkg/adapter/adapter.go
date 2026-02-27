package adapter

import (
	"context"

	"github.com/marmos91/dittofs/pkg/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Adapter represents a protocol-specific server adapter that can be managed by DittoServer.
//
// Each adapter implements a specific file sharing protocol (e.g., NFS, SMB)
// and provides a unified interface for lifecycle management. All adapters share the
// same metadata and content repositories, ensuring consistency across protocols.
//
// Lifecycle:
//  1. Creation: Adapter is created with protocol-specific configuration
//  2. Repository injection: SetStores() provides shared backend access
//  3. Startup: Serve() starts the protocol server and blocks until shutdown
//  4. Shutdown: Stop() initiates graceful shutdown with timeout
//
// Thread safety:
// Implementations must be safe for concurrent use. SetStores() is called
// once before Serve(), but Stop() may be called concurrently with Serve().
type Adapter interface {
	// Serve starts the protocol server and blocks until the context is cancelled
	// or an unrecoverable error occurs.
	//
	// When the context is cancelled, Serve must initiate graceful shutdown:
	//   - Stop accepting new connections
	//   - Wait for active operations to complete (with timeout)
	//   - Clean up resources
	//   - Return context.Canceled or nil
	//
	// If Serve returns before context cancellation, DittoServer treats it as
	// a fatal error and stops all other adapters.
	//
	// Parameters:
	//   - ctx: Controls the server lifecycle. Cancellation triggers shutdown.
	//
	// Returns:
	//   - nil on graceful shutdown
	//   - context.Canceled if cancelled via context
	//   - error if startup fails or shutdown is not graceful
	Serve(ctx context.Context) error

	// SetRuntime injects the shared Runtime containing all stores and shares.
	//
	// This method is called exactly once by Runtime before Serve() is called.
	// Implementations should store the runtime for use during operation to
	// resolve shares and access their corresponding stores.
	//
	// Parameters:
	//   - rt: Runtime containing all metadata stores, content stores, and shares
	//
	// Thread safety:
	// Called before Serve(), no synchronization needed.
	SetRuntime(rt *runtime.Runtime)

	// Stop initiates graceful shutdown of the protocol server.
	//
	// This method may be called concurrently with Serve() during DittoServer shutdown.
	// Implementations must:
	//   - Be safe to call multiple times (idempotent)
	//   - Be safe to call concurrently with Serve()
	//   - Respect the context timeout for shutdown operations
	//   - Clean up all resources (listeners, connections, goroutines)
	//
	// Parameters:
	//   - ctx: Controls the shutdown timeout. When cancelled, force cleanup.
	//
	// Returns:
	//   - nil if shutdown completed successfully
	//   - error if shutdown exceeded timeout or encountered errors
	Stop(ctx context.Context) error

	// Protocol returns the human-readable protocol name for logging and metrics.
	//
	// Examples: "NFS", "SMB", "WebDAV", "FTP"
	//
	// The returned value should be constant for the lifecycle of the adapter.
	Protocol() string

	// Port returns the TCP/UDP port the adapter is listening on.
	//
	// This is used for logging and health checks. The returned value should
	// be constant after Serve() is called.
	//
	// Returns 0 if the adapter has not yet started or uses dynamic port allocation.
	Port() int

	// MapError translates a domain error into a protocol-specific ProtocolError.
	//
	// Each adapter must implement this method to convert domain errors (e.g.,
	// metadata.ErrNoEntity, payload.ErrContentNotFound) into the appropriate
	// wire-format error code for the protocol (NFS status codes, NTSTATUS, etc.).
	//
	// Returns nil if the error cannot be mapped to a protocol-specific error.
	MapError(err error) ProtocolError
}

// OplockBreakerProviderKey is the Runtime adapter provider key for the OplockBreaker.
// Used with Runtime.SetAdapterProvider / GetAdapterProvider to register and retrieve
// the cross-protocol oplock breaker without import cycles between protocol packages.
const OplockBreakerProviderKey = "oplock_breaker"

// OplockBreaker provides cross-protocol oplock break coordination.
// Adapters holding opportunistic locks register an implementation via
// Runtime.SetAdapterProvider("oplock_breaker", breaker).
// Other adapters retrieve and call it to trigger breaks before conflicting operations.
//
// This generic interface decouples protocol adapters: NFS handlers don't
// import SMB packages and vice versa. The OplockManager in the SMB adapter
// satisfies this interface.
type OplockBreaker interface {
	// CheckAndBreakForWrite triggers lease break for write-conflicting oplocks.
	// Returns nil if no break needed, ErrLeaseBreakPending if break initiated.
	CheckAndBreakForWrite(ctx context.Context, fileHandle lock.FileHandle) error

	// CheckAndBreakForRead triggers lease break for read-conflicting oplocks (Write leases).
	CheckAndBreakForRead(ctx context.Context, fileHandle lock.FileHandle) error

	// CheckAndBreakForDelete triggers lease break for Handle leases before deletion.
	CheckAndBreakForDelete(ctx context.Context, fileHandle lock.FileHandle) error
}

// IdentityMappingAdapter extends Adapter with protocol-specific identity mapping.
//
// Adapters that support authentication implement this interface to convert
// auth.AuthResult values into protocol-specific identities. This allows the
// runtime to perform identity mapping uniformly while each adapter handles
// the protocol-specific translation logic.
//
//   - NFS: Maps AUTH_UNIX UIDs, AUTH_NULL, RPCSEC_GSS Kerberos principals
//   - SMB: Maps NTLM sessions, SPNEGO/Kerberos negotiations
//
// All adapters embedding BaseAdapter satisfy this interface because BaseAdapter
// provides a default MapIdentity stub that returns an error. The runtime detects
// non-supporting adapters by checking for a non-nil error from MapIdentity
// rather than by type assertion.
type IdentityMappingAdapter interface {
	Adapter
	auth.IdentityMapper
}
