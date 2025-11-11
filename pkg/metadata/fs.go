package metadata

// FSStat contains dynamic filesystem statistics.
// This structure is returned by the repository to inform NFS clients
// about current filesystem capacity and usage.
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

// FSInfo contains static filesystem information and capabilities.
// This structure is returned by the repository to inform NFS clients
// about server limits, preferences, and supported features.
type FSInfo struct {
	// RtMax is the maximum size in bytes of a READ request
	// Clients should not exceed this value in READ operations
	RtMax uint32

	// RtPref is the preferred size in bytes of a READ request
	// Optimal performance is achieved using this size
	RtPref uint32

	// RtMult is the suggested multiple for READ request sizes
	// READ sizes should ideally be multiples of this value
	RtMult uint32

	// WtMax is the maximum size in bytes of a WRITE request
	// Clients should not exceed this value in WRITE operations
	WtMax uint32

	// WtPref is the preferred size in bytes of a WRITE request
	// Optimal performance is achieved using this size
	WtPref uint32

	// WtMult is the suggested multiple for WRITE request sizes
	// WRITE sizes should ideally be multiples of this value
	WtMult uint32

	// DtPref is the preferred size in bytes of a READDIR request
	// Optimal performance is achieved using this size
	DtPref uint32

	// MaxFileSize is the maximum file size in bytes supported by the server
	MaxFileSize uint64

	// TimeDelta represents the server's time resolution
	// Indicates the granularity of timestamps
	TimeDelta TimeDelta

	// Properties is a bitmask of filesystem properties
	// Indicates supported features (hard links, symlinks, etc.)
	Properties uint32
}

// PathConf contains POSIX-compatible filesystem information.
// This structure is returned by the repository to inform NFS clients
// about filesystem properties and limitations that may vary per file or filesystem.
type PathConf struct {
	// Linkmax is the maximum number of hard links to a file.
	// Typical values: 32767 (ext4), 65000 (XFS), or higher.
	Linkmax uint32

	// NameMax is the maximum filename component length in bytes.
	// Typical value: 255 bytes for most modern filesystems.
	NameMax uint32

	// NoTrunc indicates if the server rejects names longer than NameMax.
	//   - true: Server returns error for long names (recommended)
	//   - false: Server silently truncates long names
	NoTrunc bool

	// ChownRestricted indicates if chown is restricted to superuser.
	//   - true: Only root can change file ownership (POSIX standard)
	//   - false: File owner can give away ownership
	ChownRestricted bool

	// CaseInsensitive indicates if filename comparisons are case-insensitive.
	//   - true: "File.txt" and "file.txt" are the same (Windows-style)
	//   - false: Filenames are case-sensitive (Unix/Linux standard)
	CaseInsensitive bool

	// CasePreserving indicates if the filesystem preserves filename case.
	//   - true: Filenames maintain their original case
	//   - false: Case information may be lost
	CasePreserving bool
}
