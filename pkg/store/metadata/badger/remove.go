package badger

import (
	"errors"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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
) (*metadata.File, error) {
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

	// Check write permission BEFORE acquiring lock to avoid unlock/relock race
	granted, err := s.CheckPermissions(ctx, parentHandle, metadata.PermissionWrite)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionWrite == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on parent directory",
		}
	}

	var returnFile *metadata.File
	var removedHandle metadata.FileHandle

	err = s.db.Update(func(txn *badger.Txn) error {
		// Verify parent exists and is a directory
		_, parentID, err := metadata.DecodeFileHandle(parentHandle)
		if err != nil {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidHandle,
				Message: "invalid parent handle",
			}
		}
		item, err := txn.Get(keyFile(parentID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "parent directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get parent: %w", err)
		}

		var parentFile *metadata.File
		err = item.Value(func(val []byte) error {
			pd, err := decodeFile(val)
			if err != nil {
				return err
			}
			parentFile = pd
			return nil
		})
		if err != nil {
			return err
		}

		if parentFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "parent is not a directory",
			}
		}

		// Find the file
		childItem, err := txn.Get(keyChild(parentID, name))
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

		var fileID uuid.UUID
		err = childItem.Value(func(val []byte) error {
			if len(val) != 16 {
				return fmt.Errorf("invalid UUID length: %d", len(val))
			}
			copy(fileID[:], val)
			return nil
		})
		if err != nil {
			return err
		}

		// Get file data
		fileItem, err := txn.Get(keyFile(fileID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		var file *metadata.File
		err = fileItem.Value(func(val []byte) error {
			fd, err := decodeFile(val)
			if err != nil {
				return err
			}
			file = fd
			return nil
		})
		if err != nil {
			return err
		}

		// Generate handle for logging
		removedHandle, _ = metadata.EncodeFileHandle(file)

		// Verify it's not a directory
		if file.Type == metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrIsDirectory,
				Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
				Path:    name,
			}
		}

		// Get link count
		linkCountItem, err := txn.Get(keyLinkCount(fileID))
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

		// Make a copy of file to return
		returnFile = &metadata.File{
			ID:        file.ID,
			ShareName: file.ShareName,
			Path:      file.Path,
			FileAttr: metadata.FileAttr{
				Type:       file.Type,
				Mode:       file.Mode,
				UID:        file.UID,
				GID:        file.GID,
				Size:       file.Size,
				Atime:      file.Atime,
				Mtime:      file.Mtime,
				Ctime:      file.Ctime,
				ContentID:  file.ContentID,
				LinkTarget: file.LinkTarget,
			},
		}

		// Decrement link count
		if linkCount > 1 {
			// File has other hard links, just decrement count
			// Empty ContentID signals to caller that content should NOT be deleted
			returnFile.ContentID = ""
			linkCount--
			if err := txn.Set(keyLinkCount(fileID), encodeUint32(linkCount)); err != nil {
				return fmt.Errorf("failed to update link count: %w", err)
			}
		} else {
			// This was the last link, remove all metadata
			// ContentID is returned so caller can delete content
			if err := txn.Delete(keyFile(fileID)); err != nil {
				return fmt.Errorf("failed to delete file: %w", err)
			}
			if err := txn.Delete(keyLinkCount(fileID)); err != nil {
				return fmt.Errorf("failed to delete link count: %w", err)
			}
			if err := txn.Delete(keyParent(fileID)); err != nil && err != badger.ErrKeyNotFound {
				return fmt.Errorf("failed to delete parent: %w", err)
			}
			// Clean up device numbers if present (ignore not found)
			_ = txn.Delete(keyDeviceNumber(fileID))
		}

		// Remove from parent's children
		if err := txn.Delete(keyChild(parentID, name)); err != nil {
			return fmt.Errorf("failed to remove child: %w", err)
		}

		// Update parent timestamps
		now := time.Now()
		parentFile.Mtime = now
		parentFile.Ctime = now
		parentBytes, err := encodeFile(parentFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(parentID), parentBytes); err != nil {
			return fmt.Errorf("failed to update parent: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	logger.Debug("REMOVE succeeded: name=%s parent_handle=%s file_handle=%s", name, parentHandle, removedHandle)

	return returnFile, nil
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

	// Check write permission BEFORE acquiring lock to avoid unlock/relock race
	granted, err := s.CheckPermissions(ctx, parentHandle, metadata.PermissionWrite)
	if err != nil {
		return err
	}
	if granted&metadata.PermissionWrite == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on parent directory",
		}
	}

	var removedHandle metadata.FileHandle

	// NFSv3 Weak Consistency Mitigation:
	// Clients may pipeline RMDIR requests before all child REMOVE operations complete.
	// If we detect a "nearly empty" directory (≤5 children), retry once after a brief
	// delay to allow in-flight operations to complete. This trades a small latency
	// increase for much better UX with tools like rm -rf and Finder.
	const maxRetryChildren = 5
	const retryDelay = 50 * time.Millisecond
	maxAttempts := 2

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		childCount, shouldRetry, attemptErr := s.attemptRemoveDirectory(ctx, parentHandle, name, &removedHandle)

		if attemptErr == nil {
			// Success!
			err = nil
			break
		}

		// If not a "directory not empty" error, fail immediately
		var storeErr *metadata.StoreError
		if !errors.As(attemptErr, &storeErr) || storeErr.Code != metadata.ErrNotEmpty {
			err = attemptErr
			break
		}

		// Decide whether to retry
		if attempt < maxAttempts && shouldRetry && childCount > 0 && childCount <= maxRetryChildren {
			logger.Debug("RMDIR retry: directory has %d children, waiting %v before retry (attempt %d/%d)",
				childCount, retryDelay, attempt, maxAttempts)

			// Release lock during wait to allow other operations to complete

			select {
			case <-time.After(retryDelay):
				// Retry
			case <-ctx.Context.Done():
				return ctx.Context.Err()
			}

		} else {
			// Don't retry - fail immediately
			err = attemptErr
			break
		}
	}

	if err != nil {
		return err
	}

	logger.Debug("RMDIR succeeded: name=%s parent_handle=%s dir_handle=%s", name, parentHandle, removedHandle)

	return nil
}

// attemptRemoveDirectory performs a single attempt at removing a directory.
// Returns (childCount, shouldRetry, error)
func (s *BadgerMetadataStore) attemptRemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	removedHandle *metadata.FileHandle,
) (int, bool, error) {
	var childCountCapture int
	var dirID uuid.UUID

	err := s.db.Update(func(txn *badger.Txn) error {
		// Verify parent exists and is a directory
		_, parentID, err := metadata.DecodeFileHandle(parentHandle)
		if err != nil {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidHandle,
				Message: "invalid parent handle",
			}
		}
		item, err := txn.Get(keyFile(parentID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "parent directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get parent: %w", err)
		}

		var parentFile *metadata.File
		err = item.Value(func(val []byte) error {
			pd, err := decodeFile(val)
			if err != nil {
				return err
			}
			parentFile = pd
			return nil
		})
		if err != nil {
			return err
		}

		if parentFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "parent is not a directory",
			}
		}

		// Find the directory
		childItem, err := txn.Get(keyChild(parentID, name))
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

		err = childItem.Value(func(val []byte) error {
			if len(val) != 16 {
				return fmt.Errorf("invalid UUID length: %d", len(val))
			}
			copy(dirID[:], val)
			return nil
		})
		if err != nil {
			return err
		}

		// Get directory data
		dirItem, err := txn.Get(keyFile(dirID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get directory: %w", err)
		}

		var dir *metadata.File
		err = dirItem.Value(func(val []byte) error {
			dd, err := decodeFile(val)
			if err != nil {
				return err
			}
			dir = dd
			return nil
		})
		if err != nil {
			return err
		}

		// Generate handle for logging
		dirHandle, _ := metadata.EncodeFileHandle(dir)
		*removedHandle = dirHandle

		// Verify it's a directory
		if dir.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
				Path:    name,
			}
		}

		// Check if directory is empty by attempting to find any children
		opts := badger.DefaultIteratorOptions
		opts.Prefix = keyChildPrefix(dirID)
		opts.PrefetchValues = false // We only need to check existence

		it := txn.NewIterator(opts)
		defer it.Close()

		it.Rewind()
		if it.Valid() {
			// Directory has children - count them
			childCount := 0
			childNames := []string{}
			for it.Rewind(); it.Valid(); it.Next() {
				childCount++
				if len(childNames) < 10 {
					key := it.Item().Key()
					prefix := keyChildPrefix(dirID)
					if len(key) > len(prefix) {
						childNames = append(childNames, string(key[len(prefix):]))
					}
				}
			}

			// Capture for return value
			childCountCapture = childCount

			logger.Warn("RMDIR attempt failed - directory not empty: name=%s handle=%s children_count=%d children_sample=%v",
				name, dirHandle, childCount, childNames)

			return &metadata.StoreError{
				Code:    metadata.ErrNotEmpty,
				Message: "directory not empty",
				Path:    name,
			}
		}

		// Remove directory metadata
		if err := txn.Delete(keyFile(dirID)); err != nil {
			return fmt.Errorf("failed to delete directory: %w", err)
		}
		if err := txn.Delete(keyLinkCount(dirID)); err != nil {
			return fmt.Errorf("failed to delete link count: %w", err)
		}
		if err := txn.Delete(keyParent(dirID)); err != nil && err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to delete parent: %w", err)
		}

		// Remove from parent's children
		if err := txn.Delete(keyChild(parentID, name)); err != nil {
			return fmt.Errorf("failed to remove child: %w", err)
		}

		// Decrement parent's link count
		// (removing a subdirectory removes one ".." reference to parent)
		parentLinkItem, err := txn.Get(keyLinkCount(parentID))
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
				if err := txn.Set(keyLinkCount(parentID), encodeUint32(parentLinkCount)); err != nil {
					return fmt.Errorf("failed to update parent link count: %w", err)
				}
			}
		}

		// Update parent timestamps
		now := time.Now()
		parentFile.Mtime = now
		parentFile.Ctime = now
		parentBytes, err := encodeFile(parentFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(parentID), parentBytes); err != nil {
			return fmt.Errorf("failed to update parent: %w", err)
		}

		return nil
	})

	// Determine if we should retry (only for "directory not empty" errors)
	shouldRetry := false
	if err != nil {
		var storeErr *metadata.StoreError
		if errors.As(err, &storeErr) && storeErr.Code == metadata.ErrNotEmpty {
			shouldRetry = true
		}
	}

	return childCountCapture, shouldRetry, err
}
