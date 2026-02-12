package memory

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Memory ClientRegistrationStore Implementation
// ============================================================================

// memoryClientStore implements lock.ClientRegistrationStore using in-memory storage.
//
// This implementation is suitable for:
//   - Testing and development environments
//   - Ephemeral deployments where client registration persistence is not required
//
// Thread Safety:
// All operations are protected by a read-write mutex, making the store
// safe for concurrent access from multiple goroutines.
type memoryClientStore struct {
	mu sync.RWMutex

	// registrations maps client ID to PersistedClientRegistration
	registrations map[string]*lock.PersistedClientRegistration
}

// newMemoryClientStore creates a new in-memory client registration store.
func newMemoryClientStore() *memoryClientStore {
	return &memoryClientStore{
		registrations: make(map[string]*lock.PersistedClientRegistration),
	}
}

// PutClientRegistration stores or updates a client registration.
func (s *memoryClientStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Clone the registration to prevent external modifications
	stored := &lock.PersistedClientRegistration{
		ClientID:     reg.ClientID,
		MonName:      reg.MonName,
		Priv:         reg.Priv,
		CallbackHost: reg.CallbackHost,
		CallbackProg: reg.CallbackProg,
		CallbackVers: reg.CallbackVers,
		CallbackProc: reg.CallbackProc,
		RegisteredAt: reg.RegisteredAt,
		ServerEpoch:  reg.ServerEpoch,
	}

	s.registrations[reg.ClientID] = stored
	return nil
}

// GetClientRegistration retrieves a registration by client ID.
func (s *memoryClientStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	reg, exists := s.registrations[clientID]
	if !exists {
		return nil, nil // Not found returns nil, nil (not an error)
	}

	// Return a clone to prevent external modifications
	return cloneClientRegistration(reg), nil
}

// DeleteClientRegistration removes a registration by client ID.
func (s *memoryClientStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.registrations, clientID)
	return nil
}

// ListClientRegistrations returns all stored registrations.
func (s *memoryClientStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*lock.PersistedClientRegistration, 0, len(s.registrations))
	for _, reg := range s.registrations {
		result = append(result, cloneClientRegistration(reg))
	}
	return result, nil
}

// DeleteAllClientRegistrations removes all registrations.
func (s *memoryClientStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	count := len(s.registrations)
	s.registrations = make(map[string]*lock.PersistedClientRegistration)
	return count, nil
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *memoryClientStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for id, reg := range s.registrations {
		if reg.MonName == monName {
			delete(s.registrations, id)
			count++
		}
	}
	return count, nil
}

// cloneClientRegistration creates a deep copy of a client registration.
func cloneClientRegistration(reg *lock.PersistedClientRegistration) *lock.PersistedClientRegistration {
	if reg == nil {
		return nil
	}
	return &lock.PersistedClientRegistration{
		ClientID:     reg.ClientID,
		MonName:      reg.MonName,
		Priv:         reg.Priv,
		CallbackHost: reg.CallbackHost,
		CallbackProg: reg.CallbackProg,
		CallbackVers: reg.CallbackVers,
		CallbackProc: reg.CallbackProc,
		RegisteredAt: reg.RegisteredAt,
		ServerEpoch:  reg.ServerEpoch,
	}
}

// ============================================================================
// MemoryMetadataStore ClientRegistrationStore Integration
// ============================================================================

// Ensure MemoryMetadataStore implements ClientRegistrationStore
var _ lock.ClientRegistrationStore = (*MemoryMetadataStore)(nil)

// The memory store has a per-store client store (not global).
// This is initialized lazily via initClientStore().

// initClientStore ensures the client store is initialized.
// Must be called with the store's write lock held.
func (s *MemoryMetadataStore) initClientStore() {
	if s.clientStore == nil {
		s.clientStore = newMemoryClientStore()
	}
}

// PutClientRegistration stores or updates a client registration.
func (s *MemoryMetadataStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initClientStore()
	return s.clientStore.PutClientRegistration(ctx, reg)
}

// GetClientRegistration retrieves a registration by client ID.
func (s *MemoryMetadataStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.clientStore == nil {
		return nil, nil // Not found
	}
	return s.clientStore.GetClientRegistration(ctx, clientID)
}

// DeleteClientRegistration removes a registration by client ID.
func (s *MemoryMetadataStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientStore == nil {
		return nil // Nothing to delete
	}
	return s.clientStore.DeleteClientRegistration(ctx, clientID)
}

// ListClientRegistrations returns all stored registrations.
func (s *MemoryMetadataStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.clientStore == nil {
		return []*lock.PersistedClientRegistration{}, nil
	}
	return s.clientStore.ListClientRegistrations(ctx)
}

// DeleteAllClientRegistrations removes all registrations.
func (s *MemoryMetadataStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientStore == nil {
		return 0, nil
	}
	return s.clientStore.DeleteAllClientRegistrations(ctx)
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *MemoryMetadataStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientStore == nil {
		return 0, nil
	}
	return s.clientStore.DeleteClientRegistrationsByMonName(ctx, monName)
}

// ClientRegistrationStore returns this store as a ClientRegistrationStore.
// This allows direct access to the interface for handler initialization.
func (s *MemoryMetadataStore) ClientRegistrationStore() lock.ClientRegistrationStore {
	return s
}
