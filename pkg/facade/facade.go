package server

import (
	"context"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/metadata"
)

// Facade represents a protocol-specific server facade that can be managed by DittoServer.
//
// Each facade implements a specific file sharing protocol (e.g., NFS, SMB)
// and provides a unified interface for lifecycle management. All facades share the
// same metadata and content repositories, ensuring consistency across protocols.
//
// Lifecycle:
//  1. Creation: Facade is created with protocol-specific configuration
//  2. Repository injection: SetRepositories() provides shared backend access
//  3. Startup: Serve() starts the protocol server and blocks until shutdown
//  4. Shutdown: Stop() initiates graceful shutdown with timeout
//
// Thread safety:
// Implementations must be safe for concurrent use. SetRepositories() is called
// once before Serve(), but Stop() may be called concurrently with Serve().
type Facade interface {
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
	// a fatal error and stops all other facades.
	//
	// Parameters:
	//   - ctx: Controls the server lifecycle. Cancellation triggers shutdown.
	//
	// Returns:
	//   - nil on graceful shutdown
	//   - context.Canceled if cancelled via context
	//   - error if startup fails or shutdown is not graceful
	Serve(ctx context.Context) error

	// SetRepositories injects the shared metadata and content repositories.
	//
	// This method is called exactly once by DittoServer before Serve() is called.
	// Implementations should store the repositories for use during operation.
	//
	// Parameters:
	//   - metadata: Repository for file system metadata (directories, permissions, etc.)
	//   - content: Repository for file content (data blocks)
	//
	// Thread safety:
	// Called before Serve(), no synchronization needed.
	SetRepositories(metadata metadata.Repository, content content.Repository)

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
	// The returned value should be constant for the lifecycle of the facade.
	Protocol() string

	// Port returns the TCP/UDP port the facade is listening on.
	//
	// This is used for logging and health checks. The returned value should
	// be constant after Serve() is called.
	//
	// Returns 0 if the facade has not yet started or uses dynamic port allocation.
	Port() int
}
