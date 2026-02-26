package adapters

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// DefaultShutdownTimeout is the default timeout for graceful adapter shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// ProtocolAdapter is the interface for protocol adapters (NFS, SMB).
type ProtocolAdapter interface {
	Serve(ctx context.Context) error
	Stop(ctx context.Context) error
	Protocol() string
	Port() int
}

// RuntimeSetter is implemented by adapters that need runtime access.
type RuntimeSetter interface {
	SetRuntime(rt any)
}

// AdapterFactory creates a ProtocolAdapter from configuration.
type AdapterFactory func(cfg *models.AdapterConfig) (ProtocolAdapter, error)

type adapterEntry struct {
	adapter ProtocolAdapter
	config  *models.AdapterConfig
	ctx     context.Context
	cancel  context.CancelFunc
	errCh   chan error
}

// Service manages protocol adapter lifecycle.
type Service struct {
	mu      sync.RWMutex
	entries map[string]*adapterEntry // keyed by adapter type (nfs, smb)
	factory AdapterFactory

	store           store.AdapterStore
	shutdownTimeout time.Duration
	runtime         any // injected into adapters implementing RuntimeSetter
}

// New creates a new adapter management service.
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

func (s *Service) SetRuntime(rt any) { s.runtime = rt }

// SetAdapterFactory must be called before CreateAdapter.
func (s *Service) SetAdapterFactory(factory AdapterFactory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.factory = factory
}

func (s *Service) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	s.shutdownTimeout = d
}

// CreateAdapter saves the adapter config to store and starts it immediately.
func (s *Service) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	if _, err := s.store.CreateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to save adapter config: %w", err)
	}

	if err := s.startAdapter(cfg); err != nil {
		_ = s.store.DeleteAdapter(ctx, cfg.Type) // rollback
		return fmt.Errorf("failed to start adapter: %w", err)
	}

	return nil
}

// DeleteAdapter stops the running adapter and removes it from store.
func (s *Service) DeleteAdapter(ctx context.Context, adapterType string) error {
	if err := s.stopAdapter(adapterType); err != nil {
		logger.Warn("Adapter stop failed during delete", "type", adapterType, "error", err)
	}

	if err := s.store.DeleteAdapter(ctx, adapterType); err != nil {
		return fmt.Errorf("failed to delete adapter from store: %w", err)
	}

	return nil
}

// UpdateAdapter updates the persisted config, then restarts the adapter.
func (s *Service) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to update adapter config: %w", err)
	}

	_ = s.stopAdapter(cfg.Type)
	if cfg.Enabled {
		if err := s.startAdapter(cfg); err != nil {
			logger.Warn("Failed to restart adapter after update", "type", cfg.Type, "error", err)
		}
	}

	return nil
}

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

func (s *Service) DisableAdapter(ctx context.Context, adapterType string) error {
	cfg, err := s.store.GetAdapter(ctx, adapterType)
	if err != nil {
		return fmt.Errorf("adapter not found: %w", err)
	}

	_ = s.stopAdapter(adapterType)
	cfg.Enabled = false
	if err := s.store.UpdateAdapter(ctx, cfg); err != nil {
		return fmt.Errorf("failed to disable adapter: %w", err)
	}

	return nil
}

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

func (s *Service) stopAdapter(adapterType string) error {
	s.mu.Lock()
	entry, exists := s.entries[adapterType]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("adapter %s not running", adapterType)
	}
	delete(s.entries, adapterType)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	logger.Info("Stopping adapter", "type", adapterType)

	if err := entry.adapter.Stop(ctx); err != nil {
		logger.Warn("Adapter stop error", "type", adapterType, "error", err)
	}

	entry.cancel()
	select {
	case <-entry.errCh:
		logger.Info("Adapter stopped", "type", adapterType)
		return nil
	case <-ctx.Done():
		logger.Warn("Adapter stop timed out", "type", adapterType)
		return fmt.Errorf("adapter %s stop timed out", adapterType)
	}
}

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

func (s *Service) ListRunningAdapters() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	types := make([]string, 0, len(s.entries))
	for t := range s.entries {
		types = append(types, t)
	}
	return types
}

func (s *Service) IsAdapterRunning(adapterType string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.entries[adapterType]
	return exists
}

// AddAdapter directly starts a pre-created adapter (for testing, bypasses store).
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

// registerAndRunAdapterLocked starts the adapter in a goroutine. Caller must hold mu.
func (s *Service) registerAndRunAdapterLocked(adp ProtocolAdapter, cfg *models.AdapterConfig) {
	if setter, ok := adp.(RuntimeSetter); ok && s.runtime != nil {
		setter.SetRuntime(s.runtime)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		logger.Info("Starting adapter", "protocol", adp.Protocol(), "port", adp.Port())
		err := adp.Serve(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
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
