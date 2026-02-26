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
type Service struct {
	mu       sync.RWMutex
	registry map[string]metadata.MetadataStore
}

func New() *Service {
	return &Service{
		registry: make(map[string]metadata.MetadataStore),
	}
}

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

func (s *Service) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	metaStore, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", name)
	}
	return metaStore, nil
}

func (s *Service) ListMetadataStores() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.registry))
	for name := range s.registry {
		names = append(names, name)
	}
	return names
}

func (s *Service) CountMetadataStores() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}

// CloseMetadataStores closes all stores that implement io.Closer.
func (s *Service) CloseMetadataStores() {
	s.mu.RLock()
	snapshot := make(map[string]metadata.MetadataStore, len(s.registry))
	maps.Copy(snapshot, s.registry)
	s.mu.RUnlock()

	for name, store := range snapshot {
		if closer, ok := store.(io.Closer); ok {
			logger.Debug("Closing metadata store", "store", name)
			if err := closer.Close(); err != nil {
				logger.Error("Metadata store close error", "store", name, "error", err)
			}
		}
	}
}
