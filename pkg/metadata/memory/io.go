package memory

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// File Content Coordination
// ============================================================================

// PrepareWrite validates a write operation and returns a write intent.
//
// This method validates permissions and file type but does NOT modify
// any metadata. Metadata changes are applied by CommitWrite after the
// content write succeeds.
//
// Two-Phase Write Pattern:
//
//	// Phase 1: Validate and prepare
//	intent, err := repo.PrepareWrite(authCtx, handle, newSize)
//	if err != nil {
//	    return err  // Validation failed, no changes made
//	}
//
//	// Phase 2: Write content
//	err = contentRepo.WriteAt(intent.ContentID, data, offset)
//	if err != nil {
//	    return err  // Content write failed, no metadata changes
//	}
//
//	// Phase 3: Commit metadata changes
//	attr, err = repo.CommitWrite(authCtx, intent)
//	if err != nil {
//	    // Content written but metadata not updated
//	    // This is detectable and can be repaired
//	    return err
//	}
func (s *MemoryMetadataStore) PrepareWrite(
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

	// Get file attributes
	key := handleToKey(handle)
	attr, exists := s.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Verify it's a regular file
	if attr.Type != metadata.FileTypeRegular {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot write to non-regular file",
		}
	}

	// Check write permission
	granted, err := s.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionWrite == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
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
	writeOp := &metadata.WriteOperation{
		Handle:       handle,
		NewSize:      newSize,
		NewMtime:     time.Now(),
		ContentID:    attr.ContentID,
		PreWriteAttr: preWriteAttr,
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
func (s *MemoryMetadataStore) CommitWrite(
	ctx *metadata.AuthContext,
	intent *metadata.WriteOperation,
) (*metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Get file attributes
	key := handleToKey(intent.Handle)
	attr, exists := s.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found (deleted after prepare)",
		}
	}

	// Verify it's still a regular file
	if attr.Type != metadata.FileTypeRegular {
		return nil, &metadata.StoreError{
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
	attr.Size = intent.NewSize
	attr.Mtime = intent.NewMtime
	attr.Ctime = now // Ctime always uses current time (metadata change time)

	// Return updated attributes
	return attr, nil
}

// PrepareRead validates a read operation and returns file metadata.
//
// This method handles the metadata aspects of file reads:
//   - Permission validation (read permission on file)
//   - Attribute retrieval (including ContentID for content repository)
//
// The method does NOT perform actual data reading. The protocol handler
// coordinates between metadata and content repositories:
//
//  1. Call PrepareRead to validate and get metadata
//  2. Read data from content repository using ContentID
//  3. Use returned ReadMetadata for protocol response
//
// Example usage:
//
//	readMeta, err := repo.PrepareRead(authCtx, handle)
//	if err != nil {
//	    return err
//	}
//
//	// Read actual data from content repository
//	data, err := contentRepo.ReadAt(readMeta.Attr.ContentID, offset, count)
//	if err != nil {
//	    return err
//	}
//
//	// Build protocol response with data and attributes
//	return buildReadResponse(data, readMeta.Attr)
func (s *MemoryMetadataStore) PrepareRead(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (*metadata.ReadMetadata, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get file attributes
	key := handleToKey(handle)
	attr, exists := s.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Verify it's a regular file
	if attr.Type != metadata.FileTypeRegular {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot read non-regular file",
		}
	}

	// Check read permission
	granted, err := s.checkPermissionsLocked(ctx, handle, metadata.PermissionRead)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionRead == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
			Message: "no read permission",
		}
	}

	// Return read metadata with pointer to attributes
	return &metadata.ReadMetadata{
		Attr: attr,
	}, nil
}
