package sqlite

import (
	"database/sql"
	"errors"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// dialect supplies SQLite statement text and database/sql error classification
// to the shared SQL core. It is stateless, so one package-level value serves
// every store and transaction.
type dialect struct{}

var sqliteDialect dialect

// IsNoRows reports database/sql's empty-result sentinel.
func (dialect) IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Chunks returns the SQLite file-chunk statements. They differ from postgres in
// placeholder syntax (?N against $N), the floor function (MAX against GREATEST)
// and the byte-ordering collation name (BINARY against "C") — text only; the
// logic that runs them is shared.
func (dialect) Chunks() *storesql.ChunkQueries { return &chunkQueries }

var chunkQueries = storesql.ChunkQueries{
	SelectByID:   `SELECT ` + fileChunkColumns + ` FROM file_blocks WHERE id = ?1`,
	SelectByHash: `SELECT ` + fileChunkColumns + ` FROM file_blocks WHERE hash = ?1 AND state = 2 /* Remote */`,
	Upsert:       putFileChunkQuery,
	Delete:       `DELETE FROM file_blocks WHERE id = ?1`,
	IncrementRef: `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = ?1`,
	DecrementRef: `UPDATE file_blocks SET ref_count = MAX(ref_count - 1, 0) WHERE id = ?1 RETURNING ref_count`,
	// state = 2 (Remote) scoping mirrors SelectByHash and the memory/badger
	// backends: a Pending row is not a valid dedup donor.
	AddRef:             `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = ?1 AND state = 2 /* Remote */`,
	ReapZeroRef:        `DELETE FROM file_blocks WHERE id = ?1 AND ref_count = 0`,
	ListByPayloadRange: listFileChunksQuery,
	EnumerateHashes:    enumerateHashesQuery,
}

// MapError translates a driver error into an ExportError. It is this
// package's own mapDBError, reached through the Dialect so a shared body can
// classify an error without importing the driver that produced it.
func (dialect) MapError(err error, operation, path string) error {
	return mapDBError(err, operation, path)
}

// Files returns the SQLite file and directory read statements.
func (dialect) Files() *storesql.FileQueries { return &fileQueries }

// inodeSelectColumns is the full inode projection GetFile and
// GetFileByPayloadID share, block-ref aggregate included, in the column order
// sqlcodec.FileRowToFileWithNlinkAndBlocks scans.
const inodeSelectColumns = `
	f.id, f.share_name, ` + inodePathExpr + `,
	f.file_type, f.mode, f.uid, f.gid, f.size,
	f.atime, f.mtime, f.ctime, f.creation_time,
	f.content_id, f.link_target, f.device_major, f.device_minor,
	f.hidden, f.acl, f.eas, f.object_id,
	f.deleted_at, f.original_path, f.deleted_by, f.nlink,
	` + blockRefsAggExpr + `
`

var fileQueries = storesql.FileQueries{
	GetFile: `SELECT ` + inodeSelectColumns + ` FROM inodes f
		WHERE f.id = ?1 AND f.share_name = ?2`,

	GetChild: `SELECT dc.child_id FROM parent_child_map dc
		WHERE dc.parent_id = ?1 AND dc.child_name = ?2`,

	GetParent: `SELECT parent_id FROM parent_child_map WHERE child_id = ?1 LIMIT 1`,

	GetLinkCount: `SELECT nlink FROM inodes WHERE id = ?1`,

	// f.acl and f.eas are hydrated here so DirEntry.Attr carries them, matching
	// the Memory and Badger backends: without those columns an ACL-aware caller
	// iterating a listing would silently fall back to POSIX mode bits. They are
	// small next to the row itself and save a GetFile per entry.
	ListChildren: `SELECT dc.child_name, dc.child_id, f.file_type, f.mode, f.uid, f.gid, f.size,
		       f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, f.acl, f.eas, f.object_id,
		       f.deleted_at, f.original_path, f.deleted_by, f.nlink
		FROM parent_child_map dc
		LEFT JOIN inodes f ON dc.child_id = f.id
		WHERE dc.parent_id = ?1 AND dc.child_name > ?2
		ORDER BY dc.child_name
		LIMIT ?3`,

	GetFileByPayloadID: `SELECT ` + inodeSelectColumns + ` FROM inodes f
		WHERE f.content_id = ?1
		LIMIT 1`,
}

var _ storesql.Dialect = dialect{}
