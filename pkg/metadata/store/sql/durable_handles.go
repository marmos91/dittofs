package sql

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DurableStore implements the dialect-independent half of
// lock.DurableHandleStore.
//
// Every read and every write here is the same on both dialects once the
// statement text is supplied. DeleteExpiredDurableHandles is not: postgres
// expires handles with interval arithmetic in one statement, while sqlite has
// to compute the deadline in Go, so each dialect keeps its own and embeds this
// type to pick up the rest.
type DurableStore struct {
	// X runs the statements. Never nil.
	X Executor
	// D supplies statement text and classifies driver errors. Never nil.
	D Dialect
}

// DurableQueries holds the durable-handle statements in one dialect's syntax.
//
// Every Get returns the full column list in the order ScanDurableHandle
// expects, so a dialect that reorders one of them silently mis-decodes every
// field after the first change.
type DurableQueries struct {
	// Put upserts one handle keyed by id.
	Put string
	// Get selects one handle by id. One parameter: the id.
	Get string
	// GetByFileID selects the lowest-id handle for a file. One parameter: the
	// 16-byte file id.
	GetByFileID string
	// GetByCreateGuid selects the lowest-id handle for a create guid. One
	// parameter: the 16-byte guid.
	GetByCreateGuid string
	// Consume deletes one handle by id and returns the row it removed, so a
	// caller cannot claim a handle another has already taken. One parameter:
	// the id.
	Consume string
	// ListByAppInstanceId selects every handle for an app instance. One
	// parameter: the 16-byte id.
	ListByAppInstanceId string
	// ListByFileHandle selects every handle for a metadata handle. One
	// parameter: the handle bytes.
	ListByFileHandle string
	// Delete removes one handle by id. One parameter: the id.
	Delete string
	// List selects every handle, oldest first. No parameters.
	List string
	// ListByShare selects every handle in a share, oldest first. One
	// parameter: the share name.
	ListByShare string
	// DeleteByID removes one handle by id, used by the sqlite expiry sweep.
	// One parameter: the id.
	DeleteByID string
	// ExpiryCandidates selects the id, disconnect time and timeout of every
	// handle, for the dialect that computes expiry outside the database. No
	// parameters.
	ExpiryCandidates string
	// DeleteExpired removes every handle whose timeout has elapsed, for the
	// dialect that can express that in SQL. One parameter: the current time.
	DeleteExpired string
}

// PutDurableHandle stores or replaces a durable handle.
func (s *DurableStore) PutDurableHandle(ctx context.Context, handle *lock.PersistedDurableHandle) error {
	_, err := s.X.Exec(ctx, s.D.Durable().Put, durableHandleArgs(handle)...)
	return err
}

// GetDurableHandle retrieves a handle by ID, reporting (nil, nil) when absent.
func (s *DurableStore) GetDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.scanOne(s.X.QueryRow(ctx, s.D.Durable().Get, id))
}

// GetDurableHandleByFileID retrieves a handle by SMB2 file ID.
func (s *DurableStore) GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.scanOne(s.X.QueryRow(ctx, s.D.Durable().GetByFileID, fileID[:]))
}

// GetDurableHandleByCreateGuid retrieves a handle by its create GUID.
func (s *DurableStore) GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*lock.PersistedDurableHandle, error) {
	return s.scanOne(s.X.QueryRow(ctx, s.D.Durable().GetByCreateGuid, createGuid[:]))
}

// ConsumeDurableHandle atomically fetches and removes a handle, so two
// reconnects racing the same handle cannot both be granted it.
func (s *DurableStore) ConsumeDurableHandle(ctx context.Context, id string) (*lock.PersistedDurableHandle, error) {
	return s.scanOne(s.X.QueryRow(ctx, s.D.Durable().Consume, id))
}

// GetDurableHandlesByAppInstanceId returns every handle for an app instance.
func (s *DurableStore) GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*lock.PersistedDurableHandle, error) {
	return s.scanMany(ctx, s.D.Durable().ListByAppInstanceId, appInstanceId[:])
}

// GetDurableHandlesByFileHandle returns every handle for a metadata handle.
func (s *DurableStore) GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*lock.PersistedDurableHandle, error) {
	return s.scanMany(ctx, s.D.Durable().ListByFileHandle, fileHandle)
}

// DeleteDurableHandle removes a handle by ID.
func (s *DurableStore) DeleteDurableHandle(ctx context.Context, id string) error {
	_, err := s.X.Exec(ctx, s.D.Durable().Delete, id)
	return err
}

// ListDurableHandles returns every stored handle.
func (s *DurableStore) ListDurableHandles(ctx context.Context) ([]*lock.PersistedDurableHandle, error) {
	return s.scanMany(ctx, s.D.Durable().List)
}

// ListDurableHandlesByShare returns every handle in one share.
func (s *DurableStore) ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*lock.PersistedDurableHandle, error) {
	return s.scanMany(ctx, s.D.Durable().ListByShare, shareName)
}

func (s *DurableStore) scanOne(row Row) (*lock.PersistedDurableHandle, error) {
	h, err := ScanDurableHandle(row.Scan)
	if s.D.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (s *DurableStore) scanMany(ctx context.Context, query string, args ...any) ([]*lock.PersistedDurableHandle, error) {
	rows, err := s.X.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*lock.PersistedDurableHandle
	for rows.Next() {
		h, err := ScanDurableHandle(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
