package metadata

import (
	"time"
)

// WriteOperation represents a validated intent to write to a file.
//
// This is returned by PrepareWrite and contains everything needed to:
//  1. Write content to the content repository
//  2. Commit metadata changes after successful write
//
// The metadata repository does NOT modify any metadata during PrepareWrite.
// This ensures consistency - metadata only changes after content is safely written.
//
// Lifecycle:
//   - PrepareWrite validates and creates intent (no metadata changes)
//   - Protocol handler writes content using ContentID from intent
//   - CommitWrite updates metadata after successful content write
//   - If content write fails, no rollback needed (metadata unchanged)
type WriteOperation struct {
	// Handle is the file being written to
	Handle FileHandle

	// NewSize is the file size after the write
	NewSize uint64

	// NewMtime is the modification time to set after write
	NewMtime time.Time

	// ContentID is the identifier for writing to content repository
	ContentID ContentID

	// PreWriteAttr contains the file attributes before the write
	// Used for protocol responses (e.g., NFS WCC data)
	PreWriteAttr *FileAttr
}

// ReadMetadata contains metadata returned by PrepareRead.
//
// This provides the protocol handler with the information needed to read
// file content from the content repository.
type ReadMetadata struct {
	// Attr contains the file attributes including the ContentID
	// The protocol handler uses ContentID to read from the content repository
	Attr *FileAttr
}

// ============================================================================
// File I/O Operations
// ============================================================================

// PrepareWrite validates a write operation and returns a write intent.
//
// This is the centralized implementation of write preparation that all stores
// should delegate to. It handles:
//   - File type validation (must be regular file)
//   - Permission checking (write permission)
//   - Building WriteOperation with pre-operation attributes
//
// The method does NOT modify any metadata. Metadata changes are applied by
// CommitWrite after the content write succeeds.
//
// Two-Phase Write Pattern:
//  1. PrepareWrite - validates and creates intent
//  2. ContentStore.WriteAt - writes actual content
//  3. CommitWrite - updates metadata (size, mtime, ctime)
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - handle: File handle to write to
//   - newSize: New file size after write (offset + data length)
//
// Returns:
//   - *WriteOperation: Intent containing ContentID and pre-write attributes
//   - error: ErrNotFound, ErrAccessDenied, ErrIsDirectory, etc.
func PrepareWrite(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
	newSize uint64,
) (*WriteOperation, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	// Verify it's a regular file
	if file.Type != FileTypeRegular {
		if file.Type == FileTypeDirectory {
			return nil, &StoreError{
				Code:    ErrIsDirectory,
				Message: "cannot write to directory",
				Path:    file.Path,
			}
		}
		return nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "cannot write to non-regular file",
			Path:    file.Path,
		}
	}

	// Check write permission
	// Owner can always write to their own files (even if mode is 0444)
	// This matches POSIX semantics where permissions are checked at open() time.
	isOwner := ctx.Identity != nil && ctx.Identity.UID != nil && *ctx.Identity.UID == file.UID

	if !isOwner {
		// Non-owner: check permissions using normal Unix permission bits
		if err := CheckWritePermission(store, ctx, handle); err != nil {
			return nil, err
		}
	}

	// Make a copy of current attributes for PreWriteAttr
	preWriteAttr := CopyFileAttr(&file.FileAttr)

	// Create write operation
	writeOp := &WriteOperation{
		Handle:       handle,
		NewSize:      newSize,
		NewMtime:     time.Now(),
		ContentID:    file.ContentID,
		PreWriteAttr: preWriteAttr,
	}

	return writeOp, nil
}

// CommitWrite applies metadata changes after a successful content write.
//
// This is the centralized implementation of write commit that all stores
// should delegate to. It handles:
//   - File size update (max of current and new size)
//   - Timestamp updates (mtime, ctime)
//   - POSIX: clearing setuid/setgid bits for non-root users
//
// Should be called after ContentStore.WriteAt succeeds.
//
// If this fails after content was written, the file is in an inconsistent
// state (content newer than metadata). This can be detected by consistency
// checkers.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - intent: The write intent from PrepareWrite
//
// Returns:
//   - *File: Updated file with new attributes
//   - error: ErrNotFound if file was deleted, etc.
func CommitWrite(
	store MetadataStore,
	ctx *AuthContext,
	intent *WriteOperation,
) (*File, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get current file state
	file, err := store.GetFile(ctx.Context, intent.Handle)
	if err != nil {
		return nil, err
	}

	// Verify it's still a regular file
	if file.Type != FileTypeRegular {
		return nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "file type changed after prepare",
			Path:    file.Path,
		}
	}

	// Apply metadata changes
	now := time.Now()

	// Use max(current_size, new_size) to handle concurrent writes completing out of order
	// This prevents a write at an earlier offset from shrinking the file
	if intent.NewSize > file.Size {
		file.Size = intent.NewSize
	}
	file.Mtime = now
	file.Ctime = now

	// POSIX: Clear setuid/setgid bits when a non-root user writes to a file
	// This is a security measure to prevent privilege escalation.
	identity := ctx.Identity
	if identity != nil && identity.UID != nil && *identity.UID != 0 {
		file.Mode &= ^uint32(0o6000) // Clear both setuid (04000) and setgid (02000)
	}

	// Store updated file
	if err := store.PutFile(ctx.Context, file); err != nil {
		return nil, err
	}

	return file, nil
}

// PrepareRead validates a read operation and returns file metadata.
//
// This is the centralized implementation of read preparation that all stores
// should delegate to. It handles:
//   - File type validation (must be regular file)
//   - Permission checking (read permission)
//   - Returning metadata including ContentID for content store
//
// The method does NOT perform actual data reading. The protocol handler
// coordinates between metadata and content stores.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - handle: File handle to read from
//
// Returns:
//   - *ReadMetadata: Contains file attributes including ContentID
//   - error: ErrNotFound, ErrAccessDenied, ErrIsDirectory, etc.
func PrepareRead(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
) (*ReadMetadata, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	// Verify it's a regular file
	if file.Type != FileTypeRegular {
		if file.Type == FileTypeDirectory {
			return nil, &StoreError{
				Code:    ErrIsDirectory,
				Message: "cannot read directory",
				Path:    file.Path,
			}
		}
		return nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "cannot read non-regular file",
			Path:    file.Path,
		}
	}

	// Check read permission
	if err := CheckReadPermission(store, ctx, handle); err != nil {
		return nil, err
	}

	// Return read metadata with a copy of attributes
	attrCopy := file.FileAttr
	return &ReadMetadata{
		Attr: &attrCopy,
	}, nil
}
