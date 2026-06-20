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

// SnapshotDrainer cancels all in-flight snapshot orchestration goroutines
// and waits (bounded by ctx) for them to drain. Threaded through Serve so
// snapshot orchestration cannot use-after-close the metadata stores or
// control-plane DB during graceful shutdown — without this hook the
// normal server path (signal -> ctx cancel -> lifecycle.shutdown) would
// call StopAllAdapters + CloseMetadataStores directly while snapshot
// goroutines still hold references to the closing stores.
type SnapshotDrainer interface {
	ShutdownSnapshots(ctx context.Context)
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

	// pinnedMachineSID, when non-empty, is an operator-supplied machine SID
	// (config/env) that initMachineSID seeds in preference to any random
	// generation. Pinning the machine SID lets multiple cluster nodes derive
	// IDENTICAL local/algorithmic SIDs from the same Unix UID/GID — see
	// pkg/auth/sid/mapper.go for the LOCKED RID formula.
	pinnedMachineSID string
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

// SetPinnedMachineSID records an operator-supplied machine SID to seed during
// Serve(). Must be called before Serve(). An empty string is a no-op (the
// machine SID is then loaded-or-generated as before). The value is validated
// and applied by initMachineSID.
func (s *Service) SetPinnedMachineSID(machineSID string) {
	if s.served {
		panic("cannot set pinned machine SID after Serve() has been called")
	}
	s.pinnedMachineSID = machineSID
}

// initMachineSID resolves the machine SID with the following precedence:
//
//  1. An operator-pinned machine SID (SetPinnedMachineSID, from config/env)
//     is validated and applied; it is persisted to the settings store so it
//     is authoritative and survives a later boot without the pin. Pinning the
//     same SID on every node makes their local/algorithmic SIDs identical —
//     required for cross-node identity parity (see pkg/auth/sid/mapper.go).
//  2. Otherwise the stored SID (first boot generated + persisted) is loaded,
//     keeping mapping stable across restarts.
//  3. Otherwise a new random SID is generated and persisted.
//
// This MUST be called before any adapters are started.
func (s *Service) initMachineSID(ctx context.Context, store MachineSIDStore) error {
	const machineSIDKey = "machine_sid"

	// Operator pin takes precedence and is honored even without a settings
	// store (ephemeral / test runtimes) so two nodes still derive identical
	// SIDs from the same config.
	if s.pinnedMachineSID != "" {
		mapper, err := sid.NewSIDMapperFromString(s.pinnedMachineSID)
		if err != nil {
			// A pin is an explicit operator intent for cross-node parity.
			// Silently falling back to a random SID would diverge this node's
			// local UID->SID encoding from the rest of the cluster, so abort
			// instead. The config layer normally rejects this earlier
			// (IdentityConfig.Validate); this guards programmatic misuse.
			return fmt.Errorf("invalid pinned machine SID %q: %w", s.pinnedMachineSID, err)
		}
		s.sidMapper = mapper
		if store != nil {
			prior, _ := store.GetSetting(ctx, machineSIDKey)
			if prior != "" && prior != s.pinnedMachineSID {
				logger.Warn("Pinned machine SID overrides a different stored value; foreign-SID mappings keyed on the old domain remain unaffected, but local UID->SID encoding changes",
					"pinned", s.pinnedMachineSID, "stored", prior)
			}
			if prior != s.pinnedMachineSID {
				if err := store.SetSetting(ctx, machineSIDKey, s.pinnedMachineSID); err != nil {
					logger.Error("Failed to persist pinned machine SID", "sid", s.pinnedMachineSID, "error", err)
				}
			}
		}
		logger.Info("Using pinned machine SID", "sid", s.pinnedMachineSID)
		return nil
	}

	if store == nil {
		logger.Warn("No settings store available, generating ephemeral machine SID")
		s.sidMapper = sid.GenerateMachineSID()
		logger.Info("Generated ephemeral machine SID", "sid", s.sidMapper.MachineSIDString())
		return nil
	}

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
			return nil
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
	return nil
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
//
// The snapshotDrainer parameter is invoked as the FIRST shutdown step so
// in-flight snapshot orchestration goroutines are cancelled and drained
// BEFORE StopAllAdapters + CloseMetadataStores — otherwise those
// goroutines would race a closing metadata store / control-plane DB.
// Pass nil to skip snapshot draining (tests that do not exercise the
// snapshot pipeline).
func (s *Service) Serve(
	ctx context.Context,
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
	machineSIDStore MachineSIDStore,
	snapshotDrainer SnapshotDrainer,
) error {
	var err error

	s.serveOnce.Do(func() {
		s.served = true
		err = s.serve(ctx, settings, adapterLoader, metadataFlusher, storeCloser, machineSIDStore, snapshotDrainer)
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
	snapshotDrainer SnapshotDrainer,
) error {
	logger.Info("Starting DittoFS runtime")

	// Initialize machine SID BEFORE any adapters start.
	// This ensures consistent identity mapping for all connections.
	if err := s.initMachineSID(ctx, machineSIDStore); err != nil {
		return fmt.Errorf("failed to initialize machine SID: %w", err)
	}

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

	s.shutdown(settings, adapterLoader, metadataFlusher, storeCloser, snapshotDrainer)

	logger.Info("DittoFS runtime stopped")
	return shutdownErr
}

func (s *Service) shutdown(
	settings SettingsInitializer,
	adapterLoader AdapterLoader,
	metadataFlusher MetadataFlusher,
	storeCloser StoreCloser,
	snapshotDrainer SnapshotDrainer,
) {
	if settings != nil {
		settings.Stop()
	}

	// Drain in-flight snapshot orchestration goroutines BEFORE stopping
	// adapters / closing metadata stores — those goroutines hold
	// references to both. ShutdownSnapshots cancels runtimeCtx (every
	// per-snap ctx derives from it) and waits, bounded by the shutdown
	// timeout. Orphans after the timeout will still exit on their own
	// since runtimeCtx is already cancelled; we just may proceed before
	// every wg.Done fires.
	if snapshotDrainer != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		snapshotDrainer.ShutdownSnapshots(drainCtx)
		cancel()
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
