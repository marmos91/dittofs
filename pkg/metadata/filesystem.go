package metadata

import "time"

// FilesystemCapabilities contains static filesystem capabilities and limits.
//
// This information is relatively stable (changes only on configuration updates)
// and describes what the filesystem supports and its limits.
type FilesystemCapabilities struct {
	// Transfer Sizes
	// These guide clients on optimal I/O sizes

	// MaxReadSize is the maximum size in bytes of a single read operation
	MaxReadSize uint32

	// PreferredReadSize is the preferred size in bytes for read operations
	// Optimal performance is typically achieved using this size
	PreferredReadSize uint32

	// MaxWriteSize is the maximum size in bytes of a single write operation
	MaxWriteSize uint32

	// PreferredWriteSize is the preferred size in bytes for write operations
	// Optimal performance is typically achieved using this size
	PreferredWriteSize uint32

	// Limits
	// These define hard limits on filesystem objects

	// MaxFileSize is the maximum file size in bytes supported
	// Example: 2^63-1 for practically unlimited
	MaxFileSize uint64

	// MaxFilenameLen is the maximum filename length in bytes
	// Typical value: 255 bytes (common Unix limit)
	MaxFilenameLen uint32

	// MaxPathLen is the maximum full path length in bytes
	// Typical value: 4096 bytes (common Unix limit)
	MaxPathLen uint32

	// MaxHardLinkCount is the maximum number of hard links to a file
	// Typical values: 32767 (ext4), 65000 (XFS)
	MaxHardLinkCount uint32

	// Features
	// These indicate what features the filesystem supports

	// SupportsHardLinks indicates whether hard links are supported
	SupportsHardLinks bool

	// SupportsSymlinks indicates whether symbolic links are supported
	SupportsSymlinks bool

	// CaseSensitive indicates whether filenames are case-sensitive
	// true: "File.txt" and "file.txt" are different (Unix)
	// false: "File.txt" and "file.txt" are the same (Windows)
	CaseSensitive bool

	// CasePreserving indicates whether filename case is preserved
	// true: Filename maintains its original case
	// false: Case information may be lost
	CasePreserving bool

	// ChownRestricted indicates if chown is restricted to the superuser
	// true: Only root/superuser can change file ownership (POSIX-compliant)
	// false: File owner can give away ownership
	// Most POSIX-compliant systems set this to true for security
	ChownRestricted bool

	// SupportsACLs indicates whether the filesystem supports Access Control Lists
	// beyond basic Unix permissions
	SupportsACLs bool

	// SupportsExtendedAttrs indicates whether extended attributes (xattrs) are supported
	SupportsExtendedAttrs bool

	// TruncatesLongNames indicates how the filesystem handles long filenames
	// true: Names longer than MaxFilenameLen are rejected with an error
	// false: Names are silently truncated to MaxFilenameLen
	TruncatesLongNames bool

	// Time Resolution
	// Indicates the granularity of timestamps

	// TimestampResolution is the resolution of file timestamps
	// Examples:
	//   - 1 nanosecond (high-resolution filesystems)
	//   - 1 microsecond (many modern filesystems)
	//   - 1 second (older filesystems)
	TimestampResolution time.Duration
}

// FilesystemStatistics contains dynamic filesystem usage statistics.
//
// This information changes frequently as files are created, modified, and deleted.
// It provides current state information about available space and capacity.
type FilesystemStatistics struct {
	// Space Statistics (in bytes)

	// TotalBytes is the total size of the filesystem in bytes
	TotalBytes uint64

	// UsedBytes is the amount of space currently in use
	UsedBytes uint64

	// AvailableBytes is the space available to the user
	// May be less than (TotalBytes - UsedBytes) if space is reserved for root
	// or if quotas are in effect
	AvailableBytes uint64

	// File Statistics (inodes)

	// TotalFiles is the total number of file slots (inodes) in the filesystem
	TotalFiles uint64

	// UsedFiles is the number of file slots currently in use
	UsedFiles uint64

	// AvailableFiles is the number of file slots available to the user
	// May be less than (TotalFiles - UsedFiles) if reserved for root
	AvailableFiles uint64

	// Stability

	// ValidFor indicates how long these statistics remain valid
	// 0 = statistics may change at any time (real-time)
	// >0 = statistics are stable for at least this duration
	ValidFor time.Duration
}
