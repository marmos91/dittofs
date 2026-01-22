// Package runtime provides runtime state management for the control plane.
//
// The runtime package manages ephemeral state that is NOT persisted to the database:
//   - Live metadata store instances
//   - Share runtime state (root handles)
//   - Active mounts from NFS clients
//
// This is the "in-memory" counterpart to the persistent store package.
package runtime

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// PrefetchConfig configures read prefetch behavior for a share.
type PrefetchConfig struct {
	// Enabled controls whether prefetch is enabled (default: true)
	Enabled bool

	// MaxFileSize is the maximum file size to prefetch in bytes.
	// Files larger than this are not prefetched to avoid cache thrashing.
	// Default: 100MB
	MaxFileSize int64

	// ChunkSize is the size of each chunk read during prefetch in bytes.
	// Default: 512KB
	ChunkSize int64
}

// WriteGatheringConfig configures the write gathering optimization.
//
// Write gathering is based on the Linux kernel's "wdelay" optimization (fs/nfsd/vfs.c).
// When multiple writes are happening to the same file, COMMIT operations wait
// briefly to allow additional writes to accumulate before flushing.
type WriteGatheringConfig struct {
	// Enabled controls whether write gathering is active.
	// Default: true
	Enabled bool

	// GatherDelay is how long COMMIT waits when recent writes are detected.
	// Similar to Linux kernel's 10ms delay in wait_for_concurrent_writes().
	// Default: 10ms. Range: 1ms to 100ms.
	GatherDelay time.Duration

	// ActiveThreshold is how recent a write must be to trigger gathering.
	// If last write was within this duration, COMMIT will wait GatherDelay.
	// Default: 10ms (same as GatherDelay for symmetry).
	ActiveThreshold time.Duration
}

// Share represents the runtime state of a configured share.
// This combines persisted configuration with live runtime state.
type Share struct {
	Name          string
	MetadataStore string              // Name of the metadata store
	RootHandle    metadata.FileHandle // Encoded file handle for the root directory
	ReadOnly      bool

	// User-based Access Control
	// DefaultPermission is the permission for users without explicit permission or unknown UIDs.
	// Values: "none" (block access), "read", "read-write", "admin"
	DefaultPermission string

	// Access Control
	AllowedClients     []string // IP addresses or CIDR ranges allowed (empty = all allowed)
	DeniedClients      []string // IP addresses or CIDR ranges denied (takes precedence)
	RequireAuth        bool     // Require authentication
	AllowedAuthMethods []string // Allowed auth methods (e.g., "anonymous", "unix")

	// Identity Mapping (Squashing)
	MapAllToAnonymous        bool   // Map all users to anonymous (all_squash)
	MapPrivilegedToAnonymous bool   // Map root to anonymous (root_squash)
	AnonymousUID             uint32 // UID for anonymous users
	AnonymousGID             uint32 // GID for anonymous users

	// Cache behavior configuration
	PrefetchConfig       PrefetchConfig       // Read prefetch settings
	WriteGatheringConfig WriteGatheringConfig // Write gathering optimization settings

	// NFS-specific options
	DisableReaddirplus bool // Prevent READDIRPLUS on this share
}

// ShareConfig contains all configuration needed to create a share in the runtime.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ReadOnly      bool

	// User-based Access Control
	DefaultPermission string

	AllowedClients     []string
	DeniedClients      []string
	RequireAuth        bool
	AllowedAuthMethods []string

	// Identity Mapping
	MapAllToAnonymous        bool
	MapPrivilegedToAnonymous bool
	AnonymousUID             uint32
	AnonymousGID             uint32

	// Root directory attributes
	RootAttr *metadata.FileAttr

	// Cache behavior configuration
	PrefetchConfig       PrefetchConfig
	WriteGatheringConfig WriteGatheringConfig

	// NFS-specific options
	DisableReaddirplus bool
}

// MountInfo represents an active NFS mount from a client.
type MountInfo struct {
	ClientAddr string // Client IP address
	ShareName  string // Name of the mounted share
	MountTime  int64  // Unix timestamp when mounted
}
