package nfs

// ============================================================================
// NFS File Attributes
// ============================================================================

// FileAttr represents NFS file attributes (fattr3 in RFC 1813 Section 2.5.5).
// These attributes describe the metadata of a file or directory in the NFS filesystem.
//
// This structure is returned by most NFS operations to provide clients with
// up-to-date information about file state.
//
// Usage:
//   - Read operations: GETATTR, LOOKUP, READ, READDIR_PLUS
//   - Write operations: CREATE, MKDIR, SETATTR, WRITE (post-op attributes)
//   - All operations that modify files include post-op attributes
type FileAttr struct {
	// Type indicates the file type (regular, directory, symlink, etc.)
	// See FileType* constants below
	Type uint32

	// Mode contains the UNIX permission bits (e.g., 0644, 0755)
	// Format: standard UNIX file mode (rwxrwxrwx)
	Mode uint32

	// Nlink is the number of hard links to this file
	Nlink uint32

	// UID is the user ID of the file owner
	UID uint32

	// GID is the group ID of the file owner
	GID uint32

	// Size is the file size in bytes
	// For directories, this is implementation-dependent
	Size uint64

	// Used is the actual disk space used in bytes
	// May differ from Size due to sparse files or block allocation
	Used uint64

	// Rdev contains the device number for special files (block/char devices)
	// Format: major/minor device numbers
	Rdev SpecData

	// Fsid is the filesystem identifier
	// Used to identify which filesystem the file resides on
	Fsid uint64

	// Fileid is the unique file identifier within the filesystem
	// Similar to UNIX inode number
	Fileid uint64

	// Atime is the time of last access
	Atime TimeVal

	// Mtime is the time of last modification
	Mtime TimeVal

	// Ctime is the time of last attribute change
	// Updated when file data OR metadata changes
	Ctime TimeVal
}

// SpecData represents device numbers for special files (RFC 1813 Section 2.5.5).
// Used for block and character device files.
type SpecData struct {
	// Major is the major device number
	Major uint32

	// Minor is the minor device number
	Minor uint32
}

// TimeVal represents an NFS timestamp (nfstime3 in RFC 1813 Section 2.5.2).
// NFS uses seconds and nanoseconds since the UNIX epoch (Jan 1, 1970 00:00:00 UTC).
//
// Usage:
//
//   - Converting from Go time.Time:
//     tv := TimeVal{
//     Seconds:  uint32(t.Unix()),
//     Nseconds: uint32(t.Nanosecond()),
//     }
//
//   - Converting to Go time.Time:
//     t := time.Unix(int64(tv.Seconds), int64(tv.Nseconds))
type TimeVal struct {
	// Seconds is the number of seconds since UNIX epoch
	Seconds uint32

	// Nseconds is the nanoseconds component (0-999999999)
	Nseconds uint32
}

// ============================================================================
// Weak Cache Consistency (WCC) Data
// ============================================================================

// WccAttr represents pre-operation weak cache consistency attributes
// (pre_op_attr in RFC 1813 Section 2.6).
//
// WCC data helps NFS clients detect if a file changed between operations
// and maintain cache consistency. The server captures file attributes before
// an operation (pre-op) and returns them alongside the post-op attributes.
//
// Usage:
//
//   - Capture before modifying a file:
//     wccAttr := captureWccAttr(fileAttr)
//
//   - Client uses this to detect concurrent modifications
//
//   - If pre-op attributes match client's cached attributes, cache is valid
//
//   - If they differ, cache must be invalidated
//
// Example:
//
//	Client has cached attributes with mtime=T1
//	Server captures pre-op with mtime=T1 (matches)
//	Server performs operation, mtime becomes T2
//	Server returns WCC: before={mtime=T1}, after={mtime=T2}
//	Client sees pre-op matches cache, so updates cache to T2
type WccAttr struct {
	// Size is the file size in bytes before the operation
	Size uint64

	// Mtime is the modification time before the operation
	Mtime TimeVal

	// Ctime is the change time before the operation
	Ctime TimeVal
}

// ============================================================================
// Directory Entry Structures
// ============================================================================

// DirEntry represents a directory entry returned by READDIR.
// This is the basic directory listing without attributes.
type DirEntry struct {
	// Fileid is the unique file identifier
	Fileid uint64

	// Name is the filename
	Name string

	// Cookie is an opaque value used for resuming directory reads
	// The client passes this back in subsequent READDIR calls
	Cookie uint64
}

// DirEntryPlus represents a directory entry with attributes returned by READDIRPLUS.
// This includes full file attributes and file handle for each entry.
type DirEntryPlus struct {
	// Fileid is the unique file identifier
	Fileid uint64

	// Name is the filename
	Name string

	// Cookie is an opaque value used for resuming directory reads
	Cookie uint64

	// Attr contains the file attributes (may be nil)
	Attr *FileAttr

	// Handle is the file handle (may be nil)
	Handle []byte
}

// FSStat contains dynamic filesystem statistics (returned by FSSTAT).
// This describes the current state and capacity of the filesystem.
type FSStat struct {
	// TotalBytes is the total size of the filesystem in bytes
	TotalBytes uint64

	// FreeBytes is the free space available in bytes
	FreeBytes uint64

	// AvailBytes is the free space available to non-privileged users
	// May be less than FreeBytes if space is reserved for root
	AvailBytes uint64

	// TotalFiles is the total number of file slots (inodes)
	TotalFiles uint64

	// FreeFiles is the number of free file slots
	FreeFiles uint64

	// AvailFiles is the free file slots available to non-privileged users
	AvailFiles uint64

	// Invarsec is the number of seconds for which the filesystem is not
	// expected to change. A value of 0 means the filesystem is expected to
	// change at any time.
	Invarsec uint32
}

// TimeGuard is used for conditional updates based on ctime.
// If Check is true and the server's current ctime doesn't match Time,
// the operation fails with NFS3ErrNotSync.
//
// This implements optimistic concurrency control to prevent lost updates
// when multiple clients modify the same file concurrently.
type TimeGuard struct {
	// Check indicates whether to perform guard checking.
	// If false, the update proceeds unconditionally.
	// If true, the update only proceeds if the file's current ctime
	// matches the Time field.
	Check bool

	// Time is the expected ctime value (change time).
	// Only used when Check is true.
	// Format: seconds since Unix epoch + nanoseconds
	Time TimeVal
}
