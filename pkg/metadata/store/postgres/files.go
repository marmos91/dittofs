package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// Read operations use direct pool queries for better performance (no transaction overhead).
// Write operations use WithTransaction for atomicity.

// GetFile retrieves file metadata by handle.
// Uses direct pool query without transaction for better performance.
// Returns ErrNotFound if handle doesn't exist.
func (s *PostgresMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, f.acl, f.eas, f.object_id,
			f.deleted_at, f.original_path, f.deleted_by, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.id = $1 AND f.share_name = $2
	`

	row := s.queryRow(ctx, query, id, shareName)
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFile", "")
	}

	// load FileAttr.Blocks from file_block_refs.
	// Only regular files carry BlockRef payloads; directories/symlinks have none.
	if file.Type == metadata.FileTypeRegular {
		blocks, err := s.loadFileBlockRefs(ctx, id)
		if err != nil {
			return nil, mapPgError(err, "GetFile", "load blocks")
		}
		file.Blocks = blocks
	}

	return file, nil
}

// PutFile stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (s *PostgresMetadataStore) PutFile(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFile(ctx, file)
	})
}

// DeleteFile removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *PostgresMetadataStore) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// GetChild resolves a name in a directory to a file handle.
// Uses direct pool query without transaction for better performance.
// Returns ErrNotFound if name doesn't exist.
func (s *PostgresMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	query := `
		SELECT dc.child_id FROM parent_child_map dc
		WHERE dc.parent_id = $1 AND dc.child_name = $2
	`

	var childID string
	err = s.queryRow(ctx, query, parentID, name).Scan(&childID)
	if err != nil {
		return nil, mapPgError(err, "GetChild", name)
	}

	return encodeFileHandle(shareName, childID)
}

// SetChild adds or updates a child entry in a directory.
func (s *PostgresMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
// Returns ErrNotFound if name doesn't exist.
func (s *PostgresMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// GetParent returns the parent handle for a file/directory.
// Uses direct pool query without transaction for better performance.
// Returns ErrNotFound for root directories (no parent).
func (s *PostgresMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, childID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `SELECT parent_id FROM parent_child_map WHERE child_id = $1 LIMIT 1`

	var parentIDStr string
	err = s.queryRow(ctx, query, childID).Scan(&parentIDStr)
	if err != nil {
		return nil, mapPgError(err, "GetParent", "")
	}

	return encodeFileHandle(shareName, parentIDStr)
}

// SetParent sets the parent handle for a file/directory.
//
// Parent tracking is handled implicitly by parent_child_map via SetChild, so
// this performs no write. It still honours context cancellation, matching the
// prior WithTransaction-based implementation, while avoiding the cost of
// opening an empty transaction.
func (s *PostgresMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return ctx.Err()
}

// GetLinkCount returns the hard link count for a file.
// Uses direct pool query without transaction for better performance.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (s *PostgresMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	var count uint32
	err = s.queryRow(ctx, `SELECT link_count FROM link_counts WHERE file_id = $1`, fileID).Scan(&count)
	if err != nil {
		// Not found means count is 0
		return 0, nil
	}

	return count, nil
}

// SetLinkCount sets the hard link count for a file.
func (s *PostgresMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ListChildren returns directory entries with pagination support.
// Uses direct pool query without transaction for better performance.
func (s *PostgresMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	shareName, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	if limit <= 0 {
		limit = 1000
	}

	// Refs #532 (PR #536 review): hydrate f.acl so DirEntry.Attr.ACL is
	// populated, matching the Memory/Badger backends. Without this column,
	// access-based enumeration (and any other ACL-aware caller iterating
	// DirEntry.Attr) silently degrades to POSIX mode bits even on files that
	// have a DACL set. ACL JSONB rows are typically small relative to the
	// listing row itself, so the extra column adds negligible per-row cost
	// versus the per-entry GetFile() round trip it avoids.
	query := `
		SELECT dc.child_name, dc.child_id, f.file_type, f.mode, f.uid, f.gid, f.size,
		       f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, f.acl, f.eas, f.object_id,
		       f.deleted_at, f.original_path, f.deleted_by, lc.link_count
		FROM parent_child_map dc
		LEFT JOIN files f ON dc.child_id = f.id
		LEFT JOIN link_counts lc ON dc.child_id = lc.file_id
		WHERE dc.parent_id = $1 AND dc.child_name > $2
		ORDER BY dc.child_name
		LIMIT $3
	`

	rows, err := s.query(ctx, query, parentID, cursor, limit+1)
	if err != nil {
		return nil, "", mapPgError(err, "ListChildren", "")
	}
	defer rows.Close()

	var entries []metadata.DirEntry
	for rows.Next() && len(entries) < limit {
		var name, childIDStr string
		var fileType int16
		var mode, uid, gid int32
		var size int64
		var atime, mtime, ctime, creationTime int64
		var hidden bool
		var aclJSON []byte
		var easJSON []byte
		var objectIDRaw []byte
		var deletedAt sql.NullInt64
		var originalPath string
		var deletedBy string
		var linkCount sql.NullInt32

		err := rows.Scan(&name, &childIDStr, &fileType, &mode, &uid, &gid, &size,
			&atime, &mtime, &ctime, &creationTime, &hidden, &aclJSON, &easJSON, &objectIDRaw,
			&deletedAt, &originalPath, &deletedBy, &linkCount)
		if err != nil {
			return nil, "", err
		}

		childHandle, err := encodeFileHandle(shareName, childIDStr)
		if err != nil {
			return nil, "", err
		}

		// Determine Nlink value
		var nlink uint32
		if linkCount.Valid {
			nlink = uint32(linkCount.Int32)
		} else {
			// Default based on file type
			if metadata.FileType(fileType) == metadata.FileTypeDirectory {
				nlink = 2
			} else {
				nlink = 1
			}
		}

		// hydrate ObjectID for directory entries.
		// NULL/empty -> zero (sentinel).
		//
		// (review iteration 1): the shape DOES NOT match
		// GetFile in this backend. GetFile populates FileAttr.Blocks via
		// loadFileBlockRefs; ListChildren intentionally does NOT (per-row
		// BlockRef hydration would be a quadratic cost on directory
		// listings). Memory and Badger backends include Blocks on
		// DirEntry.Attr because their underlying serialisation already
		// carries the slice — Postgres' relational model splits
		// FileAttr.Blocks into a separate table (file_block_refs) and
		// listing rows skip the join. Callers MUST treat
		// DirEntry.Attr.Blocks as not-loaded for Postgres and re-read
		// via GetFile if the BlockRef list is needed for hot-path
		// resolution. Current short-circuit code (FindByObjectID-driven)
		// never consults DirEntry.Attr.ObjectID, so this asymmetry is
		// benign at the call surface.
		attr := &metadata.FileAttr{
			Type:         metadata.FileType(fileType),
			Mode:         uint32(mode),
			Nlink:        nlink,
			UID:          uint32(uid),
			GID:          uint32(gid),
			Size:         uint64(size),
			Atime:        pgNanosToTime(atime),
			Mtime:        pgNanosToTime(mtime),
			Ctime:        pgNanosToTime(ctime),
			CreationTime: pgNanosToTime(creationTime),
			Hidden:       hidden,
		}
		if len(objectIDRaw) > 0 {
			if len(objectIDRaw) != block.HashSize {
				return nil, "", fmt.Errorf(
					"postgres ListChildren: object_id has invalid length %d (want %d)",
					len(objectIDRaw), block.HashSize,
				)
			}
			copy(attr.ObjectID[:], objectIDRaw)
		}

		// Recycle-bin metadata (#190): carried on DirEntry.Attr so trash
		// enumeration via listing reflects recycle state without a re-read.
		// deleted_at is BIGINT unix-nanoseconds; decode via pgNanosToTime.
		if deletedAt.Valid {
			t := pgNanosToTime(deletedAt.Int64)
			attr.DeletedAt = &t
		}
		attr.OriginalPath = originalPath
		attr.DeletedBy = deletedBy

		// Refs #532 (PR #536 review): mirror fileRowToFileWithNlink. A
		// malformed ACL row is treated as "no ACL" rather than failing the
		// whole listing — same lenient behaviour the GetFile path has had
		// since the column was introduced.
		if len(aclJSON) > 0 {
			var fileACL acl.ACL
			if err := json.Unmarshal(aclJSON, &fileACL); err == nil {
				attr.ACL = &fileACL
			}
		}

		// Hydrate EAs for directory entries (same lenient unmarshal as ACL).
		if len(easJSON) > 0 {
			var eas map[string][]byte
			if err := json.Unmarshal(easJSON, &eas); err == nil && len(eas) > 0 {
				attr.EAs = eas
			}
		}

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
			Attr:   attr,
		}

		entries = append(entries, entry)
	}

	// Surface any error that terminated the iteration early (e.g. a network
	// drop mid-stream). Without this check a partial result would be returned
	// as a complete, successful listing.
	if err := rows.Err(); err != nil {
		return nil, "", mapPgError(err, "ListChildren", "")
	}

	nextCursor := ""
	if len(entries) >= limit {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
// Uses direct pool query without transaction for better performance.
func (s *PostgresMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT meta FROM filesystem_meta WHERE share_name = $1`

	var data []byte
	err := s.queryRow(ctx, query, shareName).Scan(&data)
	if err != nil {
		// Return defaults if not found
		return &metadata.FilesystemMeta{
			Capabilities: s.capabilities,
		}, nil
	}

	var meta metadata.FilesystemMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (s *PostgresMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// ============================================================================
// Payload ID Operations
// ============================================================================

// FindByObjectID looks up a file by its Merkle-root ObjectID and returns the
// canonical BlockRef list of the matching row. Returns (nil, nil) on miss
// (zero-valued input or no matching row).
//
// Uses the partial UNIQUE index files_object_id_idx; the LIMIT 1 is defensive
// (the partial UNIQUE constraint already enforces single-row matches for
// non-NULL object_id values).
func (s *PostgresMetadataStore) FindByObjectID(ctx context.Context, objectID block.ObjectID) ([]block.BlockRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if objectID.IsZero() {
		return nil, nil
	}

	var fileID uuid.UUID
	err := s.queryRow(ctx,
		`SELECT id FROM files WHERE object_id = $1 LIMIT 1`,
		objectID[:],
	).Scan(&fileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err, "FindByObjectID", objectID.String())
	}

	return s.loadFileBlockRefs(ctx, fileID)
}

// GetFileByPayloadID retrieves a file by its content ID (used by cache flusher)
func (s *PostgresMetadataStore) GetFileByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (*metadata.File, error) {
	if payloadID == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "content ID cannot be empty",
		}
	}

	// Use content_id_hash (MD5 of content_id) for index lookup to avoid
	// PostgreSQL btree 2704-byte limit. The content_id can exceed this limit
	// for files with paths near PATH_MAX (4096 bytes).
	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, f.acl, f.eas, f.object_id,
			f.deleted_at, f.original_path, f.deleted_by, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.content_id_hash = md5($1)
		LIMIT 1
	`

	row := s.queryRow(ctx, query, string(payloadID))
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFileByPayloadID", string(payloadID))
	}

	return file, nil
}

// CountObjectIDIndexRows implements the storetest.ObjectIDIndexAccessor
// optional capability. Returns the number of files indexed under the
// given objectID via the partial UNIQUE index files_object_id_idx.
//
// Test-only — never call from production code. Used by the
// ConcurrentQuiesceRace scenario to assert exactly one row
// survives the first-committer-wins resolution.
//
// Zero-valued objectID inputs short-circuit to (0, nil) without backend
// access, mirroring FindByObjectID's partial/skip-zero discipline.
func (s *PostgresMetadataStore) CountObjectIDIndexRows(ctx context.Context, objectID block.ObjectID) (int, error) {
	if objectID.IsZero() {
		return 0, nil
	}
	var n int
	err := s.queryRow(ctx,
		`SELECT count(*) FROM files WHERE object_id = $1`,
		objectID[:],
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count files.object_id: %w", err)
	}
	return n, nil
}
