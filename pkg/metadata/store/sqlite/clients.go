package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// SQLite ClientRegistrationStore Implementation
// ============================================================================

// sqliteClientStore implements lock.ClientRegistrationStore using SQLite.
//
// It persists NSM registrations across restarts for crash recovery.
//
// Storage Model:
//   - nsm_client_registrations table: stores PersistedClientRegistration
//   - Indexes on callback_host, registered_at, mon_name
//
// Thread Safety:
// All operations use database transactions for atomicity.
type sqliteClientStore struct {
	pool execer
}

// newSQLiteClientStore creates a new SQLite client registration store.
func newSQLiteClientStore(pool execer) *sqliteClientStore {
	return &sqliteClientStore{
		pool: pool,
	}
}

// PutClientRegistration stores or updates a client registration using upsert.
func (s *sqliteClientStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	query := `
		INSERT INTO nsm_client_registrations (
			client_id, mon_name, priv, callback_host, callback_prog,
			callback_vers, callback_proc, registered_at, server_epoch
		)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
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

// GetClientRegistration retrieves a registration by client ID.
func (s *sqliteClientStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	query := `
		SELECT client_id, mon_name, priv, callback_host, callback_prog,
		       callback_vers, callback_proc, registered_at, server_epoch
		FROM nsm_client_registrations
		WHERE client_id = ?1
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

	if errors.Is(err, sql.ErrNoRows) {
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
func (s *sqliteClientStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	query := `DELETE FROM nsm_client_registrations WHERE client_id = ?1`
	_, err := s.pool.Exec(ctx, query, clientID)
	return err
}

// ListClientRegistrations returns all stored registrations.
func (s *sqliteClientStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
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
func (s *sqliteClientStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	query := `DELETE FROM nsm_client_registrations`
	result, err := s.pool.Exec(ctx, query)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *sqliteClientStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	query := `DELETE FROM nsm_client_registrations WHERE mon_name = ?1`
	result, err := s.pool.Exec(ctx, query, monName)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// ============================================================================
// SQLiteMetadataStore ClientRegistrationStore Integration
// ============================================================================

// Ensure SQLiteMetadataStore implements ClientRegistrationStore
var _ lock.ClientRegistrationStore = (*SQLiteMetadataStore)(nil)

// getClientStore returns the client store, initializing if needed.
func (s *SQLiteMetadataStore) getClientStore() *sqliteClientStore {
	s.clientStoreMu.Lock()
	defer s.clientStoreMu.Unlock()
	if s.clientStore == nil {
		s.clientStore = newSQLiteClientStore(s.conn())
	}
	return s.clientStore
}

// PutClientRegistration stores or updates a client registration.
func (s *SQLiteMetadataStore) PutClientRegistration(ctx context.Context, reg *lock.PersistedClientRegistration) error {
	return s.getClientStore().PutClientRegistration(ctx, reg)
}

// GetClientRegistration retrieves a registration by client ID.
func (s *SQLiteMetadataStore) GetClientRegistration(ctx context.Context, clientID string) (*lock.PersistedClientRegistration, error) {
	return s.getClientStore().GetClientRegistration(ctx, clientID)
}

// DeleteClientRegistration removes a registration by client ID.
func (s *SQLiteMetadataStore) DeleteClientRegistration(ctx context.Context, clientID string) error {
	return s.getClientStore().DeleteClientRegistration(ctx, clientID)
}

// ListClientRegistrations returns all stored registrations.
func (s *SQLiteMetadataStore) ListClientRegistrations(ctx context.Context) ([]*lock.PersistedClientRegistration, error) {
	return s.getClientStore().ListClientRegistrations(ctx)
}

// DeleteAllClientRegistrations removes all registrations.
func (s *SQLiteMetadataStore) DeleteAllClientRegistrations(ctx context.Context) (int, error) {
	return s.getClientStore().DeleteAllClientRegistrations(ctx)
}

// DeleteClientRegistrationsByMonName removes all registrations monitoring a specific host.
func (s *SQLiteMetadataStore) DeleteClientRegistrationsByMonName(ctx context.Context, monName string) (int, error) {
	return s.getClientStore().DeleteClientRegistrationsByMonName(ctx, monName)
}

// ClientRegistrationStore returns this store as a ClientRegistrationStore.
// This allows direct access to the interface for handler initialization.
func (s *SQLiteMetadataStore) ClientRegistrationStore() lock.ClientRegistrationStore {
	return s
}
