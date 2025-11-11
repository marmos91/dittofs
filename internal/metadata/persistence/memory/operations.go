package memory

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ============================================================================
// Permission Helper Functions
// ============================================================================

// hasWritePermission checks if the user has write permission on a file/directory.
//
// Permission check logic:
//   - Root (UID 0): Always granted
//   - Owner: Check owner write bit (mode & 0200)
//   - Group member: Check group write bit (mode & 0020)
//   - Other: Check other write bit (mode & 0002)
//   - AUTH_NULL: Only if world-writable (mode & 0002)
//
// Parameters:
//   - attr: File attributes containing mode, uid, gid
//   - ctx: Authentication context (may be nil for unauthenticated access)
//
// Returns:
//   - bool: true if write permission granted, false otherwise
func hasWritePermission(ctx *metadata.AuthContext, attr *metadata.FileAttr) bool {
	// No authentication context: deny by default
	if ctx == nil {
		return false
	}

	// AUTH_NULL: only grant write if world-writable
	if ctx.AuthFlavor == 0 {
		return (attr.Mode & 0002) != 0
	}

	// Authenticated access requires both UID and GID
	if ctx.UID == nil || ctx.GID == nil {
		return false
	}

	uid := *ctx.UID
	gid := *ctx.GID

	// Root user bypasses all permission checks
	if uid == 0 {
		return true
	}

	// Owner permissions
	if uid == attr.UID {
		return (attr.Mode & 0200) != 0
	}

	// Group permissions
	if gid == attr.GID || containsGID(ctx.GIDs, attr.GID) {
		return (attr.Mode & 0020) != 0
	}

	// Other permissions
	return (attr.Mode & 0002) != 0
}

// hasReadPermission checks if the user has read permission on a file/directory.
//
// Permission check logic follows the same pattern as hasWritePermission
// but checks read bits (0400/0040/0004) instead of write bits.
//
// Parameters:
//   - attr: File attributes containing mode, uid, gid
//   - ctx: Authentication context (may be nil for unauthenticated access)
//
// Returns:
//   - bool: true if read permission granted, false otherwise
func hasReadPermission(ctx *metadata.AuthContext, attr *metadata.FileAttr) bool {
	// No authentication context: deny by default
	if ctx == nil {
		return false
	}

	// AUTH_NULL: only grant read if world-readable
	if ctx.AuthFlavor == 0 {
		return (attr.Mode & 0004) != 0
	}

	// Authenticated access requires both UID and GID
	if ctx.UID == nil || ctx.GID == nil {
		return false
	}

	uid := *ctx.UID
	gid := *ctx.GID

	// Root user bypasses all permission checks
	if uid == 0 {
		return true
	}

	// Owner permissions
	if uid == attr.UID {
		return (attr.Mode & 0400) != 0
	}

	// Group permissions
	if gid == attr.GID || containsGID(ctx.GIDs, attr.GID) {
		return (attr.Mode & 0040) != 0
	}

	// Other permissions
	return (attr.Mode & 0004) != 0
}

// hasExecutePermission checks if the user has execute permission on a directory.
//
// Execute permission on directories (also called "search" permission) is required
// to access files within the directory or traverse through it.
//
// Parameters:
//   - attr: Directory attributes containing mode, uid, gid
//   - ctx: Authentication context (may be nil for unauthenticated access)
//
// Returns:
//   - bool: true if execute permission granted, false otherwise
func hasExecutePermission(ctx *metadata.AuthContext, attr *metadata.FileAttr) bool {
	// No authentication context: deny by default
	if ctx == nil {
		return false
	}

	// AUTH_NULL: only grant execute if world-executable
	if ctx.AuthFlavor == 0 {
		return (attr.Mode & 0001) != 0
	}

	// Authenticated access requires both UID and GID
	if ctx.UID == nil || ctx.GID == nil {
		return false
	}

	uid := *ctx.UID
	gid := *ctx.GID

	// Root user bypasses all permission checks
	if uid == 0 {
		return true
	}

	// Owner permissions
	if uid == attr.UID {
		return (attr.Mode & 0100) != 0
	}

	// Group permissions
	if gid == attr.GID || containsGID(ctx.GIDs, attr.GID) {
		return (attr.Mode & 0010) != 0
	}

	// Other permissions
	return (attr.Mode & 0001) != 0
}

// isOwnerOrRoot checks if the authenticated user is the file owner or root.
//
// This is commonly used for operations that require ownership, such as:
//   - Changing file permissions (chmod)
//   - Changing file timestamps
//   - Setting extended attributes
//
// Parameters:
//   - attr: File attributes containing uid
//   - ctx: Authentication context (may be nil)
//
// Returns:
//   - bool: true if user is owner or root, false otherwise
func isOwnerOrRoot(ctx *metadata.AuthContext, attr *metadata.FileAttr) bool {
	if ctx == nil || ctx.AuthFlavor == 0 || ctx.UID == nil {
		return false
	}

	uid := *ctx.UID
	return uid == 0 || uid == attr.UID
}

// canChangeGroup checks if the user can change a file's group to the specified GID.
//
// Group changes are allowed if:
//   - User is root (UID 0)
//   - User is owner AND is a member of the target group
//
// Parameters:
//   - attr: File attributes containing uid
//   - targetGID: The new group ID being set
//   - ctx: Authentication context (may be nil)
//
// Returns:
//   - bool: true if group change is allowed, false otherwise
func canChangeGroup(ctx *metadata.AuthContext, attr *metadata.FileAttr, targetGID uint32) bool {
	if ctx == nil || ctx.AuthFlavor == 0 || ctx.UID == nil || ctx.GID == nil {
		return false
	}

	uid := *ctx.UID
	gid := *ctx.GID

	// Root can always change group
	if uid == 0 {
		return true
	}

	// Owner can change if they're in the target group
	if uid != attr.UID {
		return false
	}

	// Check if user is in the target group
	return gid == targetGID || containsGID(ctx.GIDs, targetGID)
}

// ============================================================================
// File Operations
// ============================================================================

// SetFileAttributes updates file attributes with access control.
//
// This implements support for the SETATTR NFS procedure (RFC 1813 section 3.3.2).
// It handles selective attribute updates based on the Set* flags in the attrs
// parameter, with proper permission checking and validation.
//
// Permission Requirements:
//   - Mode changes: Only owner or root
//   - UID changes: Only root
//   - GID changes: Only root (or owner if in supplementary groups)
//   - Size changes: Write permission required
//   - Time changes: Write permission or owner
//
// The method automatically updates ctime (change time) whenever any attribute
// is modified, as required by RFC 1813.
//
// Size Changes:
// Size modifications require coordination with the content repository:
//   - Truncation (new < old): Remove trailing content
//   - Extension (new > old): Pad with zeros
//   - Zero: Delete all content
//
// Note: In this in-memory implementation, content coordination is not yet
// implemented. A production implementation would delegate to a content
// repository for actual data truncation/extension.
//
// Parameters:
//   - ctx: Authentication context for access control
//   - handle: The file handle to update
//   - attrs: The attributes to set (only Set* = true are modified)
//
// Returns error if:
//   - Context is cancelled
//   - File not found
//   - Access denied (insufficient permissions)
//   - Not owner (for ownership/permission changes)
//   - Invalid attribute values (e.g., negative size)
//   - Attempting to set size on directory or special file
//   - I/O error
func (r *MemoryRepository) SetFileAttributes(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	attrs *metadata.SetAttrs,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled before setting file attributes: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled while setting file attributes: %w", err)
	}

	// ========================================================================
	// Step 1: Verify file exists and get current attributes
	// ========================================================================

	key := handleToKey(handle)
	fileAttr, exists := r.files[key]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "file not found",
		}
	}

	// Track if any attributes were modified (for ctime update)
	modified := false

	// ========================================================================
	// Step 2: Check permissions and apply attribute updates
	// ========================================================================

	// ------------------------------------------------------------------------
	// Mode (permissions) - only owner or root can change
	// ------------------------------------------------------------------------

	if attrs.SetMode {
		// Check if user is owner or root
		if !isOwnerOrRoot(ctx, fileAttr) {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "only owner or root can change permissions",
			}
		}

		// Validate mode value (only use lower 12 bits)
		if attrs.Mode > 0o7777 {
			return &metadata.ExportError{
				Code:    metadata.ExportErrServerFault,
				Message: fmt.Sprintf("invalid mode value: 0%o (max 0o7777)", attrs.Mode),
			}
		}

		fileAttr.Mode = attrs.Mode
		modified = true

		logger.Debug("SetFileAttributes: mode changed to 0%o for handle %x", attrs.Mode, handle)
	}

	// ------------------------------------------------------------------------
	// UID (owner) - only root can change ownership
	// ------------------------------------------------------------------------

	if attrs.SetUID {
		// Only root can change file ownership
		if ctx == nil || ctx.AuthFlavor == 0 || ctx.UID == nil || *ctx.UID != 0 {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "only root can change file ownership",
			}
		}

		fileAttr.UID = attrs.UID
		modified = true

		logger.Debug("SetFileAttributes: uid changed to %d for handle %x", attrs.UID, handle)
	}

	// ------------------------------------------------------------------------
	// GID (group) - only root or owner (if in target group) can change
	// ------------------------------------------------------------------------

	if attrs.SetGID {
		// Check if user can change group
		if !canChangeGroup(ctx, fileAttr, attrs.GID) {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "only root or owner (if in target group) can change group",
			}
		}

		fileAttr.GID = attrs.GID
		modified = true

		logger.Debug("SetFileAttributes: gid changed to %d for handle %x", attrs.GID, handle)
	}

	// ------------------------------------------------------------------------
	// Size - write permission required, only valid for regular files
	// ------------------------------------------------------------------------

	if attrs.SetSize {
		// Verify this is a regular file
		if fileAttr.Type != metadata.FileTypeRegular {
			return &metadata.ExportError{
				Code:    metadata.ExportErrServerFault,
				Message: fmt.Sprintf("cannot set size on non-regular file (type=%d)", fileAttr.Type),
			}
		}

		// Check write permission
		if !hasWritePermission(ctx, fileAttr) {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "write permission denied for size change",
			}
		}

		// Update size
		// TODO: In a production implementation, coordinate with content repository
		// to actually truncate or extend the file content:
		//   - If attrs.Size < fileAttr.Size: Truncate content
		//   - If attrs.Size > fileAttr.Size: Extend with zeros
		//   - If attrs.Size == 0: Delete all content
		oldSize := fileAttr.Size
		fileAttr.Size = attrs.Size
		modified = true

		logger.Debug("SetFileAttributes: size changed from %d to %d for handle %x", oldSize, attrs.Size, handle)

		// Update mtime when size changes (POSIX semantics)
		fileAttr.Mtime = time.Now()
	}

	// ------------------------------------------------------------------------
	// Atime (access time) - owner or write permission required
	// ------------------------------------------------------------------------

	if attrs.SetAtime {
		// Check if user can set atime (owner or write permission)
		if !isOwnerOrRoot(ctx, fileAttr) && !hasWritePermission(ctx, fileAttr) {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "insufficient permission to set atime",
			}
		}

		fileAttr.Atime = attrs.Atime
		modified = true

		logger.Debug("SetFileAttributes: atime changed to %v for handle %x", attrs.Atime, handle)
	}

	// ------------------------------------------------------------------------
	// Mtime (modification time) - owner or write permission required
	// ------------------------------------------------------------------------

	if attrs.SetMtime {
		// Check if user can set mtime (owner or write permission)
		if !isOwnerOrRoot(ctx, fileAttr) && !hasWritePermission(ctx, fileAttr) {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "insufficient permission to set mtime",
			}
		}

		fileAttr.Mtime = attrs.Mtime
		modified = true

		logger.Debug("SetFileAttributes: mtime changed to %v for handle %x", attrs.Mtime, handle)
	}

	// ========================================================================
	// Step 3: Update ctime if any attributes were modified
	// ========================================================================
	// Per RFC 1813, ctime (change time) is automatically updated by the
	// server whenever any file metadata changes. Clients cannot set it.

	if modified {
		fileAttr.Ctime = time.Now()
		logger.Debug("SetFileAttributes: ctime updated to %v for handle %x", fileAttr.Ctime, handle)
	}

	// ========================================================================
	// Step 4: Save updated attributes
	// ========================================================================

	r.files[key] = fileAttr

	return nil
}

// CreateLink creates a hard link to an existing file.
//
// This implements support for the LINK NFS procedure (RFC 1813 section 3.3.15).
// A hard link creates a new directory entry that references the same file data
// as an existing file. Both names refer to identical file content and attributes.
//
// Implementation Notes:
//   - This implementation uses a handle-based system where multiple directory
//     entries can reference the same handle without tracking a separate link count
//   - In a real implementation with link counting, you would increment nlink here
//   - The file's change time (ctime) is updated to reflect metadata modification
//
// RFC 1813 Requirements:
//   - The link and file must be on the same filesystem
//   - The server should check write permission on the target directory
//   - The file's ctime should be updated
//   - Hard links to directories are typically not allowed
//
// Parameters:
//   - ctx: Authentication context for access control
//   - dirHandle: Target directory where the link will be created
//   - name: Name for the new link
//   - fileHandle: File to link to
//
// Returns error if:
//   - Context is cancelled
//   - Source file not found
//   - Target directory not found or not a directory
//   - Access denied (no write permission on directory)
//   - Name already exists in directory
//   - Cross-filesystem link attempted (implementation-specific)
func (r *MemoryRepository) CreateLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	fileHandle metadata.FileHandle,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled before creating link: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled while creating link: %w", err)
	}

	// ========================================================================
	// Step 1: Verify source file exists
	// ========================================================================

	fileKey := handleToKey(fileHandle)
	fileAttr, exists := r.files[fileKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "source file not found",
		}
	}

	// ========================================================================
	// Step 2: Verify target directory exists and is a directory
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	dirAttr, exists := r.files[dirKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "target directory not found",
		}
	}

	if dirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "target is not a directory",
		}
	}

	// ========================================================================
	// Step 3: Check write access to directory
	// ========================================================================

	if !hasWritePermission(ctx, dirAttr) {
		return &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on target directory",
		}
	}

	// ========================================================================
	// Step 4: Verify name doesn't already exist
	// ========================================================================

	if r.children[dirKey] == nil {
		r.children[dirKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[dirKey][name]; exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("name already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 5: Create the link
	// ========================================================================

	// Add the directory entry pointing to the existing file handle
	r.children[dirKey][name] = fileHandle

	// ========================================================================
	// Step 6: Update timestamps
	// ========================================================================

	now := time.Now()

	// Update directory modification time (contents changed)
	dirAttr.Mtime = now
	dirAttr.Ctime = now
	r.files[dirKey] = dirAttr

	// Update file change time (metadata changed - link count would increase)
	fileAttr.Ctime = now
	r.files[fileKey] = fileAttr

	logger.Debug("CreateLink: created link '%s' in directory %x to file %x", name, dirHandle, fileHandle)

	// Note: In a real implementation with link counting, you would also
	// increment fileAttr.Nlink here. However, in this handle-based system,
	// the link count is computed dynamically when needed.

	return nil
}

// CreateDirectory creates a new directory with the specified attributes.
//
// This implements support for the MKDIR NFS procedure (RFC 1813 section 3.3.9).
// It handles the complete directory creation workflow including permission checks,
// attribute completion with defaults, and parent directory updates.
//
// Attribute Completion:
// The protocol layer provides partial attributes (type, mode, uid, gid) which
// may have defaults applied. This method completes them with:
//   - Size: 4096 (standard directory size)
//   - Timestamps: Current time for atime, mtime, ctime
//   - ContentID: Empty (directories don't have content blobs)
//
// RFC 1813 Requirements:
//   - Check write permission on parent directory
//   - Verify name doesn't already exist
//   - Apply appropriate default permissions if not specified
//   - Update parent directory timestamps (mtime, ctime)
//
// Parameters:
//   - ctx: Authentication context for access control
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new directory
//   - attr: Partial attributes (type, mode, uid, gid) - may have defaults
//
// Returns:
//   - FileHandle: Handle of the newly created directory
//   - error: Returns error if:
//   - Context is cancelled
//   - Access denied (no write permission on parent)
//   - Name already exists
//   - Parent is not a directory
//   - I/O error
func (r *MemoryRepository) CreateDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (metadata.FileHandle, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before creating directory: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled while creating directory: %w", err)
	}

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check write access to parent directory
	// ========================================================================

	if !hasWritePermission(ctx, parentAttr) {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on parent directory",
		}
	}

	// ========================================================================
	// Step 3: Verify name doesn't already exist
	// ========================================================================

	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("directory already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 4: Complete directory attributes with defaults
	// ========================================================================

	now := time.Now()

	// Start with the attributes provided by the protocol layer
	completeAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, // Always directory
		Mode: attr.Mode,                  // From protocol layer (or default 0755)
		UID:  attr.UID,                   // From protocol layer (or authenticated user)
		GID:  attr.GID,                   // From protocol layer (or authenticated group)

		// Server-assigned attributes
		Size:      4096, // Standard directory size
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Directories don't have content blobs
	}

	// ========================================================================
	// Step 5: Generate unique directory handle
	// ========================================================================

	dirHandle := r.generateFileHandle(name)

	// ========================================================================
	// Step 6: Create directory metadata
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	r.files[dirKey] = completeAttr

	// Initialize empty children map for the new directory
	r.children[dirKey] = make(map[string]metadata.FileHandle)

	// ========================================================================
	// Step 7: Link directory to parent
	// ========================================================================

	r.children[parentKey][name] = dirHandle

	// Set parent relationship
	r.parents[dirKey] = parentHandle

	// ========================================================================
	// Step 8: Update parent directory timestamps
	// ========================================================================
	// The parent directory's mtime and ctime should be updated when a child
	// is added, as this modifies the directory's contents.

	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	logger.Debug("CreateDirectory: created directory '%s' in parent %x with handle %x", name, parentHandle, dirHandle)

	return dirHandle, nil
}

// CreateSpecialFile creates a special file (device, socket, or FIFO).
//
// This implements support for the MKNOD NFS procedure (RFC 1813 section 3.3.11).
// Special files are non-regular files that represent:
//   - Character devices (terminals, serial ports, etc.)
//   - Block devices (disks, partitions, etc.)
//   - Sockets (IPC endpoints)
//   - FIFOs/named pipes (IPC channels)
//
// Device Files:
// For character and block devices, the majorDevice and minorDevice parameters
// specify the device numbers. In this in-memory implementation, device numbers
// are stored in the SymlinkTarget field as "device:major:minor". A production
// implementation would use proper device-specific storage.
//
// Security:
// Device file creation typically requires root privileges (UID 0) to prevent
// unauthorized hardware access. This is enforced by checking the authenticated
// user's UID.
//
// RFC 1813 Requirements:
//   - Check write permission on parent directory
//   - Check privilege requirements (root for devices)
//   - Verify name doesn't already exist
//   - Apply appropriate default permissions
//   - Update parent directory timestamps
//
// Parameters:
//   - ctx: Authentication context for access control
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new special file
//   - attr: Partial attributes (type, mode, uid, gid) from protocol layer
//   - majorDevice: Major device number (for block/char devices, 0 otherwise)
//   - minorDevice: Minor device number (for block/char devices, 0 otherwise)
//
// Returns:
//   - FileHandle: Handle of the newly created special file
//   - error: Returns error if:
//   - Context is cancelled
//   - Access denied (no write permission or insufficient privileges)
//   - Name already exists
//   - Parent is not a directory
//   - Invalid file type
//   - I/O error
func (r *MemoryRepository) CreateSpecialFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
	majorDevice uint32,
	minorDevice uint32,
) (metadata.FileHandle, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before creating special file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled while creating special file: %w", err)
	}

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check write access to parent directory
	// ========================================================================

	if !hasWritePermission(ctx, parentAttr) {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on parent directory",
		}
	}

	// ========================================================================
	// Step 3: Check privilege requirements for device creation
	// ========================================================================
	// Device files (character and block devices) typically require root
	// privileges to create. This is a security measure to prevent
	// unauthorized hardware access.

	if attr.Type == metadata.FileTypeChar || attr.Type == metadata.FileTypeBlock {
		// Check if user has sufficient privileges (root or CAP_MKNOD)
		if ctx == nil || ctx.UID == nil || *ctx.UID != 0 {
			// Non-root user attempting to create a device file
			return nil, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: "device file creation requires root privileges",
			}
		}
	}

	// ========================================================================
	// Step 4: Verify name doesn't already exist
	// ========================================================================

	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("file already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 5: Complete file attributes with defaults
	// ========================================================================

	now := time.Now()

	// Start with the attributes provided by the protocol layer
	completeAttr := &metadata.FileAttr{
		Type: attr.Type, // FileTypeChar, FileTypeBlock, FileTypeSocket, or FileTypeFifo
		Mode: attr.Mode, // From protocol layer (or default)
		UID:  attr.UID,  // From protocol layer (or authenticated user)
		GID:  attr.GID,  // From protocol layer (or authenticated group)

		// Server-assigned attributes
		Size:      0, // Special files have no content
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Special files don't have content blobs
	}

	// Apply default mode if not specified
	if completeAttr.Mode == 0 {
		completeAttr.Mode = 0644 // Default: rw-r--r--
	}

	// ========================================================================
	// Step 6: Store device numbers (implementation-specific)
	// ========================================================================
	// In this in-memory implementation, we store device numbers in the
	// SymlinkTarget field as a formatted string. In a real implementation,
	// you would store this in a proper device-specific field.
	//
	// Note: This is a demonstration of how to handle device numbers.
	// A production implementation should use a proper storage mechanism.

	if attr.Type == metadata.FileTypeChar || attr.Type == metadata.FileTypeBlock {
		// Store device numbers as a string in the format "device:major:minor"
		// This is just for demonstration - a real implementation would
		// store these properly in the filesystem metadata
		completeAttr.SymlinkTarget = fmt.Sprintf("device:%d:%d", majorDevice, minorDevice)
	}

	// ========================================================================
	// Step 7: Generate unique file handle
	// ========================================================================

	fileHandle := r.generateFileHandle(name)

	// ========================================================================
	// Step 8: Create special file metadata
	// ========================================================================

	fileKey := handleToKey(fileHandle)
	r.files[fileKey] = completeAttr

	// ========================================================================
	// Step 9: Link special file to parent
	// ========================================================================

	r.children[parentKey][name] = fileHandle

	// Set parent relationship
	r.parents[fileKey] = parentHandle

	// ========================================================================
	// Step 10: Update parent directory timestamps
	// ========================================================================
	// The parent directory's mtime and ctime should be updated when a child
	// is added, as this modifies the directory's contents.

	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	logger.Debug("CreateSpecialFile: created special file '%s' (type=%d) in parent %x with handle %x",
		name, attr.Type, parentHandle, fileHandle)

	return fileHandle, nil
}

// ReadDir reads directory entries with pagination support.
//
// This implements support for the READDIR NFS procedure (RFC 1813 section 3.3.16).
// It provides efficient directory listing with cookie-based pagination to handle
// directories with many entries.
//
// Cookie Semantics:
//   - 0: Start of directory (returns "." first)
//   - 1: After "." entry (returns ".." next)
//   - 2: After ".." entry (returns regular entries)
//   - 3+: After each regular entry (one cookie per entry)
//
// The cookie values are opaque to the client and must be treated as continuation
// tokens. The server guarantees that using a returned cookie will resume the
// listing at the next entry.
//
// Pagination:
// The count parameter is used as a hint to limit response size. The server
// estimates the XDR-encoded size of each entry and stops when adding another
// entry would exceed the count. This prevents:
//   - Client buffer overflows
//   - Excessive network transmission times
//   - Server memory exhaustion
//
// Stable Ordering:
// Entries are returned in a stable, sorted order (alphabetical by name) to
// ensure consistent pagination. Without stable ordering, entries could be
// skipped or duplicated if the directory changes between requests.
//
// RFC 1813 Requirements:
//   - Check read and execute permission on the directory
//   - Include "." and ".." entries
//   - Provide stable ordering for pagination
//   - Return EOF flag when all entries have been sent
//
// Parameters:
//   - ctx: Authentication context for access control
//   - dirHandle: Directory to read
//   - cookie: Starting position (0 = beginning)
//   - count: Maximum response size in bytes (approximate)
//
// Returns:
//   - []DirEntry: List of entries starting from cookie
//   - bool: EOF flag (true if all entries returned)
//   - error: Access denied, context cancelled, or I/O errors
func (r *MemoryRepository) ReadDir(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	cookie uint64,
	count uint32,
) ([]metadata.DirEntry, bool, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, false, fmt.Errorf("context cancelled before reading directory: %w", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, false, fmt.Errorf("context cancelled while reading directory: %w", err)
	}

	// ========================================================================
	// Step 1: Verify directory exists and is a directory
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	dirAttr, exists := r.files[dirKey]
	if !exists {
		return nil, false, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "directory not found",
		}
	}

	// Verify it's actually a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		return nil, false, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check read/execute permission on directory
	// ========================================================================
	// Execute (search) permission is required to read directory contents
	// Read permission is required to list the directory

	if !hasReadPermission(ctx, dirAttr) || !hasExecutePermission(ctx, dirAttr) {
		return nil, false, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "read/execute permission denied on directory",
		}
	}

	// ========================================================================
	// Step 3: Build entries list with pagination
	// ========================================================================

	entries := make([]metadata.DirEntry, 0)
	currentCookie := uint64(1)

	// Track estimated size incrementally as we add entries
	// XDR encoding overhead per entry:
	//   4 bytes (value_follows) + 8 bytes (fileid) +
	//   4 bytes (name length) + name bytes + padding (0-3 bytes) + 8 bytes (cookie)
	//   = 24 bytes + name length + padding
	estimatedSize := uint32(0)

	// Reserve space for response overhead (status, attrs, verifier, eof, end marker)
	const responseOverhead = 200
	estimatedSize += responseOverhead

	// Extract directory file ID for "." entry
	dirFileid := extractFileIDFromHandle(dirHandle)

	// ========================================================================
	// Add "." entry (cookie 1)
	// ========================================================================

	if cookie == 0 {
		entry := metadata.DirEntry{
			Fileid: dirFileid,
			Name:   ".",
			Cookie: currentCookie,
		}

		// Calculate size for this entry
		nameLen := len(entry.Name)
		padding := (4 - (nameLen % 4)) % 4
		entrySize := 24 + uint32(nameLen) + uint32(padding)

		entries = append(entries, entry)
		estimatedSize += entrySize
	}
	currentCookie++

	// ========================================================================
	// Add ".." entry (cookie 2)
	// ========================================================================

	if cookie <= 1 {
		// Get parent file ID
		parentFileid := dirFileid // Default to self if no parent
		if parentHandle, err := r.GetParent(context.Background(), dirHandle); err == nil {
			parentFileid = extractFileIDFromHandle(parentHandle)
		}

		entry := metadata.DirEntry{
			Fileid: parentFileid,
			Name:   "..",
			Cookie: currentCookie,
		}

		// Calculate size for this entry
		nameLen := len(entry.Name)
		padding := (4 - (nameLen % 4)) % 4
		entrySize := 24 + uint32(nameLen) + uint32(padding)

		entries = append(entries, entry)
		estimatedSize += entrySize
	}
	currentCookie++

	// ========================================================================
	// Add regular entries (cookies 3+)
	// ========================================================================

	// Get all children
	children := r.children[dirKey]
	if children != nil {
		// We need a stable ordering for pagination to work correctly
		// Sort names alphabetically for consistent iteration
		names := make([]string, 0, len(children))
		for name := range children {
			names = append(names, name)
		}

		// Sort for stable ordering (O(n log n) with optimized quicksort)
		slices.Sort(names)

		// Iterate through children, skipping entries before cookie
		for _, name := range names {
			// Check context periodically during iteration
			if err := ctx.Context.Err(); err != nil {
				return nil, false, fmt.Errorf("context cancelled during directory read: %w", err)
			}

			handle := children[name]

			// Skip entries before the requested cookie
			if currentCookie <= cookie {
				currentCookie++
				continue
			}

			// Calculate size for this entry BEFORE adding it
			nameLen := len(name)
			padding := (4 - (nameLen % 4)) % 4
			entrySize := 24 + uint32(nameLen) + uint32(padding)

			// Check if adding this entry would exceed the count limit
			if estimatedSize+entrySize > count {
				// We've reached the count limit, but haven't seen all entries
				// Return what we have so far (EOF = false)
				logger.Debug("ReadDir: pagination limit reached at entry '%s' (cookie=%d, estimated=%d, count=%d)",
					name, currentCookie, estimatedSize+entrySize, count)
				return entries, false, nil
			}

			// Extract file ID from handle
			fileid := extractFileIDFromHandle(handle)

			entry := metadata.DirEntry{
				Fileid: fileid,
				Name:   name,
				Cookie: currentCookie,
			}

			// Add entry and increment size (O(1) operation)
			entries = append(entries, entry)
			estimatedSize += entrySize

			currentCookie++
		}
	}

	// ========================================================================
	// Step 4: Return results with EOF flag
	// ========================================================================

	logger.Debug("ReadDir: completed listing directory %x (entries=%d, eof=true)", dirHandle, len(entries))

	// We've returned all entries - EOF = true
	return entries, true, nil
}

// RemoveFile removes a file (not a directory) from a directory.
//
// This implements support for the REMOVE NFS procedure (RFC 1813 section 3.3.12).
// It performs complete cleanup including:
//   - Permission validation
//   - File deletion
//   - Directory entry removal
//   - Parent timestamp updates
//
// This is distinct from RMDIR which removes directories. Attempting to remove
// a directory with REMOVE will fail - this enforces proper directory handling
// and prevents accidental removal of non-empty directories.
//
// RFC 1813 Requirements:
//   - Check write permission on parent directory
//   - Verify the file exists and is not a directory
//   - Remove the directory entry
//   - Update parent directory timestamps (mtime, ctime)
//   - Return the removed file's attributes for client cache updates
//
// Parameters:
//   - ctx: Authentication context for access control
//   - parentHandle: Handle of the parent directory
//   - filename: Name of the file to remove
//
// Returns:
//   - *metadata.FileAttr: The attributes of the removed file (for response)
//   - error: Returns error if:
//   - Context is cancelled
//   - Access denied (no write permission on parent)
//   - File not found
//   - File is a directory (use RemoveDirectory instead)
//   - Parent is not a directory
//   - I/O error
func (r *MemoryRepository) RemoveFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	filename string,
) (*metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before removing file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled while removing file: %w", err)
	}

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check write permission on parent directory
	// ========================================================================

	if !hasWritePermission(ctx, parentAttr) {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on parent directory",
		}
	}

	// ========================================================================
	// Step 3: Verify file exists in directory
	// ========================================================================

	if r.children[parentKey] == nil {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("file not found: %s", filename),
		}
	}

	fileHandle, exists := r.children[parentKey][filename]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("file not found: %s", filename),
		}
	}

	// ========================================================================
	// Step 4: Get file attributes and verify it's not a directory
	// ========================================================================

	fileKey := handleToKey(fileHandle)
	fileAttr, exists := r.files[fileKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "file handle exists but attributes missing",
		}
	}

	// Don't allow removing directories with REMOVE (use RMDIR instead)
	if fileAttr.Type == metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "cannot remove directory with REMOVE (use RMDIR)",
		}
	}

	// ========================================================================
	// Step 5: Remove file from parent directory
	// ========================================================================

	delete(r.children[parentKey], filename)

	// Remove parent relationship
	delete(r.parents, fileKey)

	// ========================================================================
	// Step 6: Delete file metadata
	// ========================================================================

	delete(r.files, fileKey)

	// ========================================================================
	// Step 7: Update parent directory timestamps
	// ========================================================================

	now := time.Now()
	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	logger.Debug("RemoveFile: removed file '%s' from parent %x", filename, parentHandle)

	// Return a copy of the file attributes for the response
	// (we make a copy since we just deleted the original)
	removedFileAttr := *fileAttr

	return &removedFileAttr, nil
}

// RemoveDirectory removes an empty directory from a parent directory.
//
// This implements support for the RMDIR NFS procedure (RFC 1813 section 3.3.13).
// Unlike REMOVE which handles files, RMDIR specifically handles directory removal
// with the additional requirement that the directory must be empty.
//
// Empty Directory Check:
// A directory is considered empty if it has no children. The "." and ".." entries
// are virtual and not stored in the children map, so they don't count against
// the empty check.
//
// This separation between REMOVE and RMDIR serves two purposes:
//  1. Prevents accidental removal of non-empty directories
//  2. Provides clear error messages for incorrect operation usage
//
// RFC 1813 Requirements:
//   - Check write permission on parent directory
//   - Verify the target is a directory
//   - Verify the directory is empty
//   - Remove the directory entry from parent
//   - Update parent directory timestamps (mtime, ctime)
//   - Return appropriate errors for non-empty directories
//
// Parameters:
//   - ctx: Authentication context for access control
//   - parentHandle: Handle of the parent directory
//   - name: Name of the directory to remove
//
// Returns error if:
//   - Context is cancelled
//   - Access denied (no write permission on parent)
//   - Directory not found
//   - Target is not a directory
//   - Directory is not empty
//   - Parent is not a directory
//   - I/O error
func (r *MemoryRepository) RemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled before removing directory: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled while removing directory: %w", err)
	}

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Verify directory exists as a child of parent
	// ========================================================================

	if r.children[parentKey] == nil {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("directory not found: %s", name),
		}
	}

	dirHandle, exists := r.children[parentKey][name]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("directory not found: %s", name),
		}
	}

	// ========================================================================
	// Step 3: Verify target is actually a directory
	// ========================================================================

	dirKey := handleToKey(dirHandle)
	dirAttr, exists := r.files[dirKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "directory metadata not found",
		}
	}

	if dirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "not a directory",
		}
	}

	// ========================================================================
	// Step 4: Check if directory is empty
	// ========================================================================
	// A directory is empty if it has no children (the "." and ".." entries
	// are virtual and not stored in the children map)

	dirChildren := r.children[dirKey]
	if len(dirChildren) > 0 {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("directory not empty: contains %d entries", len(dirChildren)),
		}
	}

	// ========================================================================
	// Step 5: Check write permission on parent directory
	// ========================================================================

	if !hasWritePermission(ctx, parentAttr) {
		return &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on parent directory",
		}
	}

	// ========================================================================
	// Step 6: Remove directory entry from parent
	// ========================================================================

	delete(r.children[parentKey], name)

	// ========================================================================
	// Step 7: Delete directory metadata and children map
	// ========================================================================

	delete(r.files, dirKey)
	delete(r.children, dirKey)

	// ========================================================================
	// Step 8: Remove parent relationship
	// ========================================================================

	delete(r.parents, dirKey)

	// ========================================================================
	// Step 9: Update parent directory timestamps
	// ========================================================================
	// The parent directory's mtime and ctime should be updated when a child
	// is removed, as this modifies the directory's contents.

	now := time.Now()
	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	logger.Debug("RemoveDirectory: removed directory '%s' from parent %x", name, parentHandle)

	return nil
}

// ReadSymlink reads the target path of a symbolic link with access control.
//
// This implements support for the READLINK NFS procedure (RFC 1813 section 3.3.5).
// Symbolic links are special files that contain a path string pointing to another
// file or directory. Reading a symlink returns this path without following it.
//
// Security:
// This method checks read permission on the symlink itself. Note that:
//   - Reading a symlink requires read permission on the symlink, not the target
//   - Symlinks themselves have permissions, though they're often ignored
//   - Following the symlink (accessing the target) requires separate permissions
//
// RFC 1813 Requirements:
//   - Verify the handle refers to a symbolic link
//   - Check read permission on the symlink
//   - Return the target path string
//   - Return symlink attributes for client cache consistency
//
// Parameters:
//   - ctx: Authentication context for access control
//   - handle: File handle of the symbolic link
//
// Returns:
//   - string: The symlink target path
//   - *metadata.FileAttr: Symlink attributes (for cache consistency)
//   - error: Returns error if:
//   - Context is cancelled
//   - Handle not found
//   - Handle is not a symlink
//   - Access denied (no read permission)
//   - Target path is missing or empty
//   - I/O error
func (r *MemoryRepository) ReadSymlink(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (string, *metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return "", nil, fmt.Errorf("context cancelled before reading symlink: %w", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return "", nil, fmt.Errorf("context cancelled while reading symlink: %w", err)
	}

	// ========================================================================
	// Step 1: Get file attributes
	// ========================================================================

	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return "", nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "symbolic link not found",
		}
	}

	// ========================================================================
	// Step 2: Verify it's a symbolic link
	// ========================================================================

	if attr.Type != metadata.FileTypeSymlink {
		return "", nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "not a symbolic link",
		}
	}

	// ========================================================================
	// Step 3: Check read permission
	// ========================================================================

	if !hasReadPermission(ctx, attr) {
		return "", nil, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "read permission denied on symbolic link",
		}
	}

	// ========================================================================
	// Step 4: Get symlink target
	// ========================================================================

	if attr.SymlinkTarget == "" {
		return "", nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "symbolic link has no target",
		}
	}

	logger.Debug("ReadSymlink: read symlink %x -> '%s'", handle, attr.SymlinkTarget)

	return attr.SymlinkTarget, attr, nil
}

// RenameFile renames or moves a file from one directory to another.
//
// This implements support for the RENAME NFS procedure (RFC 1813 section 3.3.14).
// RENAME is used to change a file's name within the same directory, move a file
// to a different directory, or atomically replace an existing file.
//
// Atomicity:
// The implementation strives for atomicity by:
//  1. Validating all preconditions before making any changes
//  2. Performing the minimal set of operations to complete the rename
//  3. Rolling back on failure (though true transactional rollback is not implemented)
//
// In a production implementation, you would use proper transaction semantics
// or a write-ahead log to ensure true atomicity.
//
// Replacement Semantics:
// When the destination name already exists:
//   - File over file: Allowed (atomic replacement)
//   - Directory over empty directory: Allowed
//   - Directory over non-empty directory: Not allowed (NFS3ErrNotEmpty)
//   - File over directory: Not allowed (NFS3ErrExist)
//   - Directory over file: Not allowed (NFS3ErrExist)
//
// RFC 1813 Requirements:
//   - Check write permission on source directory (to remove entry)
//   - Check write permission on destination directory (to add entry)
//   - Verify source file/directory exists
//   - Handle atomic replacement of destination if allowed
//   - Ensure destination is not a non-empty directory
//   - Update parent relationships for cross-directory moves
//   - Update directory timestamps (mtime, ctime) for both directories
//   - Prevent renaming "." or ".." (validated by protocol layer)
//
// Special Cases:
//   - Same directory, same name: Success (no-op)
//   - Same directory, different name: Simple rename
//   - Different directory: Move with potential rename
//   - Over existing file: Replace atomically
//
// Parameters:
//   - ctx: Authentication context for access control
//   - fromDirHandle: Source directory handle
//   - fromName: Current name of the file/directory
//   - toDirHandle: Destination directory handle
//   - toName: New name for the file/directory
//
// Returns error if:
//   - Context is cancelled
//   - Source file/directory not found
//   - Source or destination directory not found
//   - Access denied (no write permission on either directory)
//   - Destination is a non-empty directory
//   - Type mismatch (file vs directory) when replacing
//   - I/O error
func (r *MemoryRepository) RenameFile(
	ctx *metadata.AuthContext,
	fromDirHandle metadata.FileHandle,
	fromName string,
	toDirHandle metadata.FileHandle,
	toName string,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled before renaming file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return fmt.Errorf("context cancelled while renaming file: %w", err)
	}

	// ========================================================================
	// Step 1: Verify source directory exists and is a directory
	// ========================================================================

	fromDirKey := handleToKey(fromDirHandle)
	fromDirAttr, exists := r.files[fromDirKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "source directory not found",
		}
	}

	if fromDirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "source is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Verify destination directory exists and is a directory
	// ========================================================================

	toDirKey := handleToKey(toDirHandle)
	toDirAttr, exists := r.files[toDirKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "destination directory not found",
		}
	}

	if toDirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "destination is not a directory",
		}
	}

	// ========================================================================
	// Step 3: Check write permission on source directory
	// ========================================================================
	// Need write permission to remove the entry from source directory

	if !hasWritePermission(ctx, fromDirAttr) {
		return &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on source directory",
		}
	}

	// ========================================================================
	// Step 4: Check write permission on destination directory
	// ========================================================================
	// Need write permission to add the entry to destination directory

	if !hasWritePermission(ctx, toDirAttr) {
		return &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on destination directory",
		}
	}

	// ========================================================================
	// Step 5: Verify source file/directory exists
	// ========================================================================

	if r.children[fromDirKey] == nil {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("source not found: %s", fromName),
		}
	}

	sourceHandle, exists := r.children[fromDirKey][fromName]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("source not found: %s", fromName),
		}
	}

	// Get source attributes to check type
	sourceKey := handleToKey(sourceHandle)
	sourceAttr, exists := r.files[sourceKey]
	if !exists {
		return &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "source handle exists but attributes missing",
		}
	}

	// ========================================================================
	// Step 6: Check if this is a no-op (same directory, same name)
	// ========================================================================

	if fromDirKey == toDirKey && fromName == toName {
		// Rename to same name in same directory - this is a no-op success
		logger.Debug("RenameFile: no-op rename (same location) for '%s'", fromName)
		return nil
	}

	// ========================================================================
	// Step 7: Check if destination already exists
	// ========================================================================

	if r.children[toDirKey] == nil {
		r.children[toDirKey] = make(map[string]metadata.FileHandle)
	}

	destHandle, destExists := r.children[toDirKey][toName]

	if destExists {
		// Destination exists - need to handle replacement

		// Get destination attributes to check type
		destKey := handleToKey(destHandle)
		destAttr, exists := r.files[destKey]
		if !exists {
			return &metadata.ExportError{
				Code:    metadata.ExportErrServerFault,
				Message: "destination handle exists but attributes missing",
			}
		}

		// ====================================================================
		// Step 7a: Validate replacement is allowed
		// ====================================================================

		// Cannot rename file over directory or directory over file
		if sourceAttr.Type == metadata.FileTypeDirectory && destAttr.Type != metadata.FileTypeDirectory {
			return &metadata.ExportError{
				Code:    metadata.ExportErrServerFault,
				Message: "cannot rename directory over file",
			}
		}

		if sourceAttr.Type != metadata.FileTypeDirectory && destAttr.Type == metadata.FileTypeDirectory {
			return &metadata.ExportError{
				Code:    metadata.ExportErrServerFault,
				Message: "cannot rename file over directory",
			}
		}

		// If renaming directory over directory, destination must be empty
		if sourceAttr.Type == metadata.FileTypeDirectory && destAttr.Type == metadata.FileTypeDirectory {
			destChildren := r.children[destKey]
			if len(destChildren) > 0 {
				return &metadata.ExportError{
					Code:    metadata.ExportErrServerFault,
					Message: fmt.Sprintf("destination directory not empty: contains %d entries", len(destChildren)),
				}
			}
		}

		// ====================================================================
		// Step 7b: Remove destination (atomic replacement)
		// ====================================================================

		logger.Debug("RenameFile: replacing existing destination '%s'", toName)

		// Remove destination from parent's children map
		delete(r.children[toDirKey], toName)

		// Delete destination metadata
		delete(r.files, destKey)

		// Delete destination's children map if it's a directory
		if destAttr.Type == metadata.FileTypeDirectory {
			delete(r.children, destKey)
		}

		// Remove parent relationship
		delete(r.parents, destKey)
	}

	// ========================================================================
	// Step 8: Perform the rename
	// ========================================================================

	// Remove source from its current parent
	delete(r.children[fromDirKey], fromName)

	// Add source to destination parent with new name
	r.children[toDirKey][toName] = sourceHandle

	// ========================================================================
	// Step 9: Update parent relationship if moving to different directory
	// ========================================================================

	if fromDirKey != toDirKey {
		r.parents[sourceKey] = toDirHandle
	}

	// ========================================================================
	// Step 10: Update timestamps
	// ========================================================================

	now := time.Now()

	// Update source file/directory change time (metadata changed)
	sourceAttr.Ctime = now
	r.files[sourceKey] = sourceAttr

	// Update source directory modification time (contents changed)
	fromDirAttr.Mtime = now
	fromDirAttr.Ctime = now
	r.files[fromDirKey] = fromDirAttr

	// Update destination directory modification time if different from source
	if fromDirKey != toDirKey {
		toDirAttr.Mtime = now
		toDirAttr.Ctime = now
		r.files[toDirKey] = toDirAttr
	}

	logger.Debug("RenameFile: renamed '%s' -> '%s' (from dir %x to dir %x)",
		fromName, toName, fromDirHandle, toDirHandle)

	return nil
}

// CreateSymlink creates a symbolic link with the specified target path.
//
// This implements support for the SYMLINK NFS procedure (RFC 1813 section 3.3.10).
// A symbolic link is a special file that contains a pathname to another file or
// directory. Unlike hard links, symlinks can span filesystems and can point to
// directories.
//
// Implementation Notes:
//   - The target path is stored in attr.SymlinkTarget without validation
//   - The symlink size is set to the length of the target path
//   - The protocol layer provides attributes with proper defaults via convertSetAttrsToMetadata
//   - Actual access control is applied when following the symlink, not on the symlink itself
//
// RFC 1813 Requirements:
//   - Check write permission on parent directory
//   - Verify name doesn't already exist
//   - Store target path without validation (dangling symlinks are allowed)
//   - Update parent directory timestamps (mtime, ctime)
//   - Set appropriate symlink attributes
//
// Parameters:
//   - ctx: Authentication context for access control
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new symbolic link
//   - target: Path that the symlink will point to
//   - attr: Partial attributes (mode, uid, gid) from protocol layer with proper defaults
//
// Returns:
//   - FileHandle: Handle of the newly created symlink
//   - error: Returns error if:
//   - Context is cancelled
//   - Access denied (no write permission on parent)
//   - Name already exists
//   - Parent is not a directory
//   - I/O error
func (r *MemoryRepository) CreateSymlink(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	target string,
	attr *metadata.FileAttr,
) (metadata.FileHandle, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before creating symlink: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled while creating symlink: %w", err)
	}

	// ========================================================================
	// Step 1: Verify parent directory exists
	// ========================================================================

	parentKey := handleToKey(parentHandle)
	parentAttr, exists := r.files[parentKey]
	if !exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "parent directory not found",
		}
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: "parent is not a directory",
		}
	}

	// ========================================================================
	// Step 2: Check write access to parent directory
	// ========================================================================

	if !hasWritePermission(ctx, parentAttr) {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied on parent directory",
		}
	}

	// ========================================================================
	// Step 3: Verify name doesn't already exist
	// ========================================================================

	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return nil, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("file already exists: %s", name),
		}
	}

	// ========================================================================
	// Step 4: Complete symlink attributes with server-assigned values
	// ========================================================================

	now := time.Now()

	// The protocol layer has already applied proper defaults via convertSetAttrsToMetadata,
	// including UID/GID from auth context. We just need to complete the remaining fields.
	completeAttr := &metadata.FileAttr{
		Type: metadata.FileTypeSymlink, // Always symlink
		Mode: attr.Mode,                // From protocol layer (default 0777 already applied)
		UID:  attr.UID,                 // From protocol layer (auth context UID or client-provided)
		GID:  attr.GID,                 // From protocol layer (auth context GID or client-provided)

		// Server-assigned attributes
		Size:          uint64(len(target)), // Size is length of target path
		Atime:         now,
		Mtime:         now,
		Ctime:         now,
		ContentID:     "",     // Symlinks don't have content blobs
		SymlinkTarget: target, // Store the target path
	}

	// ========================================================================
	// Step 5: Generate unique symlink handle
	// ========================================================================

	symlinkHandle := r.generateFileHandle(name)

	// ========================================================================
	// Step 6: Create symlink metadata
	// ========================================================================

	symlinkKey := handleToKey(symlinkHandle)
	r.files[symlinkKey] = completeAttr

	// ========================================================================
	// Step 7: Link symlink to parent
	// ========================================================================

	r.children[parentKey][name] = symlinkHandle

	// Set parent relationship
	r.parents[symlinkKey] = parentHandle

	// ========================================================================
	// Step 8: Update parent directory timestamps
	// ========================================================================
	// The parent directory's mtime and ctime should be updated when a child
	// is added, as this modifies the directory's contents.

	parentAttr.Mtime = now
	parentAttr.Ctime = now
	r.files[parentKey] = parentAttr

	logger.Debug("CreateSymlink: created symlink '%s' -> '%s' in parent %x with handle %x",
		name, target, parentHandle, symlinkHandle)

	return symlinkHandle, nil
}

// WriteFile writes data to a file with proper permission checking and metadata updates.
//
// This implements the business logic for the WRITE NFS procedure (RFC 1813 section 3.3.7).
// It handles the complete write workflow including permission validation, size management,
// and timestamp updates.
//
// Write Operation Flow:
//  1. Verify file exists and is a regular file
//  2. Check write permission on the file
//  3. Capture pre-operation attributes (WCC data)
//  4. Delegate actual data write to content repository
//  5. Update file size if writing beyond EOF
//  6. Update file timestamps (mtime always, ctime if size changed)
//  7. Return updated attributes and WCC data
//
// Permission Check:
// Write permission requires one of:
//   - Owner with write bit (mode & 0200)
//   - Group member with write bit (mode & 0020)
//   - Other with write bit (mode & 0002)
//
// File Size Management:
// When offset + len(data) > current size:
//   - File is extended to new size
//   - Bytes between old EOF and write offset are implicitly zero (sparse)
//   - This is standard Unix behavior for positional writes
//
// Timestamp Updates:
// Per POSIX and NFS semantics:
//   - mtime (modification time): Always updated on successful write
//   - ctime (change time): Updated when metadata changes (e.g., size extension)
//   - atime (access time): Not updated on write
//
// Weak Cache Consistency (WCC):
// The returned WccAttr contains size and timestamps before the operation.
// Clients use this to detect concurrent modifications by comparing with
// their cached attributes.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - handle: File handle to write to
//   - newSize: New file size after write operation
//
// Returns:
//   - *FileAttr: Updated file attributes after the write
//   - uint64: Pre-operation size (for WCC)
//   - time.Time: Pre-operation mtime (for WCC)
//   - time.Time: Pre-operation ctime (for WCC)
//   - error: Returns ExportError with specific code if operation fails:
//   - Context is cancelled
//   - ExportErrNotFound: File doesn't exist
//   - ExportErrAccessDenied: No write permission
//   - ExportErrServerFault: Not a regular file or other validation errors
//
// Example:
//
//	newAttr, preSize, preMtime, preCtime, err := repo.WriteFile(ctx, handle, 1024)
//	if err != nil {
//	    // Handle error (permission denied, file not found, etc.)
//	}
//	// File size now reflects write
//	// newAttr.Mtime reflects write time
func (r *MemoryRepository) WriteFile(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	newSize uint64,
) (*metadata.FileAttr, uint64, time.Time, time.Time, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, 0, time.Time{}, time.Time{}, fmt.Errorf("context cancelled before writing file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, 0, time.Time{}, time.Time{}, fmt.Errorf("context cancelled while writing file: %w", err)
	}

	// ========================================================================
	// Step 1: Verify file exists
	// ========================================================================

	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return nil, 0, time.Time{}, time.Time{}, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: "file not found",
		}
	}

	// ========================================================================
	// Step 2: Verify it's a regular file
	// ========================================================================

	if attr.Type != metadata.FileTypeRegular {
		return nil, 0, time.Time{}, time.Time{}, &metadata.ExportError{
			Code:    metadata.ExportErrServerFault,
			Message: fmt.Sprintf("not a regular file: type=%d", attr.Type),
		}
	}

	// ========================================================================
	// Step 3: Capture pre-operation attributes
	// ========================================================================

	preSize := attr.Size
	preMtime := attr.Mtime
	preCtime := attr.Ctime

	// ========================================================================
	// Step 4: Check write permission
	// ========================================================================

	if !hasWritePermission(ctx, attr) {
		return nil, preSize, preMtime, preCtime, &metadata.ExportError{
			Code:    metadata.ExportErrAccessDenied,
			Message: "write permission denied",
		}
	}

	// ========================================================================
	// Step 5: Ensure file has a content ID
	// ========================================================================

	if attr.ContentID == "" {
		var handlePart []byte
		if len(handle) >= 16 {
			handlePart = handle[:16]
		} else {
			handlePart = handle
		}
		attr.ContentID = content.ContentID(fmt.Sprintf("file-%x", handlePart))
	}

	// ========================================================================
	// Step 6: Update file size if needed
	// ========================================================================

	sizeChanged := false
	if newSize > attr.Size {
		attr.Size = newSize
		sizeChanged = true
	}

	// ========================================================================
	// Step 7: Update file timestamps
	// ========================================================================

	now := time.Now()
	attr.Mtime = now

	if sizeChanged {
		attr.Ctime = now
	}

	// ========================================================================
	// Step 8: Save updated attributes
	// ========================================================================

	r.files[key] = attr

	logger.Debug("WriteFile: updated file %x (oldSize=%d, newSize=%d, sizeChanged=%v)",
		handle, preSize, attr.Size, sizeChanged)

	// Return a copy of the attributes
	attrCopy := *attr

	return &attrCopy, preSize, preMtime, preCtime, nil
}
