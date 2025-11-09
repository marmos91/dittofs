package metadata

// DirEntry represents a directory entry returned by ReadDir.
type DirEntry struct {
	// Fileid is the unique file identifier
	Fileid uint64

	// Name is the filename
	Name string

	// Cookie is an opaque value for pagination
	Cookie uint64
}

// Interface for NFS metadata persistence
type Repository interface {
	// Export operations
	AddExport(path string, options ExportOptions, rootAttr *FileAttr) error
	GetExports() ([]Export, error)
	FindExport(path string) (*Export, error)
	DeleteExport(path string) error

	// GetRootHandle returns the root file handle for an export path
	GetRootHandle(exportPath string) (FileHandle, error)

	// File operations
	CreateFile(handle FileHandle, attr *FileAttr) error
	GetFile(handle FileHandle) (*FileAttr, error)
	UpdateFile(handle FileHandle, attr *FileAttr) error
	// DeleteFile deletes file metadata by handle.
	//
	// WARNING: This is a low-level operation that bypasses all safety checks.
	// It does NOT:
	//   - Check permissions
	//   - Remove the file from its parent directory
	//   - Update parent directory timestamps
	//   - Verify the file can be safely deleted
	//
	// In most cases, you should use RemoveFile instead, which performs
	// proper validation and cleanup.
	//
	// This method should only be used for:
	//   - Internal cleanup operations
	//   - Orphaned handle garbage collection
	//   - Operations that have already performed all necessary checks
	//
	// Returns error if the handle doesn't exist.
	DeleteFile(handle FileHandle) error

	// Directory hierarchy operations
	SetParent(child FileHandle, parent FileHandle) error
	GetParent(child FileHandle) (FileHandle, error)

	AddChild(parent FileHandle, name string, child FileHandle) error
	GetChild(parent FileHandle, name string) (FileHandle, error)
	GetChildren(parent FileHandle) (map[string]FileHandle, error)
	DeleteChild(parent FileHandle, name string) error

	// Helper method to add files/directories to a parent
	AddFileToDirectory(parentHandle FileHandle, name string, attr *FileAttr) (FileHandle, error)

	// Mount tracking operations
	// RecordMount records an active mount by a client
	RecordMount(exportPath string, clientAddr string, authFlavor uint32, machineName string, uid *uint32, gid *uint32) error

	// RemoveMount removes a mount record when a client unmounts
	RemoveMount(exportPath string, clientAddr string) error

	// GetMounts returns all active mounts, optionally filtered by export path
	// If exportPath is empty, returns all mounts
	GetMounts(exportPath string) ([]MountEntry, error)

	// IsClientMounted checks if a specific client has an active mount
	IsClientMounted(exportPath string, clientAddr string) (bool, error)

	// CheckExportAccess verifies if a client can access an export and returns effective credentials.
	// Returns an AccessDecision with details about the authorization and an AuthContext with
	// the effective UID/GID after applying squashing rules (AllSquash, RootSquash).
	//
	// The returned AuthContext should be used for all subsequent permission checks to ensure
	// consistent application of squashing rules throughout the mount session.
	CheckExportAccess(
		exportPath string,
		clientAddr string,
		authFlavor uint32,
		uid *uint32,
		gid *uint32,
		gids []uint32,
	) (*AccessDecision, *AuthContext, error)

	// CheckAccess verifies if a client has the requested access permissions on a file.
	// It performs Unix-style permission checking based on file ownership, mode bits,
	// and client credentials.
	//
	// Parameters:
	//   - handle: The file handle to check permissions for
	//   - requested: Bitmap of requested permissions (AccessRead, AccessModify, etc.)
	//   - ctx: Authentication context with client credentials
	//
	// Returns:
	//   - uint32: Bitmap of granted permissions (subset of requested)
	//   - error: Returns error only for internal failures; denied access returns 0 permissions
	CheckAccess(handle FileHandle, requested uint32, ctx *AccessCheckContext) (uint32, error)

	// GetMountsByClient returns all active mounts for a specific client
	// Used by UMNTALL to determine what will be removed
	GetMountsByClient(clientAddr string) ([]MountEntry, error)

	// RemoveAllMounts removes all mount records for a specific client
	// Used by UMNTALL to clean up all mounts in one operation
	RemoveAllMounts(clientAddr string) error

	// ServerConfig operations
	// SetServerConfig sets the server-wide configuration
	SetServerConfig(config ServerConfig) error

	// GetServerConfig returns the current server configuration
	GetServerConfig() (ServerConfig, error)

	// CheckDumpAccess verifies if a client can call the DUMP procedure
	// Returns error if access is denied, nil if allowed
	CheckDumpAccess(clientAddr string) error

	// GetFSInfo returns the static filesystem information and capabilities
	// This is used by the FSINFO NFS procedure to inform clients about
	// server limits and preferences (max transfer sizes, properties, etc.)
	GetFSInfo(handle FileHandle) (*FSInfo, error)

	// GetFSStats returns the dynamic filesystem statistics
	// This is used by the FSSTAT NFS procedure to inform clients about
	// current filesystem capacity, usage, and available space/inodes
	GetFSStats(handle FileHandle) (*FSStat, error)

	// CreateLink creates a hard link to an existing file.
	//
	// This adds a new directory entry that references the same file data.
	// The file's link count (nlink) should be incremented.
	//
	// Parameters:
	//   - dirHandle: Target directory where the link will be created
	//   - name: Name for the new link
	//   - fileHandle: File to link to
	//   - ctx: Authentication context for access control
	//
	// Returns error if:
	//   - Access denied (no write permission on directory)
	//   - Name already exists in directory
	//   - Cross-filesystem link attempted (implementation-specific)
	//   - fileHandle or dirHandle is invalid
	CreateLink(dirHandle FileHandle, name string, fileHandle FileHandle, ctx *AuthContext) error

	// CreateDirectory creates a new directory with the specified attributes.
	//
	// This method is responsible for:
	//  1. Checking write permission on the parent directory
	//  2. Verifying the name doesn't already exist
	//  3. Building complete directory attributes with defaults
	//  4. Creating the directory metadata
	//  5. Linking it to the parent directory
	//  6. Setting up the parent-child relationship
	//  7. Updating parent directory timestamps
	//
	// Parameters:
	//   - parentHandle: Handle of the parent directory
	//   - name: Name for the new directory
	//   - attr: Partial attributes (type, mode, uid, gid) - may have defaults
	//   - ctx: Authentication context for access control
	//
	// Returns:
	//   - FileHandle: Handle of the newly created directory
	//   - error: Returns error if:
	//     * Access denied (no write permission on parent)
	//     * Name already exists
	//     * Parent is not a directory
	//     * I/O error
	//
	// The implementation should:
	//   - Complete the attr structure with size (typically 4096), timestamps, etc.
	//   - Check that the caller has write permission on the parent directory
	//   - Ensure the directory has proper default attributes if not specified
	CreateDirectory(parentHandle FileHandle, name string, attr *FileAttr, ctx *AuthContext) (FileHandle, error)

	// GetPathConf returns POSIX-compatible filesystem information.
	// This is used by the PATHCONF NFS procedure to inform clients about
	// filesystem properties and limitations (filename lengths, link limits, etc.)
	GetPathConf(handle FileHandle) (*PathConf, error)

	// CreateSpecialFile creates a special file (device, socket, or FIFO).
	//
	// This method is responsible for:
	//  1. Checking write permission on the parent directory
	//  2. Checking privilege requirements (device creation often requires root)
	//  3. Verifying the name doesn't already exist
	//  4. Building complete file attributes with defaults
	//  5. Storing device numbers (for block/char devices)
	//  6. Creating the special file metadata
	//  7. Linking it to the parent directory
	//  8. Setting up the parent-child relationship
	//  9. Updating parent directory timestamps
	//
	// Parameters:
	//   - parentHandle: Handle of the parent directory
	//   - name: Name for the new special file
	//   - attr: Partial attributes (type, mode, uid, gid) - may have defaults
	//   - majorDevice: Major device number (for block/char devices, 0 otherwise)
	//   - minorDevice: Minor device number (for block/char devices, 0 otherwise)
	//   - ctx: Authentication context for access control
	//
	// Returns:
	//   - FileHandle: Handle of the newly created special file
	//   - error: Returns error if:
	//     * Access denied (no write permission on parent or insufficient privileges)
	//     * Name already exists
	//     * Parent is not a directory
	//     * Invalid file type
	//     * I/O error
	//
	// The implementation should:
	//   - Complete the attr structure with size (0), timestamps, etc.
	//   - Check that the caller has write permission on the parent directory
	//   - For device files, check that the caller has sufficient privileges (typically root)
	//   - Store device numbers appropriately (implementation-specific)
	//   - Ensure the special file has proper default attributes if not specified
	CreateSpecialFile(parentHandle FileHandle, name string, attr *FileAttr, majorDevice uint32, minorDevice uint32, ctx *AuthContext) (FileHandle, error)

	// ReadDir reads directory entries with pagination support.
	//
	// This method handles:
	//  - Checking read/execute permission on the directory
	//  - Building "." and ".." entries
	//  - Iterating through regular entries
	//  - Cookie-based pagination
	//  - Respecting count limits
	//
	// Parameters:
	//   - dirHandle: Directory to read
	//   - cookie: Starting position (0 = beginning)
	//   - count: Maximum response size in bytes (hint)
	//   - ctx: Authentication context for access control
	//
	// Returns:
	//   - []DirEntry: List of entries starting from cookie
	//   - bool: EOF flag (true if all entries returned)
	//   - error: Access denied or I/O errors
	ReadDir(dirHandle FileHandle, cookie uint64, count uint32, ctx *AuthContext) ([]DirEntry, bool, error)

	// ReadSymlink reads the target path of a symbolic link.
	//
	// This method is responsible for:
	//  1. Verifying the handle is a valid symbolic link
	//  2. Checking read permission on the symlink
	//  3. Retrieving the symlink target path
	//  4. Returning symlink attributes for cache consistency
	//
	// Parameters:
	//   - handle: File handle of the symbolic link
	//   - ctx: Authentication context for access control
	//
	// Returns:
	//   - string: The symlink target path
	//   - *FileAttr: Symlink attributes
	//   - error: Returns error if:
	//     * Handle not found
	//     * Handle is not a symlink
	//     * Access denied (no read permission)
	//     * Target path is missing or empty
	//     * I/O error
	ReadSymlink(handle FileHandle, ctx *AuthContext) (string, *FileAttr, error)

	// Add to internal/metadata/repository.go
	// Add to internal/metadata/repository.go

	// RemoveFile removes a file (not a directory) from a directory.
	//
	// This method is responsible for:
	//  1. Verifying the file exists and is not a directory
	//  2. Checking write permission on the parent directory
	//  3. Removing the file from the directory
	//  4. Deleting the file metadata
	//  5. Updating parent directory timestamps
	//
	// Parameters:
	//   - parentHandle: Handle of the parent directory
	//   - filename: Name of the file to remove
	//   - ctx: Authentication context for access control
	//
	// Returns:
	//   - *FileAttr: The attributes of the removed file (for response)
	//   - error: Returns error if:
	//     * Access denied (no write permission on parent)
	//     * File not found
	//     * File is a directory (use RemoveDirectory instead)
	//     * Parent is not a directory
	//     * I/O error
	RemoveFile(parentHandle FileHandle, filename string, ctx *AuthContext) (*FileAttr, error)
}
