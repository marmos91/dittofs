package metadata

import (
	"time"
)

// ============================================================================
// File Removal Operations (MetadataService methods)
// ============================================================================

// RemoveFile removes a file from its parent directory.
//
// This handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Sticky bit enforcement
//   - Hard link management (decrement or set nlink=0)
//   - Parent timestamp updates
//
// Important: This method does NOT delete the file's content data.
// The returned File includes PayloadID for caller to coordinate content deletion.
// PayloadID is empty if other hard links still reference the content.
//
// POSIX Compliance:
//   - When last link is removed, nlink is set to 0 (not deleted)
//   - This allows fstat() on open file descriptors to return nlink=0
func (s *MetadataService) RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error) {
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

	// Check write permission on parent
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
		return nil, err
	}

	// Get child handle
	fileHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's not a directory
	if file.Type == FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
			Path:    name,
		}
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &file.FileAttr); err != nil {
		return nil, err
	}

	// Get current link count
	linkCount, err := store.GetLinkCount(ctx.Context, fileHandle)
	if err != nil {
		// If we can't get link count, assume 1
		linkCount = 1
	}

	now := time.Now()

	// Prepare return value
	returnFile := &File{
		ID:        file.ID,
		ShareName: file.ShareName,
		Path:      file.Path,
		FileAttr:  file.FileAttr,
	}

	// Execute all write operations in a single transaction for better performance.
	err = store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Handle link count
		if linkCount > 1 {
			// File has other hard links, just decrement count
			// Empty PayloadID signals caller NOT to delete content
			returnFile.PayloadID = ""
			returnFile.Nlink = linkCount - 1
			returnFile.Ctime = now

			// Update file's link count and ctime
			if err := tx.SetLinkCount(ctx.Context, fileHandle, linkCount-1); err != nil {
				return err
			}

			// Update file's ctime
			file.Ctime = now
			if err := tx.PutFile(ctx.Context, file); err != nil {
				return err
			}
		} else {
			// Last link - set nlink=0 but keep metadata for POSIX compliance
			returnFile.Nlink = 0
			returnFile.Ctime = now

			// Set link count to 0
			if err := tx.SetLinkCount(ctx.Context, fileHandle, 0); err != nil {
				return err
			}

			// Update file's ctime and nlink
			file.Ctime = now
			file.Nlink = 0
			if err := tx.PutFile(ctx.Context, file); err != nil {
				return err
			}
		}

		// Remove from parent's children
		if err := tx.DeleteChild(ctx.Context, parentHandle, name); err != nil {
			return err
		}

		// Update parent timestamps
		parent.Mtime = now
		parent.Ctime = now
		return tx.PutFile(ctx.Context, parent)
	})

	if err != nil {
		return nil, err
	}

	return returnFile, nil
}
