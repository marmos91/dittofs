package sql

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// ============================================================================
// Share reads
// ============================================================================
//
// As with the file reads, these are pure and so want the identical body on the
// pool path and the transaction path. GetShareOptions is the uncached backing
// read; the store shadows it with its share cache and calls back into this one
// for the miss.

// GetRootHandle returns the root handle for a share, reporting ErrNotFound when
// the share does not exist.
func (c *Core) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rootID uuid.UUID
	if err := c.X.QueryRow(ctx, c.D.Shares().GetRootHandle, shareName).Scan(&rootID); err != nil {
		return nil, c.D.MapError(err, "GetRootHandle", shareName)
	}

	return metadata.EncodeShareHandle(shareName, rootID)
}

// GetShareOptions reads a share's options straight from the backing store,
// reporting ErrNotFound when the share does not exist.
//
// The returned value is freshly decoded on every call and aliases nothing, so
// a caller may hold or mutate it. That is what lets the store layer cache it
// by handing out copies.
func (c *Core) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var optionsJSON []byte
	if err := c.X.QueryRow(ctx, c.D.Shares().GetShareOptions, shareName).Scan(&optionsJSON); err != nil {
		return nil, c.D.MapError(err, "GetShareOptions", shareName)
	}

	var options metadata.ShareOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, fmt.Errorf("failed to unmarshal share options: %w", err)
		}
	}

	return &options, nil
}

// ListShares returns the names of every share in the store.
func (c *Core) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := c.X.Query(ctx, c.D.Shares().ListShares)
	if err != nil {
		return nil, c.D.MapError(err, "ListShares", "")
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}

	// Surface an error that terminated the iteration early so a partial share
	// list is not returned as if it were complete.
	if err := rows.Err(); err != nil {
		return nil, c.D.MapError(err, "ListShares", "")
	}

	return names, nil
}

// GetFilesystemMeta returns a share's stored filesystem metadata, or the
// store's configured capabilities when the share has none.
//
// Only a missing row reports the defaults. Every other read failure is
// returned, so a query that cannot reach its table is not mistaken for a share
// that was never written to.
func (c *Core) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var data []byte
	if err := c.X.QueryRow(ctx, c.D.Shares().GetFilesystemMeta, shareName).Scan(&data); err != nil {
		// The context can give out while the query is in flight, and a
		// request that is cancelled or past its deadline must not read as a
		// share with default capabilities.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !c.D.IsNoRows(err) {
			return nil, c.D.MapError(err, "GetFilesystemMeta", shareName)
		}
		return &metadata.FilesystemMeta{Capabilities: c.Caps()}, nil
	}

	var meta metadata.FilesystemMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// PutFilesystemMeta stores a share's filesystem metadata, replacing whatever
// was there.
func (c *Core) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	if _, err := c.X.Exec(ctx, c.D.Shares().PutFilesystemMeta, shareName, data); err != nil {
		return c.D.MapError(err, "PutFilesystemMeta", shareName)
	}
	return nil
}

// UpdateShareOptions replaces an existing share's options, reporting
// metadata.ErrNotFound when the share does not exist.
func (c *Core) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(options)
	if err != nil {
		return err
	}

	result, err := c.X.Exec(ctx, c.D.Shares().SetShareOptions, data, shareName)
	if err != nil {
		return c.D.MapError(err, "UpdateShareOptions", shareName)
	}
	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}
	return nil
}

// DeleteShare removes a share and every inode belonging to it, reporting
// metadata.ErrNotFound when the share does not exist.
func (c *Core) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Aggregate the usage about to disappear before the rows go, so the
	// store's quota cache can be decremented once the transaction commits.
	if err := c.collectShareQuotaFreed(ctx, shareName, "uid", metadata.QuotaScopeUser); err != nil {
		return err
	}
	if err := c.collectShareQuotaFreed(ctx, shareName, "gid", metadata.QuotaScopeGroup); err != nil {
		return err
	}

	// Drop the share row first: shares.root_file_id references inodes(id)
	// WITHOUT ON DELETE CASCADE, so the inode rows cannot be removed while the
	// share still points at the root inode.
	result, err := c.X.Exec(ctx, c.D.Shares().DeleteShare, shareName)
	if err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}
	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	// The contract is "removes a share and all its metadata"; dropping only the
	// share row orphans every inodes/parent_child_map/file_block_refs row.
	// parent_child_map and file_block_refs both cascade from inodes(id).
	if _, err := c.X.Exec(ctx, c.D.Shares().DeleteShareInodes, shareName); err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}

	return nil
}

// collectShareQuotaFreed aggregates the regular-file usage of a share being
// deleted, grouped by uid or gid, and records the negative delta so the
// in-memory usage cache is decremented after the commit.
func (c *Core) collectShareQuotaFreed(ctx context.Context, shareName, col string, scope metadata.QuotaScope) error {
	query := fmt.Sprintf(c.D.Shares().ShareQuotaFreed, col, col)
	rows, err := c.X.Query(ctx, query, shareName, int(metadata.FileTypeRegular))
	if err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}
	defer rows.Close()

	for rows.Next() {
		var id, bytes, files int64
		if err := rows.Scan(&id, &bytes, &files); err != nil {
			return c.D.MapError(err, "DeleteShare", shareName)
		}
		c.Quota.AddKeyed(
			basestore.QuotaKey{Share: shareName, Scope: scope, ID: uint32(id)},
			metadata.UsageStat{Bytes: -bytes, Files: -files},
		)
	}
	if err := rows.Err(); err != nil {
		return c.D.MapError(err, "DeleteShare", shareName)
	}
	return nil
}

// GetExistingRootDirectory loads a share's root directory, or (nil, nil) when
// the share has no root yet.
//
// The root is resolved through the share row's root_file_id: with the path
// column gone, that pointer is the authoritative answer to "which inode is
// this share's root".
func (c *Core) GetExistingRootDirectory(ctx context.Context, shareName string) (*metadata.File, error) {
	var (
		id           uuid.UUID
		fileType     int16
		mode         int32
		uid          int32
		gid          int32
		size         int64
		atime        int64
		mtime        int64
		ctime        int64
		creationTime int64
		hidden       bool
		nlink        int32
	)

	err := c.X.QueryRow(ctx, c.D.Shares().SelectRootInode, shareName).Scan(
		&id, &fileType, &mode, &uid, &gid, &size,
		&atime, &mtime, &ctime, &creationTime, &hidden, &nlink,
	)
	if c.D.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/",
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileType(fileType),
			Mode:         uint32(mode),
			Nlink:        uint32(nlink),
			UID:          uint32(uid),
			GID:          uint32(gid),
			Size:         uint64(size),
			Atime:        sqlcodec.FiletimeToTime(atime),
			Mtime:        sqlcodec.FiletimeToTime(mtime),
			Ctime:        sqlcodec.FiletimeToTime(ctime),
			CreationTime: sqlcodec.FiletimeToTime(creationTime),
			Hidden:       hidden,
		},
	}, nil
}

// CreateRootDirectory creates a share's root directory, or reconciles the
// existing one against attr when the share already has a root.
//
// Reconciling rather than returning the stored root as-is is what makes
// re-attaching a share with changed root ownership or mode take effect: the
// configured attributes are the intent, and the stored inode is a cache of a
// previous run's intent. A caller that only wants to read the root uses
// GetExistingRootDirectory.
//
// The probe and the create are one statement pair with no commit between
// them, so a concurrent caller cannot slip between them and leave an orphaned
// root inode behind; the caller supplies the transaction by running this on a
// Core bound to it.
func (c *Core) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if shareName == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "share name cannot be empty",
		}
	}

	uid := attr.UID
	gid := attr.GID
	mode := attr.Mode
	if mode == 0 {
		mode = 0o755
	}

	c.Log.Info("Creating root directory", "share", shareName, "uid", uid, "gid", gid)

	existing, err := c.GetExistingRootDirectory(ctx, shareName)
	if err != nil {
		return nil, c.D.MapError(err, "CreateRootDirectory", shareName)
	}
	if existing != nil {
		return c.reconcileRootDirectory(ctx, shareName, existing, mode, uid, gid)
	}

	rootID := uuid.New()
	now := time.Now()

	_, err = c.X.Exec(ctx, c.D.Shares().InsertRootInode,
		rootID,
		shareName,
		int16(metadata.FileTypeDirectory),
		int32(mode),
		int32(uid),
		int32(gid),
		int64(0),
		sqlcodec.TimeToFiletime(now), // atime
		sqlcodec.TimeToFiletime(now), // mtime
		sqlcodec.TimeToFiletime(now), // ctime
		sqlcodec.TimeToFiletime(now), // creation_time
		nil,                          // content_id, NULL for directories
		nil,                          // link_target
		nil,                          // device_major
		nil,                          // device_minor
	)
	if err != nil {
		return nil, c.D.MapError(err, "CreateRootDirectory", shareName)
	}

	if _, err := c.X.Exec(ctx, c.D.Shares().UpsertShareRoot, shareName, rootID); err != nil {
		return nil, c.D.MapError(err, "CreateRootDirectory", shareName)
	}

	c.Log.Info("Root directory created", "share", shareName, "root_id", rootID)

	return &metadata.File{
		ID:        rootID,
		ShareName: shareName,
		Path:      "/",
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeDirectory,
			Mode:         mode,
			Nlink:        2, // "." and the parent's entry
			UID:          uid,
			GID:          gid,
			Size:         0,
			Atime:        now,
			Mtime:        now,
			Ctime:        now,
			CreationTime: now,
		},
	}, nil
}

// reconcileRootDirectory brings an existing root inode in line with the
// configured mode and owner, rewriting the row only when something actually
// differs so an unchanged share costs one read and no write.
func (c *Core) reconcileRootDirectory(ctx context.Context, shareName string, root *metadata.File, mode, uid, gid uint32) (*metadata.File, error) {
	needsUpdate := false
	if root.Mode != mode {
		c.Log.Info("Updating root directory mode from config",
			"share", shareName,
			"oldMode", fmt.Sprintf("%o", root.Mode),
			"newMode", fmt.Sprintf("%o", mode))
		root.Mode = mode
		needsUpdate = true
	}
	if root.UID != uid {
		c.Log.Info("Updating root directory UID from config",
			"share", shareName, "oldUID", root.UID, "newUID", uid)
		root.UID = uid
		needsUpdate = true
	}
	if root.GID != gid {
		c.Log.Info("Updating root directory GID from config",
			"share", shareName, "oldGID", root.GID, "newGID", gid)
		root.GID = gid
		needsUpdate = true
	}

	if !needsUpdate {
		c.Log.Info("Root directory already exists, returning existing",
			"share", shareName, "root_id", root.ID)
		return root, nil
	}

	now := time.Now()
	_, err := c.X.Exec(ctx, c.D.Shares().UpdateRootAttrs,
		int32(root.Mode), int32(root.UID), int32(root.GID),
		sqlcodec.TimeToFiletime(now), root.ID,
	)
	if err != nil {
		return nil, c.D.MapError(err, "CreateRootDirectory", shareName)
	}
	root.Ctime = now

	c.Log.Info("Root directory attributes updated from config",
		"share", shareName, "root_id", root.ID)
	return root, nil
}
