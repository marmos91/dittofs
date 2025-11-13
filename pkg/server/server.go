package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// DittoServer manages the lifecycle of multiple protocol adapters that share
// common metadata and content repositories.
//
// Architecture:
// DittoServer orchestrates different file sharing protocols (NFS, SMB, WebDAV, etc.)
// that are represented as Adapter implementations. All adapters share the same backend
// repositories, providing a unified view of the file system across all protocols.
//
// Lifecycle:
//  1. Creation: New() with repositories
//  2. Registration: AddAdapter() for each protocol
//  3. Startup: Serve() starts all adapters concurrently
//  4. Shutdown: Context cancellation triggers graceful shutdown of all adapters
//
// Thread safety:
// DittoServer is safe for concurrent use. AddAdapter() may be called concurrently
// with other methods. Serve() should only be called once per server instance.
//
// Example usage:
//
//	server := New(metadataRepo, contentRepo)
//	server.AddAdapter(nfs.New(nfsConfig))
//	server.AddAdapter(smb.New(smbConfig))
//
//	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
//	defer cancel()
//
//	if err := server.Serve(ctx); err != nil && err != context.Canceled {
//	    log.Fatal(err)
//	}
type DittoServer struct {
	// metadata is the shared metadata repository for all adapters
	metadata metadata.MetadataStore

	// content is the shared content repository for all adapters
	content content.ContentStore

	// adapters contains all registered protocol adapters
	adapters []adapter.Adapter

	// mu protects the adapters slice and serving flag
	mu sync.RWMutex

	// serveOnce ensures Serve() is only called once
	serveOnce sync.Once

	// served indicates whether Serve() has been called
	// Protected by serveOnce, no additional locking needed
	served bool
}

// New creates a new DittoServer with the provided repositories.
//
// The repositories are shared across all adapters added to this server, ensuring
// that file system operations are consistent regardless of which protocol is used
// to access the data.
//
// Parameters:
//   - metadata: Repository for file system metadata operations (required)
//   - content: Repository for file content operations (required)
//
// Returns a configured but not yet started DittoServer. Call AddAdapter() to
// register protocols, then Serve() to start the server.
//
// Panics if either repository is nil (indicates programmer error).
func New(metadata metadata.MetadataStore, content content.ContentStore) *DittoServer {
	if metadata == nil {
		panic("metadata repository cannot be nil")
	}
	if content == nil {
		panic("content repository cannot be nil")
	}

	return &DittoServer{
		metadata: metadata,
		content:  content,
		adapters: make([]adapter.Adapter, 0, 4), // Pre-allocate for common case of 2-4 adapters
	}
}

// AddAdapter registers a new protocol adapter with the server.
//
// This method injects the shared repositories into the adapter and adds it to the
// list of adapters that will be started when Serve() is called.
//
// AddAdapter() may be called multiple times to register different protocol adapters.
// Each adapter must implement a different protocol or listen on a different port.
// Duplicate protocols or port conflicts are detected and return an error.
//
// Parameters:
//   - a: The protocol adapter to register (must not be nil)
//
// Returns:
//   - error if the adapter is invalid or conflicts with an existing adapter
//
// Panics if:
//   - adapter is nil (programmer error)
//   - Serve() has already been called (server is running)
//
// Thread safety:
// Safe to call concurrently from multiple goroutines before Serve() is called.
//
// Example:
//
//	if err := server.AddAdapter(nfs.New(nfsConfig)); err != nil {
//	    log.Fatal(err)
//	}
//	if err := server.AddAdapter(smb.New(smbConfig)); err != nil {
//	    log.Fatal(err)
//	}
func (s *DittoServer) AddAdapter(a adapter.Adapter) error {
	if a == nil {
		panic("adapter cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if Serve() has been called
	if s.served {
		panic("cannot add adapter after Serve() has been called")
	}

	protocol := a.Protocol()
	port := a.Port()

	// Validate no duplicate protocols
	for _, existing := range s.adapters {
		if existing.Protocol() == protocol {
			return fmt.Errorf("adapter for protocol %s already registered", protocol)
		}
	}

	// Validate no port conflicts
	for _, existing := range s.adapters {
		if existing.Port() == port {
			return fmt.Errorf("port %d already in use by %s adapter",
				port, existing.Protocol())
		}
	}

	// Inject shared repositories
	a.SetStores(s.metadata, s.content)

	// Register the adapter
	s.adapters = append(s.adapters, a)

	logger.Info("Registered %s adapter on port %d", protocol, port)

	return nil
}

// Serve starts all registered adapters and blocks until the context is cancelled
// or an adapter fails to start.
//
// Serve() orchestrates the lifecycle of all adapters:
//  1. Validates that at least one adapter is registered
//  2. Starts all adapters concurrently in separate goroutines
//  3. Monitors for context cancellation or adapter failures
//  4. On shutdown signal: stops all adapters in reverse order
//  5. Waits for all adapters to complete shutdown
//
// Shutdown behavior:
// When the context is cancelled or an adapter fails:
//   - All adapters receive Stop() calls in reverse registration order
//   - Each adapter has 30 seconds (configurable via stopTimeout) to shut down
//   - Adapters are stopped concurrently after the Stop() calls are issued
//   - Serve() waits for all adapters to complete before returning
//
// Error handling:
//   - If any adapter fails to start: stops all already-started adapters and returns error
//   - If context is cancelled: initiates graceful shutdown and returns context.Canceled
//   - If any adapter fails during operation: stops all adapters and returns the error
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
// AddAdapter() must not be called after Serve() is called.
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
	// Get snapshot of adapters under lock
	s.mu.Lock()
	if len(s.adapters) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("no adapters registered; call AddAdapter() before Serve()")
	}
	adapters := make([]adapter.Adapter, len(s.adapters))
	copy(adapters, s.adapters)
	s.mu.Unlock()

	logger.Info("Starting DittoServer with %d adapter(s)", len(adapters))

	// Channel to collect errors from adapter goroutines
	// Buffered to prevent goroutine leaks if multiple adapters fail simultaneously
	errChan := make(chan adapterError, len(adapters))

	// WaitGroup to track adapter goroutines
	var wg sync.WaitGroup

	// Start all adapters concurrently
	startTime := time.Now()
	for _, adp := range adapters {
		wg.Add(1)
		go func(a adapter.Adapter) {
			defer wg.Done()

			protocol := a.Protocol()
			port := a.Port()

			logger.Info("Starting %s adapter on port %d", protocol, port)

			if err := a.Serve(ctx); err != nil {
				// Only log and report unexpected errors
				// context.Canceled is expected during shutdown
				if err != context.Canceled && ctx.Err() == nil {
					logger.Error("%s adapter failed: %v", protocol, err)
					errChan <- adapterError{protocol: protocol, err: err}
				} else {
					logger.Debug("%s adapter stopped gracefully", protocol)
				}
			} else {
				logger.Info("%s adapter stopped", protocol)
			}
		}(adp)
	}

	// Log successful startup after a brief delay to allow adapters to initialize
	go func() {
		time.Sleep(100 * time.Millisecond)
		logger.Info("All adapters started successfully in %v", time.Since(startTime))
	}()

	// Wait for either context cancellation or adapter error
	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received (reason: %v)", ctx.Err())
		s.stopAllAdapters(adapters)
		shutdownErr = ctx.Err()

	case adapterErr := <-errChan:
		logger.Error("Adapter %s failed: %v - initiating shutdown of all adapters",
			adapterErr.protocol, adapterErr.err)
		s.stopAllAdapters(adapters)
		shutdownErr = fmt.Errorf("%s adapter error: %w", adapterErr.protocol, adapterErr.err)
	}

	// Wait for all adapter goroutines to complete
	logger.Debug("Waiting for all adapters to complete shutdown")
	wg.Wait()

	logger.Info("DittoServer stopped gracefully")

	return shutdownErr
}

// adapterError pairs an adapter protocol name with its error for better error reporting.
type adapterError struct {
	protocol string
	err      error
}

// stopAllAdapters initiates graceful shutdown of all adapters in reverse registration order.
//
// Adapters are stopped in reverse order to handle dependencies (e.g., if adapter B depends
// on adapter A, B should be stopped first). Each adapter receives a Stop() call with a
// timeout context.
//
// The shutdown process:
//  1. Create a context with 30-second timeout for all Stop() operations
//  2. Call Stop() on each adapter in reverse order
//  3. Log any errors but continue stopping remaining adapters
//  4. Return after all Stop() calls complete or timeout expires
//
// Note: This method only initiates shutdown. The caller must wait for adapter
// goroutines to complete using the WaitGroup.
//
// Parameters:
//   - adapters: Snapshot of adapters to stop (in registration order)
func (s *DittoServer) stopAllAdapters(adapters []adapter.Adapter) {
	// Create a context with timeout for all shutdown operations
	// This prevents a single misbehaving adapter from blocking shutdown indefinitely
	const stopTimeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()

	logger.Info("Initiating graceful shutdown of %d adapter(s)", len(adapters))

	// Stop adapters in reverse registration order
	// This handles potential dependencies between adapters
	for i := len(adapters) - 1; i >= 0; i-- {
		adp := adapters[i]
		protocol := adp.Protocol()

		logger.Debug("Stopping %s adapter (port %d)", protocol, adp.Port())

		// Call Stop() with timeout context
		// We don't wait for Stop() to complete here - the adapter's Serve() goroutine
		// will handle actual cleanup. Stop() just signals the adapter to begin shutdown.
		if err := adp.Stop(ctx); err != nil && err != context.Canceled {
			logger.Error("Error stopping %s adapter: %v", protocol, err)
		} else {
			logger.Debug("%s adapter stop signal sent", protocol)
		}
	}
}

// Adapters returns a snapshot of currently registered adapters.
//
// The returned slice is a copy and safe to iterate over without holding locks.
// Modifications to the returned slice do not affect the server's adapter list.
//
// Returns:
//   - A copy of the adapters slice (never nil, may be empty)
//
// Thread safety:
// Safe to call concurrently with AddAdapter() and Serve().
func (s *DittoServer) Adapters() []adapter.Adapter {
	s.mu.RLock()
	defer s.mu.RUnlock()

	adapters := make([]adapter.Adapter, len(s.adapters))
	copy(adapters, s.adapters)
	return adapters
}
