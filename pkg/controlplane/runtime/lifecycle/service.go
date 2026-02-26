package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

const DefaultShutdownTimeout = 30 * time.Second

// AuxiliaryServer is implemented by HTTP servers (API, Metrics).
type AuxiliaryServer interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Port() int
}

type SettingsInitializer interface {
	LoadInitial(ctx context.Context) error
	Start(ctx context.Context)
	Stop()
}

type AdapterLoader interface {
	LoadAdaptersFromStore(ctx context.Context) error
	StopAllAdapters() error
}

type MetadataFlusher interface {
	FlushAllPendingWritesForShutdown(timeout time.Duration) (int, error)
}

type StoreCloser interface {
	CloseMetadataStores()
}

// Service orchestrates server startup and graceful shutdown.
type Service struct {
	shutdownTimeout time.Duration
	apiServer       AuxiliaryServer
	serveOnce       sync.Once
	served          bool
}

func New(shutdownTimeout time.Duration) *Service {
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	return &Service{
		shutdownTimeout: shutdownTimeout,
	}
}

func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.shutdownTimeout = d
}

// SetAPIServer must be called before Serve().
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

func (s *Service) serve(
	ctx context.Context,
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
) error {
	logger.Info("Starting DittoFS runtime")

	if settings != nil {
		if err := settings.LoadInitial(ctx); err != nil {
			logger.Warn("Failed to load initial adapter settings", "error", err)
		}
		settings.Start(ctx)
	}

	if err := adapterLoader.LoadAdaptersFromStore(ctx); err != nil {
		return fmt.Errorf("failed to load adapters: %w", err)
	}

	apiErrChan := make(chan error, 1)
	if s.apiServer != nil {
		go func() {
			if err := s.apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
				apiErrChan <- err
			}
		}()
	}

	var shutdownErr error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received", "reason", ctx.Err())
		shutdownErr = ctx.Err()
	case err := <-apiErrChan:
		logger.Error("API server failed, initiating shutdown", "error", err)
		shutdownErr = fmt.Errorf("API server error: %w", err)
	}

	s.shutdown(settings, adapterLoader, metadataFlusher, storeCloser)

	logger.Info("DittoFS runtime stopped")
	return shutdownErr
}

func (s *Service) shutdown(
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
) {
	if settings != nil {
		settings.Stop()
	}

	logger.Info("Stopping all adapters")
	if err := adapterLoader.StopAllAdapters(); err != nil {
		logger.Warn("Error stopping adapters", "error", err)
	}

	if metadataFlusher != nil {
		flushed, err := metadataFlusher.FlushAllPendingWritesForShutdown(10 * time.Second)
		if err != nil {
			logger.Warn("Error flushing pending writes", "error", err, "flushed", flushed)
		} else if flushed > 0 {
			logger.Info("Flushed pending metadata writes", "count", flushed)
		}
	}

	if storeCloser != nil {
		storeCloser.CloseMetadataStores()
	}

	if s.apiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.apiServer.Stop(ctx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}
}
