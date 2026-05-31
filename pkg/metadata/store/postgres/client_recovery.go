package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// PostgreSQL ClientRecoveryStore Implementation
// ============================================================================

// postgresRecoveryStore implements lock.ClientRecoveryStore using PostgreSQL.
//
// Storage Model:
//   - v4_client_recovery table: stores V4ClientRecoveryRecord
//   - keyed by clientid_string (server-global; share is not part of the key)
//
// Thread Safety:
// All operations are single statements executed against the connection pool.
type postgresRecoveryStore struct {
	pool *pgxpool.Pool
}

// newPostgresRecoveryStore creates a new PostgreSQL client recovery store.
func newPostgresRecoveryStore(pool *pgxpool.Pool) *postgresRecoveryStore {
	return &postgresRecoveryStore{pool: pool}
}

// PutClientRecovery stores or replaces the record for a confirmed client using
// an upsert keyed by clientid_string (latest wins, one row).
//
// Put REPLACES the full record, reclaim_complete included, to stay byte-for-byte
// identical to the memory/badger backends (which overwrite the whole struct).
// Semantically a re-confirm means the client's state is fresh, so reclaim starts
// over; callers persist reclaim completion through RecordReclaimComplete, not
// through Put.
func (s *postgresRecoveryStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	query := `
		INSERT INTO v4_client_recovery (
			clientid_string, clientid, boot_verifier, principal,
			confirmed_at, server_epoch, reclaim_complete
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (clientid_string) DO UPDATE SET
			clientid = EXCLUDED.clientid,
			boot_verifier = EXCLUDED.boot_verifier,
			principal = EXCLUDED.principal,
			confirmed_at = EXCLUDED.confirmed_at,
			server_epoch = EXCLUDED.server_epoch,
			reclaim_complete = EXCLUDED.reclaim_complete
	`

	_, err := s.pool.Exec(ctx, query,
		rec.ClientIDString,
		int64(rec.ClientID),
		rec.BootVerifier[:], // [8]byte -> BYTEA
		rec.Principal,
		rec.ConfirmedAt,
		int64(rec.ServerEpoch),
		rec.ReclaimComplete,
	)
	return err
}

// DeleteClientRecovery removes the record for a client.
func (s *postgresRecoveryStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	query := `DELETE FROM v4_client_recovery WHERE clientid_string = $1`
	_, err := s.pool.Exec(ctx, query, clientIDString)
	return err
}

// scanRecoveryRecord scans one row into a V4ClientRecoveryRecord, copying the
// BYTEA boot_verifier into the fixed [8]byte array.
func scanRecoveryRecord(row pgx.Row) (*lock.V4ClientRecoveryRecord, error) {
	var rec lock.V4ClientRecoveryRecord
	var clientID, serverEpoch int64
	var bootVerifier []byte

	err := row.Scan(
		&rec.ClientIDString,
		&clientID,
		&bootVerifier,
		&rec.Principal,
		&rec.ConfirmedAt,
		&serverEpoch,
		&rec.ReclaimComplete,
	)
	if err != nil {
		return nil, err
	}

	rec.ClientID = uint64(clientID)
	rec.ServerEpoch = uint64(serverEpoch)
	if len(bootVerifier) == 8 {
		copy(rec.BootVerifier[:], bootVerifier)
	}
	return &rec, nil
}

// ListClientRecovery returns all stored records.
func (s *postgresRecoveryStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	query := `
		SELECT clientid_string, clientid, boot_verifier, principal,
		       confirmed_at, server_epoch, reclaim_complete
		FROM v4_client_recovery
		ORDER BY confirmed_at
	`

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]*lock.V4ClientRecoveryRecord, 0)
	for rows.Next() {
		rec, err := scanRecoveryRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// RecordReclaimComplete marks the client's record reclaim-complete. A missing
// record is a no-op (RowsAffected == 0), not an error.
func (s *postgresRecoveryStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	query := `UPDATE v4_client_recovery SET reclaim_complete = TRUE WHERE clientid_string = $1`
	_, err := s.pool.Exec(ctx, query, clientIDString)
	return err
}

// ============================================================================
// PostgresMetadataStore ClientRecoveryStore Integration
// ============================================================================

// Ensure PostgresMetadataStore implements ClientRecoveryStore.
var _ lock.ClientRecoveryStore = (*PostgresMetadataStore)(nil)

// getRecoveryStore returns the recovery store, initializing if needed.
func (s *PostgresMetadataStore) getRecoveryStore() *postgresRecoveryStore {
	s.recoveryStoreMu.Lock()
	defer s.recoveryStoreMu.Unlock()
	if s.recoveryStore == nil {
		s.recoveryStore = newPostgresRecoveryStore(s.pool)
	}
	return s.recoveryStore
}

// PutClientRecovery stores or replaces a client recovery record.
func (s *PostgresMetadataStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	return s.getRecoveryStore().PutClientRecovery(ctx, rec)
}

// DeleteClientRecovery removes a client recovery record.
func (s *PostgresMetadataStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	return s.getRecoveryStore().DeleteClientRecovery(ctx, clientIDString)
}

// ListClientRecovery returns all stored client recovery records.
func (s *PostgresMetadataStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	return s.getRecoveryStore().ListClientRecovery(ctx)
}

// RecordReclaimComplete marks a client's recovery record reclaim-complete.
func (s *PostgresMetadataStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	return s.getRecoveryStore().RecordReclaimComplete(ctx, clientIDString)
}

// ClientRecoveryStore returns this store as a ClientRecoveryStore.
// This allows direct access to the interface for handler initialization.
func (s *PostgresMetadataStore) ClientRecoveryStore() lock.ClientRecoveryStore {
	return s
}
