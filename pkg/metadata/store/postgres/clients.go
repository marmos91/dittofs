package postgres

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// PostgreSQL ClientRegistrationStore Implementation
// ============================================================================

// postgresClientStore implements lock.ClientRegistrationStore using PostgreSQL.
//
// This implementation is suitable for:
//   - Production deployments requiring NSM registration persistence
//   - Distributed multi-node servers for crash recovery
//
// Storage Model:
//   - nsm_client_registrations table: stores PersistedClientRegistration
//   - Indexes on callback_host, registered_at, mon_name
//
// Thread Safety:
// All operations use database transactions for atomicity.
type postgresClientStore struct {
	pool *pgxpool.Pool
	mu   sync.RWMutex
}

// newPostgresClientStore creates a new PostgreSQL client registration store.
func newPostgresClientStore(pool *pgxpool.Pool) *postgresClientStore {
	return &postgresClientStore{
		pool: pool,
	}
}

// PutClientRegistration stores or updates a client registration using upsert.
func (s *postgresClientStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	query := `
		INSERT INTO nsm_client_registrations (
			client_id, mon_name, priv, callback_host, callback_prog,
			callback_vers, callback_proc, registered_at, server_epoch
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (client_id) DO UPDATE SET
			mon_name = EXCLUDED.mon_name,
			priv = EXCLUDED.priv,
			callback_host = EXCLUDED.callback_host,
			callback_prog = EXCLUDED.callback_prog,
			callback_vers = EXCLUDED.callback_vers,
			callback_proc = EXCLUDED.callback_proc,
			registered_at = EXCLUDED.registered_at,
			server_epoch = EXCLUDED.server_epoch
	`

	_, err := s.pool.Exec(ctx, query,
		reg.ClientID,
		reg.MonName,
		reg.Priv[:], // Convert [16]byte to slice
		reg.CallbackHost,
		reg.CallbackProg,
		reg.CallbackVers,
		reg.CallbackProc,
		reg.RegisteredAt,
		reg.ServerEpoch,
	)
	return err
}

// putClientRegistrationTx stores a registration within an existing transaction.
func (s *postgresClientStore) putClientRegistrationTx(ctx context.Context, tx pgx.Tx, reg *lock.PersistedClientRegistration) error {
	query := `
		INSERT INTO nsm_client_registrations (
			client_id, mon_name, priv, callback_host, callback_prog,
			callback_vers, callback_proc, registered_at, server_epoch
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (client_id) DO UPDATE SET
			mon_name = EXCLUDED.mon_name,
			priv = EXCLUDED.priv,
			callback_host = EXCLUDED.callback_host,
			callback_prog = EXCLUDED.callback_prog,
			callback_vers = EXCLUDED.callback_vers,
			callback_proc = EXCLUDED.callback_proc,
			registered_at = EXCLUDED.registered_at,
			server_epoch = EXCLUDED.server_epoch
	`

	_, err := tx.Exec(ctx, query,
		reg.ClientID,
		reg.MonName,
		reg.Priv[:],
		reg.CallbackHost,
		reg.CallbackProg,
		reg.CallbackVers,
		reg.CallbackProc,
		reg.RegisteredAt,
		reg.ServerEpoch,
	)
	return err
}

// GetClientRegistration retrieves a registration by client ID.
func (s *postgresClientStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	query := `
		SELECT client_id, mon_name, priv, callback_host, callback_prog,
		       callback_vers, callback_proc, registered_at, server_epoch
		FROM nsm_client_registrations
		WHERE client_id = $1
	`

	var reg lock.PersistedClientRegistration
	var privBytes []byte

	err := s.pool.QueryRow(ctx, query, clientID).Scan(
		&reg.ClientID,
		&reg.MonName,
		&privBytes,
		&reg.CallbackHost,
		&reg.CallbackProg,
		&reg.CallbackVers,
		&reg.CallbackProc,
		&reg.RegisteredAt,
		&reg.ServerEpoch,
	)

	if err == pgx.ErrNoRows {
		return nil, nil // Not found returns nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Copy priv bytes to fixed array
	if len(privBytes) == 16 {
		copy(reg.Priv[:], privBytes)
	}

	return &reg, nil
}

// DeleteClientRegistration removes a registration by client ID.
func (s *postgresClientStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	query := `DELETE FROM nsm_client_registrations WHERE client_id = $1`
	_, err := s.pool.Exec(ctx, query, clientID)
	return err
}

// deleteClientRegistrationTx removes a registration within an existing transaction.
func (s *postgresClientStore) deleteClientRegistrationTx(ctx context.Context, tx pgx.Tx, clientID string) error {
	query := `DELETE FROM nsm_client_registrations WHERE client_id = $1`
	_, err := tx.Exec(ctx, query, clientID)
	return err
}

// ListClientRegistrations returns all stored registrations.
func (s *postgresClientStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	query := `
		SELECT client_id, mon_name, priv, callback_host, callback_prog,
		       callback_vers, callback_proc, registered_at, server_epoch
		FROM nsm_client_registrations
		ORDER BY registered_at
	`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*lock.PersistedClientRegistration
	for rows.Next() {
		var reg lock.PersistedClientRegistration
		var privBytes []byte

		err := rows.Scan(
			&reg.ClientID,
			&reg.MonName,
			&privBytes,
			&reg.CallbackHost,
			&reg.CallbackProg,
			&reg.CallbackVers,
			&reg.CallbackProc,
			&reg.RegisteredAt,
			&reg.ServerEpoch,
		)
		if err != nil {
			return nil, err
		}

		if len(privBytes) == 16 {
			copy(reg.Priv[:], privBytes)
		}

		result = append(result, &reg)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

// DeleteAllClientRegistrations removes all registrations.
func (s *postgresClientStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	query := `DELETE FROM nsm_client_registrations`
	result, err := s.pool.Exec(ctx, query)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *postgresClientStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	query := `DELETE FROM nsm_client_registrations WHERE mon_name = $1`
	result, err := s.pool.Exec(ctx, query, monName)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// ============================================================================
// PostgresMetadataStore ClientRegistrationStore Integration
// ============================================================================

// Ensure PostgresMetadataStore implements ClientRegistrationStore
var _ lock.ClientRegistrationStore = (*PostgresMetadataStore)(nil)

// getClientStore returns the client store, initializing if needed.
func (s *PostgresMetadataStore) getClientStore() *postgresClientStore {
	s.clientStoreMu.Lock()
	defer s.clientStoreMu.Unlock()
	if s.clientStore == nil {
		s.clientStore = newPostgresClientStore(s.pool)
	}
	return s.clientStore
}

// PutClientRegistration stores or updates a client registration.
func (s *PostgresMetadataStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	return s.getClientStore().PutClientRegistration(ctx, reg)
}

// GetClientRegistration retrieves a registration by client ID.
func (s *PostgresMetadataStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	return s.getClientStore().GetClientRegistration(ctx, clientID)
}

// DeleteClientRegistration removes a registration by client ID.
func (s *PostgresMetadataStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	return s.getClientStore().DeleteClientRegistration(ctx, clientID)
}

// ListClientRegistrations returns all stored registrations.
func (s *PostgresMetadataStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	return s.getClientStore().ListClientRegistrations(ctx)
}

// DeleteAllClientRegistrations removes all registrations.
func (s *PostgresMetadataStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	return s.getClientStore().DeleteAllClientRegistrations(ctx)
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *PostgresMetadataStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	return s.getClientStore().DeleteClientRegistrationsByMonName(ctx, monName)
}

// ClientRegistrationStore returns this store as a ClientRegistrationStore.
// This allows direct access to the interface for handler initialization.
func (s *PostgresMetadataStore) ClientRegistrationStore() lock.ClientRegistrationStore {
	return s
}
