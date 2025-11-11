package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/metadata"
)

// SetServerConfig sets the server-wide configuration.
//
// This stores global server settings that apply across all exports,
// including:
//   - DUMP access control lists
//   - Server capabilities
//   - Default filesystem limits
//   - Other server-wide policies
//
// Parameters:
//   - config: The server configuration to store
//
// Returns:
//   - error: Always returns nil (reserved for future validation)
func (r *MemoryRepository) SetServerConfig(ctx context.Context, config metadata.ServerConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.serverConfig = config
	return nil
}

// GetServerConfig returns the current server configuration.
//
// This retrieves the global server settings for use in protocol handlers
// and access control checks.
//
// Returns:
//   - ServerConfig: The current server configuration
//   - error: Always returns nil (reserved for future use)
func (r *MemoryRepository) GetServerConfig(ctx context.Context) (metadata.ServerConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.serverConfig, nil
}

// GetMaxWriteSize returns the maximum write size for validation.
//
// This returns a value that matches or exceeds the wtmax advertised in FSINFO.
// The validation limit can be larger than wtmax to provide tolerance for
// non-compliant clients while still preventing abuse.
//
// Current implementation:
//   - wtmax (advertised to clients): 64KB
//   - validation limit (enforced): 1MB
//
// This allows clients that respect FSINFO to use the optimal 64KB size,
// while preventing malicious clients from sending excessively large writes.
//
// Returns:
//   - uint32: Maximum write size (1MB = 1048576 bytes)
func (r *MemoryRepository) GetMaxWriteSize(ctx context.Context) uint32 {
	// Return 1MB as a reasonable upper bound for write validation.
	// This is 16x larger than the advertised wtmax (64KB), providing
	// significant tolerance while still preventing DoS attacks.
	//
	// Note: Most well-behaved NFS clients will limit writes to wtmax (64KB)
	// based on the FSINFO response, so they will never hit this limit.
	return 1024 * 1024 // 1MB
}

// GetFSInfo returns the static filesystem information and capabilities.
//
// This implements support for the FSINFO NFS procedure (RFC 1813 section 3.3.19).
// It provides clients with information about server capabilities and preferences,
// including:
//   - Maximum and preferred transfer sizes
//   - Maximum file size
//   - Time resolution
//   - Filesystem properties (hard links, symlinks, etc.)
//
// The values returned are reasonable defaults for a general-purpose NFS server.
// In a production implementation, these could be:
//   - Configured via ServerConfig
//   - Queried from the underlying storage backend
//   - Tuned based on server resources and network conditions
//
// Transfer Size Guidelines (RFC 1813):
//   - rtmax/wtmax: Maximum sizes the server can handle
//   - rtpref/wtpref: Preferred sizes for optimal performance
//   - rtmult/wtmult: Alignment requirements (typically filesystem block size)
//
// Properties Bitmap (RFC 1813):
//   - FSFLink (0x0001): Hard links supported
//   - FSFSymlink (0x0002): Symbolic links supported
//   - FSFHomogeneous (0x0008): PATHCONF same for all files
//   - FSFCanSetTime (0x0010): Server can set file times
//
// Parameters:
//   - handle: A file handle within the filesystem (used to identify export)
//
// Returns:
//   - *FSInfo: Static filesystem information
//   - error: Returns error if handle not found
func (r *MemoryRepository) GetFSInfo(ctx context.Context, handle metadata.FileHandle) (*metadata.FSInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Verify handle exists
	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return nil, fmt.Errorf("file not found")
	}

	// Return filesystem information
	// These are reasonable defaults that can be customized via ServerConfig in the future
	return &metadata.FSInfo{
		RtMax:       65536,     // 64KB max read (common NFS default)
		RtPref:      32768,     // 32KB preferred read (balances throughput vs latency)
		RtMult:      4096,      // 4KB read multiple (typical filesystem block size)
		WtMax:       65536,     // 64KB max write (matches read for symmetry)
		WtPref:      32768,     // 32KB preferred write (matches read)
		WtMult:      4096,      // 4KB write multiple (matches read)
		DtPref:      8192,      // 8KB preferred readdir (fits ~50-100 entries)
		MaxFileSize: 1<<63 - 1, // Max file size (practically unlimited)
		TimeDelta: metadata.TimeDelta{
			Seconds:  0,
			Nseconds: 1, // 1 nanosecond time granularity (high resolution)
		},
		// Properties: hard links, symlinks, homogeneous PATHCONF, can set time
		// These correspond to FSFLink | FSFSymlink | FSFHomogeneous | FSFCanSetTime
		Properties: 0x0001 | 0x0002 | 0x0008 | 0x0010,
	}, nil
}

// GetFSStats returns the dynamic filesystem statistics.
//
// This implements support for the FSSTAT NFS procedure (RFC 1813 section 3.3.18).
// It provides clients with current filesystem capacity and usage information,
// including:
//   - Total, free, and available space (in bytes)
//   - Total, free, and available inodes (file count)
//   - Filesystem stability (invarsec)
//
// The distinction between "free" and "available":
//   - Free: Total free space on the filesystem
//   - Available: Space available to non-root users (may be less due to reserved space)
//
// For this in-memory implementation, we return reasonable defaults that suggest
// a large, mostly-empty filesystem. A production implementation should:
//   - Query the actual backend storage
//   - Track real usage statistics
//   - Apply appropriate reserved space calculations
//   - Consider quota limits
//
// The invarsec field indicates filesystem stability:
//   - 0: Filesystem can change at any time (typical for network filesystems)
//   - >0: Number of seconds attributes are guaranteed stable
//
// Parameters:
//   - handle: A file handle within the filesystem (used to identify export)
//
// Returns:
//   - *FSStat: Dynamic filesystem statistics
//   - error: Returns error if handle not found
func (r *MemoryRepository) GetFSStats(ctx context.Context, handle metadata.FileHandle) (*metadata.FSStat, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Verify handle exists
	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return nil, fmt.Errorf("file not found")
	}

	// Return filesystem statistics
	// These are reasonable defaults for a virtual/in-memory filesystem
	// Real implementations should query actual backend storage
	return &metadata.FSStat{
		TotalBytes: 1024 * 1024 * 1024 * 1024, // 1TB total capacity
		FreeBytes:  512 * 1024 * 1024 * 1024,  // 512GB free space
		AvailBytes: 512 * 1024 * 1024 * 1024,  // 512GB available (same as free for non-root)
		TotalFiles: 1000000,                   // 1M inodes total
		FreeFiles:  900000,                    // 900K free inodes (90% available)
		AvailFiles: 900000,                    // 900K available (same as free for non-root)
		Invarsec:   0,                         // Filesystem can change at any time
	}, nil
}

// GetPathConf returns POSIX-compatible filesystem information and properties.
//
// This implements support for the PATHCONF NFS procedure (RFC 1813 section 3.3.20).
// It provides clients with information about filesystem limitations and behaviors,
// including:
//   - Maximum hard link count
//   - Maximum filename length
//   - Filename truncation behavior
//   - Ownership change restrictions
//   - Case sensitivity and preservation
//
// These values define the POSIX compatibility level of the filesystem and help
// clients make appropriate decisions about file operations.
//
// Key Properties:
//   - Linkmax: Maximum number of hard links (typical ext4: 32767)
//   - NameMax: Maximum filename length in bytes (typical Unix: 255)
//   - NoTrunc: Reject names that are too long (true = strict, false = truncate)
//   - ChownRestricted: Only root can chown (true = POSIX standard behavior)
//   - CaseInsensitive: Case-sensitive filenames (false = Unix standard)
//   - CasePreserving: Preserve filename case (true = standard behavior)
//
// Parameters:
//   - handle: A file handle within the filesystem
//
// Returns:
//   - *PathConf: POSIX filesystem properties
//   - error: Returns error if handle not found
func (r *MemoryRepository) GetPathConf(ctx context.Context, handle metadata.FileHandle) (*metadata.PathConf, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Verify handle exists
	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return nil, fmt.Errorf("file not found")
	}

	// Return filesystem PATHCONF properties
	// These are reasonable defaults that match POSIX-compliant filesystems
	return &metadata.PathConf{
		Linkmax:         32767, // Max hard links (typical for ext4)
		NameMax:         255,   // Max filename length in bytes (Unix standard)
		NoTrunc:         true,  // Reject names that are too long (strict mode)
		ChownRestricted: true,  // Only root can chown (POSIX standard)
		CaseInsensitive: false, // Case-sensitive filenames (Unix standard)
		CasePreserving:  true,  // Preserve filename case (standard behavior)
	}, nil
}
