package badger

import (
	"context"
	"fmt"
	"slices"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Lookup resolves a name within a directory to a file handle and attributes.
//
// This is the fundamental operation for path resolution, combining directory
// search, permission checking, and attribute retrieval into a single atomic
// operation using a BadgerDB read transaction.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - dirHandle: Directory to search in
//   - name: Name to resolve (including "." and "..")
//
// Returns:
//   - FileHandle: Handle of the resolved file/directory
//   - *FileAttr: Complete attributes of the resolved file/directory
//   - error: ErrNotFound, ErrNotDirectory, ErrAccessDenied, or context errors
func (s *BadgerMetadataStore) Lookup(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
) (metadata.FileHandle, *metadata.FileAttr, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var targetHandle metadata.FileHandle
	var targetAttr *metadata.FileAttr

	err := s.db.View(func(txn *badger.Txn) error {
		// Get directory data
		item, err := txn.Get(keyFile(dirHandle))
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
		err = item.Value(func(val []byte) error {
			fd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			dirData = fd
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
			}
		}

		// Check execute/traverse permission on directory (search permission)
		// Release lock temporarily for CheckPermissions
		s.mu.RUnlock()
		granted, err := s.CheckPermissions(ctx, dirHandle, metadata.PermissionTraverse)
		s.mu.RLock()

		if err != nil {
			return err
		}
		if granted&metadata.PermissionTraverse == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no search permission on directory",
			}
		}

		// Handle special names
		switch name {
		case ".":
			// Current directory - return directory itself
			targetHandle = dirHandle
			targetAttr = dirData.Attr

		case "..":
			// Parent directory
			parentItem, err := txn.Get(keyParent(dirHandle))
			if err == badger.ErrKeyNotFound {
				// This is a root directory, ".." refers to itself
				targetHandle = dirHandle
				targetAttr = dirData.Attr
			} else if err != nil {
				return fmt.Errorf("failed to get parent: %w", err)
			} else {
				err = parentItem.Value(func(val []byte) error {
					targetHandle = metadata.FileHandle(val)
					return nil
				})
				if err != nil {
					return err
				}

				// Get parent attributes
				parentFileItem, err := txn.Get(keyFile(targetHandle))
				if err != nil {
					return fmt.Errorf("failed to get parent file: %w", err)
				}
				err = parentFileItem.Value(func(val []byte) error {
					parentData, err := decodeFileData(val)
					if err != nil {
						return err
					}
					targetAttr = parentData.Attr
					return nil
				})
				if err != nil {
					return err
				}
			}

		default:
			// Regular name lookup
			childItem, err := txn.Get(keyChild(dirHandle, name))
			if err == badger.ErrKeyNotFound {
				return &metadata.StoreError{
					Code:    metadata.ErrNotFound,
					Message: fmt.Sprintf("name not found: %s", name),
					Path:    name,
				}
			}
			if err != nil {
				return fmt.Errorf("failed to lookup child: %w", err)
			}

			err = childItem.Value(func(val []byte) error {
				targetHandle = metadata.FileHandle(val)
				return nil
			})
			if err != nil {
				return err
			}

			// Get child attributes
			childFileItem, err := txn.Get(keyFile(targetHandle))
			if err == badger.ErrKeyNotFound {
				return &metadata.StoreError{
					Code:    metadata.ErrNotFound,
					Message: "target file not found",
				}
			}
			if err != nil {
				return fmt.Errorf("failed to get child file: %w", err)
			}

			err = childFileItem.Value(func(val []byte) error {
				childData, err := decodeFileData(val)
				if err != nil {
					return err
				}
				targetAttr = childData.Attr
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	// Return a copy of attributes to prevent external modification
	attrCopy := *targetAttr
	return targetHandle, &attrCopy, nil
}

// GetFile retrieves file attributes by handle.
//
// This is a lightweight operation that only reads metadata without permission
// checking using a BadgerDB read transaction.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: The file handle to query
//
// Returns:
//   - *FileAttr: Complete file attributes
//   - error: ErrNotFound, ErrInvalidHandle, or context errors
func (s *BadgerMetadataStore) GetFile(
	ctx context.Context,
	handle metadata.FileHandle,
) (*metadata.FileAttr, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Check for invalid (empty) handle
	if len(handle) == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var attr *metadata.FileAttr

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyFile(handle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		return item.Value(func(val []byte) error {
			fileData, err := decodeFileData(val)
			if err != nil {
				return err
			}
			attr = fileData.Attr
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	// Return a copy to prevent external modification
	attrCopy := *attr
	return &attrCopy, nil
}

// SetFileAttributes updates file attributes with validation and access control.
//
// This implements selective attribute updates based on the Set* flags in attrs,
// using a BadgerDB write transaction for atomicity.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - handle: The file handle to update
//   - attrs: Attributes to set (only non-nil fields are modified)
//
// Returns:
//   - error: ErrNotFound, ErrAccessDenied, ErrPermissionDenied, etc.
func (s *BadgerMetadataStore) SetFileAttributes(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	attrs *metadata.SetAttrs,
) error {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		// Get file data
		item, err := txn.Get(keyFile(handle))
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
		err = item.Value(func(val []byte) error {
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

		attr := fileData.Attr

		identity := ctx.Identity
		if identity == nil || identity.UID == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "authentication required to modify attributes",
			}
		}

		uid := *identity.UID
		isOwner := uid == attr.UID
		isRoot := uid == 0

		// Track if any changes were made (for ctime update)
		changed := false

		// Validate and apply mode changes
		if attrs.Mode != nil {
			if !isOwner && !isRoot {
				return &metadata.StoreError{
					Code:    metadata.ErrPermissionDenied,
					Message: "only owner or root can change mode",
				}
			}

			newMode := *attrs.Mode & 0o7777
			attr.Mode = newMode
			changed = true
		}

		// Validate and apply UID changes
		if attrs.UID != nil {
			if !isRoot {
				return &metadata.StoreError{
					Code:    metadata.ErrPermissionDenied,
					Message: "only root can change ownership",
				}
			}

			attr.UID = *attrs.UID
			changed = true
		}

		// Validate and apply GID changes
		if attrs.GID != nil {
			if !isRoot {
				if !isOwner {
					return &metadata.StoreError{
						Code:    metadata.ErrPermissionDenied,
						Message: "only owner or root can change group",
					}
				}

				// Check if owner is member of target group
				targetGID := *attrs.GID
				isMember := false
				if identity.GID != nil && *identity.GID == targetGID {
					isMember = true
				}
				if !isMember && slices.Contains(identity.GIDs, targetGID) {
					isMember = true
				}

				if !isMember {
					return &metadata.StoreError{
						Code:    metadata.ErrPermissionDenied,
						Message: "owner must be member of target group",
					}
				}
			}

			attr.GID = *attrs.GID
			changed = true
		}

		// Validate and apply size changes
		if attrs.Size != nil {
			if attr.Type != metadata.FileTypeRegular {
				return &metadata.StoreError{
					Code:    metadata.ErrInvalidArgument,
					Message: "cannot change size of non-regular file",
				}
			}

			// Check write permission
			s.mu.Unlock()
			granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
			s.mu.Lock()
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrAccessDenied,
					Message: "no write permission",
				}
			}

			attr.Size = *attrs.Size
			attr.Mtime = time.Now()
			changed = true
		}

		// Apply atime changes
		if attrs.Atime != nil {
			if !isOwner && !isRoot {
				s.mu.Unlock()
				granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
				s.mu.Lock()
				if err != nil {
					return err
				}
				if granted&metadata.PermissionWrite == 0 {
					return &metadata.StoreError{
						Code:    metadata.ErrAccessDenied,
						Message: "no permission to change atime",
					}
				}
			}

			attr.Atime = *attrs.Atime
			changed = true
		}

		// Apply mtime changes
		if attrs.Mtime != nil {
			if !isOwner && !isRoot {
				s.mu.Unlock()
				granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
				s.mu.Lock()
				if err != nil {
					return err
				}
				if granted&metadata.PermissionWrite == 0 {
					return &metadata.StoreError{
						Code:    metadata.ErrAccessDenied,
						Message: "no permission to change mtime",
					}
				}
			}

			attr.Mtime = *attrs.Mtime
			changed = true
		}

		// Update ctime if any changes were made
		if changed {
			attr.Ctime = time.Now()

			// Store updated file data
			fileBytes, err := encodeFileData(fileData)
			if err != nil {
				return err
			}
			if err := txn.Set(keyFile(handle), fileBytes); err != nil {
				return fmt.Errorf("failed to update file data: %w", err)
			}
		}

		return nil
	})
}

// Create creates a new file or directory.
//
// The type is determined by attr.Type (must be FileTypeRegular or FileTypeDirectory).
// This uses a BadgerDB write transaction to ensure atomicity of all related operations.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new file/directory
//   - attr: Attributes including Type (must be Regular or Directory)
//
// Returns:
//   - FileHandle: Handle of the newly created file/directory
//   - error: ErrInvalidArgument, ErrAccessDenied, ErrAlreadyExists, or other errors
func (s *BadgerMetadataStore) Create(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (metadata.FileHandle, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate type
	if attr.Type != metadata.FileTypeRegular && attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "Create only supports regular files and directories",
		}
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

	var newHandle metadata.FileHandle

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

		// Check if name already exists
		_, err = txn.Get(keyChild(parentHandle, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Build full path and generate deterministic handle
		fullPath, err := buildFullPath(s, parentHandle, name)
		if err != nil {
			return fmt.Errorf("failed to build full path: %w", err)
		}

		handle, isPathBased := generateFileHandle(parentData.ShareName, fullPath)
		newHandle = handle

		// If hash-based, store the reverse mapping
		if !isPathBased {
			if err := storeHashedHandleMapping(txn, handle, parentData.ShareName, fullPath); err != nil {
				return fmt.Errorf("failed to store handle mapping: %w", err)
			}
		}

		now := time.Now()

		// Set defaults if not provided
		mode := attr.Mode
		if mode == 0 {
			if attr.Type == metadata.FileTypeDirectory {
				mode = 0755
			} else {
				mode = 0644
			}
		}

		// Use authenticated user's credentials if not provided
		uid := attr.UID
		gid := attr.GID
		if ctx.Identity != nil && ctx.Identity.UID != nil {
			if uid == 0 {
				uid = *ctx.Identity.UID
			}
			if gid == 0 && ctx.Identity.GID != nil {
				gid = *ctx.Identity.GID
			}
		}

		// Complete file attributes
		newAttr := &metadata.FileAttr{
			Type:       attr.Type,
			Mode:       mode & 0o7777,
			UID:        uid,
			GID:        gid,
			Size:       0,
			Atime:      now,
			Mtime:      now,
			Ctime:      now,
			LinkTarget: "",
		}

		// Type-specific initialization
		if attr.Type == metadata.FileTypeRegular {
			// Generate ContentID for regular files
			// Format: shareName/path/to/file (e.g., "export/docs/report.pdf")
			// This mirrors the filesystem structure in S3 for easy inspection and recovery
			contentID := buildContentID(parentData.ShareName, fullPath)
			newAttr.ContentID = metadata.ContentID(contentID)
		} else {
			newAttr.ContentID = ""
		}

		// Store file with ShareName inherited from parent
		fileData := &fileData{
			Attr:      newAttr,
			ShareName: parentData.ShareName,
		}
		fileBytes, err := encodeFileData(fileData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(handle), fileBytes); err != nil {
			return fmt.Errorf("failed to store file: %w", err)
		}

		// Set link count
		var linkCount uint32
		if attr.Type == metadata.FileTypeDirectory {
			linkCount = 2 // "." and parent entry
		} else {
			linkCount = 1
		}
		if err := txn.Set(keyLinkCount(handle), encodeUint32(linkCount)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		// Add to parent's children
		if err := txn.Set(keyChild(parentHandle, name), []byte(handle)); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Set parent relationship
		if err := txn.Set(keyParent(handle), []byte(parentHandle)); err != nil {
			return fmt.Errorf("failed to set parent: %w", err)
		}

		// Update parent timestamps
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

	return newHandle, nil
}

// CreateHardLink creates a hard link to an existing file.
//
// This uses a BadgerDB write transaction to atomically create the link and
// increment the link count.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - dirHandle: Target directory where the link will be created
//   - name: Name for the new link
//   - targetHandle: File to link to
//
// Returns:
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) CreateHardLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	targetHandle metadata.FileHandle,
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
		// Verify target exists and is not a directory
		targetItem, err := txn.Get(keyFile(targetHandle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "target file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get target: %w", err)
		}

		var targetData *fileData
		err = targetItem.Value(func(val []byte) error {
			td, err := decodeFileData(val)
			if err != nil {
				return err
			}
			targetData = td
			return nil
		})
		if err != nil {
			return err
		}

		if targetData.Attr.Type == metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrIsDirectory,
				Message: "cannot create hard link to directory",
			}
		}

		// Verify directory exists
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

		if dirData.Attr.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
			}
		}

		// Check write permission on directory
		s.mu.Unlock()
		granted, err := s.CheckPermissions(ctx, dirHandle, metadata.PermissionWrite)
		s.mu.Lock()
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on directory",
			}
		}

		// Check if name already exists
		_, err = txn.Get(keyChild(dirHandle, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Add link
		if err := txn.Set(keyChild(dirHandle, name), []byte(targetHandle)); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Increment link count
		linkCountItem, err := txn.Get(keyLinkCount(targetHandle))
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

		linkCount++
		if err := txn.Set(keyLinkCount(targetHandle), encodeUint32(linkCount)); err != nil {
			return fmt.Errorf("failed to update link count: %w", err)
		}

		// Update target ctime and directory timestamps
		now := time.Now()
		targetData.Attr.Ctime = now
		targetBytes, err := encodeFileData(targetData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(targetHandle), targetBytes); err != nil {
			return fmt.Errorf("failed to update target: %w", err)
		}

		dirData.Attr.Mtime = now
		dirData.Attr.Ctime = now
		dirBytes, err := encodeFileData(dirData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(dirHandle), dirBytes); err != nil {
			return fmt.Errorf("failed to update directory: %w", err)
		}

		return nil
	})
}

// Move moves or renames a file or directory atomically.
//
// This uses a BadgerDB write transaction to ensure all changes are atomic.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - fromDir: Source directory handle
//   - fromName: Current name
//   - toDir: Destination directory handle
//   - toName: New name
//
// Returns:
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) Move(
	ctx *metadata.AuthContext,
	fromDir metadata.FileHandle,
	fromName string,
	toDir metadata.FileHandle,
	toName string,
) error {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate names
	if fromName == "" || fromName == "." || fromName == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid source name",
			Path:    fromName,
		}
	}
	if toName == "" || toName == "." || toName == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid destination name",
			Path:    toName,
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(txn *badger.Txn) error {
		// Get source handle
		sourceItem, err := txn.Get(keyChild(fromDir, fromName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("source not found: %s", fromName),
				Path:    fromName,
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get source: %w", err)
		}

		var sourceHandle metadata.FileHandle
		err = sourceItem.Value(func(val []byte) error {
			sourceHandle = metadata.FileHandle(val)
			return nil
		})
		if err != nil {
			return err
		}

		// Check if same location (no-op)
		if handleToKey(fromDir) == handleToKey(toDir) && fromName == toName {
			return nil
		}

		// Get source data
		sourceFileItem, err := txn.Get(keyFile(sourceHandle))
		if err != nil {
			return fmt.Errorf("failed to get source file: %w", err)
		}

		var sourceData *fileData
		err = sourceFileItem.Value(func(val []byte) error {
			sd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			sourceData = sd
			return nil
		})
		if err != nil {
			return err
		}

		// Check write permission on source directory
		s.mu.Unlock()
		granted, err := s.CheckPermissions(ctx, fromDir, metadata.PermissionWrite)
		s.mu.Lock()
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on source directory",
			}
		}

		// Check write permission on destination directory
		s.mu.Unlock()
		granted, err = s.CheckPermissions(ctx, toDir, metadata.PermissionWrite)
		s.mu.Lock()
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on destination directory",
			}
		}

		// Check if destination exists
		destChildItem, err := txn.Get(keyChild(toDir, toName))
		if err == nil {
			// Destination exists - handle replacement
			var destHandle metadata.FileHandle
			err = destChildItem.Value(func(val []byte) error {
				destHandle = metadata.FileHandle(val)
				return nil
			})
			if err != nil {
				return err
			}

			// Get destination data
			destFileItem, err := txn.Get(keyFile(destHandle))
			if err != nil {
				return fmt.Errorf("failed to get destination file: %w", err)
			}

			var destData *fileData
			err = destFileItem.Value(func(val []byte) error {
				dd, err := decodeFileData(val)
				if err != nil {
					return err
				}
				destData = dd
				return nil
			})
			if err != nil {
				return err
			}

			// Validate replacement rules
			if sourceData.Attr.Type == metadata.FileTypeDirectory {
				if destData.Attr.Type != metadata.FileTypeDirectory {
					return &metadata.StoreError{
						Code:    metadata.ErrNotDirectory,
						Message: "cannot replace non-directory with directory",
					}
				}
				// Check if destination directory is empty
				opts := badger.DefaultIteratorOptions
				opts.Prefix = keyChildPrefix(destHandle)
				opts.PrefetchValues = false

				it := txn.NewIterator(opts)
				defer it.Close()

				it.Rewind()
				if it.Valid() {
					return &metadata.StoreError{
						Code:    metadata.ErrNotEmpty,
						Message: "destination directory not empty",
						Path:    toName,
					}
				}
			} else {
				if destData.Attr.Type == metadata.FileTypeDirectory {
					return &metadata.StoreError{
						Code:    metadata.ErrIsDirectory,
						Message: "cannot replace directory with non-directory",
					}
				}
			}

			// Remove destination - properly cleanup all metadata
			if err := txn.Delete(keyChild(toDir, toName)); err != nil {
				return fmt.Errorf("failed to remove destination child: %w", err)
			}
			// Delete destination file metadata
			if err := txn.Delete(keyFile(destHandle)); err != nil {
				return fmt.Errorf("failed to delete destination file: %w", err)
			}
			if err := txn.Delete(keyLinkCount(destHandle)); err != nil && err != badger.ErrKeyNotFound {
				return fmt.Errorf("failed to delete destination link count: %w", err)
			}
			if err := txn.Delete(keyParent(destHandle)); err != nil && err != badger.ErrKeyNotFound {
				return fmt.Errorf("failed to delete destination parent: %w", err)
			}
			// Clean up device numbers and handle mapping if present (ignore not found)
			_ = txn.Delete(keyDeviceNumber(destHandle))
			_ = txn.Delete(keyHandleMapping(destHandle))
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check destination: %w", err)
		}

		// Remove from source directory
		if err := txn.Delete(keyChild(fromDir, fromName)); err != nil {
			return fmt.Errorf("failed to remove from source: %w", err)
		}

		// Add to destination directory
		if err := txn.Set(keyChild(toDir, toName), []byte(sourceHandle)); err != nil {
			return fmt.Errorf("failed to add to destination: %w", err)
		}

		// Update parent if different directory
		if handleToKey(fromDir) != handleToKey(toDir) {
			if err := txn.Set(keyParent(sourceHandle), []byte(toDir)); err != nil {
				return fmt.Errorf("failed to update parent: %w", err)
			}
		}

		// Update timestamps
		now := time.Now()
		sourceData.Attr.Ctime = now

		// Update source file
		sourceBytes, err := encodeFileData(sourceData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(sourceHandle), sourceBytes); err != nil {
			return fmt.Errorf("failed to update source file: %w", err)
		}

		// Update source directory
		fromDirItem, err := txn.Get(keyFile(fromDir))
		if err != nil {
			return fmt.Errorf("failed to get source directory: %w", err)
		}
		var fromDirData *fileData
		err = fromDirItem.Value(func(val []byte) error {
			dd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			fromDirData = dd
			return nil
		})
		if err != nil {
			return err
		}
		fromDirData.Attr.Mtime = now
		fromDirData.Attr.Ctime = now
		fromDirBytes, err := encodeFileData(fromDirData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(fromDir), fromDirBytes); err != nil {
			return fmt.Errorf("failed to update source directory: %w", err)
		}

		// Update destination directory if different
		if handleToKey(fromDir) != handleToKey(toDir) {
			toDirItem, err := txn.Get(keyFile(toDir))
			if err != nil {
				return fmt.Errorf("failed to get destination directory: %w", err)
			}
			var toDirData *fileData
			err = toDirItem.Value(func(val []byte) error {
				dd, err := decodeFileData(val)
				if err != nil {
					return err
				}
				toDirData = dd
				return nil
			})
			if err != nil {
				return err
			}
			toDirData.Attr.Mtime = now
			toDirData.Attr.Ctime = now
			toDirBytes, err := encodeFileData(toDirData)
			if err != nil {
				return err
			}
			if err := txn.Set(keyFile(toDir), toDirBytes); err != nil {
				return fmt.Errorf("failed to update destination directory: %w", err)
			}
		}

		return nil
	})
}
