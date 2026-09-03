package postgres

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// postgresDurableStore is the shared durable-handle store plus the one
// operation that cannot be shared: expiring handles, which postgres does in a
// single statement with interval arithmetic.
type postgresDurableStore struct {
	*storesql.DurableStore
}

func newPostgresDurableStore(st *PostgresMetadataStore) *postgresDurableStore {
	return &postgresDurableStore{
		DurableStore: &storesql.DurableStore{X: poolExecer{s: st}, D: pgDialect},
	}
}

// DeleteExpiredDurableHandles removes every handle whose timeout has elapsed.
func (s *postgresDurableStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	result, err := s.X.Exec(ctx, s.D.Durable().DeleteExpired, now)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// PostgresMetadataStore DurableHandleStore delegation

var _ lock.DurableHandleStore = (*PostgresMetadataStore)(nil)

func (s *PostgresMetadataStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	return s.durableStore.PutDurableHandle(ctx, handle)
}

func (s *PostgresMetadataStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandle(ctx, id)
}

func (s *PostgresMetadataStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandleByFileID(ctx, fileID)
}

func (s *PostgresMetadataStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandleByCreateGuid(ctx, createGuid)
}

func (s *PostgresMetadataStore) ConsumeDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.ConsumeDurableHandle(ctx, id)
}

func (s *PostgresMetadataStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandlesByAppInstanceId(ctx, appInstanceId)
}

func (s *PostgresMetadataStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandlesByFileHandle(ctx, fileHandle)
}

func (s *PostgresMetadataStore) DeleteDurableHandle(ctx context.Context, id string) error {
	return s.durableStore.DeleteDurableHandle(ctx, id)
}

func (s *PostgresMetadataStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.ListDurableHandles(ctx)
}

func (s *PostgresMetadataStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.ListDurableHandlesByShare(ctx, shareName)
}

func (s *PostgresMetadataStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	return s.durableStore.DeleteExpiredDurableHandles(ctx, now)
}

// DurableHandleStore returns this store as a DurableHandleStore.
func (s *PostgresMetadataStore) DurableHandleStore() lock.DurableHandleStore {
	return s
}
