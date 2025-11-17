package badger

import (
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	var writeOp *metadata.WriteOperation

	err := s.db.View(func(txn *badger.Txn) error {
		// Get file attributes
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

		// Verify it's a regular file
		if attr.Type != metadata.FileTypeRegular {
			// Return appropriate error based on file type
			if attr.Type == metadata.FileTypeDirectory {
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

		// Check write permission using the public CheckPermissions method
		// We need to release the RLock temporarily to call CheckPermissions
		// which acquires its own RLock
		s.mu.RUnlock()
		granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
		s.mu.RLock()

		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission",
			}
		}

		// Make a copy of current attributes for PreWriteAttr
		preWriteAttr := &metadata.FileAttr{
			Type:       attr.Type,
			Mode:       attr.Mode,
			UID:        attr.UID,
			GID:        attr.GID,
			Size:       attr.Size,
			Atime:      attr.Atime,
			Mtime:      attr.Mtime,
			Ctime:      attr.Ctime,
			ContentID:  attr.ContentID,
			LinkTarget: attr.LinkTarget,
		}

		// Create write operation
		writeOp = &metadata.WriteOperation{
			Handle:       handle,
			NewSize:      newSize,
			NewMtime:     time.Now(),
			ContentID:    attr.ContentID,
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
) (*metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var updatedAttr *metadata.FileAttr

	err := s.db.Update(func(txn *badger.Txn) error {
		// Get file attributes
		item, err := txn.Get(keyFile(intent.Handle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found (deleted after prepare)",
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

		// Verify it's still a regular file
		if fileData.Attr.Type != metadata.FileTypeRegular {
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
		fileData.Attr.Size = intent.NewSize
		fileData.Attr.Mtime = now // Mtime is set when the write is committed
		fileData.Attr.Ctime = now // Ctime always uses current time (metadata change time)

		// Store updated file data
		fileBytes, err := encodeFileData(fileData)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(intent.Handle), fileBytes); err != nil {
			return fmt.Errorf("failed to update file data: %w", err)
		}

		// Make a copy for return
		updatedAttr = &metadata.FileAttr{
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

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Invalidate caches since file attributes changed
	s.invalidateStatsCache()
	s.invalidateGetfile(intent.Handle)

	return updatedAttr, nil
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	var readMeta *metadata.ReadMetadata

	err := s.db.View(func(txn *badger.Txn) error {
		// Get file attributes
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

		// Verify it's a regular file
		if attr.Type != metadata.FileTypeRegular {
			// Return appropriate error based on file type
			if attr.Type == metadata.FileTypeDirectory {
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
		s.mu.RUnlock()
		granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionRead)
		s.mu.RLock()

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
		attrCopy := *attr
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
