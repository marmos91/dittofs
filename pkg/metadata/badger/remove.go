package badger

import (
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// RemoveFile removes a file's metadata from its parent directory.
//
// This performs metadata cleanup including permission validation, type checking,
// and directory entry removal using a BadgerDB write transaction for atomicity.
//
// WARNING: This method does NOT delete the file's content data. The caller must
// coordinate content deletion with the content repository using the ContentID
// from the returned FileAttr.
//
// Hard Links:
// If the file has multiple hard links (linkCount > 1), this removes only one link.
// The ContentID in the returned attributes is set to empty to signal that content
// should NOT be deleted (other links still reference it). Only when the last link
// is removed will the ContentID be returned for content cleanup.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - parentHandle: Handle of the parent directory
//   - name: Name of the file to remove
//
// Returns:
//   - *FileAttr: Attributes of the removed file (includes ContentID for cleanup)
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) RemoveFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.FileAttr, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate name
	if name == "" || name == "." || name == ".." {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var returnAttr *metadata.FileAttr

	err := s.db.Update(func(txn *badger.Txn) error {
		// Verify parent exists and is a directory
		item, err := txn.Get(keyFile(parentHandle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "parent directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get parent: %w", err)
		}

		var parentData *fileData
		err = item.Value(func(val []byte) error {
			pd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			parentData = pd
			return nil
		})
		if err != nil {
			return err
		}

		if parentData.Attr.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "parent is not a directory",
			}
		}

		// Check write permission on parent
		s.mu.Unlock()
		granted, err := s.CheckPermissions(ctx, parentHandle, metadata.PermissionWrite)
		s.mu.Lock()
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on parent directory",
			}
		}

		// Find the file
		childItem, err := txn.Get(keyChild(parentHandle, name))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("file not found: %s", name),
				Path:    name,
			}
		}
		if err != nil {
			return fmt.Errorf("failed to find child: %w", err)
		}

		var fileHandle metadata.FileHandle
		err = childItem.Value(func(val []byte) error {
			fileHandle = metadata.FileHandle(val)
			return nil
		})
		if err != nil {
			return err
		}

		// Get file data
		fileItem, err := txn.Get(keyFile(fileHandle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		var fileData *fileData
		err = fileItem.Value(func(val []byte) error {
			fd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			fileData = fd
			return nil
		})
		if err != nil {
			return err
		}

		// Verify it's not a directory
		if fileData.Attr.Type == metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrIsDirectory,
				Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
				Path:    name,
			}
		}

		// Get link count
		linkCountItem, err := txn.Get(keyLinkCount(fileHandle))
		if err != nil {
			return fmt.Errorf("failed to get link count: %w", err)
		}

		var linkCount uint32
		err = linkCountItem.Value(func(val []byte) error {
			lc, err := decodeUint32(val)
			if err != nil {
				return err
			}
			linkCount = lc
			return nil
		})
		if err != nil {
			return err
		}

		// Make a copy of attributes to return
		returnAttr = &metadata.FileAttr{
			Type:       fileData.Attr.Type,
			Mode:       fileData.Attr.Mode,
			UID:        fileData.Attr.UID,
			GID:        fileData.Attr.GID,
			Size:       fileData.Attr.Size,
			Atime:      fileData.Attr.Atime,
			Mtime:      fileData.Attr.Mtime,
			Ctime:      fileData.Attr.Ctime,
			ContentID:  fileData.Attr.ContentID,
			LinkTarget: fileData.Attr.LinkTarget,
		}

		// Decrement link count
		if linkCount > 1 {
			// File has other hard links, just decrement count
			// Empty ContentID signals to caller that content should NOT be deleted
			returnAttr.ContentID = ""
			linkCount--
			if err := txn.Set(keyLinkCount(fileHandle), encodeUint32(linkCount)); err != nil {
				return fmt.Errorf("failed to update link count: %w", err)
			}
		} else {
			// This was the last link, remove all metadata
			// ContentID is returned so caller can delete content
			if err := txn.Delete(keyFile(fileHandle)); err != nil {
				return fmt.Errorf("failed to delete file: %w", err)
			}
			if err := txn.Delete(keyLinkCount(fileHandle)); err != nil {
				return fmt.Errorf("failed to delete link count: %w", err)
			}
			if err := txn.Delete(keyParent(fileHandle)); err != nil && err != badger.ErrKeyNotFound {
				return fmt.Errorf("failed to delete parent: %w", err)
			}
			// Clean up device numbers and handle mapping if present (ignore not found)
			_ = txn.Delete(keyDeviceNumber(fileHandle))
			_ = txn.Delete(keyHandleMapping(fileHandle))
		}

		// Remove from parent's children
		if err := txn.Delete(keyChild(parentHandle, name)); err != nil {
			return fmt.Errorf("failed to remove child: %w", err)
		}

		// Update parent timestamps
		now := time.Now()
		parentData.Attr.Mtime = now
		parentData.Attr.Ctime = now
		parentBytes, err := encodeFileData(parentData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(parentHandle), parentBytes); err != nil {
			return fmt.Errorf("failed to update parent: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return returnAttr, nil
}

// RemoveDirectory removes an empty directory's metadata from its parent.
//
// This performs metadata cleanup including permission validation, type checking,
// empty check, and directory entry removal using a BadgerDB write transaction.
//
// Empty Directory:
// A directory is considered empty if it has no children. This is checked by
// attempting to iterate over children with the directory's key prefix.
//
// Link Counts:
// Removing a directory decrements the parent directory's link count (because the
// removed directory's ".." entry pointed to the parent).
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - parentHandle: Handle of the parent directory
//   - name: Name of the directory to remove
//
// Returns:
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) RemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) error {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate name
	if name == "" || name == "." || name == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		// Verify parent exists and is a directory
		item, err := txn.Get(keyFile(parentHandle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "parent directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get parent: %w", err)
		}

		var parentData *fileData
		err = item.Value(func(val []byte) error {
			pd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			parentData = pd
			return nil
		})
		if err != nil {
			return err
		}

		if parentData.Attr.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "parent is not a directory",
			}
		}

		// Check write permission on parent
		s.mu.Unlock()
		granted, err := s.CheckPermissions(ctx, parentHandle, metadata.PermissionWrite)
		s.mu.Lock()
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on parent directory",
			}
		}

		// Find the directory
		childItem, err := txn.Get(keyChild(parentHandle, name))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("directory not found: %s", name),
				Path:    name,
			}
		}
		if err != nil {
			return fmt.Errorf("failed to find child: %w", err)
		}

		var dirHandle metadata.FileHandle
		err = childItem.Value(func(val []byte) error {
			dirHandle = metadata.FileHandle(val)
			return nil
		})
		if err != nil {
			return err
		}

		// Get directory data
		dirItem, err := txn.Get(keyFile(dirHandle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get directory: %w", err)
		}

		var dirData *fileData
		err = dirItem.Value(func(val []byte) error {
			dd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			dirData = dd
			return nil
		})
		if err != nil {
			return err
		}

		// Verify it's a directory
		if dirData.Attr.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
				Path:    name,
			}
		}

		// Check if directory is empty by attempting to find any children
		opts := badger.DefaultIteratorOptions
		opts.Prefix = keyChildPrefix(dirHandle)
		opts.PrefetchValues = false // We only need to check existence

		it := txn.NewIterator(opts)
		defer it.Close()

		it.Rewind()
		if it.Valid() {
			// Directory has children, not empty
			return &metadata.StoreError{
				Code:    metadata.ErrNotEmpty,
				Message: "directory not empty",
				Path:    name,
			}
		}

		// Remove directory metadata
		if err := txn.Delete(keyFile(dirHandle)); err != nil {
			return fmt.Errorf("failed to delete directory: %w", err)
		}
		if err := txn.Delete(keyLinkCount(dirHandle)); err != nil {
			return fmt.Errorf("failed to delete link count: %w", err)
		}
		if err := txn.Delete(keyParent(dirHandle)); err != nil && err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to delete parent: %w", err)
		}
		// Clean up handle mapping if it's a hashed handle (ignore not found)
		_ = txn.Delete(keyHandleMapping(dirHandle))

		// Remove from parent's children
		if err := txn.Delete(keyChild(parentHandle, name)); err != nil {
			return fmt.Errorf("failed to remove child: %w", err)
		}

		// Decrement parent's link count
		// (removing a subdirectory removes one ".." reference to parent)
		parentLinkItem, err := txn.Get(keyLinkCount(parentHandle))
		if err == nil {
			var parentLinkCount uint32
			err = parentLinkItem.Value(func(val []byte) error {
				plc, err := decodeUint32(val)
				if err != nil {
					return err
				}
				parentLinkCount = plc
				return nil
			})
			if err != nil {
				return err
			}

			if parentLinkCount > 0 {
				parentLinkCount--
				if err := txn.Set(keyLinkCount(parentHandle), encodeUint32(parentLinkCount)); err != nil {
					return fmt.Errorf("failed to update parent link count: %w", err)
				}
			}
		}

		// Update parent timestamps
		now := time.Now()
		parentData.Attr.Mtime = now
		parentData.Attr.Ctime = now
		parentBytes, err := encodeFileData(parentData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(parentHandle), parentBytes); err != nil {
			return fmt.Errorf("failed to update parent: %w", err)
		}

		return nil
	})
}
