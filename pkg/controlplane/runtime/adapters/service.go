package adapters

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// DefaultShutdownTimeout is the default timeout for graceful adapter shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// ProtocolAdapter is an interface for protocol adapters (NFS, SMB) that can be
// managed by the adapter service. This interface is defined here to break the
// import cycle between runtime and adapter packages.
type ProtocolAdapter interface {
	// Serve starts the protocol server and blocks until the context is cancelled.
	Serve(ctx context.Context) error
	// Stop initiates graceful shutdown of the protocol server.
	Stop(ctx context.Context) error
	// Protocol returns the protocol name (e.g., "NFS", "SMB").
	Protocol() string
	// Port returns the TCP port the adapter is listening on.
	Port() int
}

// RuntimeSetter is implemented by adapters that need runtime access.
// The runtime parameter is typed as any to avoid import cycles with the parent package.
type RuntimeSetter interface {
	SetRuntime(rt any)
}

// AdapterFactory is a function that creates a ProtocolAdapter from configuration.
type AdapterFactory func(cfg *models.AdapterConfig) (ProtocolAdapter, error)

// adapterEntry holds adapter state for lifecycle management.
type adapterEntry struct {
	adapter ProtocolAdapter
	config  *models.AdapterConfig
	ctx     context.Context
	cancel  context.CancelFunc
	errCh   chan error
}

// Service manages protocol adapter lifecycle: creation, startup, shutdown,
// and configuration persistence.
type Service struct {
	mu      sync.RWMutex
	entries map[string]*adapterEntry // key: adapter type (nfs, smb)
	factory AdapterFactory

	store           store.AdapterStore
	shutdownTimeout time.Duration

	// runtime is stored as any to avoid import cycle with parent package.
	// It is injected into adapters that implement RuntimeSetter.
	runtime any
}

// New creates a new adapter management service.
// The adapterStore parameter may be nil for testing scenarios.
func New(adapterStore store.AdapterStore, shutdownTimeout time.Duration) *Service {
	if shutdownTimeout == 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	return &Service{
		entries:         make(map[string]*adapterEntry),
		store:           adapterStore,
		shutdownTimeout: shutdownTimeout,
	}
}

// SetRuntime stores the runtime reference for injection into adapters.
func (s *Service) SetRuntime(rt any) {
	s.runtime = rt
}

// SetAdapterFactory sets the factory function for creating adapters.
// This must be called before CreateAdapter or LoadAdaptersFromStore.
func (s *Service) SetAdapterFactory(factory AdapterFactory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.factory = factory
}

// SetShutdownTimeout sets the maximum time to wait for graceful adapter shutdown.
func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.shutdownTimeout = d
}

// CreateAdapter saves the adapter config to store AND starts it immediately.
// This is the method that API handlers should call - it ensures both persistent
// store and in-memory state are updated together.
func (s *Service) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	if _, err := s.store.CreateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to save adapter config: %w", err)
	}

	if err := s.startAdapter(cfg); err != nil {
		// Rollback: delete from store
		_ = s.store.DeleteAdapter(ctx, cfg.Type)
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

// DeleteAdapter stops the running adapter (drains connections) AND removes from store.
func (s *Service) DeleteAdapter(ctx context.Context, adapterType string) error {
	if err := s.stopAdapter(adapterType); err != nil {
		logger.Warn("Adapter stop failed during delete", "type", adapterType, "error", err)
		// Continue with deletion even if stop fails
	}

	if err := s.store.DeleteAdapter(ctx, adapterType); err != nil {
		return fmt.Errorf("failed to delete adapter from store: %w", err)
	}

	return nil
}

// UpdateAdapter restarts the adapter with new configuration.
// Updates store first, then restarts the running adapter.
func (s *Service) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to update adapter config: %w", err)
	}

	// Stop old adapter (if running)
	_ = s.stopAdapter(cfg.Type)

	// Start with new config if enabled
	if cfg.Enabled {
		if err := s.startAdapter(cfg); err != nil {
			logger.Warn("Failed to restart adapter after update", "type", cfg.Type, "error", err)
			// Don't fail the update - config was saved successfully
		}
	}

	return nil
}

// EnableAdapter enables an adapter and starts it.
func (s *Service) EnableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := s.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	cfg.Enabled = true
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to enable adapter: %w", err)
	}

	if err := s.startAdapter(cfg); err != nil {
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

// DisableAdapter stops an adapter and disables it.
func (s *Service) DisableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := s.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	// Stop the adapter first
	_ = s.stopAdapter(adapterType)

	// Update store
	cfg.Enabled = false
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to disable adapter: %w", err)
	}

	return nil
}

// startAdapter creates and starts an adapter from config.
func (s *Service) startAdapter(cfg *models.AdapterConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.entries[cfg.Type]; exists {
		return fmt.Errorf("adapter %s already running", cfg.Type)
	}

	if s.factory == nil {
		return fmt.Errorf("adapter factory not set")
	}

	adp, err := s.factory(cfg)
	if err != nil {
		return fmt.Errorf("failed to create adapter: %w", err)
	}

	s.registerAndRunAdapterLocked(adp, cfg)
	return nil
}

// stopAdapter stops a running adapter with connection draining.
func (s *Service) stopAdapter(adapterType string) error {
	s.mu.Lock()
	entry, exists := s.entries[adapterType]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("adapter %s not running", adapterType)
	}
	delete(s.entries, adapterType)
	s.mu.Unlock()

	// Create timeout context for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	logger.Info("Stopping adapter", "type", adapterType)

	// Signal adapter to stop (triggers connection draining)
	if err := entry.adapter.Stop(ctx); err != nil {
		logger.Warn("Adapter stop error", "type", adapterType, "error", err)
	}

	// Cancel adapter's context
	entry.cancel()

	// Wait for adapter goroutine
	select {
	case <-entry.errCh:
		logger.Info("Adapter stopped", "type", adapterType)
		return nil
	case <-ctx.Done():
		logger.Warn("Adapter stop timed out", "type", adapterType)
		return fmt.Errorf("adapter %s stop timed out", adapterType)
	}
}

// StopAllAdapters stops all running adapters (for shutdown).
func (s *Service) StopAllAdapters() error {
	s.mu.RLock()
	types := make([]string, 0, len(s.entries))
	for t := range s.entries {
		types = append(types, t)
	}
	s.mu.RUnlock()

	var lastErr error
	for _, t := range types {
		if err := s.stopAdapter(t); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// LoadAdaptersFromStore loads enabled adapters from store and starts them.
// This is called during server startup.
func (s *Service) LoadAdaptersFromStore(ctx context.Context) error {
	adapters, err := s.store.ListAdapters(ctx)
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w", err)
	}

	for _, cfg := range adapters {
		if !cfg.Enabled {
			logger.Info("Adapter disabled, skipping", "type", cfg.Type)
			continue
		}

		if err := s.startAdapter(cfg); err != nil {
			return fmt.Errorf("failed to start adapter %s: %w", cfg.Type, err)
		}
	}

	return nil
}

// ListRunningAdapters returns information about currently running adapters.
func (s *Service) ListRunningAdapters() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	types := make([]string, 0, len(s.entries))
	for t := range s.entries {
		types = append(types, t)
	}
	return types
}

// IsAdapterRunning checks if an adapter is currently running.
func (s *Service) IsAdapterRunning(adapterType string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.entries[adapterType]
	return exists
}

// AddAdapter adds and starts a pre-created adapter directly.
// This bypasses the store and is primarily for testing.
// The adapter will be registered under its Protocol() name.
func (s *Service) AddAdapter(adapter ProtocolAdapter) error {
	adapterType := adapter.Protocol()

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.entries[adapterType]; exists {
		return fmt.Errorf("adapter %s already running", adapterType)
	}

	cfg := &models.AdapterConfig{Type: adapterType, Port: adapter.Port(), Enabled: true}
	s.registerAndRunAdapterLocked(adapter, cfg)
	return nil
}

// registerAndRunAdapterLocked injects the runtime, starts the adapter in a goroutine,
// and records it in the entries map. Caller must hold mu.
func (s *Service) registerAndRunAdapterLocked(adp ProtocolAdapter, cfg *models.AdapterConfig) {
	if setter, ok := adp.(RuntimeSetter); ok && s.runtime != nil {
		setter.SetRuntime(s.runtime)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		logger.Info("Starting adapter", "protocol", adp.Protocol(), "port", adp.Port())
		err := adp.Serve(ctx)
		if err != nil && err != context.Canceled && ctx.Err() == nil {
			logger.Error("Adapter failed", "protocol", adp.Protocol(), "error", err)
		}
		errCh <- err
	}()

	s.entries[cfg.Type] = &adapterEntry{
		adapter: adp,
		config:  cfg,
		ctx:     ctx,
		cancel:  cancel,
		errCh:   errCh,
	}

	logger.Info("Adapter started", "type", cfg.Type, "port", cfg.Port)
}
