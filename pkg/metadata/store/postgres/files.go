package postgres

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// These methods delegate to transaction methods via WithTransaction.
// This ensures consistency and avoids duplicating SQL queries.

// GetFile retrieves file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *PostgresMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	var result *metadata.File
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetFile(ctx, handle)
		return err
	})
	return result, err
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
// Returns ErrNotFound if name doesn't exist.
func (s *PostgresMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	var result metadata.FileHandle
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetChild(ctx, dirHandle, name)
		return err
	})
	return result, err
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
// Returns ErrNotFound for root directories (no parent).
func (s *PostgresMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	var result metadata.FileHandle
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetParent(ctx, handle)
		return err
	})
	return result, err
}

// SetParent sets the parent handle for a file/directory.
func (s *PostgresMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetParent(ctx, handle, parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (s *PostgresMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	var result uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetLinkCount(ctx, handle)
		return err
	})
	return result, err
}

// SetLinkCount sets the hard link count for a file.
func (s *PostgresMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ListChildren returns directory entries with pagination support.
func (s *PostgresMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	var entries []metadata.DirEntry
	var nextCursor string

	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		entries, nextCursor, err = tx.ListChildren(ctx, dirHandle, cursor, limit)
		return err
	})

	return entries, nextCursor, err
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (s *PostgresMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	var meta *metadata.FilesystemMeta

	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		meta, err = tx.GetFilesystemMeta(ctx, shareName)
		return err
	})

	return meta, err
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

	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.content_id = $1
		LIMIT 1
	`

	row := s.pool.QueryRow(ctx, query, string(payloadID))
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFileByPayloadID", string(payloadID))
	}

	return file, nil
}
