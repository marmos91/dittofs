package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// postgresDurableStore implements lock.DurableHandleStore using PostgreSQL.
type postgresDurableStore struct {
	pool *pgxpool.Pool
}

func newPostgresDurableStore(pool *pgxpool.Pool) *postgresDurableStore {
	return &postgresDurableStore{
		pool: pool,
	}
}

const durableHandleColumns = `
	id, file_id, path, share_name, desired_access, share_access,
	create_options, metadata_handle, payload_id, oplock_level,
	lease_key, lease_state, create_guid, app_instance_id,
	username, session_key_hash, is_v2, created_at, disconnected_at,
	timeout_ms, server_start_time
`

func scanDurableHandle(row pgx.Row) (*lock.PersistedDurableHandle, error) {
	var h lock.PersistedDurableHandle
	var fileIDBytes, leaseKeyBytes, createGuidBytes, appInstanceIdBytes, sessionKeyHashBytes []byte

	err := row.Scan(
		&h.ID,
		&fileIDBytes,
		&h.Path,
		&h.ShareName,
		&h.DesiredAccess,
		&h.ShareAccess,
		&h.CreateOptions,
		&h.MetadataHandle,
		&h.PayloadID,
		&h.OplockLevel,
		&leaseKeyBytes,
		&h.LeaseState,
		&createGuidBytes,
		&appInstanceIdBytes,
		&h.Username,
		&sessionKeyHashBytes,
		&h.IsV2,
		&h.CreatedAt,
		&h.DisconnectedAt,
		&h.TimeoutMs,
		&h.ServerStartTime,
	)

	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	copyFixedByteArrays(&h, fileIDBytes, leaseKeyBytes, createGuidBytes,
		appInstanceIdBytes, sessionKeyHashBytes)
	return &h, nil
}

func scanDurableHandleRows(rows pgx.Rows) ([]*lock.PersistedDurableHandle, error) {
	defer rows.Close()

	var result []*lock.PersistedDurableHandle
	for rows.Next() {
		var h lock.PersistedDurableHandle
		var fileIDBytes, leaseKeyBytes, createGuidBytes, appInstanceIdBytes, sessionKeyHashBytes []byte

		err := rows.Scan(
			&h.ID,
			&fileIDBytes,
			&h.Path,
			&h.ShareName,
			&h.DesiredAccess,
			&h.ShareAccess,
			&h.CreateOptions,
			&h.MetadataHandle,
			&h.PayloadID,
			&h.OplockLevel,
			&leaseKeyBytes,
			&h.LeaseState,
			&createGuidBytes,
			&appInstanceIdBytes,
			&h.Username,
			&sessionKeyHashBytes,
			&h.IsV2,
			&h.CreatedAt,
			&h.DisconnectedAt,
			&h.TimeoutMs,
			&h.ServerStartTime,
		)
		if err != nil {
			return nil, err
		}

		copyFixedByteArrays(&h, fileIDBytes, leaseKeyBytes, createGuidBytes, appInstanceIdBytes, sessionKeyHashBytes)
		result = append(result, &h)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func copyFixedByteArrays(h *lock.PersistedDurableHandle, fileID, leaseKey, createGuid, appInstanceId, sessionKeyHash []byte) {
	if len(fileID) == 16 {
		copy(h.FileID[:], fileID)
	}
	if len(leaseKey) == 16 {
		copy(h.LeaseKey[:], leaseKey)
	}
	if len(createGuid) == 16 {
		copy(h.CreateGuid[:], createGuid)
	}
	if len(appInstanceId) == 16 {
		copy(h.AppInstanceId[:], appInstanceId)
	}
	if len(sessionKeyHash) == 32 {
		copy(h.SessionKeyHash[:], sessionKeyHash)
	}
}

// nullableBytes16 returns nil for zero-value [16]byte arrays (stored as NULL in PostgreSQL).
func nullableBytes16(b [16]byte) []byte {
	var zero [16]byte
	if b == zero {
		return nil
	}
	return b[:]
}

func (s *postgresDurableStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	query := `
		INSERT INTO durable_handles (
			id, file_id, path, share_name, desired_access, share_access,
			create_options, metadata_handle, payload_id, oplock_level,
			lease_key, lease_state, create_guid, app_instance_id,
			username, session_key_hash, is_v2, created_at, disconnected_at,
			timeout_ms, server_start_time
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		ON CONFLICT (id) DO UPDATE SET
			file_id = EXCLUDED.file_id,
			path = EXCLUDED.path,
			share_name = EXCLUDED.share_name,
			desired_access = EXCLUDED.desired_access,
			share_access = EXCLUDED.share_access,
			create_options = EXCLUDED.create_options,
			metadata_handle = EXCLUDED.metadata_handle,
			payload_id = EXCLUDED.payload_id,
			oplock_level = EXCLUDED.oplock_level,
			lease_key = EXCLUDED.lease_key,
			lease_state = EXCLUDED.lease_state,
			create_guid = EXCLUDED.create_guid,
			app_instance_id = EXCLUDED.app_instance_id,
			username = EXCLUDED.username,
			session_key_hash = EXCLUDED.session_key_hash,
			is_v2 = EXCLUDED.is_v2,
			created_at = EXCLUDED.created_at,
			disconnected_at = EXCLUDED.disconnected_at,
			timeout_ms = EXCLUDED.timeout_ms,
			server_start_time = EXCLUDED.server_start_time
	`

	_, err := s.pool.Exec(ctx, query,
		handle.ID,
		handle.FileID[:],
		handle.Path,
		handle.ShareName,
		handle.DesiredAccess,
		handle.ShareAccess,
		handle.CreateOptions,
		handle.MetadataHandle,
		handle.PayloadID,
		handle.OplockLevel,
		nullableBytes16(handle.LeaseKey),
		handle.LeaseState,
		nullableBytes16(handle.CreateGuid),
		nullableBytes16(handle.AppInstanceId),
		handle.Username,
		handle.SessionKeyHash[:],
		handle.IsV2,
		handle.CreatedAt,
		handle.DisconnectedAt,
		handle.TimeoutMs,
		handle.ServerStartTime,
	)
	return err
}

func (s *postgresDurableStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles WHERE id = $1`
	return scanDurableHandle(s.pool.QueryRow(ctx, query, id))
}

func (s *postgresDurableStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles WHERE file_id = $1 LIMIT 1`
	return scanDurableHandle(s.pool.QueryRow(ctx, query, fileID[:]))
}

func (s *postgresDurableStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles WHERE create_guid = $1 LIMIT 1`
	return scanDurableHandle(s.pool.QueryRow(ctx, query, createGuid[:]))
}

func (s *postgresDurableStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles WHERE app_instance_id = $1 ORDER BY created_at`
	rows, err := s.pool.Query(ctx, query, appInstanceId[:])
	if err != nil {
		return nil, err
	}
	return scanDurableHandleRows(rows)
}

func (s *postgresDurableStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles WHERE metadata_handle = $1 ORDER BY created_at`
	rows, err := s.pool.Query(ctx, query, fileHandle)
	if err != nil {
		return nil, err
	}
	return scanDurableHandleRows(rows)
}

func (s *postgresDurableStore) DeleteDurableHandle(ctx context.Context, id string) error {
	query := `DELETE FROM durable_handles WHERE id = $1`
	_, err := s.pool.Exec(ctx, query, id)
	return err
}

func (s *postgresDurableStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles ORDER BY created_at`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return scanDurableHandleRows(rows)
}

func (s *postgresDurableStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	query := `SELECT ` + durableHandleColumns + ` FROM durable_handles WHERE share_name = $1 ORDER BY created_at`
	rows, err := s.pool.Query(ctx, query, shareName)
	if err != nil {
		return nil, err
	}
	return scanDurableHandleRows(rows)
}

func (s *postgresDurableStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	query := `
		DELETE FROM durable_handles
		WHERE disconnected_at + (timeout_ms || ' milliseconds')::interval <= $1
	`
	result, err := s.pool.Exec(ctx, query, now)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// PostgresMetadataStore DurableHandleStore delegation

var _ lock.DurableHandleStore = (*PostgresMetadataStore)(nil)

func (s *PostgresMetadataStore) getDurableStore() *postgresDurableStore {
	s.durableStoreMu.Lock()
	defer s.durableStoreMu.Unlock()
	if s.durableStore == nil {
		s.durableStore = newPostgresDurableStore(s.pool)
	}
	return s.durableStore
}

func (s *PostgresMetadataStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	return s.getDurableStore().PutDurableHandle(ctx, handle)
}

func (s *PostgresMetadataStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandle(ctx, id)
}

func (s *PostgresMetadataStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandleByFileID(ctx, fileID)
}

func (s *PostgresMetadataStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandleByCreateGuid(ctx, createGuid)
}

func (s *PostgresMetadataStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandlesByAppInstanceId(ctx, appInstanceId)
}

func (s *PostgresMetadataStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().GetDurableHandlesByFileHandle(ctx, fileHandle)
}

func (s *PostgresMetadataStore) DeleteDurableHandle(ctx context.Context, id string) error {
	return s.getDurableStore().DeleteDurableHandle(ctx, id)
}

func (s *PostgresMetadataStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().ListDurableHandles(ctx)
}

func (s *PostgresMetadataStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	return s.getDurableStore().ListDurableHandlesByShare(ctx, shareName)
}

func (s *PostgresMetadataStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	return s.getDurableStore().DeleteExpiredDurableHandles(ctx, now)
}

// DurableHandleStore returns this store as a DurableHandleStore.
func (s *PostgresMetadataStore) DurableHandleStore() lock.DurableHandleStore {
	return s
}
