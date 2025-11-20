package badger

import (
	"fmt"
	"strconv"
	"time"

	badger "github.com/dgraph-io/badger/v4"
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
		opts.Prefix = keyChildPrefix(dirHandle)

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
			prefix := keyChildPrefix(dirHandle)
			if len(key) <= len(prefix) {
				continue
			}
			childName := string(key[len(prefix):])

			// Get child handle
			// IMPORTANT: Must copy val because it's only valid inside the callback
			var childHandle metadata.FileHandle
			err = item.Value(func(val []byte) error {
				// Make a copy of val since it's only valid during this callback
				handleCopy := make([]byte, len(val))
				copy(handleCopy, val)
				childHandle = metadata.FileHandle(handleCopy)
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

			// Create directory entry
			// DirEntry uses file ID (uint64) - compute hash of handle for ID
			entry := metadata.DirEntry{
				ID:     fileHandleToID(childHandle),
				Name:   childName,
				Handle: childHandle, // Avoid Lookup() in READDIRPLUS
				Attr:   nil,         // TODO: Could populate from cache for further optimization
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
//   - *FileAttr: Attributes of the symlink itself (not the target)
//   - error: ErrNotFound, ErrInvalidArgument, ErrAccessDenied, or context errors
func (s *BadgerMetadataStore) ReadSymlink(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (string, *metadata.FileAttr, error) {
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
	var attr *metadata.FileAttr

	err = s.db.View(func(txn *badger.Txn) error {
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

		// Verify it's a symlink
		if fileData.Attr.Type != metadata.FileTypeSymlink {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "not a symbolic link",
			}
		}

		target = fileData.Attr.LinkTarget
		attr = fileData.Attr

		return nil
	})

	if err != nil {
		return "", nil, err
	}

	// Return copy of attributes
	attrCopy := *attr
	return target, &attrCopy, nil
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
) (metadata.FileHandle, error) {
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

	// Validate target
	if target == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "symlink target cannot be empty",
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

	var newHandle metadata.FileHandle

	err = s.db.Update(func(txn *badger.Txn) error {
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

		// Set defaults
		mode := attr.Mode
		if mode == 0 {
			mode = 0777 // Symlinks typically have 0777
		}

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

		// Create symlink attributes
		newAttr := &metadata.FileAttr{
			Type:       metadata.FileTypeSymlink,
			Mode:       mode & 0o7777,
			UID:        uid,
			GID:        gid,
			Size:       uint64(len(target)),
			Atime:      now,
			Mtime:      now,
			Ctime:      now,
			LinkTarget: target,
			ContentID:  "",
		}

		// Store symlink
		fileData := &fileData{
			Attr:      newAttr,
			ShareName: parentData.ShareName,
		}
		fileBytes, err := encodeFileData(fileData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(handle), fileBytes); err != nil {
			return fmt.Errorf("failed to store symlink: %w", err)
		}

		// Set link count
		if err := txn.Set(keyLinkCount(handle), encodeUint32(1)); err != nil {
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
//   - FileHandle: Handle of the newly created special file
//   - error: Various errors based on validation failures
func (s *BadgerMetadataStore) CreateSpecialFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	fileType metadata.FileType,
	attr *metadata.FileAttr,
	deviceMajor, deviceMinor uint32,
) (metadata.FileHandle, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate file type
	switch fileType {
	case metadata.FileTypeBlockDevice, metadata.FileTypeCharDevice,
		metadata.FileTypeSocket, metadata.FileTypeFIFO:
		// Valid special file types
	default:
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: fmt.Sprintf("invalid special file type: %d", fileType),
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

	// Check if user is root (required for device files)
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		if ctx.Identity == nil || ctx.Identity.UID == nil || *ctx.Identity.UID != 0 {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "only root can create device files",
			}
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

	var newHandle metadata.FileHandle

	err = s.db.Update(func(txn *badger.Txn) error {
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

		// Set defaults
		mode := attr.Mode
		if mode == 0 {
			mode = 0644
		}

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

		// Create special file attributes
		newAttr := &metadata.FileAttr{
			Type:       fileType,
			Mode:       mode & 0o7777,
			UID:        uid,
			GID:        gid,
			Size:       0,
			Atime:      now,
			Mtime:      now,
			Ctime:      now,
			LinkTarget: "",
			ContentID:  "",
		}

		// Store special file
		fileData := &fileData{
			Attr:      newAttr,
			ShareName: parentData.ShareName,
		}
		fileBytes, err := encodeFileData(fileData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(handle), fileBytes); err != nil {
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
			if err := txn.Set(keyDeviceNumber(handle), devBytes); err != nil {
				return fmt.Errorf("failed to store device numbers: %w", err)
			}
		}

		// Set link count
		if err := txn.Set(keyLinkCount(handle), encodeUint32(1)); err != nil {
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
