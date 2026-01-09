package metadata

import (
	"time"
)

// ReadDirPage represents one page of directory entries returned by ReadDirectory.
type ReadDirPage struct {
	// Entries contains the directory entries for this page.
	Entries []DirEntry

	// NextToken is the pagination token to use for retrieving the next page.
	NextToken string

	// HasMore indicates whether more entries are available after this page.
	HasMore bool
}

// ============================================================================
// Directory Operations (MetadataService methods)
// ============================================================================

// ReadDirectory reads one page of directory entries with permission checking.
func (s *MetadataService) ReadDirectory(ctx *AuthContext, dirHandle FileHandle, token string, maxBytes uint32) (*ReadDirPage, error) {
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
		limit = int(maxBytes / 200)
		if limit < 10 {
			limit = 10
		}
	}

	// Call store's CRUD ListChildren method
	entries, nextToken, err := store.ListChildren(ctx.Context, dirHandle, token, limit)
	if err != nil {
		return nil, err
	}

	return &ReadDirPage{
		Entries:   entries,
		NextToken: nextToken,
		HasMore:   nextToken != "",
	}, nil
}

// RemoveDirectory removes an empty directory from its parent.
func (s *MetadataService) RemoveDirectory(ctx *AuthContext, parentHandle FileHandle, name string) error {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
		return err
	}

	// Get child handle
	dirHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    name,
		}
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &dir.FileAttr); err != nil {
		return err
	}

	// Check if directory is empty
	entries, _, err := store.ListChildren(ctx.Context, dirHandle, "", 1)
	if err == nil && len(entries) > 0 {
		return &StoreError{
			Code:    ErrNotEmpty,
			Message: "directory not empty",
			Path:    name,
		}
	}

	// Remove directory entry
	if err := store.DeleteFile(ctx.Context, dirHandle); err != nil {
		return err
	}

	// Remove from parent's children
	if err := store.DeleteChild(ctx.Context, parentHandle, name); err != nil {
		return err
	}

	// Update parent's link count (removing ".." reference)
	parentLinkCount, err := store.GetLinkCount(ctx.Context, parentHandle)
	if err == nil && parentLinkCount > 0 {
		if err := store.SetLinkCount(ctx.Context, parentHandle, parentLinkCount-1); err != nil {
			// Non-fatal, continue
		}
	}

	// Update parent timestamps
	now := time.Now()
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutFile(ctx.Context, parent); err != nil {
		return err
	}

	return nil
}

// CreateDirectory creates a new directory in a parent directory.
func (s *MetadataService) CreateDirectory(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error) {
	return s.createEntry(ctx, parentHandle, name, attr, FileTypeDirectory, "", 0, 0)
}
