package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// CreateRootDirectory creates the root directory for a share
func (s *PostgresMetadataStore) CreateRootDirectory(
	ctx context.Context,
	shareName string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	if shareName == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "share name cannot be empty",
		}
	}

	// Apply defaults
	uid := attr.UID
	gid := attr.GID
	mode := attr.Mode
	if mode == 0 {
		mode = 0o755
	}

	s.logger.Info("Creating root directory",
		"share", shareName,
		"uid", uid,
		"gid", gid,
	)

	// Begin transaction
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Generate UUID for root directory
	rootID := uuid.New()

	now := time.Now()

	// Insert root directory file
	insertFileQuery := `
		INSERT INTO files (
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime,
			content_id, link_target, device_major, device_minor
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14, $15
		)
	`

	_, err = tx.Exec(ctx, insertFileQuery,
		rootID,                            // id
		shareName,                         // share_name
		"/",                               // path (root)
		int16(metadata.FileTypeDirectory), // file_type
		int32(mode),                       // mode
		int32(uid),                        // uid
		int32(gid),                        // gid
		int64(0),                          // size
		now,                               // atime
		now,                               // mtime
		now,                               // ctime
		nil,                               // content_id (NULL for directories)
		nil,                               // link_target (NULL)
		nil,                               // device_major (NULL)
		nil,                               // device_minor (NULL)
	)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Insert into link_counts (directories start with link count = 2: "." and parent reference)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx, insertLinkCountQuery, rootID, 2)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Insert into shares table
	insertShareQuery := `
		INSERT INTO shares (share_name, root_file_id)
		VALUES ($1, $2)
		ON CONFLICT (share_name) DO UPDATE
		SET root_file_id = EXCLUDED.root_file_id
	`

	_, err = tx.Exec(ctx, insertShareQuery, shareName, rootID)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	s.logger.Info("Root directory created successfully",
		"share", shareName,
		"root_id", rootID,
	)

	// Build File
	file := &metadata.File{
		ID:        rootID,
		ShareName: shareName,
		Path:      "/",
		FileAttr: metadata.FileAttr{
			Type:  metadata.FileTypeDirectory,
			Mode:  mode,
			UID:   uid,
			GID:   gid,
			Size:  0,
			Atime: now,
			Mtime: now,
			Ctime: now,
		},
	}

	return file, nil
}

// ReadDirectory reads directory entries with pagination
func (s *PostgresMetadataStore) ReadDirectory(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	token string,
	maxBytes uint32,
) (*metadata.ReadDirPage, error) {
	// Decode directory handle
	shareName, dirID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	// Get directory file
	dir, err := s.getFileByID(ctx.Context, dirID, shareName)
	if err != nil {
		return nil, err
	}

	// Verify it's a directory
	if dir.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
			Path:    dir.Path,
		}
	}

	// Check read permission
	if err := s.checkAccess(dir, ctx, metadata.PermissionRead); err != nil {
		return nil, err
	}

	// Parse token (offset-based pagination)
	offset := 0
	if token != "" {
		_, err := fmt.Sscanf(token, "%d", &offset)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "invalid pagination token",
			}
		}
	}

	// Estimate max entries based on maxBytes (conservative estimate: ~200 bytes per entry)
	maxEntries := 1000
	if maxBytes > 0 {
		maxEntries = int(maxBytes / 200)
		if maxEntries < 10 {
			maxEntries = 10 // Minimum entries per page
		}
	}

	// Query children with pagination
	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			pcm.child_name
		FROM files f
		INNER JOIN parent_child_map pcm ON f.id = pcm.child_id
		WHERE pcm.parent_id = $1
		ORDER BY pcm.child_name
		LIMIT $2 OFFSET $3
	`

	rows, err := s.pool.Query(ctx.Context, query, dirID, maxEntries+1, offset)
	if err != nil {
		return nil, mapPgError(err, "ReadDirectory", dir.Path)
	}
	defer rows.Close()

	var entries []metadata.DirEntry
	count := 0

	for rows.Next() {
		count++
		// If we got more than maxEntries, we have more pages
		if count > maxEntries {
			break
		}

		// Scan row (including child_name from join)
		file, childName, err := scanDirectoryEntry(rows)
		if err != nil {
			return nil, mapPgError(err, "ReadDirectory", dir.Path)
		}

		// Encode child handle
		childHandle, err := metadata.EncodeFileHandle(file)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrIOError,
				Message: "failed to encode child handle",
			}
		}

		// Create directory entry
		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   childName,
			Handle: childHandle,
			Attr:   &file.FileAttr,
		}

		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, mapPgError(err, "ReadDirectory", dir.Path)
	}

	// Generate next token if there are more entries
	nextToken := ""
	hasMore := false
	if count > maxEntries {
		nextToken = fmt.Sprintf("%d", offset+maxEntries)
		hasMore = true
	}

	page := &metadata.ReadDirPage{
		Entries:   entries,
		NextToken: nextToken,
		HasMore:   hasMore,
	}

	return page, nil
}

// scanDirectoryEntry scans a directory entry row including the child name
func scanDirectoryEntry(rows pgx.Rows) (*metadata.File, string, error) {
	var (
		id          uuid.UUID
		shareName   string
		path        string
		fileType    int16
		mode        int32
		uid         int32
		gid         int32
		size        int64
		atime       time.Time
		mtime       time.Time
		ctime       time.Time
		contentID   *string
		linkTarget  *string
		deviceMajor *int32
		deviceMinor *int32
		childName   string
	)

	err := rows.Scan(
		&id, &shareName, &path,
		&fileType, &mode, &uid, &gid, &size,
		&atime, &mtime, &ctime,
		&contentID, &linkTarget, &deviceMajor, &deviceMinor,
		&childName,
	)
	if err != nil {
		return nil, "", err
	}

	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:  metadata.FileType(fileType),
			Mode:  uint32(mode),
			UID:   uint32(uid),
			GID:   uint32(gid),
			Size:  uint64(size),
			Atime: atime,
			Mtime: mtime,
			Ctime: ctime,
		},
	}

	if contentID != nil {
		file.ContentID = metadata.ContentID(*contentID)
	}

	if linkTarget != nil {
		file.LinkTarget = *linkTarget
	}

	return file, childName, nil
}
