package metadata

import (
	"time"
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
func (s *MetadataService) ReadDirectory(ctx *AuthContext, dirHandle FileHandle, cookie uint64, maxBytes uint32) (*ReadDirPage, error) {
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
		// Non-fatal error, ignore
		_ = store.SetLinkCount(ctx.Context, parentHandle, parentLinkCount-1)
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
