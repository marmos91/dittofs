package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/facade"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// DittoServer manages the lifecycle of multiple protocol facades that share
// common metadata and content repositories.
//
// Architecture:
// DittoServer implements a facade pattern where different file sharing protocols
// (NFS, SMB, WebDAV, etc.) are represented as Facade implementations. All facades
// share the same backend repositories, providing a unified view of the file system
// across all protocols.
//
// Lifecycle:
//  1. Creation: New() with repositories
//  2. Registration: AddFacade() for each protocol
//  3. Startup: Serve() starts all facades concurrently
//  4. Shutdown: Context cancellation triggers graceful shutdown of all facades
//
// Thread safety:
// DittoServer is safe for concurrent use. AddFacade() may be called concurrently
// with other methods. Serve() should only be called once per server instance.
//
// Example usage:
//
//	server := New(metadataRepo, contentRepo)
//	server.AddFacade(nfs.New(nfsConfig)).
//	       AddFacade(smb.New(smbConfig))
//
//	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
//	defer cancel()
//
//	if err := server.Serve(ctx); err != nil && err != context.Canceled {
//	    log.Fatal(err)
//	}
type DittoServer struct {
	// metadata is the shared metadata repository for all facades
	metadata metadata.Repository

	// content is the shared content repository for all facades
	content content.Repository

	// facades contains all registered protocol facades
	facades []facade.Facade

	// mu protects the facades slice and serving flag
	mu sync.RWMutex

	// serveOnce ensures Serve() is only called once
	serveOnce sync.Once

	// served indicates whether Serve() has been called
	// Protected by serveOnce, no additional locking needed
	served bool
}

// New creates a new DittoServer with the provided repositories.
//
// The repositories are shared across all facades added to this server, ensuring
// that file system operations are consistent regardless of which protocol is used
// to access the data.
//
// Parameters:
//   - metadata: Repository for file system metadata operations (required)
//   - content: Repository for file content operations (required)
//
// Returns a configured but not yet started DittoServer. Call AddFacade() to
// register protocols, then Serve() to start the server.
//
// Panics if either repository is nil (indicates programmer error).
func New(metadata metadata.Repository, content content.Repository) *DittoServer {
	if metadata == nil {
		panic("metadata repository cannot be nil")
	}
	if content == nil {
		panic("content repository cannot be nil")
	}

	return &DittoServer{
		metadata: metadata,
		content:  content,
		facades:  make([]facade.Facade, 0, 4), // Pre-allocate for common case of 2-4 facades
	}
}

// AddFacade registers a new protocol facade with the server.
//
// This method injects the shared repositories into the facade and adds it to the
// list of facades that will be started when Serve() is called.
//
// AddFacade() may be called multiple times to register different protocol facades.
// Each facade must implement a different protocol or listen on a different port.
// Duplicate protocols or port conflicts are detected and return an error.
//
// Parameters:
//   - facade: The protocol facade to register (must not be nil)
//
// Returns:
//   - The server instance to allow method chaining
//   - An error if the facade is invalid or conflicts with an existing facade
//
// Panics if:
//   - facade is nil (programmer error)
//   - Serve() has already been called (server is running)
//
// Thread safety:
// Safe to call concurrently from multiple goroutines before Serve() is called.
//
// Example:
//
//	if err := server.AddFacade(nfs.New(nfsConfig)); err != nil {
//	    log.Fatal(err)
//	}
//	if err := server.AddFacade(smb.New(smbConfig)); err != nil {
//	    log.Fatal(err)
//	}
func (s *DittoServer) AddFacade(f facade.Facade) error {
	if f == nil {
		panic("facade cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if Serve() has been called
	if s.served {
		panic("cannot add facade after Serve() has been called")
	}

	protocol := f.Protocol()
	port := f.Port()

	// Validate no duplicate protocols
	for _, existing := range s.facades {
		if existing.Protocol() == protocol {
			return fmt.Errorf("facade for protocol %s already registered", protocol)
		}
	}

	// Validate no port conflicts
	for _, existing := range s.facades {
		if existing.Port() == port {
			return fmt.Errorf("port %d already in use by %s facade",
				port, existing.Protocol())
		}
	}

	// Inject shared repositories
	f.SetRepositories(s.metadata, s.content)

	// Register the facade
	s.facades = append(s.facades, f)

	logger.Info("Registered %s facade on port %d", protocol, port)

	return nil
}

// Serve starts all registered facades and blocks until the context is cancelled
// or a facade fails to start.
//
// Serve() orchestrates the lifecycle of all facades:
//  1. Validates that at least one facade is registered
//  2. Starts all facades concurrently in separate goroutines
//  3. Monitors for context cancellation or facade failures
//  4. On shutdown signal: stops all facades in reverse order
//  5. Waits for all facades to complete shutdown
//
// Shutdown behavior:
// When the context is cancelled or a facade fails:
//   - All facades receive Stop() calls in reverse registration order
//   - Each facade has 30 seconds (configurable via stopTimeout) to shut down
//   - Facades are stopped concurrently after the Stop() calls are issued
//   - Serve() waits for all facades to complete before returning
//
// Error handling:
//   - If any facade fails to start: stops all already-started facades and returns error
//   - If context is cancelled: initiates graceful shutdown and returns context.Canceled
//   - If any facade fails during operation: stops all facades and returns the error
//
// Parameters:
//   - ctx: Controls server lifecycle. Cancellation triggers graceful shutdown.
//
// Returns:
//   - nil on successful graceful shutdown
//   - context.Canceled if shutdown was triggered by context cancellation
//   - error if startup failed or a facade encountered an error
//
// Panics if Serve() is called more than once on the same DittoServer instance.
//
// Thread safety:
// Serve() must only be called once. Calling it multiple times will panic.
// AddFacade() must not be called after Serve() is called.
func (s *DittoServer) Serve(ctx context.Context) error {
	var err error

	s.serveOnce.Do(func() {
		s.served = true
		err = s.serve(ctx)
	})

	// If Do() was a no-op (second call), panic
	// This is safe because served is only set inside Do()
	if !s.served {
		panic("impossible state: Serve() completed but served flag not set")
	}

	// On second call, Do() is a no-op and err remains nil
	// Check if this is actually a duplicate call
	if err == nil {
		// If we got here and err is nil, either:
		// 1. First call and serve() returned nil (valid)
		// 2. Second call and Do() was no-op (invalid - panic)
		// We can't distinguish these cases reliably, so use a different approach
		panic("Serve() has already been called on this server instance")
	}

	return err
}

// serve is the internal implementation of Serve().
// Separated to allow serveOnce protection.
func (s *DittoServer) serve(ctx context.Context) error {
	// Get snapshot of facades under lock
	s.mu.Lock()
	if len(s.facades) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("no facades registered; call AddFacade() before Serve()")
	}
	facades := make([]facade.Facade, len(s.facades))
	copy(facades, s.facades)
	s.mu.Unlock()

	logger.Info("Starting DittoServer with %d facade(s)", len(facades))

	// Channel to collect errors from facade goroutines
	// Buffered to prevent goroutine leaks if multiple facades fail simultaneously
	errChan := make(chan facadeError, len(facades))

	// WaitGroup to track facade goroutines
	var wg sync.WaitGroup

	// Start all facades concurrently
	startTime := time.Now()
	for _, fac := range facades {
		wg.Add(1)
		go func(f facade.Facade) {
			defer wg.Done()

			protocol := f.Protocol()
			port := f.Port()

			logger.Info("Starting %s facade on port %d", protocol, port)

			if err := f.Serve(ctx); err != nil {
				// Only log and report unexpected errors
				// context.Canceled is expected during shutdown
				if err != context.Canceled && ctx.Err() == nil {
					logger.Error("%s facade failed: %v", protocol, err)
					errChan <- facadeError{protocol: protocol, err: err}
				} else {
					logger.Debug("%s facade stopped gracefully", protocol)
				}
			} else {
				logger.Info("%s facade stopped", protocol)
			}
		}(fac)
	}

	// Log successful startup after a brief delay to allow facades to initialize
	go func() {
		time.Sleep(100 * time.Millisecond)
		logger.Info("All facades started successfully in %v", time.Since(startTime))
	}()

	// Wait for either context cancellation or facade error
	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received (reason: %v)", ctx.Err())
		s.stopAllFacades(facades)
		shutdownErr = ctx.Err()

	case facadeErr := <-errChan:
		logger.Error("Facade %s failed: %v - initiating shutdown of all facades",
			facadeErr.protocol, facadeErr.err)
		s.stopAllFacades(facades)
		shutdownErr = fmt.Errorf("%s facade error: %w", facadeErr.protocol, facadeErr.err)
	}

	// Wait for all facade goroutines to complete
	logger.Debug("Waiting for all facades to complete shutdown")
	wg.Wait()

	logger.Info("DittoServer stopped gracefully")

	return shutdownErr
}

// facadeError pairs a facade protocol name with its error for better error reporting.
type facadeError struct {
	protocol string
	err      error
}

// stopAllFacades initiates graceful shutdown of all facades in reverse registration order.
//
// Facades are stopped in reverse order to handle dependencies (e.g., if facade B depends
// on facade A, B should be stopped first). Each facade receives a Stop() call with a
// timeout context.
//
// The shutdown process:
//  1. Create a context with 30-second timeout for all Stop() operations
//  2. Call Stop() on each facade in reverse order
//  3. Log any errors but continue stopping remaining facades
//  4. Return after all Stop() calls complete or timeout expires
//
// Note: This method only initiates shutdown. The caller must wait for facade
// goroutines to complete using the WaitGroup.
//
// Parameters:
//   - facades: Snapshot of facades to stop (in registration order)
func (s *DittoServer) stopAllFacades(facades []facade.Facade) {
	// Create a context with timeout for all shutdown operations
	// This prevents a single misbehaving facade from blocking shutdown indefinitely
	const stopTimeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()

	logger.Info("Initiating graceful shutdown of %d facade(s)", len(facades))

	// Stop facades in reverse registration order
	// This handles potential dependencies between facades
	for i := len(facades) - 1; i >= 0; i-- {
		facade := facades[i]
		protocol := facade.Protocol()

		logger.Debug("Stopping %s facade (port %d)", protocol, facade.Port())

		// Call Stop() with timeout context
		// We don't wait for Stop() to complete here - the facade's Serve() goroutine
		// will handle actual cleanup. Stop() just signals the facade to begin shutdown.
		if err := facade.Stop(ctx); err != nil && err != context.Canceled {
			logger.Error("Error stopping %s facade: %v", protocol, err)
		} else {
			logger.Debug("%s facade stop signal sent", protocol)
		}
	}
}

// Facades returns a snapshot of currently registered facades.
//
// The returned slice is a copy and safe to iterate over without holding locks.
// Modifications to the returned slice do not affect the server's facade list.
//
// Returns:
//   - A copy of the facades slice (never nil, may be empty)
//
// Thread safety:
// Safe to call concurrently with AddFacade() and Serve().
func (s *DittoServer) Facades() []facade.Facade {
	s.mu.RLock()
	defer s.mu.RUnlock()

	facades := make([]facade.Facade, len(s.facades))
	copy(facades, s.facades)
	return facades
}
