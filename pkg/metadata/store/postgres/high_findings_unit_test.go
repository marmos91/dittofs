package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestStoreError_Unwrap_IsRetryable proves that a StoreError carrying a
// *pgconn.PgError as its Cause exposes that PgError through errors.As, which is
// exactly what isRetryableError relies on to detect serialization/deadlock
// failures after mapPgError has converted the raw error.
func TestStoreError_Unwrap_IsRetryable(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "40001"}
	se := &metadata.StoreError{
		Code:    metadata.ErrIOError,
		Message: "transaction conflict, retry",
		Cause:   pgErr,
	}

	var found *pgconn.PgError
	if !errors.As(se, &found) {
		t.Fatal("errors.As(*pgconn.PgError) returned false — StoreError.Unwrap not wired")
	}
	if found.Code != "40001" {
		t.Fatalf("found PgError.Code = %q, want %q", found.Code, "40001")
	}

	// And the store-layer retry detector must agree.
	if !isRetryableError(se) {
		t.Fatal("isRetryableError(StoreError{Cause: 40001}) = false, want true")
	}
}

// TestStoreError_Unwrap_DeadlockRetryable mirrors the above for deadlocks.
func TestStoreError_Unwrap_DeadlockRetryable(t *testing.T) {
	se := mapPgError(&pgconn.PgError{Code: "40P01"}, "PutFile", "")
	if !isRetryableError(se) {
		t.Fatalf("isRetryableError(mapped 40P01) = false, want true (err=%v)", se)
	}
}

// errAfterNRows is a minimal pgx.Rows fake that yields n rows then reports err
// from Err(). Only Next() and Err() carry behaviour; the rest are no-ops so the
// type satisfies the pgx.Rows interface.
type errAfterNRows struct {
	n    int
	seen int
	err  error
}

func (r *errAfterNRows) Next() bool                    { r.seen++; return r.seen <= r.n }
func (r *errAfterNRows) Err() error                    { return r.err }
func (r *errAfterNRows) Close()                        {}
func (r *errAfterNRows) Scan(dest ...any) error        { return nil }
func (r *errAfterNRows) Values() ([]any, error)        { return nil, nil }
func (r *errAfterNRows) RawValues() [][]byte           { return nil }
func (r *errAfterNRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *errAfterNRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *errAfterNRows) Conn() *pgx.Conn { return nil }

// TestPoolRows_ErrPropagated proves poolRows.Err() delegates to the wrapped
// rows, so the rows.Err() checks added to ListChildren/ListShares actually
// surface a mid-stream failure rather than silently truncating the result.
func TestPoolRows_ErrPropagated(t *testing.T) {
	sentinel := errors.New("mid-stream network error")
	wrapped := &poolRows{rows: &errAfterNRows{n: 0, err: sentinel}}

	for wrapped.Next() { //nolint:revive // draining the zero-row iteration
	}

	if got := wrapped.Err(); !errors.Is(got, sentinel) {
		t.Fatalf("poolRows.Err() = %v, want sentinel %v", got, sentinel)
	}
}
