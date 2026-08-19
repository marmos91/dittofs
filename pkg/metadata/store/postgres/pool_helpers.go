package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poolConnectionAcquireTimeout is the maximum time to wait for a connection from the pool.
// This matches the timeout used in WithTransaction for consistency.
const poolConnectionAcquireTimeout = 10 * time.Second

// ============================================================================
// Pool Helper Methods
// ============================================================================
//
// These helpers wrap direct pool operations with a connection acquire timeout
// to prevent indefinite blocking when the pool is exhausted.
//
// The pgxpool library does NOT have a built-in acquire timeout configuration.
// When all connections are in use, pool.Query/QueryRow/Exec will block
// indefinitely unless the context has a timeout.
//
// The NFS handler context (from context.Background()) has no timeout,
// so without these helpers, any pool operation can hang forever under
// high concurrent load (e.g., POSIX compliance tests).
//
// All operations use the same poolConnectionAcquireTimeout (10s) as WithTransaction
// for consistency.

// acquireConn takes a connection from the pool under the shared acquire
// timeout, which scopes the wait only: the returned connection runs its
// statements on ctx. Callers MUST Release it.
func (s *PostgresMetadataStore) acquireConn(ctx context.Context) (*pgxpool.Conn, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer cancel()
	return s.pool.Acquire(acquireCtx)
}

// rowQuerier is the single-row query surface an open transaction (pgx.Tx.QueryRow)
// and the pool wrapper (PostgresMetadataStore.queryRow) both satisfy, so one
// query body can run on either.
type rowQuerier func(ctx context.Context, sql string, args ...any) pgx.Row

// acquireConn checks out a pooled connection under the shared acquire timeout.
// The timeout bounds ONLY the checkout: the returned connection is used with the
// caller's own context so a lazily-read result set is not cancelled the moment
// this function returns. A checkout that times out while the caller's context is
// still live is reported as pool exhaustion; anything else is mapped as a normal
// query error against op/sql. The caller owns releasing the connection.
func (s *PostgresMetadataStore) acquireConn(ctx context.Context, op, sql string) (*pgxpool.Conn, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer cancel()

	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		if acquireCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, fmt.Errorf("connection acquire timeout after %v: pool may be exhausted", poolConnectionAcquireTimeout)
		}
		return nil, mapPgError(err, op, sql)
	}
	return conn, nil
}

// queryRow executes a query that returns at most one row with connection acquire timeout.
// This prevents indefinite blocking when the pool is exhausted.
func (s *PostgresMetadataStore) queryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	// Check context before acquiring connection
	if ctx.Err() != nil {
		return &errorRow{err: ctx.Err()}
	}

	// pgx.QueryRow is lazy: the row is read off the wire inside the caller's
	// Scan(), bound to the context passed here. Run it on the parent ctx (not the
	// acquire context, which is cancelled the instant acquireConn returns) and
	// release the connection after Scan.
	conn, err := s.acquireConn(ctx, "queryRow", sql)
	if err != nil {
		return &errorRow{err: err}
	}

	return &poolRow{row: conn.QueryRow(ctx, sql, args...), conn: conn}
}

// query executes a query that returns rows with connection acquire timeout.
// This prevents indefinite blocking when the pool is exhausted.
// Caller MUST close the returned Rows when done.
func (s *PostgresMetadataStore) query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	// Check context before acquiring connection
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	conn, err := s.acquireConn(ctx, "query", sql)
	if err != nil {
		return nil, err
	}

	// Execute query
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		conn.Release()
		return nil, mapPgError(err, "query", sql)
	}

	// Wrap rows to release connection when closed
	return &poolRows{rows: rows, conn: conn}, nil
}

// exec executes a statement with connection acquire timeout.
// This prevents indefinite blocking when the pool is exhausted.
// Returns the command tag for checking rows affected.
func (s *PostgresMetadataStore) exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	// Check context before acquiring connection
	if err := ctx.Err(); err != nil {
		return pgconn.CommandTag{}, err
	}

	conn, err := s.acquireConn(ctx, "exec", sql)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()

	// Execute statement
	tag, err := conn.Exec(ctx, sql, args...)
	if err != nil {
		return pgconn.CommandTag{}, mapPgError(err, "exec", sql)
	}
	return tag, nil
}

// ============================================================================
// Helper Types for Connection Management
// ============================================================================

// errorRow implements pgx.Row for returning errors
type errorRow struct {
	err error
}

func (r *errorRow) Scan(dest ...any) error {
	return r.err
}

// poolRow wraps a single-row pgx.Row and releases the pooled connection once
// the caller scans it. The query runs on the caller's (parent) context, so the
// connection must stay checked out until Scan reads the row off the wire — at
// which point it is released exactly once.
type poolRow struct {
	row  pgx.Row
	conn *pgxpool.Conn
}

func (r *poolRow) Scan(dest ...any) error {
	defer r.conn.Release()
	return r.row.Scan(dest...)
}

// poolRows wraps pgx.Rows and releases the connection when closed
type poolRows struct {
	rows pgx.Rows
	conn *pgxpool.Conn
}

func (r *poolRows) Close() {
	r.rows.Close()
	r.conn.Release()
}

func (r *poolRows) Err() error {
	return r.rows.Err()
}

func (r *poolRows) Next() bool {
	return r.rows.Next()
}

func (r *poolRows) Scan(dest ...any) error {
	return r.rows.Scan(dest...)
}

func (r *poolRows) Values() ([]any, error) {
	return r.rows.Values()
}

func (r *poolRows) RawValues() [][]byte {
	return r.rows.RawValues()
}

func (r *poolRows) FieldDescriptions() []pgconn.FieldDescription {
	return r.rows.FieldDescriptions()
}

func (r *poolRows) CommandTag() pgconn.CommandTag {
	return r.rows.CommandTag()
}

func (r *poolRows) Conn() *pgx.Conn {
	return r.rows.Conn()
}
