package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/sid"
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

// MachineSIDStore provides access to the SettingsStore for machine SID
// persistence. The lifecycle service uses this to load or generate the
// machine SID on first boot, ensuring consistent identity mapping across
// restarts.
type MachineSIDStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Service orchestrates server startup and graceful shutdown.
type Service struct {
	shutdownTimeout time.Duration
	apiServer       AuxiliaryServer
	serveOnce       sync.Once
	served          bool

	// sidMapper is the machine SID mapper, initialized on first Serve().
	// It is exposed via SIDMapper() for adapters to use.
	sidMapper *sid.SIDMapper
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

// SIDMapper returns the machine SID mapper initialized during Serve().
// Returns nil if Serve() has not been called yet.
func (s *Service) SIDMapper() *sid.SIDMapper {
	return s.sidMapper
}

// initMachineSID loads or generates the machine SID from the settings store.
// On first boot (no stored SID), a new random SID is generated and persisted.
// On subsequent boots, the stored SID is loaded to ensure consistent mapping.
// This MUST be called before any adapters are started.
func (s *Service) initMachineSID(ctx context.Context, store MachineSIDStore) {
	if store == nil {
		logger.Warn("No settings store available, generating ephemeral machine SID")
		s.sidMapper = sid.GenerateMachineSID()
		logger.Info("Generated ephemeral machine SID", "sid", s.sidMapper.MachineSIDString())
		return
	}

	const machineSIDKey = "machine_sid"

	stored, err := store.GetSetting(ctx, machineSIDKey)
	if err != nil {
		logger.Warn("Failed to read machine SID from store, generating new one", "error", err)
		stored = ""
	}

	if stored != "" {
		// Load existing machine SID
		mapper, err := sid.NewSIDMapperFromString(stored)
		if err != nil {
			logger.Error("Invalid stored machine SID, generating new one",
				"stored", stored, "error", err)
		} else {
			s.sidMapper = mapper
			logger.Info("Loaded machine SID from store", "sid", stored)
			return
		}
	}

	// First boot: generate and persist
	s.sidMapper = sid.GenerateMachineSID()
	sidStr := s.sidMapper.MachineSIDString()

	if err := store.SetSetting(ctx, machineSIDKey, sidStr); err != nil {
		logger.Error("Failed to persist machine SID", "sid", sidStr, "error", err)
	} else {
		logger.Info("Generated and persisted machine SID", "sid", sidStr)
	}
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
//
// The machineSIDStore parameter is used to load or generate the machine SID
// for Windows identity mapping. Pass nil to use an ephemeral SID (testing).
func (s *Service) Serve(
	ctx context.Context,
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
	machineSIDStore MachineSIDStore,
) error {
	var err error

	s.serveOnce.Do(func() {
		s.served = true
		err = s.serve(ctx, settings, adapterLoader, metadataFlusher, storeCloser, machineSIDStore)
	})

	return err
}

func (s *Service) serve(
	ctx context.Context,
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
	machineSIDStore MachineSIDStore,
) error {
	logger.Info("Starting DittoFS runtime")

	// Initialize machine SID BEFORE any adapters start.
	// This ensures consistent identity mapping for all connections.
	s.initMachineSID(ctx, machineSIDStore)

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
		flushed, err := metadataFlusher.FlushAllPendingWritesForShutdown(s.shutdownTimeout)
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
		ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.apiServer.Stop(ctx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}
}
