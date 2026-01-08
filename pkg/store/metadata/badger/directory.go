package badger

import (
	"fmt"
	"strconv"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ReadDirectory reads one page of directory entries with pagination support.
//
// Pagination uses opaque tokens (offset-based in this implementation).
// This uses a BadgerDB read transaction with an iterator to scan children.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - dirHandle: Directory to read
//   - token: Pagination token (empty string = start, or offset from previous page)
//   - maxBytes: Maximum response size hint in bytes (0 = use default of 8192)
//
// Returns:
//   - *ReadDirPage: Page of entries with pagination info
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) ReadDirectory(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	token string,
	maxBytes uint32,
) (*metadata.ReadDirPage, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Check read and execute permissions BEFORE acquiring lock to avoid unlock/relock race
	var granted metadata.Permission
	var err error
	granted, err = s.CheckPermissions(ctx, dirHandle,
		metadata.PermissionRead|metadata.PermissionTraverse)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionRead == 0 || granted&metadata.PermissionTraverse == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no read or execute permission on directory",
		}
	}

	// Acquire read lock to ensure consistency during read

	var page *metadata.ReadDirPage

	err = s.db.View(func(txn *badger.Txn) error {
		// Get directory data
		_, dirID, err := metadata.DecodeFileHandle(dirHandle)
		if err != nil {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidHandle,
				Message: "invalid directory handle",
			}
		}
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

		var dir *metadata.File
		err = item.Value(func(val []byte) error {
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

		// Verify it's a directory
		if dir.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
			}
		}

		// Parse token
		offset := 0
		if token != "" {
			parsedOffset, err := strconv.Atoi(token)
			if err != nil {
				return &metadata.StoreError{
					Code:    metadata.ErrInvalidArgument,
					Message: "invalid pagination token",
				}
			}
			offset = parsedOffset
		}

		// Default maxBytes
		if maxBytes == 0 {
			maxBytes = 8192
		}

		// Scan children using range iterator
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = true
		opts.Prefix = keyChildPrefix(dir.ID)

		it := txn.NewIterator(opts)
		defer it.Close()

		var entries []metadata.DirEntry
		var estimatedSize uint32
		currentOffset := 0

		for it.Rewind(); it.Valid(); it.Next() {
			// Check context periodically
			if currentOffset%100 == 0 {
				if err := ctx.Context.Err(); err != nil {
					return err
				}
			}

			item := it.Item()
			key := item.Key()

			// Extract child name from key: "c:<parent>:<name>"
			prefix := keyChildPrefix(dir.ID)
			if len(key) <= len(prefix) {
				continue
			}
			childName := string(key[len(prefix):])

			// Get child UUID from value
			var childID uuid.UUID
			err = item.Value(func(val []byte) error {
				if len(val) != 16 {
					return fmt.Errorf("invalid UUID length: %d", len(val))
				}
				copy(childID[:], val)
				return nil
			})
			if err != nil {
				return err
			}

			// Skip entries before offset
			if currentOffset < offset {
				currentOffset++
				continue
			}

			// Get child file to generate handle
			childItem, err := txn.Get(keyFile(childID))
			if err != nil {
				// Child file not found - skip this entry
				currentOffset++
				continue
			}

			var childFile *metadata.File
			err = childItem.Value(func(val []byte) error {
				cf, err := decodeFile(val)
				if err != nil {
					return err
				}
				childFile = cf
				return nil
			})
			if err != nil {
				return err
			}

			// Generate file handle for child
			childHandle, err := metadata.EncodeFileHandle(childFile)
			if err != nil {
				// Skip entries we can't encode
				currentOffset++
				continue
			}

			// Create directory entry with attributes for SMB directory listing
			// FileAttr is a value-only struct (no pointer fields), so this shallow copy is safe
			attrCopy := childFile.FileAttr
			entry := metadata.DirEntry{
				ID:     fileHandleToID(childHandle),
				Name:   childName,
				Handle: childHandle,
				Attr:   &attrCopy,
			}

			// Estimate size (rough estimate: name + some overhead)
			entrySize := uint32(len(childName) + 200) // Rough estimate
			if estimatedSize+entrySize > maxBytes && len(entries) > 0 {
				// Reached size limit, stop here
				page = &metadata.ReadDirPage{
					Entries:   entries,
					NextToken: strconv.Itoa(currentOffset),
					HasMore:   true,
				}
				return nil
			}

			entries = append(entries, entry)
			estimatedSize += entrySize
			currentOffset++
		}

		// No more entries
		page = &metadata.ReadDirPage{
			Entries:   entries,
			NextToken: "",
			HasMore:   false,
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return page, nil
}

// ReadSymlink reads the target path of a symbolic link.
//
// This returns the path stored in the symlink without following it or validating
// that the target exists. Also returns the symlink's attributes for cache consistency.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - handle: File handle of the symbolic link
//
// Returns:
//   - string: The target path stored in the symlink
//   - *File: Complete file information for the symlink itself (not the target)
//   - error: ErrNotFound, ErrInvalidArgument, ErrAccessDenied, or context errors
func (s *BadgerMetadataStore) ReadSymlink(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (string, *metadata.File, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return "", nil, err
	}

	// Check read permission BEFORE acquiring lock to avoid unlock/relock race
	granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionRead)
	if err != nil {
		return "", nil, err
	}
	if granted&metadata.PermissionRead == 0 {
		return "", nil, &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no read permission on symlink",
		}
	}

	var target string
	var file *metadata.File

	err = s.db.View(func(txn *badger.Txn) error {
		// Get file data
		_, id, err := metadata.DecodeFileHandle(handle)
		if err != nil {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidHandle,
				Message: "invalid file handle",
			}
		}
		item, err := txn.Get(keyFile(id))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		err = item.Value(func(val []byte) error {
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

		// Verify it's a symlink
		if file.Type != metadata.FileTypeSymlink {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "not a symbolic link",
			}
		}

		target = file.LinkTarget

		return nil
	})

	if err != nil {
		return "", nil, err
	}

	return target, file, nil
}

// CreateSymlink creates a symbolic link pointing to a target path.
//
// This uses a BadgerDB write transaction to ensure atomicity.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new symlink
//   - target: Path the symlink will point to (can be absolute or relative)
//   - attr: Partial attributes (mode, uid, gid may be set)
//
// Returns:
//   - FileHandle: Handle of the newly created symlink
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) CreateSymlink(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	target string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Validate target
	if err := metadata.ValidateSymlinkTarget(target); err != nil {
		return nil, err
	}

	// Decode parent handle before acquiring lock
	_, parentID, err := metadata.DecodeFileHandle(parentHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
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

	// Lock parent directory to serialize concurrent operations
	mu := s.lockDir(parentID.String())
	defer s.unlockDir(parentID.String(), mu)

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

		// Check if name already exists
		_, err = txn.Get(keyChild(parentFile.ID, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Build full path and generate new UUID
		fullPath := buildFullPath(parentFile.Path, name)

		// Validate path length (POSIX PATH_MAX = 4096)
		if err := metadata.ValidatePath(fullPath); err != nil {
			return err
		}

		newID := uuid.New()

		// Set symlink type and apply defaults
		attr.Type = metadata.FileTypeSymlink
		metadata.ApplyCreateDefaults(attr, ctx, target)

		// Create complete File struct for symlink (with Nlink = 1)
		newFile = &metadata.File{
			ID:        newID,
			ShareName: parentFile.ShareName,
			Path:      fullPath,
			FileAttr: metadata.FileAttr{
				Type:         metadata.FileTypeSymlink,
				Mode:         attr.Mode,
				UID:          attr.UID,
				GID:          attr.GID,
				Nlink:        1,
				Size:         attr.Size,
				Atime:        attr.Atime,
				Mtime:        attr.Mtime,
				Ctime:        attr.Ctime,
				CreationTime: attr.CreationTime,
				LinkTarget:   target,
				ContentID:    "",
			},
		}

		// Store symlink
		fileBytes, err := encodeFile(newFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(newID), fileBytes); err != nil {
			return fmt.Errorf("failed to store symlink: %w", err)
		}

		// Also store link count separately for efficient updates
		if err := txn.Set(keyLinkCount(newID), encodeUint32(1)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		// Add to parent's children (store UUID bytes)
		if err := txn.Set(keyChild(parentID, name), newID[:]); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Set parent relationship (store parent UUID bytes)
		if err := txn.Set(keyParent(newID), parentID[:]); err != nil {
			return fmt.Errorf("failed to set parent: %w", err)
		}

		// Update parent timestamps
		parentFile.Mtime = attr.Mtime
		parentFile.Ctime = attr.Ctime
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

// CreateSpecialFile creates a special file (device, socket, or FIFO).
//
// This uses a BadgerDB write transaction to ensure atomicity.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new special file
//   - fileType: Type of special file to create
//   - attr: Partial attributes (mode, uid, gid may be set)
//   - deviceMajor: Major device number (for block/char devices, 0 otherwise)
//   - deviceMinor: Minor device number (for block/char devices, 0 otherwise)
//
// Returns:
//   - *File: Complete file information for the newly created special file
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) CreateSpecialFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	fileType metadata.FileType,
	attr *metadata.FileAttr,
	deviceMajor, deviceMinor uint32,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate file type
	if err := metadata.ValidateSpecialFileType(fileType); err != nil {
		return nil, err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Check if user is root (required for device files)
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		if err := metadata.RequiresRoot(ctx); err != nil {
			return nil, err
		}
	}

	// Decode parent handle before acquiring lock
	_, parentID, err := metadata.DecodeFileHandle(parentHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
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

	// Lock parent directory to serialize concurrent operations
	mu := s.lockDir(parentID.String())
	defer s.unlockDir(parentID.String(), mu)

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

		// Check if name already exists
		_, err = txn.Get(keyChild(parentFile.ID, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Build full path and generate new UUID
		fullPath := buildFullPath(parentFile.Path, name)

		// Validate path length (POSIX PATH_MAX = 4096)
		if err := metadata.ValidatePath(fullPath); err != nil {
			return err
		}

		newID := uuid.New()

		// Set special file type and apply defaults
		attr.Type = fileType
		metadata.ApplyCreateDefaults(attr, ctx, "")

		// Compute Rdev for device files
		var rdev uint64
		if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
			rdev = metadata.MakeRdev(deviceMajor, deviceMinor)
		}

		// Create complete File struct for special file (with Nlink = 1)
		newFile = &metadata.File{
			ID:        newID,
			ShareName: parentFile.ShareName,
			Path:      fullPath,
			FileAttr: metadata.FileAttr{
				Type:         fileType,
				Mode:         attr.Mode,
				UID:          attr.UID,
				GID:          attr.GID,
				Nlink:        1,
				Size:         attr.Size,
				Atime:        attr.Atime,
				Mtime:        attr.Mtime,
				Ctime:        attr.Ctime,
				CreationTime: attr.CreationTime,
				LinkTarget:   "",
				ContentID:    "",
				Rdev:         rdev,
			},
		}

		// Store special file
		fileBytes, err := encodeFile(newFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(newID), fileBytes); err != nil {
			return fmt.Errorf("failed to store special file: %w", err)
		}

		// Store device numbers if applicable
		if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
			devNum := &deviceNumber{
				Major: deviceMajor,
				Minor: deviceMinor,
			}
			devBytes, err := encodeDeviceNumber(devNum)
			if err != nil {
				return err
			}
			if err := txn.Set(keyDeviceNumber(newID), devBytes); err != nil {
				return fmt.Errorf("failed to store device numbers: %w", err)
			}
		}

		// Also store link count separately for efficient updates
		if err := txn.Set(keyLinkCount(newID), encodeUint32(1)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		// Add to parent's children (store UUID bytes)
		if err := txn.Set(keyChild(parentID, name), newID[:]); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Set parent relationship (store parent UUID bytes)
		if err := txn.Set(keyParent(newID), parentID[:]); err != nil {
			return fmt.Errorf("failed to set parent: %w", err)
		}

		// Update parent timestamps
		parentFile.Mtime = attr.Mtime
		parentFile.Ctime = attr.Ctime
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
