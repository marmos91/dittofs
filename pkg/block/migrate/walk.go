package migrate

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// WalkCallback is invoked for every regular FILE in the share tree.
// Directories are NOT delivered (the migration tool is interested in
// files only). Returning a non-nil error aborts the walk.
type WalkCallback func(handle metadata.FileHandle, file *metadata.File) error

// walkPageSize is the per-ListChildren batch size. Chosen large enough
// to amortize per-call overhead while still bounding peak memory in a
// pathological wide-directory case.
const walkPageSize = 256

// WalkShareFiles walks every regular file in the named share, recursing
// into directories via metadata.MetadataStore primitives (GetRootHandle
// + ListChildren + GetFile). Pagination is handled internally (cursor
// loop). Context cancellation aborts the walk and returns ctx.Err().
// Callback errors abort the walk and are returned wrapped.
//
// Note: This helper is deliberately self-contained — the metadata
// store does not expose a single WalkFiles method (03 review
// flagged this), and the existing primitives (GetRootHandle +
// ListChildren) compose into the walk we need without modifying the
// MetadataStore interface.
func WalkShareFiles(
	ctx context.Context,
	mds metadata.Store,
	shareName string,
	fn WalkCallback,
) error {
	root, err := mds.GetRootHandle(ctx, shareName)
	if err != nil {
		return fmt.Errorf("migrate: get root handle for share %q: %w", shareName, err)
	}
	return walkDir(ctx, mds, root, fn)
}

// walkDir recurses one directory. Callers pass the directory handle
// returned from a prior ListChildren entry; pagination is iterated
// internally until the next-cursor is empty.
func walkDir(
	ctx context.Context,
	mds metadata.Store,
	dir metadata.FileHandle,
	fn WalkCallback,
) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, next, err := mds.ListChildren(ctx, dir, cursor, walkPageSize)
		if err != nil {
			return fmt.Errorf("migrate: list children: %w", err)
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			child, err := mds.GetFile(ctx, e.Handle)
			if err != nil {
				return fmt.Errorf("migrate: get file for handle %v: %w", e.Handle, err)
			}
			if child == nil {
				// Defensive: stale dir entry pointing at a deleted
				// inode. Skip rather than abort — the file is gone.
				continue
			}
			if child.Type == metadata.FileTypeDirectory {
				if err := walkDir(ctx, mds, e.Handle, fn); err != nil {
					return err
				}
				continue
			}
			// Only regular files are delivered to the callback. The
			// migration tool does not re-chunk symlinks, devices
			// sockets, or fifos — they have no payload.
			if child.Type != metadata.FileTypeRegular {
				continue
			}
			if err := fn(e.Handle, child); err != nil {
				return fmt.Errorf("migrate: walk callback: %w", err)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return nil
}
