package metadata

import (
	"context"
	"time"

	"github.com/google/uuid"
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
//   - Protocol handler writes content using PayloadID from intent
//   - CommitWrite updates metadata after successful content write
//   - If content write fails, no rollback needed (metadata unchanged)
type WriteOperation struct {
	// Handle is the file being written to
	Handle FileHandle

	// NewSize is the file size after the write
	NewSize uint64

	// NewMtime is the modification time to set after write
	NewMtime time.Time

	// PayloadID is the identifier for writing to content repository
	PayloadID PayloadID

	// PreWriteAttr contains the file attributes before the write
	// Used for protocol responses (e.g., NFS WCC data)
	PreWriteAttr *FileAttr

	// IsCOW indicates this write triggered copy-on-write for a hard-linked file.
	// When true, PayloadID is a newly generated ID (not the original file's PayloadID).
	IsCOW bool

	// COWSourcePayloadID is the original PayloadID to copy unmodified blocks from.
	// Only set when IsCOW is true.
	COWSourcePayloadID PayloadID

	// OldObjectID is the original ObjectID before COW was triggered.
	// Used for decrementing reference counts.
	// Only set when IsCOW is true.
	OldObjectID ContentHash
}

// ReadMetadata contains metadata returned by PrepareRead.
//
// This provides the protocol handler with the information needed to read
// file content from the content repository.
type ReadMetadata struct {
	// Attr contains the file attributes including the PayloadID
	// The protocol handler uses PayloadID to read from the content repository
	Attr *FileAttr
}

// ============================================================================
// File I/O Operations (MetadataService methods)
// ============================================================================

// PrepareWrite validates a write operation and returns a write intent.
//
// This handles:
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
func (s *MetadataService) PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Fast path: check if we have cached file metadata from a previous write
	// This avoids store.GetFile for sequential writes to the same file
	var file *File
	if cachedFile := s.pendingWrites.GetCachedFile(handle); cachedFile != nil {
		file = cachedFile
	} else {
		// Slow path: fetch from store
		store, err := s.storeForHandle(handle)
		if err != nil {
			return nil, err
		}

		fetchedFile, err := store.GetFile(ctx.Context, handle)
		if err != nil {
			return nil, err
		}
		file = fetchedFile

		// Cache for subsequent writes (only for regular files)
		if file.Type == FileTypeRegular {
			s.pendingWrites.SetCachedFile(handle, file)
		}
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
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return nil, err
		}
	}

	// Make a copy of current attributes for PreWriteAttr
	preWriteAttr := CopyFileAttr(&file.FileAttr)

	// Detect COW trigger: hard-linked file with finalized content
	// When a file has multiple hard links (Nlink > 1) and has finalized content
	// (ObjectID is set), writing to it requires copy-on-write semantics to
	// ensure other hard links continue to see the original content.
	needsCOW := file.Nlink > 1 && !file.ObjectID.IsZero()

	// Create write operation
	writeOp := &WriteOperation{
		Handle:       handle,
		NewSize:      newSize,
		NewMtime:     time.Now(),
		PayloadID:    file.PayloadID,
		PreWriteAttr: preWriteAttr,
	}

	if needsCOW {
		// Generate new PayloadID for this file's private copy
		// Format: {originalPayloadID}-cow-{shortUUID}
		newPayloadID := PayloadID(string(file.PayloadID) + "-cow-" + uuid.New().String()[:8])

		writeOp.PayloadID = newPayloadID
		writeOp.IsCOW = true
		writeOp.COWSourcePayloadID = file.PayloadID
		writeOp.OldObjectID = file.ObjectID
	}

	return writeOp, nil
}

// CommitWrite applies metadata changes after a successful content write.
//
// This handles:
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
// CONCURRENCY: Uses a transaction to ensure atomic read-modify-write.
// This prevents race conditions where concurrent writes would result in
// the smaller size being stored if it commits last.
func (s *MetadataService) CommitWrite(ctx *AuthContext, intent *WriteOperation) (*File, error) {
	// Check if deferred commits are enabled
	s.mu.RLock()
	deferredCommit := s.deferredCommit
	s.mu.RUnlock()

	if deferredCommit {
		return s.deferredCommitWrite(ctx, intent)
	}
	return s.immediateCommitWrite(ctx, intent)
}

// applyCOWState updates file attributes for copy-on-write operations.
// When a hard-linked file is written to, it gets a new PayloadID and tracks
// the original PayloadID as the COW source for lazy copying of unmodified blocks.
func applyCOWState(attr *FileAttr, payloadID PayloadID, cowSource PayloadID) {
	attr.PayloadID = payloadID
	attr.COWSourcePayloadID = cowSource
	attr.ObjectID = ContentHash{} // Clear - file is no longer finalized
}

// deferredCommitWrite records the write in pending state without touching the store.
// The actual commit happens on FlushPendingWrites (called by NFS COMMIT).
func (s *MetadataService) deferredCommitWrite(ctx *AuthContext, intent *WriteOperation) (*File, error) {
	// Determine if we need to clear setuid/setgid
	clearSetuid := ctx.Identity != nil && ctx.Identity.UID != nil && *ctx.Identity.UID != 0

	// Record in pending writes tracker (lock-free for the hot path)
	state := s.pendingWrites.RecordWrite(intent.Handle, intent, clearSetuid)

	// Build a synthetic File response with the pending state
	// This avoids a store read on the hot path
	resultAttr := *intent.PreWriteAttr
	resultAttr.Size = state.MaxSize
	resultAttr.Mtime = state.LastMtime
	resultAttr.Ctime = state.LastMtime
	if state.ClearSetuidSetgid {
		resultAttr.Mode &= ^uint32(0o6000)
	}

	if state.IsCOW {
		applyCOWState(&resultAttr, state.PayloadID, state.COWSourcePayloadID)
	}

	// Extract share name and ID from handle for File struct
	shareName, id, err := DecodeFileHandle(intent.Handle)
	if err != nil {
		// Fallback to empty values - the caller mainly needs the attrs
		shareName = ""
		id = uuid.Nil
	}

	return &File{
		ID:        id,
		ShareName: shareName,
		FileAttr:  resultAttr,
	}, nil
}

// immediateCommitWrite performs the traditional synchronous commit.
func (s *MetadataService) immediateCommitWrite(ctx *AuthContext, intent *WriteOperation) (*File, error) {
	store, err := s.storeForHandle(intent.Handle)
	if err != nil {
		return nil, err
	}

	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Use transaction for atomic read-modify-write
	// This ensures max(size) is calculated and applied atomically
	var resultFile *File
	err = store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Get current file state
		file, err := tx.GetFile(ctx.Context, intent.Handle)
		if err != nil {
			return err
		}

		// Verify it's still a regular file
		if file.Type != FileTypeRegular {
			return &StoreError{
				Code:    ErrIsDirectory,
				Message: "file type changed after prepare",
				Path:    file.Path,
			}
		}

		// Apply metadata changes
		now := time.Now()

		if intent.IsCOW {
			applyCOWState(&file.FileAttr, intent.PayloadID, intent.COWSourcePayloadID)
		}

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
		if err := tx.PutFile(ctx.Context, file); err != nil {
			return err
		}

		resultFile = file
		return nil
	})

	if err != nil {
		return nil, err
	}

	return resultFile, nil
}

// FlushPendingWrites commits all pending metadata changes to the store.
// This should be called on NFS COMMIT or when closing a file.
// Returns the number of files flushed and any error encountered.
func (s *MetadataService) FlushPendingWrites(ctx *AuthContext) (int, error) {
	entries := s.pendingWrites.PopAllPending()
	if len(entries) == 0 {
		return 0, nil
	}

	flushed := 0
	var lastErr error

	for _, entry := range entries {
		if err := s.flushPendingWrite(ctx, entry.Handle, entry.State); err != nil {
			lastErr = err
			// Continue flushing other files
		} else {
			flushed++
		}
	}

	return flushed, lastErr
}

// FlushPendingWriteForFile commits pending metadata for a specific file.
// Returns true if there was pending data to flush.
func (s *MetadataService) FlushPendingWriteForFile(ctx *AuthContext, handle FileHandle) (bool, error) {
	state, exists := s.pendingWrites.PopPending(handle)
	if !exists {
		return false, nil
	}

	return true, s.flushPendingWrite(ctx, handle, state)
}

// flushPendingWrite applies a single pending write to the store.
func (s *MetadataService) flushPendingWrite(ctx *AuthContext, handle FileHandle, state *PendingWriteState) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	return store.WithTransaction(ctx.Context, func(tx Transaction) error {
		file, err := tx.GetFile(ctx.Context, handle)
		if err != nil {
			return err
		}

		if state.IsCOW {
			applyCOWState(&file.FileAttr, state.PayloadID, state.COWSourcePayloadID)
		}

		// Apply pending changes
		if state.MaxSize > file.Size {
			file.Size = state.MaxSize
		}
		file.Mtime = state.LastMtime
		file.Ctime = state.LastMtime
		if state.ClearSetuidSetgid {
			file.Mode &= ^uint32(0o6000)
		}

		return tx.PutFile(ctx.Context, file)
	})
}

// GetPendingSize returns the pending size for a file if there are uncommitted writes.
// Used by GETATTR to return accurate file size before flush.
func (s *MetadataService) GetPendingSize(handle FileHandle) (uint64, bool) {
	return s.pendingWrites.GetPendingSize(handle)
}

// FlushAllPendingWritesForShutdown flushes all pending metadata writes during shutdown.
// Unlike FlushPendingWrites, this method doesn't require an AuthContext and uses a
// background context with a timeout. This should be called during graceful shutdown
// to ensure all metadata changes are persisted before closing stores.
func (s *MetadataService) FlushAllPendingWritesForShutdown(timeout time.Duration) (int, error) {
	entries := s.pendingWrites.PopAllPending()
	if len(entries) == 0 {
		return 0, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Create a minimal auth context for store operations
	authCtx := &AuthContext{Context: ctx}

	flushed := 0
	var lastErr error

	for _, entry := range entries {
		if err := s.flushPendingWrite(authCtx, entry.Handle, entry.State); err != nil {
			lastErr = err
			// Continue flushing other files
		} else {
			flushed++
		}
	}

	return flushed, lastErr
}

// PrewarmWriteCache pre-populates the file metadata cache for a file.
// Call this after CREATE to eliminate cold-start penalty on first WRITE.
// The file parameter should be the newly created file with its attributes.
func (s *MetadataService) PrewarmWriteCache(handle FileHandle, file *File) {
	if file == nil || file.Type != FileTypeRegular {
		return
	}
	s.pendingWrites.SetCachedFile(handle, file)
}

// PrepareRead validates a read operation and returns file metadata.
//
// This handles:
//   - File type validation (must be regular file)
//   - Permission checking (read permission)
//   - Returning metadata including PayloadID for content store
//
// The method does NOT perform actual data reading. The protocol handler
// coordinates between metadata and content stores.
func (s *MetadataService) PrepareRead(ctx *AuthContext, handle FileHandle) (*ReadMetadata, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

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
	if err := s.checkReadPermission(ctx, handle); err != nil {
		return nil, err
	}

	// Return read metadata with a copy of attributes
	attrCopy := file.FileAttr
	return &ReadMetadata{
		Attr: &attrCopy,
	}, nil
}
