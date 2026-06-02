package metadata

import (
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ReadDirPage represents one page of directory entries returned by ReadDirectory.
type ReadDirPage struct {
	// Entries contains the directory entries for this page.
	// Each entry includes a Cookie field for pagination.
	Entries []DirEntry

	// NextCookie is the NFS cookie to use for retrieving the next page.
	// Pass this as the cookie parameter in the next ReadDirectory call.
	// Value of 0 means no more entries (or start of directory).
	NextCookie uint64

	// HasMore indicates whether more entries are available after this page.
	HasMore bool

	// DirMtime is the directory's modification time at the time of the listing.
	// Used by NFS cookie verifier to detect directory changes between READDIR calls.
	DirMtime time.Time
}

// ============================================================================
// Directory Operations (MetadataService methods)
// ============================================================================

// ReadDirectory reads one page of directory entries with permission checking.
//
// The cookie parameter is an opaque uint64 value:
//   - 0: Start from the beginning of the directory
//   - Non-zero: Resume from the position after the entry with this cookie
//
// Each returned entry includes a Cookie field that can be used to resume
// listing from that point. The NextCookie field indicates the cookie to
// use for the next page (0 if no more entries).
func (s *Service) ReadDirectory(ctx *AuthContext, dirHandle FileHandle, cookie uint64, maxBytes uint32) (*ReadDirPage, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}

	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get directory entry to verify type
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    dir.Path,
		}
	}

	// Check read and traverse permissions
	granted, err := s.checkFilePermissions(ctx, dirHandle, PermissionRead|PermissionTraverse)
	if err != nil {
		return nil, err
	}
	if granted&PermissionRead == 0 || granted&PermissionTraverse == 0 {
		return nil, &StoreError{
			Code:    ErrAccessDenied,
			Message: "no read or execute permission on directory",
			Path:    dir.Path,
		}
	}

	// Estimate max entries from maxBytes (rough estimate: ~200 bytes per entry)
	limit := 1000
	if maxBytes > 0 {
		limit = max(int(maxBytes/200), 10)
	}

	// Convert cookie to store token using cookie manager
	token := s.cookies.GetToken(cookie)

	// Call store's CRUD ListChildren method
	entries, nextToken, err := store.ListChildren(ctx.Context, dirHandle, token, limit)
	if err != nil {
		return nil, err
	}

	// Generate cookies for each entry
	for i := range entries {
		entries[i].Cookie = s.cookies.GenerateCookie(dirHandle, entries[i].Name)
	}

	// Generate next cookie from next token
	var nextCookie uint64
	if nextToken != "" {
		nextCookie = s.cookies.GenerateCookie(dirHandle, nextToken)
	}

	return &ReadDirPage{
		Entries:    entries,
		NextCookie: nextCookie,
		HasMore:    nextToken != "",
		DirMtime:   dir.Mtime,
	}, nil
}

// RemoveDirectory removes an empty directory from its parent. The returned
// DirWcc carries the parent directory's pre/post attributes captured atomically
// with the mutation for protocol weak-cache-consistency data (H9).
func (s *Service) RemoveDirectory(ctx *AuthContext, parentHandle FileHandle, name string) (*DirWcc, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Get child handle
	dirHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    name,
		}
	}

	// Check delete permission: WRITE on parent (POSIX) or owner-of-dir (Windows DELETE).
	if err := s.checkDeletePermission(ctx, parentHandle, dir); err != nil {
		return nil, err
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &dir.FileAttr); err != nil {
		return nil, err
	}

	// Recycle instead of destroying when the share has trash enabled. A
	// non-empty directory moves as a single subtree with one DeletedAt on its
	// root, so this guard precedes the empty-directory check. Deletes already
	// inside #recycle, and excluded names, fall through to permanent removal.
	if s.trashPolicy != nil {
		shareName := shareNameForHandle(parentHandle)
		if cfg, ok := s.trashPolicy.TrashConfigForShare(shareName); ok && cfg.Enabled {
			origRel := strings.TrimPrefix(buildPath(parent.Path, name), "/")
			if !inRecycle(origRel) && !cfg.Excluded(name) {
				// Best-effort pre/post WCC around the recycle Move (rare path).
				wccBefore := CopyFileAttr(&parent.FileAttr)
				if _, rErr := s.recycleNode(ctx, shareName, parentHandle, name, origRel); rErr != nil {
					return nil, rErr
				}
				wcc := &DirWcc{Before: wccBefore}
				if after, aErr := store.GetFile(ctx.Context, parentHandle); aErr == nil {
					wcc.After = CopyFileAttr(&after.FileAttr)
				}
				s.notifyDirChange(shareName, parentHandle, lock.DirChangeRemoveEntry, ctx)
				return wcc, nil
			}
		}
	}

	// Check if directory is empty
	entries, _, err := store.ListChildren(ctx.Context, dirHandle, "", 1)
	if err == nil && len(entries) > 0 {
		return nil, &StoreError{
			Code:    ErrNotEmpty,
			Message: "directory not empty",
			Path:    name,
		}
	}

	// wcc brackets the parent attributes around the mutation, captured inside
	// the transaction below (H9).
	wcc := &DirWcc{}

	// Execute all write operations in a single transaction for better performance.
	txErr := store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Re-read the parent inside the transaction so the pre-op snapshot and
		// the timestamp mutation derive from the same committed state.
		if txParent, pErr := tx.GetFile(ctx.Context, parentHandle); pErr == nil && txParent != nil {
			parent = txParent
		}
		wcc.Before = CopyFileAttr(&parent.FileAttr)

		// Remove directory entry
		if err := tx.DeleteFile(ctx.Context, dirHandle); err != nil {
			return err
		}

		// Remove from parent's children
		if err := tx.DeleteChild(ctx.Context, parentHandle, name); err != nil {
			return err
		}

		// Update parent's link count (removing ".." reference)
		parentLinkCount, err := tx.GetLinkCount(ctx.Context, parentHandle)
		if err == nil && parentLinkCount > 0 {
			if err := tx.SetLinkCount(ctx.Context, parentHandle, parentLinkCount-1); err != nil {
				return err
			}
		}

		// Update parent timestamps (including Atime per MS-FSA 2.1.4.4)
		now := time.Now()
		parent.Mtime = now
		parent.Ctime = now
		parent.Atime = now
		wcc.After = CopyFileAttr(&parent.FileAttr)
		return tx.PutFile(ctx.Context, parent)
	})

	if txErr != nil {
		return nil, txErr
	}

	s.notifyDirChange(shareNameForHandle(parentHandle), parentHandle, lock.DirChangeRemoveEntry, ctx)
	return wcc, nil
}

// CreateDirectory creates a new directory in a parent directory. The returned
// DirWcc carries the parent's pre/post attributes captured atomically (H9).
func (s *Service) CreateDirectory(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, *DirWcc, error) {
	file, wcc, err := s.createEntry(ctx, parentHandle, name, attr, FileTypeDirectory, "", 0, 0)
	if err != nil {
		return nil, nil, err
	}
	s.notifyDirChange(shareNameForHandle(parentHandle), parentHandle, lock.DirChangeAddEntry, ctx)
	return file, wcc, nil
}
