package metadata

import (
	"time"
)

// ReadDirPage represents one page of directory entries returned by ReadDirectory.
//
// This structure supports paginated directory reading, which is essential for:
//   - Large directories that don't fit in a single response
//   - Memory-efficient directory traversal
//   - Protocol compliance (NFS, SMB, S3 all use pagination)
//   - Incremental UI updates (show entries as they arrive)
//
// Pagination Flow:
//
//	// Start reading directory
//	page, err := repo.ReadDirectory(ctx, dirHandle, "", 8192)
//	if err != nil {
//	    return err
//	}
//
//	// Process first page
//	for _, entry := range page.Entries {
//	    fmt.Printf("%s\n", entry.Name)
//	}
//
//	// Continue if more pages exist
//	for page.HasMore {
//	    page, err = repo.ReadDirectory(ctx, dirHandle, page.NextToken, 8192)
//	    if err != nil {
//	        return err
//	    }
//	    for _, entry := range page.Entries {
//	        fmt.Printf("%s\n", entry.Name)
//	    }
//	}
//
// Special Entries:
//
// The implementation may include "." (current directory) and ".." (parent directory)
// as the first entries when reading from the beginning (token=""). These are not
// real directory entries but virtual entries required by POSIX semantics.
//
// Empty Pages:
//
// An empty Entries slice with HasMore=true is valid and indicates that the
// implementation couldn't fit even one entry in the size limit. The caller
// should increase maxBytes and retry with the same token.
//
// Token Invalidation:
//
// Tokens may become invalid if:
//   - The directory is modified between pagination calls
//   - Too much time passes between calls (timeout)
//   - The server restarts
//
// When a token becomes invalid, ReadDirectory returns ErrInvalidArgument.
// Clients should restart pagination from the beginning (token="").
//
// Thread Safety:
//
// ReadDirPage instances are immutable after creation and safe for concurrent
// reading. However, pagination tokens are tied to a specific point-in-time
// view of the directory. Concurrent modifications may cause:
//   - Entries to be skipped (if renamed/moved before being read)
//   - Entries to appear twice (if renamed into already-read portion)
//   - Tokens to become invalid
//
// For consistent directory snapshots, implementations should use appropriate
// locking or versioning strategies.
type ReadDirPage struct {
	// Entries contains the directory entries for this page.
	//
	// The order of entries is implementation-specific but should be stable
	// (consistent across pagination calls for the same directory state).
	// Common ordering strategies:
	//   - Alphabetical by name (most user-friendly)
	//   - Inode/ID order (most efficient for filesystem iteration)
	//   - Insertion order (simplest for some implementations)
	//
	// May be empty if:
	//   - The directory is empty (and HasMore=false)
	//   - No entries fit within the size limit (and HasMore=true, rare)
	//
	// When token="" (first page), may include special entries "." and ".."
	// as the first two entries, following POSIX conventions.
	Entries []DirEntry

	// NextToken is the pagination token to use for retrieving the next page.
	//
	// Token Semantics:
	//   - Empty string (""): No more pages, pagination complete
	//   - Non-empty: Pass this value to ReadDirectory to get the next page
	//
	// Token Properties:
	//   - Opaque: Clients must treat as an opaque string
	//   - Ephemeral: May expire after some time or server restart
	//   - Stateless: Should not require server-side session state (preferred)
	//   - URL-safe: May contain only characters safe for URLs/JSON (recommended)
	//
	// Token Format Examples (implementation-specific):
	//   - Offset-based: "0", "100", "200" (simple but fragile under modifications)
	//   - Name-based: "file123.txt" (resume after this filename)
	//   - Cursor-based: "cursor:YXJyYXk=" (base64-encoded state)
	//   - Composite: "v1:ts:1234567890:name:file.txt" (versioned, structured)
	//
	// Token Validation:
	//   - Implementations must validate tokens and return ErrInvalidArgument
	//     for invalid, expired, or corrupted tokens
	//   - Tokens from one directory should not work for another directory
	//   - Tokens should include integrity checks (HMAC, checksum) if security matters
	NextToken string

	// HasMore indicates whether more entries are available after this page.
	//
	// This is a convenience field equivalent to (NextToken != "").
	// It allows for more readable code:
	//
	//	// Using HasMore (clearer intent)
	//	for page.HasMore {
	//	    page, err = repo.ReadDirectory(ctx, dirHandle, page.NextToken, size)
	//	}
	//
	//	// Using NextToken (also valid)
	//	for page.NextToken != "" {
	//	    page, err = repo.ReadDirectory(ctx, dirHandle, page.NextToken, size)
	//	}
	//
	// Invariant: HasMore == (NextToken != "")
	//
	// Implementations should ensure this invariant:
	//   - If NextToken is empty, HasMore must be false
	//   - If NextToken is non-empty, HasMore must be true
	HasMore bool
}

// ============================================================================
// Directory Operations
// ============================================================================

// ReadDirectory reads one page of directory entries with permission checking.
//
// This is the centralized implementation of directory reading that all stores
// should delegate to. It handles:
//   - Permission checking (read + traverse on directory)
//   - Delegation to store's ListChildren CRUD operation
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - dirHandle: Handle of directory to read
//   - token: Pagination token (empty for first page)
//   - maxBytes: Maximum response size hint
//
// Returns:
//   - *ReadDirPage: Page of directory entries
//   - error: ErrNotDirectory, ErrAccessDenied, etc.
func ReadDirectory(
	store MetadataStore,
	ctx *AuthContext,
	dirHandle FileHandle,
	token string,
	maxBytes uint32,
) (*ReadDirPage, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get directory entry to verify type
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    dir.Path,
		}
	}

	// Check read and traverse permissions
	granted, err := CheckFilePermissions(store, ctx, dirHandle, PermissionRead|PermissionTraverse)
	if err != nil {
		return nil, err
	}
	if granted&PermissionRead == 0 || granted&PermissionTraverse == 0 {
		return nil, &StoreError{
			Code:    ErrAccessDenied,
			Message: "no read or execute permission on directory",
			Path:    dir.Path,
		}
	}

	// Estimate max entries from maxBytes (rough estimate: ~200 bytes per entry)
	limit := 1000
	if maxBytes > 0 {
		limit = int(maxBytes / 200)
		if limit < 10 {
			limit = 10
		}
	}

	// Call store's CRUD ListChildren method
	entries, nextToken, err := store.ListChildren(ctx.Context, dirHandle, token, limit)
	if err != nil {
		return nil, err
	}

	return &ReadDirPage{
		Entries:   entries,
		NextToken: nextToken,
		HasMore:   nextToken != "",
	}, nil
}

// RemoveDirectory removes an empty directory from its parent.
//
// This is the centralized implementation of directory removal that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Sticky bit enforcement
//   - Empty check (directory must have no children)
//   - Parent link count update (removing ".." reference)
//   - Parent timestamp updates
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name of directory to remove
//
// Returns:
//   - error: Various errors for validation, permission, not empty, etc.
func RemoveDirectory(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
) error {
	// Validate name
	if err := ValidateName(name); err != nil {
		return err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := CheckWritePermission(store, ctx, parentHandle); err != nil {
		return err
	}

	// Get child handle
	dirHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    name,
		}
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &dir.FileAttr); err != nil {
		return err
	}

	// Check if directory is empty by trying to list children (using CRUD directly)
	// We use limit=1 to just check if any children exist
	entries, _, err := store.ListChildren(ctx.Context, dirHandle, "", 1)
	if err == nil && len(entries) > 0 {
		return &StoreError{
			Code:    ErrNotEmpty,
			Message: "directory not empty",
			Path:    name,
		}
	}

	// Remove directory entry
	if err := store.DeleteFile(ctx.Context, dirHandle); err != nil {
		return err
	}

	// Remove from parent's children
	if err := store.DeleteChild(ctx.Context, parentHandle, name); err != nil {
		return err
	}

	// Update parent's link count (removing ".." reference)
	parentLinkCount, err := store.GetLinkCount(ctx.Context, parentHandle)
	if err == nil && parentLinkCount > 0 {
		if err := store.SetLinkCount(ctx.Context, parentHandle, parentLinkCount-1); err != nil {
			// Non-fatal, continue
		}
	}

	// Update parent timestamps
	now := time.Now()
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutFile(ctx.Context, parent); err != nil {
		return err
	}

	return nil
}

// CreateDirectory creates a new directory in a parent directory.
//
// This is the centralized implementation of directory creation that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Name collision detection
//   - Default attribute application
//   - Parent link count update (new ".." reference)
//   - Parent timestamp updates
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name for the new directory
//   - attr: Initial attributes (Mode, UID, GID)
//
// Returns:
//   - *File: Created directory's complete metadata
//   - error: ErrAlreadyExists, ErrAccessDenied, etc.
func CreateDirectory(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
) (*File, error) {
	return createEntry(store, ctx, parentHandle, name, attr, FileTypeDirectory, "", 0, 0)
}
