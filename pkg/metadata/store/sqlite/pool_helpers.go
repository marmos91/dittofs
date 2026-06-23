package sqlite

import (
	"context"
	"database/sql"
	"time"
)

// poolConnectionAcquireTimeout bounds how long a single statement waits for a
// connection from the underlying *sql.DB pool. SQLite is an embedded,
// single-writer engine, so contention is resolved by the busy_timeout pragma
// rather than a large connection pool; this timeout is a defense-in-depth
// ceiling that keeps an operation from blocking forever if the (size-limited)
// pool is momentarily saturated.
const poolConnectionAcquireTimeout = 10 * time.Second

// ============================================================================
// database/sql executor shim
// ============================================================================
//
// The SQLite store is a near-verbatim port of the Postgres store, which was
// written against pgx (pool.QueryRow(ctx, sql, args...) etc.). To keep the
// query bodies unchanged, the helper methods below present the SAME surface
// over database/sql: a single-row Row, a streaming Rows, and an Exec result —
// each exposing only the handful of methods the store actually uses.
//
// SQL-dialect differences ($N placeholders -> ?, Postgres-only functions) are
// adapted in the queries themselves; the API shape is preserved here.

// sqlExecutor is satisfied by both *sql.DB and *sql.Tx. It is the lowest common
// denominator the store needs.
type sqlExecutor interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// scanRow is the single-row scan surface used across the store (mirrors
// pgx.Row).
type scanRow interface {
	Scan(dest ...any) error
}

// scanRows is the streaming surface used across the store (mirrors pgx.Rows,
// minus the wire-format accessors only backup.go needed — backup is
// reimplemented without them).
type scanRows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// errorRow returns a deferred error from Scan (mirrors pgx's lazy error rows).
type errorRow struct{ err error }

func (r errorRow) Scan(dest ...any) error { return r.err }

// cmdResult mirrors pgx's CommandTag.RowsAffected() (single int64 return) so
// the ported bodies can write `result.RowsAffected()` without the (n, err)
// shape of database/sql's sql.Result. The affected count is captured eagerly
// from the sql.Result at Exec time; a RowsAffected error (never produced by
// the SQLite driver) collapses to 0.
type cmdResult struct{ affected int64 }

func (c cmdResult) RowsAffected() int64 { return c.affected }

// sqlRows adapts *sql.Rows to the rows interface. The only shape difference is
// Close(): database/sql returns an error, pgx returns nothing. Close errors on
// SQLite are not actionable, so they are swallowed (matching pgx semantics);
// iteration errors are surfaced through Err().
type sqlRows struct{ *sql.Rows }

func (r sqlRows) Close()     { _ = r.Rows.Close() }
func (r sqlRows) Err() error { return r.Rows.Err() }

// execer wraps an sqlExecutor so its methods match the pgx pool/tx surface used
// by the store: QueryRow/Query/Exec taking (ctx, sql, args...). Both the
// store-level pool path and the per-transaction path use one of these.
type execer struct {
	e  sqlExecutor
	op string // label for error mapping ("query", "exec", "tx", ...)
}

func (x execer) QueryRow(ctx context.Context, query string, args ...any) scanRow {
	if err := ctx.Err(); err != nil {
		return errorRow{err: err}
	}
	return x.e.QueryRowContext(ctx, query, args...)
}

func (x execer) Query(ctx context.Context, query string, args ...any) (scanRows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := x.e.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err, x.op, query)
	}
	return sqlRows{r}, nil
}

func (x execer) Exec(ctx context.Context, query string, args ...any) (cmdResult, error) {
	if err := ctx.Err(); err != nil {
		return cmdResult{}, err
	}
	res, err := x.e.ExecContext(ctx, query, args...)
	if err != nil {
		return cmdResult{}, mapDBError(err, x.op, query)
	}
	n, _ := res.RowsAffected()
	return cmdResult{affected: n}, nil
}

// ============================================================================
// Store-level (non-transactional) pool helpers
// ============================================================================

// queryRow executes a single-row query against the shared *sql.DB.
func (s *SQLiteMetadataStore) queryRow(ctx context.Context, query string, args ...any) scanRow {
	return execer{e: s.db, op: "queryRow"}.QueryRow(ctx, query, args...)
}

// query executes a multi-row query against the shared *sql.DB. The caller MUST
// Close the returned rows.
func (s *SQLiteMetadataStore) query(ctx context.Context, query string, args ...any) (scanRows, error) {
	return execer{e: s.db, op: "query"}.Query(ctx, query, args...)
}

// exec executes a statement against the shared *sql.DB and returns the result
// for RowsAffected inspection.
func (s *SQLiteMetadataStore) exec(ctx context.Context, query string, args ...any) (cmdResult, error) {
	return execer{e: s.db, op: "exec"}.Exec(ctx, query, args...)
}

// conn returns the pgx-shaped executor over the shared *sql.DB used by the lazy
// sub-stores (locks/clients/durable/recovery). They were written against a pool
// handle named `pool`; an execer presents the same QueryRow/Query/Exec surface.
func (s *SQLiteMetadataStore) conn() execer {
	return execer{e: s.db, op: "substore"}
}
