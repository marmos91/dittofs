package metadata

import (
	"context"
)

// ============================================================================
// MetadataStore Interface
// ============================================================================

// MetadataStore provides protocol-agnostic metadata management for filesystem operations.
//
// This interface is designed to be used by multiple protocol handlers (NFS, SMB, FTP, etc.)
// without exposing protocol-specific concepts. Protocol handlers are responsible for
// translating between their wire formats and these generic operations.
//
// Separation of Concerns:
//
// The metadata store manages filesystem structure and metadata (file handles,
// attributes, permissions, directory hierarchy) but does NOT manage file content.
// File content is stored separately in a content repository.
//
// Content Coordination:
//   - PrepareWrite/PrepareRead: Protocol handler uses ContentID to read/write content
//   - RemoveFile: Protocol handler uses returned ContentID to coordinate content deletion
//   - Content cleanup can also be handled by garbage collection
//
// This separation allows:
//   - Independent scaling of metadata and content storage
//   - Content deduplication (multiple files referencing same ContentID)
//   - Flexible content storage backends (local disk, S3, distributed storage)
//   - Safe handling of hard links (content persists until all links removed)
//
// File Handle Implementation Strategy:
//
// Implementations MUST use path-based file handles to enable filesystem import/export
// and metadata reconstruction from content stores. The recommended format is:
//
//	Format: "shareName:fullPath"
//	Example: []byte("/export:/images/photo.jpg")
//
// This path-based approach provides:
//   - Determinism: Same path always generates the same handle
//   - Reversibility: Path can be extracted from handle for import/export
//   - Stability: Handles remain stable across server restarts
//   - Human-readable: Easy to debug and inspect
//   - Import-ready: Enables future filesystem import features
//
// For implementations requiring NFS compatibility (64-byte handle limit), consider
// a hybrid approach:
//   - Path-based format for short paths (≤ 64 bytes)
//   - Hash-based fallback with reverse mapping for long paths
//
// See pkg/metadata/badger/handle.go for a reference implementation of the hybrid
// approach with automatic fallback for long paths.
//
// File ID (Inode) Generation:
//
// Implementations MUST use HandleToFileID() to convert handles to inodes.
// Custom hashing will cause circular directory structures and inode collisions.
// See HandleToFileID() and ReadDirectory() documentation for details.
//
// Design Principles:
//   - Protocol-agnostic: No NFS/SMB/FTP-specific types or values
//   - Consistent error handling: All operations return StoreError for business logic errors
//   - Context-aware: All operations respect context cancellation and timeouts
//   - Permission-aware: Operations that modify data require AuthContext for access control
//   - Atomic operations: High-level operations (Lookup, Create, etc.) are atomic
//
// Thread Safety:
// Implementations must be safe for concurrent use by multiple goroutines.
type MetadataStore interface {
	// ========================================================================
	// Handle Operations
	// ========================================================================

	// GetShareNameForHandle returns the share name for a given file handle.
	//
	// This is needed to extract the share name from handles. Since handles are
	// encoded as "shareName:/path", this method decodes the share name portion.
	//
	// Handle Format Compatibility:
	// This method works with both path-based and hash-based handles:
	//   - Path-based: "shareName:/path" - share extracted from prefix
	//   - Hash-based: "64-char-hex" - share looked up from internal mapping
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - handle: File handle (path-based or hash-based)
	//
	// Returns:
	//   - string: Name of the share this handle belongs to
	//   - error: ErrNotFound if handle doesn't exist, ErrInvalidHandle if handle
	//     is malformed, or context cancellation error
	GetShareNameForHandle(ctx context.Context, handle FileHandle) (string, error)

	// ========================================================================
	// Access Control
	// ========================================================================

	// CheckPermissions performs file-level permission checking.
	//
	// This implements Unix-style permission checking based on file ownership,
	// mode bits, and client credentials. The method checks if the authenticated
	// user has the requested permissions on a specific file or directory.
	//
	// Permission Check Logic:
	//   - Root (UID 0): Bypass all checks (all permissions granted)
	//   - Owner: Check owner permission bits (rwx)
	//   - Group member: Check group permission bits (rwx)
	//   - Other: Check other permission bits (rwx)
	//   - Anonymous: Only world-readable/writable/executable
	//
	// Directory vs File Permissions:
	//   - Directories: read=list, execute=traverse, write=modify entries
	//   - Files: read=read data, execute=run, write=modify data
	//
	// Parameters:
	//   - ctx: Authentication context with client credentials
	//   - handle: The file handle to check permissions for
	//   - requested: Bitmap of requested permissions (PermissionRead, PermissionWrite, etc.)
	//
	// Returns:
	//   - Permission: Bitmap of granted permissions (subset of requested)
	//   - error: Only for internal failures (file not found) or context cancellation.
	//     Access denial returns 0 permissions, not an error.
	CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error)

	// ========================================================================
	// File/Directory Lookup and Attributes
	// ========================================================================

	// Lookup resolves a name within a directory to a file handle and attributes.
	//
	// This is the fundamental operation for path resolution. It combines directory
	// search, permission checking, and attribute retrieval into a single atomic
	// operation.
	//
	// Special Names:
	//   - ".": Returns the directory itself (dirHandle and its attributes)
	//   - "..": Returns the parent directory (parent handle and its attributes)
	//   - Regular names: Returns the child (child handle and its attributes)
	//
	// Permission Requirements:
	//   - Execute/traverse permission on the directory (search permission)
	//   - For "..": Execute permission on parent (if checking parent access)
	//
	// This replaces the pattern of:
	//   1. GetFile(dirHandle) - verify directory
	//   2. GetChild(dirHandle, name) - resolve name
	//   3. GetFile(childHandle) - get attributes
	//
	// With a single atomic operation that includes permission checking.
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - dirHandle: Directory to search in
	//   - name: Name to resolve (including "." and "..")
	//
	// Returns:
	//   - FileHandle: Handle of the resolved file/directory
	//   - *FileAttr: Complete attributes of the resolved file/directory
	//   - error: ErrNotFound if name doesn't exist, ErrNotDirectory if dirHandle
	//     is not a directory, ErrAccessDenied if no search permission, or context errors
	Lookup(ctx *AuthContext, dirHandle FileHandle, name string) (FileHandle, *FileAttr, error)

	// GetFile retrieves file attributes by handle.
	//
	// This is a lightweight operation that only reads metadata without permission
	// checking. It's used for operations where permission checking has already been
	// performed or is not required (e.g., getting attributes after successful lookup).
	//
	// For operations requiring permission checking, use Lookup or PrepareRead instead.
	//
	// Parameters:
	//   - handle: The file handle to query
	//
	// Returns:
	//   - *FileAttr: Complete file attributes
	//   - error: ErrNotFound if handle doesn't exist, ErrInvalidHandle if handle
	//     is malformed, or context cancellation error
	GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error)

	// SetFileAttributes updates file attributes with validation and access control.
	//
	// This implements selective attribute updates based on the Set* flags in attrs.
	// Only attributes with their corresponding Set* flag set to true are modified.
	// Other attributes remain unchanged.
	//
	// Permission Requirements:
	//   - Mode changes: Owner or root
	//   - UID changes: Root only
	//   - GID changes: Root, or owner if member of target group
	//   - Size changes: Write permission
	//   - Time changes: Write permission or owner
	//
	// Automatic Updates:
	//   - Ctime (change time): Always updated when any attribute changes
	//   - Mtime (modification time): Updated when size changes
	//
	// Size Changes:
	// Size modifications coordinate with the content repository:
	//   - Truncation (new < old): Content repository should remove trailing data
	//   - Extension (new > old): Content repository should pad with zeros
	//   - Zero: Content repository should delete all content
	//
	// The metadata repository updates the size metadata, but the protocol handler
	// must coordinate actual content changes with the content repository.
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - handle: The file handle to update
	//   - attrs: Attributes to set (only Set* = true are modified)
	//
	// Returns:
	//   - error: ErrNotFound if file doesn't exist, ErrAccessDenied if insufficient
	//     permissions, ErrPermissionDenied for ownership/permission changes by non-owner,
	//     ErrInvalidArgument for invalid values, or context errors
	SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) error

	// ========================================================================
	// File/Directory Creation
	// ========================================================================

	// CreateRootDirectory creates a root directory for a share without a parent.
	//
	// This is a special operation used during share initialization to create the
	// top-level directory for a share. Unlike regular directories created with Create(),
	// root directories have no parent handle.
	//
	// The implementation will:
	//   - Generate a handle using the format "shareName:/"
	//   - Initialize the directory with the provided attributes
	//   - Set link count to 2 (. and the share reference)
	//   - Store metadata atomically
	//
	// This method should only be called during share setup, not during normal filesystem
	// operations. Regular directory creation should use Create().
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - shareName: Name of the share (used to generate the root handle)
	//   - attr: Directory attributes (Type must be FileTypeDirectory)
	//
	// Returns:
	//   - FileHandle: Handle of the newly created root directory
	//   - error: ErrAlreadyExists if root already exists, ErrInvalidArgument if not a directory
	CreateRootDirectory(ctx context.Context, shareName string, attr *FileAttr) (FileHandle, error)

	// Create creates a new file or directory.
	//
	// The type of object created is determined by attr.Type:
	//   - FileTypeRegular: Creates a regular file (empty, size 0)
	//   - FileTypeDirectory: Creates a directory
	//
	// Other types must use their specific creation methods:
	//   - FileTypeSymlink: Use CreateSymlink
	//   - FileTypeBlockDevice, FileTypeCharDevice: Use CreateSpecialFile
	//   - FileTypeSocket, FileTypeFIFO: Use CreateSpecialFile
	//
	// Required fields in attr:
	//   - Type: Must be FileTypeRegular or FileTypeDirectory
	//   - Mode: Permission bits (if 0, defaults to 0644 for files, 0755 for directories)
	//   - UID, GID: Owner (if 0, uses authenticated user's credentials)
	//
	// The implementation completes attr with:
	//   - Size: 0 for files, implementation-specific for directories
	//   - Timestamps: Current time for atime, mtime, ctime
	//   - ContentID: Generated for files, empty for directories
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - parentHandle: Handle of the parent directory
	//   - name: Name for the new file/directory
	//   - attr: Attributes including Type (must be Regular or Directory)
	//
	// Returns:
	//   - FileHandle: Handle of the newly created file/directory
	//   - error: ErrInvalidArgument if Type is invalid, ErrAccessDenied if no write
	//     permission, ErrAlreadyExists if name exists, or other errors
	Create(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (FileHandle, error)

	// CreateSymlink creates a symbolic link pointing to a target path.
	//
	// A symbolic link is a special file that contains a pathname. Unlike hard links,
	// symlinks can:
	//   - Point to directories
	//   - Cross filesystem boundaries
	//   - Point to non-existent paths (dangling symlinks)
	//
	// Target Path:
	// The target path is stored without validation. Dangling symlinks (pointing to
	// non-existent paths) are allowed. The target is resolved when the symlink is
	// followed, not when it's created.
	//
	// Attribute Completion:
	//   - Type: FileTypeSymlink
	//   - Size: Length of target path string
	//   - Timestamps: Current time for atime, mtime, ctime
	//   - LinkTarget: The provided target path
	//   - ContentID: Empty (symlinks don't have content)
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
	//   - error: ErrAccessDenied if no write permission, ErrAlreadyExists if name exists,
	//     ErrNotDirectory if parent is not a directory, or context errors
	CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (FileHandle, error)

	// CreateSpecialFile creates a special file (device, socket, or FIFO).
	//
	// Special files represent system resources:
	//   - FileTypeBlockDevice: Block devices (disks, partitions)
	//   - FileTypeCharDevice: Character devices (terminals, serial ports)
	//   - FileTypeSocket: Unix domain sockets (IPC endpoints)
	//   - FileTypeFIFO: Named pipes (IPC channels)
	//
	// Device Files:
	// For block and character devices, deviceMajor and deviceMinor specify the
	// device numbers. The storage of device numbers is implementation-specific.
	// For sockets and FIFOs, device numbers should be 0.
	//
	// Security:
	// Device file creation typically requires root privileges (UID 0) to prevent
	// unauthorized hardware access. The implementation should enforce this for
	// FileTypeBlockDevice and FileTypeCharDevice.
	//
	// Attribute Completion:
	//   - Type: As specified in fileType parameter
	//   - Size: 0 (special files have no content)
	//   - Timestamps: Current time for atime, mtime, ctime
	//   - ContentID: Empty (special files don't have content)
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
	//   - error: ErrAccessDenied if insufficient privileges, ErrAlreadyExists if name
	//     exists, ErrNotDirectory if parent is not a directory, ErrInvalidArgument
	//     for invalid fileType, or context errors
	CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, deviceMajor, deviceMinor uint32) (FileHandle, error)

	// CreateHardLink creates a hard link to an existing file.
	//
	// A hard link creates a new directory entry that references the same file data
	// as an existing file. Both names refer to identical content and attributes.
	// Changes to the file through one name are visible through all names.
	//
	// Hard Link Restrictions:
	//   - Source and target must be in the same filesystem
	//   - Most implementations don't allow hard links to directories
	//   - Some filesystems have maximum link count limits
	//
	// Link Count:
	// The file's link count (nlink) should be incremented. When the last link is
	// removed, the file's data is deleted.
	//
	// Timestamps:
	//   - Target file: Ctime updated (metadata changed)
	//   - Parent directory: Mtime and ctime updated (contents changed)
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - dirHandle: Target directory where the link will be created
	//   - name: Name for the new link
	//   - targetHandle: File to link to
	//
	// Returns:
	//   - error: ErrAccessDenied if no write permission on directory, ErrAlreadyExists
	//     if name exists, ErrNotFound if targetHandle doesn't exist, ErrIsDirectory
	//     if trying to link a directory, ErrNotSupported if cross-filesystem, or
	//     context errors
	CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) error

	// ========================================================================
	// File/Directory Removal
	// ========================================================================

	// RemoveFile removes a file's metadata from its parent directory.
	//
	// This performs metadata cleanup including:
	//   - Permission validation (write permission on parent)
	//   - Type checking (must not be a directory)
	//   - File metadata deletion
	//   - Directory entry removal from parent
	//   - Parent timestamp updates
	//
	// WARNING: This method does NOT delete the file's content data.
	// The protocol handler is responsible for coordinating content deletion
	// with the content repository using the ContentID from the returned FileAttr.
	//
	// Hard Links:
	// If the file has multiple hard links, this removes only one link. The file's
	// content should only be deleted when the last link is removed (when a link
	// count reaches 0, if tracked).
	//
	// Recommended Protocol Handler Pattern:
	//
	//	// 1. Remove metadata and get file attributes
	//	attr, err := repo.RemoveFile(authCtx, parentHandle, filename)
	//	if err != nil {
	//	    return err
	//	}
	//
	//	// 2. Delete content from content repository (if last link)
	//	if attr.ContentID != "" {
	//	    // Check if this was the last link (implementation-specific)
	//	    if isLastLink {
	//	        err = contentRepo.Delete(ctx, attr.ContentID)
	//	        if err != nil {
	//	            // Log error but don't fail the remove operation
	//	            // Content can be garbage collected later
	//	        }
	//	    }
	//	}
	//
	// Returns:
	//   - *FileAttr: Attributes of the removed file (includes ContentID for content cleanup)
	//   - error: ErrAccessDenied, ErrNotFound, ErrIsDirectory, or context errors
	RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*FileAttr, error)

	// RemoveDirectory removes an empty directory's metadata from its parent.
	//
	// This performs metadata cleanup including:
	//   - Permission validation (write permission on parent)
	//   - Type checking (must be a directory)
	//   - Empty check (no children)
	//   - Directory metadata deletion
	//   - Directory entry removal from parent
	//   - Parent timestamp updates
	//
	// Content Deletion:
	// WARNING: This method does NOT delete any associated content data. Directories
	// typically don't have content in the content repository (ContentID is empty),
	// but if your implementation stores directory-specific data, cleanup is
	// the protocol handler's or garbage collector's responsibility.
	//
	// Returns:
	//   - error: ErrAccessDenied, ErrNotFound, ErrNotDirectory, ErrNotEmpty, or context errors
	RemoveDirectory(ctx *AuthContext, parentHandle FileHandle, name string) error

	// ========================================================================
	// File/Directory Operations
	// ========================================================================

	// Move moves or renames a file or directory atomically.
	//
	// This operation can:
	//   - Rename within the same directory (fromDir == toDir, different names)
	//   - Move to a different directory (fromDir != toDir, same or different name)
	//   - Move and rename in a single atomic operation
	//   - Atomically replace an existing file/directory at the destination
	//
	// Atomicity:
	// The operation should appear atomic to concurrent clients. All preconditions
	// are validated before making changes.
	//
	// Replacement Semantics:
	// When the destination name exists:
	//   - File over file: Allowed (atomic replacement)
	//   - Directory over empty directory: Allowed
	//   - Directory over non-empty directory: NOT allowed (ErrNotEmpty)
	//   - File over directory: NOT allowed (ErrIsDirectory)
	//   - Directory over file: NOT allowed (ErrNotDirectory)
	//
	// Permission Requirements:
	//   - Write permission on source directory (to remove entry)
	//   - Write permission on destination directory (to add entry)
	//
	// Special Cases:
	//   - Same directory, same name: Success (no-op)
	//   - Moving "." or ".." should be rejected by protocol layer validation
	//
	// Timestamps:
	//   - Moved file/directory: Ctime updated (metadata changed)
	//   - Source directory: Mtime and ctime updated (contents changed)
	//   - Destination directory: Mtime and ctime updated (if different from source)
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - fromDir: Source directory handle
	//   - fromName: Current name
	//   - toDir: Destination directory handle
	//   - toName: New name
	//
	// Returns:
	//   - error: ErrAccessDenied if no write permission on either directory,
	//     ErrNotFound if source doesn't exist, ErrNotEmpty if replacing non-empty
	//     directory, ErrIsDirectory/ErrNotDirectory for type mismatches, or context errors
	Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error

	// ReadSymlink reads the target path of a symbolic link.
	//
	// This returns the path stored in the symlink without following it. The target
	// path may be:
	//   - Absolute or relative
	//   - Point to a non-existent location (dangling symlink)
	//   - Point to another symlink (chain of symlinks)
	//
	// Permission Requirements:
	//   - Read permission on the symlink itself (not the target)
	//
	// Note: Reading a symlink requires read permission on the symlink file, not
	// execute permission. Execute permission is required when traversing through
	// a symlink (following it), which is a separate operation.
	//
	// Returns Attributes:
	// The method also returns the symlink's attributes for cache consistency.
	// This allows protocols to update their cached attributes in a single operation.
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - handle: File handle of the symbolic link
	//
	// Returns:
	//   - string: The target path stored in the symlink
	//   - *FileAttr: Attributes of the symlink itself (not the target)
	//   - error: ErrNotFound if handle doesn't exist, ErrInvalidArgument if handle
	//     is not a symlink, ErrAccessDenied if no read permission, or context errors
	ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *FileAttr, error)

	// ReadDirectory reads one page of directory entries with pagination support.
	//
	// Pagination:
	//   - Start with token="" (empty string) to read from beginning
	//   - Use page.NextToken for subsequent pages
	//   - page.NextToken="" (empty) indicates no more pages
	//   - page.HasMore is a convenience flag (same as NextToken != "")
	//
	// Pagination Tokens:
	// Tokens are opaque strings managed by the implementation. The format may
	// encode position, state, or other pagination information. Implementations
	// may use:
	//   - Simple offsets: "0", "100", "200"
	//   - Filenames: "file123.txt" (resume after this file)
	//   - Encoded cursors: "cursor:YXJyYXk=" (base64 state)
	//   - Structured tokens: "v1:dir:abc:offset:50"
	//
	// Clients must:
	//   - Treat tokens as opaque (never parse or construct)
	//   - Use empty string "" to start pagination
	//   - Pass NextToken unchanged to continue pagination
	//   - Not assume tokens are comparable or ordered
	//   - Not cache tokens across sessions (may become invalid)
	//
	// Implementations must:
	//   - Return "" for NextToken when no more pages
	//   - Validate token format and return ErrInvalidArgument if invalid
	//   - Handle token="" as "start from beginning"
	//   - Ensure tokens are URL-safe if needed by protocol
	//   - Use HandleToFileID(handle) to populate DirEntry.ID (required for inode consistency)
	//
	// Example - Read all entries:
	//
	//	var allEntries []DirEntry
	//	token := ""
	//	for {
	//	    page, err := repo.ReadDirectory(authCtx, dirHandle, token, 8192)
	//	    if err != nil {
	//	        return err
	//	    }
	//	    allEntries = append(allEntries, page.Entries...)
	//	    if !page.HasMore {
	//	        break
	//	    }
	//	    token = page.NextToken
	//	}
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - dirHandle: Directory to read
	//   - token: Pagination token (empty string = start, or NextToken from previous page)
	//   - maxBytes: Maximum response size hint in bytes (0 = use default of 8192)
	//
	// Returns:
	//   - *ReadDirPage: Page of entries with pagination info
	//   - error: ErrNotFound if directory doesn't exist, ErrNotDirectory if handle
	//     is not a directory, ErrPermissionDenied if no read/execute permission,
	//     ErrInvalidArgument if token is invalid, or context errors
	ReadDirectory(ctx *AuthContext, dirHandle FileHandle, token string, maxBytes uint32) (*ReadDirPage, error)

	// ========================================================================
	// File Content Coordination
	// ========================================================================

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
	//	    return err  // Content write failed, no metadata changes, nothing to rollback
	//	}
	//
	//	// Phase 3: Commit metadata changes
	//	err = repo.CommitWrite(authCtx, intent)
	//	if err != nil {
	//	    // Content written but metadata not updated
	//	    // This is detectable and can be repaired by consistency checks
	//	    return err
	//	}
	//
	// Permission Requirements:
	//   - Write permission on the file
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - handle: File handle to write to
	//   - newSize: New file size after write (offset + data length)
	//
	// Returns:
	//   - *WriteIntent: Intent containing ContentID and new attributes
	//   - error: ErrNotFound, ErrPermissionDenied, ErrIsDirectory, or context errors
	PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error)

	// CommitWrite applies metadata changes after a successful content write.
	//
	// This should be called after ContentRepository.WriteAt succeeds to update
	// the file's size and modification time.
	//
	// If this fails after content was written, the file is in an inconsistent
	// state (content newer than metadata). This can be detected by consistency
	// checkers comparing ContentID timestamps with file mtime.
	//
	// Parameters:
	//   - ctx: Authentication context (must be same user as PrepareWrite)
	//   - intent: The write intent from PrepareWrite
	//
	// Returns:
	//   - *FileAttr: Updated file attributes after commit
	//   - error: ErrNotFound if file was deleted, ErrStaleHandle if file changed,
	//     or context errors
	CommitWrite(ctx *AuthContext, intent *WriteOperation) (*FileAttr, error)

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
	// Permission Requirements:
	//   - Read permission on the file
	//
	// The returned attributes include the ContentID which the protocol handler
	// uses to retrieve the actual file data from the content repository.
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - handle: File handle to read from
	//
	// Returns:
	//   - *ReadMetadata: Contains file attributes including ContentID
	//   - error: ErrNotFound if file doesn't exist, ErrAccessDenied if no read
	//     permission, ErrIsDirectory if trying to read a directory, or context errors
	//
	// Example usage:
	//
	//	// Protocol handler for READ operation
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
	PrepareRead(ctx *AuthContext, handle FileHandle) (*ReadMetadata, error)

	// ========================================================================
	// Filesystem Information
	// ========================================================================

	// GetFilesystemCapabilities returns static filesystem capabilities and limits.
	//
	// This provides information about what the filesystem supports and its limits.
	// The information is relatively static (changes only on configuration updates
	// or server restart).
	//
	// Protocol handlers use this to:
	//   - Inform clients about server capabilities
	//   - Negotiate optimal transfer sizes
	//   - Determine feature support (hard links, symlinks, ACLs, etc.)
	//
	// Example protocol mappings:
	//   - NFS: FSINFO procedure
	//   - SMB: Query FS Information
	//
	// The handle parameter is used to identify which filesystem to query (in case
	// the server manages multiple filesystems with different capabilities).
	//
	// Parameters:
	//   - handle: A file handle within the filesystem to query
	//
	// Returns:
	//   - *FilesystemCapabilities: Static filesystem capabilities and limits
	//   - error: ErrNotFound if handle doesn't exist, or context errors
	GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error)

	// SetFilesystemCapabilities updates the filesystem capabilities for this store.
	//
	// This method allows updating the static capabilities after store creation,
	// which is useful during initialization when capabilities are loaded from
	// global configuration.
	//
	// This is typically called once during server initialization before the store
	// is used by any clients.
	//
	// Parameters:
	//   - capabilities: The new filesystem capabilities to use
	SetFilesystemCapabilities(capabilities FilesystemCapabilities)

	// GetFilesystemStatistics returns dynamic filesystem statistics.
	//
	// This provides current information about filesystem usage and availability.
	// The information is dynamic and may change frequently as files are created,
	// modified, or deleted.
	//
	// Protocol handlers use this to:
	//   - Report disk space to clients
	//   - Implement quota enforcement
	//   - Provide capacity planning information
	//
	// Example protocol mappings:
	//   - NFS: FSSTAT procedure
	//   - SMB: Query FS Size Information
	//
	// The handle parameter is used to identify which filesystem to query.
	//
	// Parameters:
	//   - handle: A file handle within the filesystem to query
	//
	// Returns:
	//   - *FilesystemStatistics: Dynamic filesystem usage statistics
	//   - error: ErrNotFound if handle doesn't exist, or context errors
	GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error)

	// ========================================================================
	// Configuration & Health
	// ========================================================================

	// SetServerConfig sets the server-wide configuration.
	//
	// This stores global server settings that apply across all shares and operations.
	// Configuration changes may affect:
	//   - Access control policies
	//   - Protocol-specific behaviors
	//   - Performance tuning parameters
	//
	// Thread Safety:
	// Configuration changes should be applied atomically. Concurrent operations
	// should see either the old or new configuration, never a partial update.
	//
	// Parameters:
	//   - config: The server configuration to apply
	//
	// Returns:
	//   - error: Only context cancellation errors
	SetServerConfig(ctx context.Context, config MetadataServerConfig) error

	// GetServerConfig returns the current server configuration.
	//
	// This retrieves the global server settings for use by protocol handlers
	// and management tools.
	//
	// Returns:
	//   - MetadataServerConfig: Current server configuration
	//   - error: Only context cancellation errors
	GetServerConfig(ctx context.Context) (MetadataServerConfig, error)

	// Healthcheck verifies the repository is operational.
	//
	// This performs a health check to ensure the repository can serve requests.
	// For implementations with external dependencies (databases, storage backends),
	// this should verify connectivity and availability.
	//
	// For in-memory implementations, this typically just returns nil since there
	// are no external dependencies to check.
	//
	// Use Cases:
	//   - Liveness probes in container orchestration
	//   - Load balancer health checks
	//   - Monitoring and alerting systems
	//   - Protocol NULL/ping procedures
	//
	// Implementation Guidelines:
	//   - Should be fast (< 1 second)
	//   - Should respect context timeouts
	//   - Should not modify any state
	//   - Should check critical subsystems only
	//
	// Returns:
	//   - error: Returns error if repository is unhealthy, nil if healthy,
	//     or context cancellation errors
	Healthcheck(ctx context.Context) error

	// ========================================================================
	// Garbage Collection Support
	// ========================================================================

	// GetAllContentIDs returns all ContentIDs referenced by metadata.
	//
	// This method is used by garbage collection to identify which content is
	// still in use. Content not in this list may be orphaned and eligible for
	// deletion.
	//
	// The method scans all files in all shares and collects their ContentIDs.
	// Only regular files with non-empty ContentIDs are included.
	//
	// Performance Considerations:
	//   - For large filesystems, this may take several seconds to minutes
	//   - Implementations should periodically check context for cancellation
	//   - Consider caching results if called frequently
	//   - Return results in batches if memory is a concern
	//
	// Thread Safety:
	// Safe for concurrent use. The returned set is a snapshot at the time
	// of the call and may become stale as files are created/deleted.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//
	// Returns:
	//   - []ContentID: List of all ContentIDs referenced by metadata
	//   - error: Returns error if scan fails or context is cancelled
	//
	// Example (garbage collection):
	//
	//	// Get all referenced content
	//	referenced, err := metadataStore.GetAllContentIDs(ctx)
	//	if err != nil {
	//	    return err
	//	}
	//
	//	// Get all existing content
	//	existing, err := contentStore.ListAllContent(ctx)
	//	if err != nil {
	//	    return err
	//	}
	//
	//	// Find orphaned content
	//	orphaned := difference(existing, referenced)
	//
	//	// Delete orphaned content
	//	contentStore.DeleteBatch(ctx, orphaned)
	GetAllContentIDs(ctx context.Context) ([]ContentID, error)
}
