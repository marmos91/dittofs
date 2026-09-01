package sql

// Dialect carries what genuinely differs at runtime between the SQL backends.
//
// It is deliberately narrow. The divergences between sqlite and postgres split
// into two kinds and only one of them needs a runtime interface:
//
//   - Text divergence — ?N versus $N, NOW() versus CURRENT_TIMESTAMP, the two
//     rewritten recursive-CTE path queries, the two block-ref aggregates. That
//     is baked into per-dialect SQL constants, supplied here as a struct of
//     statements. A Placeholder(n int) method would move the same two-line
//     difference into an indirection and buy nothing.
//   - Behavioural divergence — error classification, and whether an error is
//     the driver's empty-result sentinel. Those are genuinely per-dialect
//     behaviour, so they are methods.
//
// Error *mapping* (MapError) and retryability (IsRetryable) stay on each
// dialect's own errors.go: postgres classifies by SQLSTATE via
// errors.As(&pgErr) and carries five error classes sqlite has no analogue for,
// while sqlite matches lowercased substrings. There is no merged errors.go and
// there was never meant to be one.
type Dialect interface {
	// IsNoRows reports whether err is this driver's empty-result sentinel:
	// sql.ErrNoRows for database/sql, pgx.ErrNoRows for pgx. A shared body
	// cannot compare against either directly, and getting this wrong turns
	// "absent" into a hard error rather than the not-found the callers expect.
	IsNoRows(err error) bool

	// Chunks returns the dialect's file-chunk statements. The pointer is
	// expected to address a package-level value, not a fresh struct per call.
	Chunks() *ChunkQueries
}

// ChunkQueries holds the file-chunk statements in one dialect's syntax. Field
// names name the operation, not the SQL, so the shared bodies read the same
// whichever dialect is underneath.
type ChunkQueries struct {
	// SelectByID selects one chunk row by id. One parameter: the id.
	SelectByID string
	// SelectByHash selects one finalized (Remote) chunk row by content hash.
	// One parameter: the hex hash.
	SelectByHash string
	// Upsert inserts or updates a chunk row. Nine parameters, in the column
	// order of the chunk table.
	Upsert string
	// Delete removes one chunk row by id. One parameter: the id.
	Delete string
	// IncrementRef bumps one row's ref_count by one. One parameter: the id.
	IncrementRef string
	// DecrementRef decrements one row's ref_count, floored at zero, and
	// returns the new value. One parameter: the id.
	DecrementRef string
	// AddRef bumps ref_count on every Remote row carrying a content hash.
	// One parameter: the hex hash.
	AddRef string
	// ReapZeroRef deletes one row by id only if its ref_count is still zero.
	// One parameter: the id.
	ReapZeroRef string
	// ListByPayloadRange selects the chunk rows whose ids fall in a payload's
	// prefix range, in byte order. Two parameters: the low and high bounds.
	ListByPayloadRange string
	// EnumerateHashes selects the GC live set. No parameters.
	EnumerateHashes string
}
