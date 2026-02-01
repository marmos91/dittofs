package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
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

	shareName, id, err := decodeFileHandle(handle)
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
			f.hidden, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.id = $1 AND f.share_name = $2
	`

	row := s.queryRow(ctx, query, id, shareName)
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFile", "")
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

	shareName, parentID, err := decodeFileHandle(dirHandle)
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

	shareName, childID, err := decodeFileHandle(handle)
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
func (s *PostgresMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetParent(ctx, handle, parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
// Uses direct pool query without transaction for better performance.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (s *PostgresMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, fileID, err := decodeFileHandle(handle)
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

	shareName, parentID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	if limit <= 0 {
		limit = 1000
	}

	query := `
		SELECT dc.child_name, dc.child_id, f.file_type, f.mode, f.uid, f.gid, f.size,
		       f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, lc.link_count
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
		var atime, mtime, ctime, creationTime time.Time
		var hidden bool
		var linkCount sql.NullInt32

		err := rows.Scan(&name, &childIDStr, &fileType, &mode, &uid, &gid, &size,
			&atime, &mtime, &ctime, &creationTime, &hidden, &linkCount)
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

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
			Attr: &metadata.FileAttr{
				Type:         metadata.FileType(fileType),
				Mode:         uint32(mode),
				Nlink:        nlink,
				UID:          uint32(uid),
				GID:          uint32(gid),
				Size:         uint64(size),
				Atime:        atime,
				Mtime:        mtime,
				Ctime:        ctime,
				CreationTime: creationTime,
				Hidden:       hidden,
			},
		}

		entries = append(entries, entry)
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
// Content ID Operations
// ============================================================================

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
			f.hidden, lc.link_count
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
