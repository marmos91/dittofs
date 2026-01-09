package metadata

import (
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// File Operations (MetadataService methods)
// ============================================================================

// RemoveFile removes a file from its parent directory.
//
// This handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Sticky bit enforcement
//   - Hard link management (decrement or set nlink=0)
//   - Parent timestamp updates
//
// Important: This method does NOT delete the file's content data.
// The returned File includes ContentID for caller to coordinate content deletion.
// ContentID is empty if other hard links still reference the content.
//
// POSIX Compliance:
//   - When last link is removed, nlink is set to 0 (not deleted)
//   - This allows fstat() on open file descriptors to return nlink=0
func (s *MetadataService) RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
		return nil, err
	}

	// Get child handle
	fileHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's not a directory
	if file.Type == FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
			Path:    name,
		}
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &file.FileAttr); err != nil {
		return nil, err
	}

	// Get current link count
	linkCount, err := store.GetLinkCount(ctx.Context, fileHandle)
	if err != nil {
		// If we can't get link count, assume 1
		linkCount = 1
	}

	now := time.Now()

	// Prepare return value
	returnFile := &File{
		ID:        file.ID,
		ShareName: file.ShareName,
		Path:      file.Path,
		FileAttr:  file.FileAttr,
	}

	// Handle link count
	if linkCount > 1 {
		// File has other hard links, just decrement count
		// Empty ContentID signals caller NOT to delete content
		returnFile.ContentID = ""
		returnFile.Nlink = linkCount - 1
		returnFile.Ctime = now

		// Update file's link count and ctime
		if err := store.SetLinkCount(ctx.Context, fileHandle, linkCount-1); err != nil {
			return nil, err
		}

		// Update file's ctime
		file.Ctime = now
		if err := store.PutFile(ctx.Context, file); err != nil {
			return nil, err
		}
	} else {
		// Last link - set nlink=0 but keep metadata for POSIX compliance
		returnFile.Nlink = 0
		returnFile.Ctime = now

		// Set link count to 0
		if err := store.SetLinkCount(ctx.Context, fileHandle, 0); err != nil {
			return nil, err
		}

		// Update file's ctime and nlink
		file.Ctime = now
		file.Nlink = 0
		if err := store.PutFile(ctx.Context, file); err != nil {
			return nil, err
		}
	}

	// Remove from parent's children
	if err := store.DeleteChild(ctx.Context, parentHandle, name); err != nil {
		return nil, err
	}

	// Update parent timestamps
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutFile(ctx.Context, parent); err != nil {
		return nil, err
	}

	return returnFile, nil
}

// Lookup resolves a name within a directory to a file handle and attributes.
//
// This handles:
//   - Special names: "." (current dir), ".." (parent dir)
//   - Permission checking (execute on directory for search)
//   - Name resolution in directory
func (s *MetadataService) Lookup(ctx *AuthContext, dirHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}

	// Get directory entry
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

	// Check execute/search permission on directory
	if err := s.checkExecutePermission(ctx, dirHandle); err != nil {
		return nil, err
	}

	// Handle special names
	if name == "." {
		return dir, nil
	}

	if name == ".." {
		parentHandle, err := store.GetParent(ctx.Context, dirHandle)
		if err != nil {
			// No parent means this is root, return self
			return dir, nil
		}
		return store.GetFile(ctx.Context, parentHandle)
	}

	// Regular name lookup
	childHandle, err := store.GetChild(ctx.Context, dirHandle, name)
	if err != nil {
		return nil, err
	}

	return store.GetFile(ctx.Context, childHandle)
}

// CreateFile creates a new regular file in a directory.
func (s *MetadataService) CreateFile(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error) {
	return s.createEntry(ctx, parentHandle, name, attr, FileTypeRegular, "", 0, 0)
}

// CreateSymlink creates a new symbolic link in a directory.
func (s *MetadataService) CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (*File, error) {
	// Validate symlink target
	if err := ValidateSymlinkTarget(target); err != nil {
		return nil, err
	}

	return s.createEntry(ctx, parentHandle, name, attr, FileTypeSymlink, target, 0, 0)
}

// CreateSpecialFile creates a special file (device, socket, or FIFO).
func (s *MetadataService) CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, deviceMajor, deviceMinor uint32) (*File, error) {
	// Validate special file type
	if err := ValidateSpecialFileType(fileType); err != nil {
		return nil, err
	}

	// Check if user is root (required for device files)
	if fileType == FileTypeBlockDevice || fileType == FileTypeCharDevice {
		if err := RequiresRoot(ctx); err != nil {
			return nil, err
		}
	}

	return s.createEntry(ctx, parentHandle, name, attr, fileType, "", deviceMajor, deviceMinor)
}

// CreateHardLink creates a hard link to an existing file.
func (s *MetadataService) CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) error {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return err
	}
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Check write permission on directory
	if err := s.checkWritePermission(ctx, dirHandle); err != nil {
		return err
	}

	// Get target file
	target, err := store.GetFile(ctx.Context, targetHandle)
	if err != nil {
		return err
	}

	// Cannot hard link directories
	if target.Type == FileTypeDirectory {
		return &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot create hard link to directory",
		}
	}

	// Check if name already exists
	_, err = store.GetChild(ctx.Context, dirHandle, name)
	if err == nil {
		return &StoreError{
			Code:    ErrAlreadyExists,
			Message: "file already exists",
			Path:    name,
		}
	}
	if !IsNotFoundError(err) {
		return err
	}

	// Add to directory's children
	if err := store.SetChild(ctx.Context, dirHandle, name, targetHandle); err != nil {
		return err
	}

	// Increment target's link count
	linkCount, _ := store.GetLinkCount(ctx.Context, targetHandle)
	if err := store.SetLinkCount(ctx.Context, targetHandle, linkCount+1); err != nil {
		// Cleanup
		_ = store.DeleteChild(ctx.Context, dirHandle, name)
		return err
	}

	// Update timestamps
	now := time.Now()
	target.Ctime = now
	_ = store.PutFile(ctx.Context, target)

	dir.Mtime = now
	dir.Ctime = now
	_ = store.PutFile(ctx.Context, dir)

	return nil
}

// ReadSymlink reads the target path of a symbolic link.
func (s *MetadataService) ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return "", nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return "", nil, err
	}

	// Verify it's a symlink
	if file.Type != FileTypeSymlink {
		return "", nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "not a symbolic link",
			Path:    file.Path,
		}
	}

	return file.LinkTarget, file, nil
}

// SetFileAttributes updates file attributes with validation and access control.
//
// Only attributes with non-nil pointers in attrs are modified.
func (s *MetadataService) SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	// Check permissions: only owner or root can change attributes
	identity := ctx.Identity
	isOwner := identity != nil && identity.UID != nil && *identity.UID == file.UID
	isRoot := identity != nil && identity.UID != nil && *identity.UID == 0

	if !isOwner && !isRoot {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: "permission denied",
			Path:    file.Path,
		}
	}

	now := time.Now()
	modified := false

	// Apply requested changes
	if attrs.Mode != nil {
		file.Mode = *attrs.Mode
		modified = true
	}

	if attrs.UID != nil {
		// Only root can change owner
		if !isRoot {
			return &StoreError{
				Code:    ErrAccessDenied,
				Message: "only root can change owner",
				Path:    file.Path,
			}
		}
		file.UID = *attrs.UID
		modified = true
	}

	if attrs.GID != nil {
		// Root can change to any group
		// Owner can change to their own supplementary groups
		if !isRoot {
			// Check if user is member of target group
			canChangeGID := false
			if identity.GID != nil && *identity.GID == *attrs.GID {
				canChangeGID = true
			}
			if !canChangeGID && identity.HasGID(*attrs.GID) {
				canChangeGID = true
			}
			if !canChangeGID {
				return &StoreError{
					Code:    ErrAccessDenied,
					Message: "not a member of target group",
					Path:    file.Path,
				}
			}
		}
		file.GID = *attrs.GID
		modified = true
	}

	if attrs.Size != nil {
		// Size change requires write permission
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return err
		}
		file.Size = *attrs.Size
		modified = true
	}

	if attrs.Atime != nil {
		file.Atime = *attrs.Atime
		modified = true
	}

	if attrs.Mtime != nil {
		file.Mtime = *attrs.Mtime
		modified = true
	}

	// Always update ctime when attributes change
	if modified {
		file.Ctime = now
		return store.PutFile(ctx.Context, file)
	}

	return nil
}

// Move moves or renames a file or directory atomically.
func (s *MetadataService) Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error {
	store, err := s.storeForHandle(fromDir)
	if err != nil {
		return err
	}

	// Validate names
	if err := ValidateName(fromName); err != nil {
		return err
	}
	if err := ValidateName(toName); err != nil {
		return err
	}

	// Same directory and same name - no-op (POSIX rename semantics)
	if string(fromDir) == string(toDir) && fromName == toName {
		return nil
	}

	// Get source directory
	srcDir, err := store.GetFile(ctx.Context, fromDir)
	if err != nil {
		return err
	}
	if srcDir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "source parent is not a directory",
		}
	}

	// Get destination directory
	dstDir, err := store.GetFile(ctx.Context, toDir)
	if err != nil {
		return err
	}
	if dstDir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "destination parent is not a directory",
		}
	}

	// Check write permission on both directories
	if err := s.checkWritePermission(ctx, fromDir); err != nil {
		return err
	}
	if err := s.checkWritePermission(ctx, toDir); err != nil {
		return err
	}

	// Get source file
	srcHandle, err := store.GetChild(ctx.Context, fromDir, fromName)
	if err != nil {
		return err
	}
	srcFile, err := store.GetFile(ctx.Context, srcHandle)
	if err != nil {
		return err
	}

	// Check sticky bit on source directory
	if err := CheckStickyBitRestriction(ctx, &srcDir.FileAttr, &srcFile.FileAttr); err != nil {
		return err
	}

	// Check if destination exists
	dstHandle, err := store.GetChild(ctx.Context, toDir, toName)
	if err == nil {
		// Destination exists - check compatibility
		dstFile, err := store.GetFile(ctx.Context, dstHandle)
		if err != nil {
			return err
		}

		// Check sticky bit on destination directory
		if err := CheckStickyBitRestriction(ctx, &dstDir.FileAttr, &dstFile.FileAttr); err != nil {
			return err
		}

		// Type compatibility checks
		if srcFile.Type == FileTypeDirectory {
			if dstFile.Type != FileTypeDirectory {
				return &StoreError{
					Code:    ErrNotDirectory,
					Message: "cannot overwrite non-directory with directory",
				}
			}
			// Check if destination directory is empty
			entries, _, err := store.ListChildren(ctx.Context, dstHandle, "", 1)
			if err == nil && len(entries) > 0 {
				return &StoreError{
					Code:    ErrNotEmpty,
					Message: "destination directory not empty",
				}
			}
		} else {
			if dstFile.Type == FileTypeDirectory {
				return &StoreError{
					Code:    ErrIsDirectory,
					Message: "cannot overwrite directory with non-directory",
				}
			}
		}

		// Remove destination
		if dstFile.Type == FileTypeDirectory {
			if err := store.DeleteFile(ctx.Context, dstHandle); err != nil {
				return err
			}
		} else {
			// For files, decrement link count or set to 0
			linkCount, _ := store.GetLinkCount(ctx.Context, dstHandle)
			if linkCount <= 1 {
				_ = store.SetLinkCount(ctx.Context, dstHandle, 0)
			} else {
				_ = store.SetLinkCount(ctx.Context, dstHandle, linkCount-1)
			}
		}

		// Remove destination from children
		if err := store.DeleteChild(ctx.Context, toDir, toName); err != nil {
			return err
		}
	} else if !IsNotFoundError(err) {
		return err
	}

	// Remove source from old parent
	if err := store.DeleteChild(ctx.Context, fromDir, fromName); err != nil {
		return err
	}

	// Add source to new parent
	if err := store.SetChild(ctx.Context, toDir, toName, srcHandle); err != nil {
		return err
	}

	// Update parent reference if directories are different
	if string(fromDir) != string(toDir) {
		if err := store.SetParent(ctx.Context, srcHandle, toDir); err != nil {
			// Non-fatal
		}

		// Update link counts for directory moves
		if srcFile.Type == FileTypeDirectory {
			// Decrement source parent's link count
			srcLinkCount, _ := store.GetLinkCount(ctx.Context, fromDir)
			if srcLinkCount > 0 {
				_ = store.SetLinkCount(ctx.Context, fromDir, srcLinkCount-1)
			}
			// Increment destination parent's link count
			dstLinkCount, _ := store.GetLinkCount(ctx.Context, toDir)
			_ = store.SetLinkCount(ctx.Context, toDir, dstLinkCount+1)
		}
	}

	// Update timestamps
	now := time.Now()
	srcFile.Ctime = now
	if err := store.PutFile(ctx.Context, srcFile); err != nil {
		// Non-fatal
	}

	srcDir.Mtime = now
	srcDir.Ctime = now
	if err := store.PutFile(ctx.Context, srcDir); err != nil {
		// Non-fatal
	}

	if string(fromDir) != string(toDir) {
		dstDir.Mtime = now
		dstDir.Ctime = now
		if err := store.PutFile(ctx.Context, dstDir); err != nil {
			// Non-fatal
		}
	}

	return nil
}

// MarkFileAsOrphaned sets a file's link count to 0, marking it as orphaned.
//
// This is used by NFS handlers for "silly rename" behavior.
func (s *MetadataService) MarkFileAsOrphaned(ctx *AuthContext, handle FileHandle) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	// Only mark regular files as orphaned (directories don't have silly rename)
	if file.Type == FileTypeDirectory {
		return nil
	}

	// Set link count to 0
	if err := store.SetLinkCount(ctx.Context, handle, 0); err != nil {
		return err
	}

	// Update file's nlink and ctime
	now := time.Now()
	file.Nlink = 0
	file.Ctime = now
	return store.PutFile(ctx.Context, file)
}

// createEntry is the internal implementation for creating files, directories, symlinks, and special files.
func (s *MetadataService) createEntry(
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
	fileType FileType,
	linkTarget string,
	deviceMajor, deviceMinor uint32,
) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
		return nil, err
	}

	// Check if name already exists
	_, err = store.GetChild(ctx.Context, parentHandle, name)
	if err == nil {
		return nil, &StoreError{
			Code:    ErrAlreadyExists,
			Message: "file already exists",
			Path:    name,
		}
	}
	// If error is not ErrNotFound, it's a real error
	if !IsNotFoundError(err) {
		return nil, err
	}

	// Generate new handle
	newHandle, err := store.GenerateHandle(ctx.Context, parent.ShareName, buildPath(parent.Path, name))
	if err != nil {
		return nil, err
	}

	// Decode handle to get ID
	_, id, err := DecodeFileHandle(newHandle)
	if err != nil {
		return nil, err
	}

	// Prepare attributes
	newAttr := *attr
	newAttr.Type = fileType
	newAttr.LinkTarget = linkTarget
	ApplyCreateDefaults(&newAttr, ctx, linkTarget)
	ApplyOwnerDefaults(&newAttr, ctx)

	// Set content ID for regular files
	if fileType == FileTypeRegular {
		newAttr.ContentID = ContentID(buildContentID(parent.ShareName, buildPath(parent.Path, name)))
	}

	// Set device numbers for block/char devices
	if fileType == FileTypeBlockDevice || fileType == FileTypeCharDevice {
		newAttr.Rdev = MakeRdev(deviceMajor, deviceMinor)
	}

	// Create the file entry
	newFile := &File{
		ID:        id,
		ShareName: parent.ShareName,
		Path:      buildPath(parent.Path, name),
		FileAttr:  newAttr,
	}
	newFile.Nlink = GetInitialLinkCount(fileType)

	// Store the entry
	if err := store.PutFile(ctx.Context, newFile); err != nil {
		return nil, err
	}

	// Set parent reference
	if err := store.SetParent(ctx.Context, newHandle, parentHandle); err != nil {
		// Cleanup on failure
		_ = store.DeleteFile(ctx.Context, newHandle)
		return nil, err
	}

	// Add to parent's children
	if err := store.SetChild(ctx.Context, parentHandle, name, newHandle); err != nil {
		// Cleanup on failure
		_ = store.DeleteFile(ctx.Context, newHandle)
		return nil, err
	}

	// For directories, increment parent's link count (new ".." reference)
	if fileType == FileTypeDirectory {
		parentLinkCount, err := store.GetLinkCount(ctx.Context, parentHandle)
		if err == nil {
			_ = store.SetLinkCount(ctx.Context, parentHandle, parentLinkCount+1)
		}
	}

	// Update parent timestamps
	now := time.Now()
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutFile(ctx.Context, parent); err != nil {
		// Non-fatal, file was created
	}

	return newFile, nil
}

// buildPath constructs a full path from parent path and child name.
func buildPath(parentPath, childName string) string {
	if parentPath == "/" {
		return "/" + childName
	}
	return parentPath + "/" + childName
}

// buildContentID constructs a content ID from share name and path.
func buildContentID(shareName, path string) string {
	// Remove leading "/" from path and combine with share name
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(shareName) > 0 && shareName[0] == '/' {
		shareName = shareName[1:]
	}
	return shareName + "/" + path
}

// File represents a file's complete identity and attributes.
type File struct {
	// ID is a unique identifier for this file.
	ID uuid.UUID `json:"id"`

	// ShareName is the share this file belongs to (e.g., "/export").
	ShareName string `json:"share_name"`

	// Path is the full path within the share (e.g., "/documents/report.pdf").
	Path string `json:"path"`

	// FileAttr is embedded for convenient access to attributes.
	FileAttr
}

// FileAttr contains the complete metadata for a file or directory.
type FileAttr struct {
	// Type is the file type (regular, directory, symlink, etc.)
	Type FileType `json:"type"`

	// Mode contains permission bits (0o7777 max)
	Mode uint32 `json:"mode"`

	// UID is the owner user ID
	UID uint32 `json:"uid"`

	// GID is the owner group ID
	GID uint32 `json:"gid"`

	// Nlink is the number of hard links referencing this file.
	Nlink uint32 `json:"nlink"`

	// Size is the file size in bytes
	Size uint64 `json:"size"`

	// Atime is the last access time
	Atime time.Time `json:"atime"`

	// Mtime is the last modification time (content changes)
	Mtime time.Time `json:"mtime"`

	// Ctime is the last change time (metadata changes)
	Ctime time.Time `json:"ctime"`

	// CreationTime is the file creation time (birth time).
	CreationTime time.Time `json:"creation_time"`

	// ContentID is the identifier for retrieving file content
	ContentID ContentID `json:"content_id"`

	// LinkTarget is the target path for symbolic links
	LinkTarget string `json:"link_target,omitempty"`

	// Rdev contains device major and minor numbers for device files.
	Rdev uint64 `json:"rdev,omitempty"`

	// Hidden indicates if the file should be hidden from directory listings.
	Hidden bool `json:"hidden,omitempty"`

	// IdempotencyToken for detecting duplicate creation requests.
	IdempotencyToken uint64 `json:"idempotency_token,omitempty"`
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
type SetAttrs struct {
	Mode         *uint32
	UID          *uint32
	GID          *uint32
	Size         *uint64
	Atime        *time.Time
	Mtime        *time.Time
	AtimeNow     bool
	MtimeNow     bool
	CreationTime *time.Time
	Hidden       *bool
}

// FileType represents the type of a filesystem object.
type FileType int

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
	FileTypeBlockDevice
	FileTypeCharDevice
	FileTypeSocket
	FileTypeFIFO
)

// ContentID is an identifier for retrieving file content from the content repository.
type ContentID string

// ============================================================================
// Device Number Helpers
// ============================================================================

// MakeRdev encodes major and minor device numbers into a single Rdev value.
func MakeRdev(major, minor uint32) uint64 {
	return (uint64(major) << 20) | uint64(minor&0xFFFFF)
}

// RdevMajor extracts the major device number from an Rdev value.
func RdevMajor(rdev uint64) uint32 {
	return uint32(rdev >> 20)
}

// RdevMinor extracts the minor device number from an Rdev value.
func RdevMinor(rdev uint64) uint32 {
	return uint32(rdev & 0xFFFFF)
}

// GetInitialLinkCount returns the initial link count for a new file.
func GetInitialLinkCount(fileType FileType) uint32 {
	if fileType == FileTypeDirectory {
		return 2 // . and parent entry
	}
	return 1
}
