package stores

import (
	"fmt"
	"io"
	"maps"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Service manages named metadata store instances.
// It provides thread-safe registration, lookup, and lifecycle management.
type Service struct {
	mu       sync.RWMutex
	registry map[string]metadata.MetadataStore
}

// New creates a new metadata store management service.
func New() *Service {
	return &Service{
		registry: make(map[string]metadata.MetadataStore),
	}
}

// RegisterMetadataStore adds a named metadata store instance.
// Returns an error if a store with the same name already exists.
func (s *Service) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	if metaStore == nil {
		return fmt.Errorf("cannot register nil metadata store")
	}
	if name == "" {
		return fmt.Errorf("cannot register metadata store with empty name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.registry[name]; exists {
		return fmt.Errorf("metadata store %q already registered", name)
	}

	s.registry[name] = metaStore
	return nil
}

// GetMetadataStore retrieves a metadata store instance by name.
func (s *Service) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	metaStore, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", name)
	}
	return metaStore, nil
}

// GetMetadataStoreForShare retrieves the metadata store used by a share.
// The storeName is the metadata store name referenced by the share.
func (s *Service) GetMetadataStoreForShare(storeName string) (metadata.MetadataStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	metaStore, exists := s.registry[storeName]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", storeName)
	}

	return metaStore, nil
}

// ListMetadataStores returns all registered metadata store names.
func (s *Service) ListMetadataStores() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.registry))
	for name := range s.registry {
		names = append(names, name)
	}
	return names
}

// CountMetadataStores returns the number of registered metadata stores.
func (s *Service) CountMetadataStores() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}

// CloseMetadataStores closes all registered metadata stores that implement io.Closer.
// This should be called during graceful shutdown.
func (s *Service) CloseMetadataStores() {
	// Collect stores while holding lock
	s.mu.RLock()
	snapshot := make(map[string]metadata.MetadataStore, len(s.registry))
	maps.Copy(snapshot, s.registry)
	s.mu.RUnlock()

	// Close stores outside of lock
	for name, store := range snapshot {
		if closer, ok := store.(io.Closer); ok {
			logger.Debug("Closing metadata store", "store", name)
			if err := closer.Close(); err != nil {
				logger.Error("Metadata store close error", "store", name, "error", err)
			}
		}
	}
}
