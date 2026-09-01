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
// Error mapping reaches the shared bodies as a MapError hook, but the mapping
// itself stays on each dialect's own errors.go: postgres classifies by SQLSTATE
// via errors.As(&pgErr) and carries five error classes sqlite has no analogue
// for, while sqlite matches lowercased substrings. Retryability is not here at
// all — only WithTransaction consults it, and that stays per-dialect. There is
// no merged errors.go and there was never meant to be one.
type Dialect interface {
	// IsNoRows reports whether err is this driver's empty-result sentinel:
	// sql.ErrNoRows for database/sql, pgx.ErrNoRows for pgx. A shared body
	// cannot compare against either directly, and getting this wrong turns
	// "absent" into a hard error rather than the not-found the callers expect.
	IsNoRows(err error) bool

	// MapError translates a driver error into the metadata.ExportError the
	// callers switch on, tagging it with the operation name and the path it
	// concerned (either may be empty). It dispatches to the dialect's own
	// errors.go rather than merging the two classifiers.
	MapError(err error, operation, path string) error

	// Chunks returns the dialect's file-chunk statements. The pointer is
	// expected to address a package-level value, not a fresh struct per call.
	Chunks() *ChunkQueries

	// Files returns the dialect's file and directory read statements, under
	// the same package-level-value expectation as Chunks.
	Files() *FileQueries

	// Shares returns the dialect's share read statements, under the same
	// package-level-value expectation as Chunks.
	Shares() *ShareQueries
}

// ShareQueries holds the share read statements in one dialect's syntax. These
// differ only in placeholder syntax, but a placeholder is not a value a driver
// will substitute, so each dialect still spells its own.
type ShareQueries struct {
	// GetRootHandle selects a share's root inode id. One parameter: the share
	// name.
	GetRootHandle string
	// GetShareOptions selects a share's options blob. One parameter: the share
	// name.
	GetShareOptions string
	// ListShares selects every share name. No parameters.
	ListShares string
	// GetFilesystemMeta selects a share's filesystem metadata blob. One
	// parameter: the share name.
	GetFilesystemMeta string
	// Statfs sums a share's regular-file bytes and counts its regular files,
	// in that column order. Two parameters: the share name and the numeric
	// regular-file type.
	Statfs string
	// PutFilesystemMeta upserts a share's filesystem metadata blob. Two
	// parameters: the share name and the encoded blob.
	PutFilesystemMeta string
}

// FileQueries holds the file and directory read statements in one dialect's
// syntax. Field names name the operation, not the SQL, so the shared bodies in
// files.go read the same whichever dialect is underneath.
//
// These differ by more than placeholder syntax: the path column is a rewritten
// recursive CTE per dialect, the block-ref aggregate likewise, and postgres
// matches a payload id through an md5 of it because a content id near PATH_MAX
// overruns its btree key limit.
type FileQueries struct {
	// GetFile selects one full inode row, block-ref aggregate included. Two
	// parameters: the file id and the share name.
	GetFile string
	// GetChild selects a directory entry's child id. Two parameters: the
	// parent id and the child name.
	GetChild string
	// GetParent selects one parent id for a child. One parameter: the child id.
	GetParent string
	// GetLinkCount selects one inode's nlink. One parameter: the file id.
	GetLinkCount string
	// ListChildren selects a page of directory entries joined to their inode
	// rows, ordered by name. Three parameters: the parent id, the exclusive
	// name cursor, and the row limit.
	ListChildren string
	// GetFileByPayloadID selects one full inode row by content id, block-ref
	// aggregate included. One parameter: the content id.
	GetFileByPayloadID string
	// SetChild inserts a directory entry, repointing an existing name at the
	// new child. Three parameters: the parent id, the child name, the child id.
	SetChild string
	// DeleteChild removes a directory entry. Two parameters: the parent id and
	// the child name.
	DeleteChild string
	// SetLinkCount writes one inode's nlink. Two parameters: the count and the
	// file id.
	SetLinkCount string
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
