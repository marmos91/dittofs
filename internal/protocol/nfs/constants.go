package nfs

// NFSv3 Procedure Numbers
// These identify the different NFS operations as defined in RFC 1813.
const (
	// NFSProcNull - Do nothing (connectivity test)
	NFSProcNull = 0

	// NFSProcGetAttr - Get file attributes
	NFSProcGetAttr = 1

	// NFSProcSetAttr - Set file attributes
	NFSProcSetAttr = 2

	// NFSProcLookup - Lookup filename
	NFSProcLookup = 3

	// NFSProcAccess - Check access permission
	NFSProcAccess = 4

	// NFSProcReadLink - Read symbolic link
	NFSProcReadLink = 5

	// NFSProcRead - Read from file
	NFSProcRead = 6

	// NFSProcWrite - Write to file
	NFSProcWrite = 7

	// NFSProcCreate - Create a file
	NFSProcCreate = 8

	// NFSProcMkdir - Create a directory
	NFSProcMkdir = 9

	// NFSProcSymlink - Create a symbolic link
	NFSProcSymlink = 10

	// NFSProcMknod - Create a special device
	NFSProcMknod = 11

	// NFSProcRemove - Remove a file
	NFSProcRemove = 12

	// NFSProcRmdir - Remove a directory
	NFSProcRmdir = 13

	// NFSProcRename - Rename a file or directory
	NFSProcRename = 14

	// NFSProcLink - Create a hard link
	NFSProcLink = 15

	// NFSProcReadDir - Read directory entries
	NFSProcReadDir = 16

	// NFSProcReadDirPlus - Extended read directory (with attributes)
	NFSProcReadDirPlus = 17

	// NFSProcFsStat - Get dynamic file system information
	NFSProcFsStat = 18

	// NFSProcFsInfo - Get static file system information
	NFSProcFsInfo = 19

	// NFSProcPathConf - Get POSIX information
	NFSProcPathConf = 20

	// NFSProcCommit - Commit cached data to stable storage
	NFSProcCommit = 21
)

// NFS Status Codes
// These are the error codes that can be returned by NFSv3 procedures.
// Defined in RFC 1813 Section 3.3.
const (
	// NFS3OK - Success
	NFS3OK = 0

	// NFS3ErrPerm - Not owner
	NFS3ErrPerm = 1

	// NFS3ErrNoEnt - No such file or directory
	NFS3ErrNoEnt = 2

	// NFS3ErrIO - I/O error
	NFS3ErrIO = 5

	// NFS3ErrAcces - Permission denied
	NFS3ErrAcces = 13

	// NFS3ErrExist - File exists
	NFS3ErrExist = 17

	// NFS3ErrNotDir - Not a directory
	NFS3ErrNotDir = 20

	// NFS3ErrIsDir - Is a directory
	NFS3ErrIsDir = 21

	// NFS3ErrInval - Invalid argument
	NFS3ErrInval = 22

	// NFS3ErrFBig - File too large
	NFS3ErrFBig = 27

	// NFS3ErrNoSpc - No space left on device
	NFS3ErrNoSpc = 28

	// NFS3ErrRofs - Read-only file system
	NFS3ErrRofs = 30

	// NFS3ErrNameTooLong - Filename too long
	NFS3ErrNameTooLong = 63

	// NFS3ErrNotEmpty - Directory not empty
	NFS3ErrNotEmpty = 66

	// NFS3ErrStale - Stale file handle
	NFS3ErrStale = 70

	// NFS3ErrBadHandle - Illegal NFS file handle
	NFS3ErrBadHandle = 10001

	// NFS3ErrNotSync - Update synchronization mismatch
	NFS3ErrNotSync = 10002

	// NFS3ErrNotSupp - Operation not supported
	NFS3ErrNotSupp = 10004
)

// FSInfo property flags (RFC 1813 Section 3.3.19)
const (
	FSFLink        = 0x0001 // Hard links supported
	FSFSymlink     = 0x0002 // Symbolic links supported
	FSFHomogeneous = 0x0008 // PATHCONF valid for all files
	FSFCanSetTime  = 0x0010 // Server can set times
)

// File type constants as defined in RFC 1813 Section 2.5.5.
// These values are used in FileAttr.Type to indicate the type of filesystem object.
const (
	// FileTypeRegular indicates a regular file
	FileTypeRegular = 1

	// FileTypeDirectory indicates a directory
	FileTypeDirectory = 2

	// FileTypeBlock indicates a block special device file
	FileTypeBlock = 3

	// FileTypeChar indicates a character special device file
	FileTypeChar = 4

	// FileTypeSymlink indicates a symbolic link
	FileTypeSymlink = 5

	// FileTypeSocket indicates a socket
	FileTypeSocket = 6

	// FileTypeFifo indicates a named pipe (FIFO)
	FileTypeFifo = 7
)

// ============================================================================
// Access Rights
// ============================================================================

// Access rights bits used in ACCESS procedure (RFC 1813 Section 3.3.4).
// These can be combined with bitwise OR to check multiple permissions.
//
// Usage:
//
//   - Check read and execute:
//     rights := AccessRead | AccessExecute
//
//   - Check all permissions:
//     rights := AccessRead | AccessLookup | AccessModify | AccessExtend | AccessDelete | AccessExecute
const (
	// AccessRead indicates permission to read file data or list directory
	AccessRead = 0x0001

	// AccessLookup indicates permission to look up names in a directory
	AccessLookup = 0x0002

	// AccessModify indicates permission to modify file data
	AccessModify = 0x0004

	// AccessExtend indicates permission to extend a file (write beyond current EOF)
	AccessExtend = 0x0008

	// AccessDelete indicates permission to delete a file or directory
	AccessDelete = 0x0010

	// AccessExecute indicates permission to execute a file or search a directory
	AccessExecute = 0x0020
)

// ============================================================================
// Write Stability
// ============================================================================

// Write stability modes (RFC 1813 Section 3.3.7).
// These indicate how committed the data should be after a WRITE operation.
const (
	// WriteUnstable means data can be cached in memory (fastest)
	// Data may be lost if server crashes before COMMIT
	WriteUnstable = 0

	// WriteDataSync means data is committed to disk before returning
	// But metadata (like mtime) may still be cached
	WriteDataSync = 1

	// WriteFileSync means both data and metadata are committed to disk
	// Slowest but safest
	WriteFileSync = 2
)

// ============================================================================
// Create Modes
// ============================================================================

// Create modes used in CREATE procedure (RFC 1813 Section 3.3.8).
const (
	// CreateUnchecked creates a file or truncates if it already exists.
	// This is the default mode for most file creation operations.
	CreateUnchecked = 0

	// CreateGuarded creates a file only if it doesn't exist.
	// Returns NFS3ErrExist if the file already exists.
	CreateGuarded = 1

	// CreateExclusive creates a file exclusively using a verifier.
	// The verifier ensures idempotent file creation across retries.
	// If the file exists and the verifier matches, the operation succeeds (idempotent).
	CreateExclusive = 2
)

// ============================================================================
// Special File Type Constants (for MKNOD)
// ============================================================================

// Device types for MKNOD (RFC 1813 Section 3.3.11)
// These identify the type of special file to create.
const (
	// NF3REG indicates a regular file (invalid for MKNOD - use CREATE instead)
	NF3REG = 1

	// NF3DIR indicates a directory (invalid for MKNOD - use MKDIR instead)
	NF3DIR = 2

	// NF3BLK indicates a block special device file
	// Block devices perform I/O in fixed-size blocks (e.g., disks, partitions)
	// Examples: /dev/sda, /dev/loop0
	NF3BLK = 3

	// NF3CHR indicates a character special device file
	// Character devices perform I/O one byte at a time (e.g., terminals, serial ports)
	// Examples: /dev/tty, /dev/null, /dev/random
	NF3CHR = 4

	// NF3LNK indicates a symbolic link (invalid for MKNOD - use SYMLINK instead)
	NF3LNK = 5

	// NF3SOCK indicates a Unix domain socket
	// Sockets are used for inter-process communication
	// Examples: /var/run/docker.sock, /tmp/X11-unix/X0
	NF3SOCK = 6

	// NF3FIFO indicates a named pipe (FIFO - First In, First Out)
	// FIFOs are used for inter-process communication with streaming data
	// Examples: /tmp/mypipe
	NF3FIFO = 7
)
