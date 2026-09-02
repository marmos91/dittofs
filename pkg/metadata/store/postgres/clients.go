package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// PostgresMetadataStore ClientRegistrationStore Integration
// ============================================================================

// Ensure PostgresMetadataStore implements ClientRegistrationStore
var _ lock.ClientRegistrationStore = (*PostgresMetadataStore)(nil)

// PutClientRegistration stores or updates a client registration.
func (s *PostgresMetadataStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	return s.clientStore.PutClientRegistration(ctx, reg)
}

// GetClientRegistration retrieves a registration by client ID.
func (s *PostgresMetadataStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	return s.clientStore.GetClientRegistration(ctx, clientID)
}

// DeleteClientRegistration removes a registration by client ID.
func (s *PostgresMetadataStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	return s.clientStore.DeleteClientRegistration(ctx, clientID)
}

// ListClientRegistrations returns all stored registrations.
func (s *PostgresMetadataStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	return s.clientStore.ListClientRegistrations(ctx)
}

// DeleteAllClientRegistrations removes all registrations.
func (s *PostgresMetadataStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	return s.clientStore.DeleteAllClientRegistrations(ctx)
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *PostgresMetadataStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	return s.clientStore.DeleteClientRegistrationsByMonName(ctx, monName)
}

// ClientRegistrationStore returns this store as a ClientRegistrationStore.
// This allows direct access to the interface for handler initialization.
func (s *PostgresMetadataStore) ClientRegistrationStore() lock.ClientRegistrationStore {
	return s
}
