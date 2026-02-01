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

// queryRow executes a query that returns at most one row with connection acquire timeout.
// This prevents indefinite blocking when the pool is exhausted.
func (s *PostgresMetadataStore) queryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	// Check context before acquiring connection
	if ctx.Err() != nil {
		return &errorRow{err: ctx.Err()}
	}

	// Apply connection acquire timeout
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer cancel()

	// Acquire connection from pool
	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		if acquireCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			// Acquire timed out, not the parent context
			return &errorRow{err: fmt.Errorf("connection acquire timeout after %v: pool may be exhausted", poolConnectionAcquireTimeout)}
		}
		return &errorRow{err: mapPgError(err, "queryRow", sql)}
	}

	// Execute query - this will release connection after row is scanned or closed
	// Note: The connection is released when the Row is scanned or when Scan returns an error
	row := conn.QueryRow(ctx, sql, args...)
	return &poolRow{row: row, conn: conn}
}

// query executes a query that returns rows with connection acquire timeout.
// This prevents indefinite blocking when the pool is exhausted.
// Caller MUST close the returned Rows when done.
func (s *PostgresMetadataStore) query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	// Check context before acquiring connection
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Apply connection acquire timeout
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer cancel()

	// Acquire connection from pool
	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		if acquireCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			// Acquire timed out, not the parent context
			return nil, fmt.Errorf("connection acquire timeout after %v: pool may be exhausted", poolConnectionAcquireTimeout)
		}
		return nil, mapPgError(err, "query", sql)
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

	// Apply connection acquire timeout
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer cancel()

	// Acquire connection from pool
	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		if acquireCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			// Acquire timed out, not the parent context
			return pgconn.CommandTag{}, fmt.Errorf("connection acquire timeout after %v: pool may be exhausted", poolConnectionAcquireTimeout)
		}
		return pgconn.CommandTag{}, mapPgError(err, "exec", sql)
	}
	defer conn.Release()

	// Execute statement
	tag, err := conn.Exec(ctx, sql, args...)
	if err != nil {
		return pgconn.CommandTag{}, mapPgError(err, "exec", sql)
	}
	return tag, nil
}

// beginTx starts a transaction with connection acquire timeout.
// This prevents indefinite blocking when the pool is exhausted.
// Caller MUST commit or rollback the returned transaction.
func (s *PostgresMetadataStore) beginTx(ctx context.Context) (pgx.Tx, error) {
	// Check context before acquiring connection
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Apply connection acquire timeout
	acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer cancel()

	// Begin transaction (this acquires a connection)
	tx, err := s.pool.Begin(acquireCtx)
	if err != nil {
		if acquireCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			// Acquire timed out, not the parent context
			return nil, fmt.Errorf("connection acquire timeout after %v: pool may be exhausted", poolConnectionAcquireTimeout)
		}
		return nil, mapPgError(err, "beginTx", "")
	}

	return tx, nil
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

// poolRow wraps a pgx.Row and releases the connection after Scan
type poolRow struct {
	row  pgx.Row
	conn *pgxpool.Conn
}

func (r *poolRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.conn.Release()
	return err
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
