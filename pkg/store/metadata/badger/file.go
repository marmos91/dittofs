package badger

import (
	"context"
	"fmt"
	"slices"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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
//   - *File: Complete file information (ID, ShareName, Path, and all attributes)
//   - error: ErrNotFound, ErrNotDirectory, ErrAccessDenied, or context errors
func (s *BadgerMetadataStore) Lookup(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Decode directory handle to get UUID
	_, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	var targetFile *metadata.File

	err = s.db.View(func(txn *badger.Txn) error {
		// Get directory data
		item, err := txn.Get(keyFile(dirID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get directory: %w", err)
		}

		var dirFile *metadata.File
		err = item.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			dirFile = f
			return nil
		})
		if err != nil {
			return err
		}

		// Verify it's a directory
		if dirFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
			}
		}

		// Check execute/traverse permission on directory (search permission)
		granted, err := s.CheckPermissions(ctx, dirHandle, metadata.PermissionTraverse)
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
			targetFile = dirFile

		case "..":
			// Parent directory
			parentItem, err := txn.Get(keyParent(dirID))
			if err == badger.ErrKeyNotFound {
				// This is a root directory, ".." refers to itself
				targetFile = dirFile
			} else if err != nil {
				return fmt.Errorf("failed to get parent: %w", err)
			} else {
				var parentID uuid.UUID
				err = parentItem.Value(func(val []byte) error {
					parentID, err = uuid.FromBytes(val)
					return err
				})
				if err != nil {
					return err
				}

				// Get parent file
				parentFileItem, err := txn.Get(keyFile(parentID))
				if err != nil {
					return fmt.Errorf("failed to get parent file: %w", err)
				}
				err = parentFileItem.Value(func(val []byte) error {
					parentFile, err := decodeFile(val)
					if err != nil {
						return err
					}
					targetFile = parentFile
					return nil
				})
				if err != nil {
					return err
				}
			}

		default:
			// Regular name lookup
			childItem, err := txn.Get(keyChild(dirID, name))
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

			var childID uuid.UUID
			err = childItem.Value(func(val []byte) error {
				childID, err = uuid.FromBytes(val)
				return err
			})
			if err != nil {
				return err
			}

			// Get child file
			childFileItem, err := txn.Get(keyFile(childID))
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
				childFile, err := decodeFile(val)
				if err != nil {
					return err
				}
				targetFile = childFile
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return targetFile, nil
}

// GetFile retrieves complete file information by handle.
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
//   - *File: Complete file information (ID, ShareName, Path, and all attributes)
//   - error: ErrNotFound, ErrInvalidHandle, or context errors
func (s *BadgerMetadataStore) GetFile(
	ctx context.Context,
	handle metadata.FileHandle,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	var file *metadata.File

	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyFile(fileID))
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
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			file = f
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return file, nil
}

// GetShareNameForHandle returns the share name for a given file handle.
//
// This works with UUID-based handles by decoding the handle format.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle (UUID-based)
//
// Returns:
//   - string: Share name the file belongs to
//   - error: ErrNotFound if handle is invalid, context errors
func (s *BadgerMetadataStore) GetShareNameForHandle(
	ctx context.Context,
	handle metadata.FileHandle,
) (string, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Decode handle to extract share name
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	return shareName, nil
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

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	return s.db.Update(func(txn *badger.Txn) error {
		// Get file data
		item, err := txn.Get(keyFile(fileID))
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
		err = item.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			file = f
			return nil
		})
		if err != nil {
			return err
		}

		identity := ctx.Identity
		if identity == nil || identity.UID == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "authentication required to modify attributes",
			}
		}

		uid := *identity.UID
		isOwner := uid == file.UID
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
			file.Mode = newMode
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

			file.UID = *attrs.UID
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

			file.GID = *attrs.GID
			changed = true
		}

		// Validate and apply size changes
		if attrs.Size != nil {
			if file.Type != metadata.FileTypeRegular {
				return &metadata.StoreError{
					Code:    metadata.ErrInvalidArgument,
					Message: "cannot change size of non-regular file",
				}
			}

			// Check write permission
			granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrAccessDenied,
					Message: "no write permission",
				}
			}

			file.Size = *attrs.Size
			file.Mtime = time.Now()
			changed = true
		}

		// Apply atime changes
		if attrs.Atime != nil {
			if !isOwner && !isRoot {
				granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
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

			file.Atime = *attrs.Atime
			changed = true
		}

		// Apply mtime changes
		if attrs.Mtime != nil {
			if !isOwner && !isRoot {
				granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
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

			file.Mtime = *attrs.Mtime
			changed = true
		}

		// Update ctime if any changes were made
		if changed {
			file.Ctime = time.Now()

			// Store updated file data
			fileBytes, err := encodeFile(file)
			if err != nil {
				return err
			}
			if err := txn.Set(keyFile(fileID), fileBytes); err != nil {
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
//   - *File: Complete file information for the newly created file/directory
//   - error: ErrInvalidArgument, ErrAccessDenied, ErrAlreadyExists, or other errors
func (s *BadgerMetadataStore) Create(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
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

	// Decode parent handle
	shareName, parentID, err := metadata.DecodeFileHandle(parentHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	var newFile *metadata.File

	err = s.db.Update(func(txn *badger.Txn) error {
		// Verify parent exists and is a directory
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
			pf, err := decodeFile(val)
			if err != nil {
				return err
			}
			parentFile = pf
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

		// Check write permission on parent
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

		// Check if name already exists
		_, err = txn.Get(keyChild(parentID, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Generate UUID for new file
		newID := uuid.New()

		// Build full path
		fullPath := buildFullPath(parentFile.Path, name)

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

		// Create file
		newFile = &metadata.File{
			ID:        newID,
			ShareName: shareName,
			Path:      fullPath,
			FileAttr: metadata.FileAttr{
				Type:       attr.Type,
				Mode:       mode & 0o7777,
				UID:        uid,
				GID:        gid,
				Size:       0,
				Atime:      now,
				Mtime:      now,
				Ctime:      now,
				LinkTarget: "",
			},
		}

		// Type-specific initialization
		if attr.Type == metadata.FileTypeRegular {
			// Generate ContentID for regular files
			contentID := buildContentID(shareName, fullPath)
			newFile.ContentID = metadata.ContentID(contentID)

			// Log ContentID generation for debugging
			logger.Debug("Generated ContentID: file='%s' fullPath='%s' contentID='%s'",
				name, fullPath, contentID)
		} else {
			newFile.ContentID = ""
		}

		// Store file
		fileBytes, err := encodeFile(newFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(newID), fileBytes); err != nil {
			return fmt.Errorf("failed to store file: %w", err)
		}

		// Set link count
		var linkCount uint32
		if attr.Type == metadata.FileTypeDirectory {
			linkCount = 2 // "." and parent entry
		} else {
			linkCount = 1
		}
		if err := txn.Set(keyLinkCount(newID), encodeUint32(linkCount)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		// Add to parent's children (store UUID bytes)
		if err := txn.Set(keyChild(parentID, name), newID[:]); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Set parent relationship (store UUID bytes)
		if err := txn.Set(keyParent(newID), parentID[:]); err != nil {
			return fmt.Errorf("failed to set parent: %w", err)
		}

		// Update parent timestamps
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

	return newFile, nil
}

// CreateHardLink creates a hard link to an existing file.
//
// TODO: This is a stub implementation. Hard links need proper implementation
// in BadgerDB with link count tracking and proper handle management.
//
// For now, this returns ErrNotSupported to indicate the feature is not yet implemented.
func (s *BadgerMetadataStore) CreateHardLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	targetHandle metadata.FileHandle,
) error {
	return &metadata.StoreError{
		Code:    metadata.ErrNotSupported,
		Message: "hard links not yet implemented in BadgerDB",
	}
}

// Move moves or renames a file or directory atomically.
//
// TODO: This is a stub implementation. Move/rename operations need proper implementation
// in BadgerDB with:
// - Atomic update of parent-child relationships
// - Proper handling of cross-directory moves
// - Timestamp updates for source and destination directories
// - Validation of replacement semantics (file over file, dir over empty dir, etc.)
//
// For now, this returns ErrNotSupported to indicate the feature is not yet implemented.
func (s *BadgerMetadataStore) Move(
	ctx *metadata.AuthContext,
	fromDir metadata.FileHandle,
	fromName string,
	toDir metadata.FileHandle,
	toName string,
) error {
	return &metadata.StoreError{
		Code:    metadata.ErrNotSupported,
		Message: "move/rename not yet implemented in BadgerDB",
	}
}
