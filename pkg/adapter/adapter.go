// Package adapter provides protocol adapter interfaces for DittoFS.
//
// Each protocol adapter (NFS, SMB) embeds BaseAdapter for shared lifecycle,
// listener and connection management, and implements Adapter to plug into
// DittoServer. Authentication mechanisms themselves live in pkg/auth.
package adapter

import (
	"context"

	"github.com/marmos91/dittofs/pkg/health"
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
//  2. Runtime injection: SetRuntime() provides the shared runtime
//  3. Startup: Serve() starts the protocol server and blocks until shutdown
//  4. Shutdown: Stop() initiates graceful shutdown with timeout
//
// Thread safety:
// Implementations must be safe for concurrent use. SetRuntime() is called
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
	// Called exactly once by Runtime before Serve() is called.
	// Implementations should store the runtime for use during operation to
	// resolve shares and access their corresponding stores.
	//
	// The parameter is typed as any to satisfy the adapters.RuntimeSetter
	// interface, which cannot import *runtime.Runtime without creating an
	// import cycle. Implementations type-assert to *runtime.Runtime.
	//
	// Parameters:
	//   - rt: Runtime containing all metadata stores, content stores, and shares
	//
	// Thread safety:
	// Called before Serve(), no synchronization needed.
	SetRuntime(rt any)

	// Stop initiates graceful shutdown of the protocol server.
	//
	// May be called concurrently with Serve() during DittoServer shutdown.
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

	// Healthcheck returns the adapter's current health as a structured
	// [health.Report] and satisfies [health.Checker]. The API layer wraps
	// this in a [health.CachedChecker] and serves it from
	// /adapter/{name}/status.
	//
	// Implementations should derive status from cheap, already-tracked
	// signals — never run a fresh probe per call. Mapping:
	//
	//   - [health.StatusDisabled] when the adapter is configured off
	//     (config.Enabled == false). Operators turned it off; nothing
	//     to probe.
	//   - [health.StatusUnknown] when the adapter exists but has not
	//     yet established a known running state — most commonly the
	//     pre-Serve startup window, but also any state the
	//     implementation cannot positively classify (canceled probe
	//     context, BaseAdapter without failed-start tracking, etc.).
	//   - [health.StatusUnhealthy] when the implementation explicitly
	//     tracks a failed lifecycle state — for example a stopped or
	//     crashed adapter, or one whose listener died after running.
	//     [BaseAdapter] tracks no such state, so it never returns
	//     Unhealthy for "configured-on but not started"; adding a
	//     serveAttempted / lastServeErr field would lift that
	//     limitation.
	//   - [health.StatusDegraded] when running but reporting recent
	//     errors above whatever per-protocol threshold the
	//     implementation tracks. [BaseAdapter] tracks no such
	//     instrumentation either; only adapters that already had a
	//     degraded-detection mechanism can return this today.
	//   - [health.StatusHealthy] when running with no recent issues.
	Healthcheck(ctx context.Context) health.Report
}

// SMBOpenFilesProviderKey is the Runtime adapter provider key under which the
// SMB handler registers itself as an open-file enumerator for the block-GC
// open-handle hold. The NFSv4 state manager needs no dedicated key: it is
// already registered under "nfs" and the runtime discovers open-file
// enumerators structurally across all registered adapter providers.
const SMBOpenFilesProviderKey = "smb_open_files"

// OplockBreaker provides cross-protocol oplock break coordination.
// Adapters holding opportunistic locks register an implementation via
// Runtime.SetAdapterProvider("oplock_breaker", breaker).
// Other adapters retrieve and call it to trigger breaks before conflicting operations.
//
// This generic interface decouples protocol adapters: NFS handlers don't
// import SMB packages and vice versa. The SMBOplockBreaker in the SMB lease
// package satisfies this interface via the shared LockManager.
type OplockBreaker interface {
	// CheckAndBreakForWrite triggers lease break for write-conflicting oplocks.
	// Returns nil if no break needed, ErrLeaseBreakPending if break initiated.
	CheckAndBreakForWrite(ctx context.Context, fileHandle lock.FileHandle) error

	// CheckAndBreakForRead triggers lease break for read-conflicting oplocks (Write leases).
	CheckAndBreakForRead(ctx context.Context, fileHandle lock.FileHandle) error

	// CheckAndBreakForDelete triggers lease break for Handle leases before deletion.
	CheckAndBreakForDelete(ctx context.Context, fileHandle lock.FileHandle) error
}
