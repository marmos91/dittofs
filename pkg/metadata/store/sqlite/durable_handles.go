package sqlite

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// sqliteDurableStore is the shared durable-handle store plus the one operation
// that cannot be shared: expiring handles, which sqlite has to compute in Go.
type sqliteDurableStore struct {
	*storesql.DurableStore
}

func newSQLiteDurableStore(x storesql.Executor) *sqliteDurableStore {
	return &sqliteDurableStore{DurableStore: &storesql.DurableStore{X: x, D: sqliteDialect}}
}

// DeleteExpiredDurableHandles removes every handle whose timeout has elapsed.
//
// SQLite has no interval arithmetic, and modernc stores a time.Time in a
// textual layout SQLite's own date functions cannot parse, so the deadline is
// computed in Go: a handle is expired when disconnected_at + timeout_ms is at
// or before now. The candidate tuples are read first and the expired ids
// deleted after; both statements run on the same single-writer connection, so
// no handle can change its expiry in between.
func (s *sqliteDurableStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.X.Query(ctx, s.D.Durable().ExpiryCandidates)
	if err != nil {
		return 0, err
	}

	var expired []string
	for rows.Next() {
		var id string
		var disconnectedAt time.Time
		var timeoutMS int64
		if err := rows.Scan(&id, &disconnectedAt, &timeoutMS); err != nil {
			rows.Close()
			return 0, err
		}
		if !disconnectedAt.Add(time.Duration(timeoutMS) * time.Millisecond).After(now) {
			expired = append(expired, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range expired {
		if _, err := s.X.Exec(ctx, s.D.Durable().DeleteByID, id); err != nil {
			return 0, err
		}
	}

	return len(expired), nil
}

// SQLiteMetadataStore DurableHandleStore delegation

var _ lock.DurableHandleStore = (*SQLiteMetadataStore)(nil)

func (s *SQLiteMetadataStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	return s.durableStore.PutDurableHandle(ctx, handle)
}

func (s *SQLiteMetadataStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandle(ctx, id)
}

func (s *SQLiteMetadataStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandleByFileID(ctx, fileID)
}

func (s *SQLiteMetadataStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandleByCreateGuid(ctx, createGuid)
}

func (s *SQLiteMetadataStore) ConsumeDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.durableStore.ConsumeDurableHandle(ctx, id)
}

func (s *SQLiteMetadataStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandlesByAppInstanceId(ctx, appInstanceId)
}

func (s *SQLiteMetadataStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.GetDurableHandlesByFileHandle(ctx, fileHandle)
}

func (s *SQLiteMetadataStore) DeleteDurableHandle(ctx context.Context, id string) error {
	return s.durableStore.DeleteDurableHandle(ctx, id)
}

func (s *SQLiteMetadataStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.ListDurableHandles(ctx)
}

func (s *SQLiteMetadataStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	return s.durableStore.ListDurableHandlesByShare(ctx, shareName)
}

func (s *SQLiteMetadataStore) DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error) {
	return s.durableStore.DeleteExpiredDurableHandles(ctx, now)
}

// DurableHandleStore returns this store as a DurableHandleStore.
func (s *SQLiteMetadataStore) DurableHandleStore() lock.DurableHandleStore {
	return s
}
