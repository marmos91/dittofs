// Package sql holds the implementation shared by the SQL-backed metadata stores.
//
// The two dialects, sqlite and postgres, were written twice: postgres against
// pgx (pool.QueryRow(ctx, sql, args...)) and sqlite as a near-verbatim port that
// presents the same surface over database/sql so the query bodies match line for
// line. This package is where that shared surface is named once, so the method
// bodies riding on it can live in one place instead of two.
//
// What crosses this seam is the executor and error classification — the things
// that genuinely differ at runtime. SQL *text* does not: ?N versus $N, NOW()
// versus CURRENT_TIMESTAMP and the handful of rewritten queries stay as
// per-dialect constants, because a Placeholder(n) method would move the same
// two-line difference into an indirection and buy nothing. The consequence to be
// honest about is that this merges logic, not SQL: what costs us maintenance is
// hand-maintained control flow duplicated around the queries, not the query text.
//
// The interface here is *consumed* by shared code; how an Executor is *produced*
// stays per-dialect, in each dialect's pool_helpers.go. Connection acquisition
// has no common shape (postgres acquires from a pgxpool with a timeout; a
// bounded *sql.DB has no per-op acquire step), and neither does transaction
// begin/commit/rollback, so neither is here.
package sql

import "context"

// Row is the single-row scan surface. *pgx.Row and *sql.Row both satisfy it
// as-is.
type Row interface {
	Scan(dest ...any) error
}

// Rows is the streaming surface. It is pgx.Rows minus the wire-format accessors
// (Values/RawValues/FieldDescriptions), which only backup.go ever wanted and
// which database/sql cannot offer; backup is implemented without them.
//
// Close returns nothing, matching pgx. database/sql's Close does return an
// error, but a close error on a read is not actionable — iteration errors are
// surfaced through Err instead.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// CommandTag reports how many rows a statement affected. pgconn.CommandTag
// satisfies it as-is; database/sql's Result has an (int64, error) shape, so the
// sqlite side captures the count eagerly at Exec time and presents it here.
type CommandTag interface {
	RowsAffected() int64
}

// Executor is what shared query bodies run against. Both a pool/connection and
// an open transaction present it, which is what lets one body serve the
// store-level and the transaction-level path.
type Executor interface {
	QueryRow(ctx context.Context, query string, args ...any) Row
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	Exec(ctx context.Context, query string, args ...any) (CommandTag, error)
}

// ErrorRow defers an error to Scan, mirroring pgx's lazy error rows. It lets
// QueryRow report a context cancellation or an acquire failure without changing
// the call shape at every call site.
type ErrorRow struct{ Err error }

// Scan returns the deferred error.
func (r ErrorRow) Scan(dest ...any) error { return r.Err }
