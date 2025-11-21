package badger

import (
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// PrepareWrite validates a write operation and returns a write intent.
//
// This method validates permissions and file type but does NOT modify
// any metadata. Metadata changes are applied by CommitWrite after the
// content write succeeds.
//
// Two-Phase Write Pattern:
//  1. PrepareWrite - validates and creates intent
//  2. ContentRepository.WriteAt - writes actual content
//  3. CommitWrite - updates metadata (size, mtime, ctime)
//
// This pattern ensures that content writes can fail without leaving
// inconsistent metadata.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - handle: File handle to write to
//   - newSize: New file size after write (offset + data length)
//
// Returns:
//   - *WriteOperation: Intent containing ContentID and new attributes
//   - error: ErrNotFound, ErrPermissionDenied, ErrIsDirectory, or context errors
func (s *BadgerMetadataStore) PrepareWrite(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	newSize uint64,
) (*metadata.WriteOperation, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	var writeOp *metadata.WriteOperation

	err := s.db.View(func(txn *badger.Txn) error {
		// Get file attributes
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

		var file *metadata.File
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

		// Verify it's a regular file
		if file.Type != metadata.FileTypeRegular {
			// Return appropriate error based on file type
			if file.Type == metadata.FileTypeDirectory {
				return &metadata.StoreError{
					Code:    metadata.ErrIsDirectory,
					Message: "cannot write to directory",
				}
			}
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "cannot write to non-regular file",
			}
		}

		// Check write permission
		// Owner can always write to their own files (even if mode is 0444)
		// This matches POSIX semantics where permissions are checked at open() time,
		// not on every write() operation.
		//
		// Without this exception, operations like "cp -p" that preserve mode 0444
		// files would fail because SETATTR changes mode to 0444 before WRITE operations.
		isOwner := ctx.Identity.UID != nil && *ctx.Identity.UID == file.UID

		if !isOwner {
			// Non-owner: check permissions using normal Unix permission bits
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
		}

		// Make a copy of current attributes for PreWriteAttr
		preWriteAttr := &metadata.FileAttr{
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
		}

		// Create write operation
		writeOp = &metadata.WriteOperation{
			Handle:       handle,
			NewSize:      newSize,
			NewMtime:     time.Now(),
			ContentID:    file.ContentID,
			PreWriteAttr: preWriteAttr,
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return writeOp, nil
}

// CommitWrite applies metadata changes after a successful content write.
//
// This should be called after ContentRepository.WriteAt succeeds to update
// the file's size and modification time.
//
// If this fails after content was written, the file is in an inconsistent
// state (content newer than metadata). This can be detected by consistency
// checkers comparing ContentID timestamps with file mtime.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context (must be same user as PrepareWrite)
//   - intent: The write intent from PrepareWrite
//
// Returns:
//   - *FileAttr: Updated file attributes after commit
//   - error: ErrNotFound if file was deleted, ErrStaleHandle if file changed,
//     or context errors
func (s *BadgerMetadataStore) CommitWrite(
	ctx *metadata.AuthContext,
	intent *metadata.WriteOperation,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	var updatedFile *metadata.File

	err := s.db.Update(func(txn *badger.Txn) error {
		// Get file attributes
		_, id, err := metadata.DecodeFileHandle(intent.Handle)
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
				Message: "file not found (deleted after prepare)",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		var file *metadata.File
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

		// Verify it's still a regular file
		if file.Type != metadata.FileTypeRegular {
			return &metadata.StoreError{
				Code:    metadata.ErrIsDirectory,
				Message: "file type changed after prepare",
			}
		}

		// Optional: Check for staleness
		// If the file has been modified by another writer between PrepareWrite and CommitWrite,
		// we could detect it here by comparing current state with PreWriteAttr
		// For now, we allow it (last writer wins)

		// Apply metadata changes
		now := time.Now()
		file.Size = intent.NewSize
		file.Mtime = now // Mtime is set when the write is committed
		file.Ctime = now // Ctime always uses current time (metadata change time)

		// Store updated file data
		fileBytes, err := encodeFile(file)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(id), fileBytes); err != nil {
			return fmt.Errorf("failed to update file data: %w", err)
		}

		// Store updated file for return
		updatedFile = file

		return nil
	})

	if err != nil {
		return nil, err
	}

	return updatedFile, nil
}

// PrepareRead validates a read operation and returns file metadata.
//
// This method handles the metadata aspects of file reads:
//   - Permission validation (read permission on file)
//   - Attribute retrieval (including ContentID for content repository)
//
// The method does NOT perform actual data reading. The protocol handler
// coordinates between metadata and content repositories.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - handle: File handle to read from
//
// Returns:
//   - *ReadMetadata: Contains file attributes including ContentID
//   - error: ErrNotFound if file doesn't exist, ErrAccessDenied if no read
//     permission, ErrIsDirectory if trying to read a directory, or context errors
func (s *BadgerMetadataStore) PrepareRead(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (*metadata.ReadMetadata, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	var readMeta *metadata.ReadMetadata

	err := s.db.View(func(txn *badger.Txn) error {
		// Get file attributes
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

		var file *metadata.File
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

		// Verify it's a regular file
		if file.Type != metadata.FileTypeRegular {
			// Return appropriate error based on file type
			if file.Type == metadata.FileTypeDirectory {
				return &metadata.StoreError{
					Code:    metadata.ErrIsDirectory,
					Message: "cannot read directory",
				}
			}
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "cannot read non-regular file",
			}
		}

		// Check read permission using the public CheckPermissions method
		// We need to release the RLock temporarily to call CheckPermissions
		granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionRead)

		if err != nil {
			return err
		}
		if granted&metadata.PermissionRead == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no read permission",
			}
		}

		// Return read metadata with a copy of attributes to prevent external modification
		attrCopy := file.FileAttr
		readMeta = &metadata.ReadMetadata{
			Attr: &attrCopy,
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return readMeta, nil
}
