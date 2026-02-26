package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// DefaultShutdownTimeout is the default timeout for graceful shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// AuxiliaryServer is an interface for auxiliary HTTP servers (API, Metrics).
type AuxiliaryServer interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Port() int
}

// SettingsInitializer loads initial settings and starts background polling.
type SettingsInitializer interface {
	LoadInitial(ctx context.Context) error
	Start(ctx context.Context)
	Stop()
}

// AdapterLoader loads and starts adapters from the persistent store.
type AdapterLoader interface {
	LoadAdaptersFromStore(ctx context.Context) error
	StopAllAdapters() error
}

// MetadataFlusher flushes pending metadata writes during shutdown.
type MetadataFlusher interface {
	FlushAllPendingWritesForShutdown(timeout time.Duration) (int, error)
}

// StoreCloser closes metadata stores during shutdown.
type StoreCloser interface {
	CloseMetadataStores()
}

// Service orchestrates server startup and graceful shutdown.
type Service struct {
	shutdownTimeout time.Duration
	apiServer       AuxiliaryServer

	// serveOnce ensures Serve() is only called once
	serveOnce sync.Once
	served    bool
}

// New creates a new lifecycle service.
func New(shutdownTimeout time.Duration) *Service {
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	return &Service{
		shutdownTimeout: shutdownTimeout,
	}
}

// SetShutdownTimeout sets the maximum time to wait for graceful shutdown.
func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.shutdownTimeout = d
}

// SetAPIServer sets the REST API HTTP server.
// Must be called before Serve().
func (s *Service) SetAPIServer(server AuxiliaryServer) {
	if s.served {
		panic("cannot set API server after Serve() has been called")
	}
	s.apiServer = server
	if server != nil {
		logger.Info("API server registered", "port", server.Port())
	}
}

// Serve starts all components and blocks until shutdown.
// It coordinates adapter loading, API server startup, and graceful shutdown.
func (s *Service) Serve(
	ctx context.Context,
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
) error {
	var err error

	s.serveOnce.Do(func() {
		s.served = true
		err = s.serve(ctx, settings, adapterLoader, metadataFlusher, storeCloser)
	})

	return err
}

// serve is the internal implementation of Serve().
func (s *Service) serve(
	ctx context.Context,
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
) error {
	logger.Info("Starting DittoFS runtime")

	// 0. Initialize settings watcher
	if settings != nil {
		if err := settings.LoadInitial(ctx); err != nil {
			logger.Warn("Failed to load initial adapter settings", "error", err)
		}
		settings.Start(ctx)
	}

	// 1. Load and start adapters from store
	if err := adapterLoader.LoadAdaptersFromStore(ctx); err != nil {
		return fmt.Errorf("failed to load adapters: %w", err)
	}

	// 2. Start API server if configured
	apiErrChan := make(chan error, 1)
	if s.apiServer != nil {
		go func() {
			if err := s.apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
				apiErrChan <- err
			}
		}()
	}

	// 3. Wait for shutdown signal or server error
	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received", "reason", ctx.Err())
		shutdownErr = ctx.Err()

	case err := <-apiErrChan:
		logger.Error("API server failed - initiating shutdown", "error", err)
		shutdownErr = fmt.Errorf("API server error: %w", err)
	}

	// 4. Graceful shutdown
	s.shutdown(settings, adapterLoader, metadataFlusher, storeCloser)

	logger.Info("DittoFS runtime stopped")
	return shutdownErr
}

// shutdown performs graceful shutdown of all components.
func (s *Service) shutdown(
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
) {
	// Stop settings watcher first (no more polling)
	if settings != nil {
		logger.Debug("Stopping settings watcher")
		settings.Stop()
	}

	// Stop all adapters (with connection draining)
	logger.Info("Stopping all adapters")
	if err := adapterLoader.StopAllAdapters(); err != nil {
		logger.Warn("Error stopping adapters", "error", err)
	}

	// Flush any pending metadata writes
	if metadataFlusher != nil {
		logger.Info("Flushing pending metadata writes")
		flushed, err := metadataFlusher.FlushAllPendingWritesForShutdown(10 * time.Second)
		if err != nil {
			logger.Warn("Error flushing pending writes", "error", err, "flushed", flushed)
		} else if flushed > 0 {
			logger.Info("Flushed pending metadata writes", "count", flushed)
		}
	}

	// Close metadata stores
	if storeCloser != nil {
		logger.Info("Closing metadata stores")
		storeCloser.CloseMetadataStores()
	}

	// Stop API server
	if s.apiServer != nil {
		logger.Debug("Stopping API server")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.apiServer.Stop(ctx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}
}
