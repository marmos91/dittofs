package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// PostgresMetadataStore ClientRecoveryStore Integration
// ============================================================================

// Ensure PostgresMetadataStore implements ClientRecoveryStore.
var _ lock.ClientRecoveryStore = (*PostgresMetadataStore)(nil)

// PutClientRecovery stores or replaces a client recovery record.
func (s *PostgresMetadataStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	return s.recoveryStore.PutClientRecovery(ctx, rec)
}

// DeleteClientRecovery removes a client recovery record.
func (s *PostgresMetadataStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	return s.recoveryStore.DeleteClientRecovery(ctx, clientIDString)
}

// ListClientRecovery returns all stored client recovery records.
func (s *PostgresMetadataStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	return s.recoveryStore.ListClientRecovery(ctx)
}

// RecordReclaimComplete marks a client's recovery record reclaim-complete.
func (s *PostgresMetadataStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	return s.recoveryStore.RecordReclaimComplete(ctx, clientIDString)
}

// ClientRecoveryStore returns this store as a ClientRecoveryStore.
// This allows direct access to the interface for handler initialization.
func (s *PostgresMetadataStore) ClientRecoveryStore() lock.ClientRecoveryStore {
	return s
}
