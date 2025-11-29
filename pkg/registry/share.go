package registry

import (
	"time"

	"github.com/marmos91/dittofs/pkg/cache/flusher"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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

// FlusherConfig configures background flusher behavior for a share.
type FlusherConfig struct {
	// SweepInterval is how often to check for idle files.
	// Default: 10s
	SweepInterval time.Duration

	// FlushTimeout is how long a file must be idle before flushing.
	// Default: 30s
	FlushTimeout time.Duration
}

// Share represents a configured share that binds together:
// - A share name (export path for NFS, share name for SMB)
// - A metadata store instance (for file/directory structure)
// - A content store instance (for file data)
// - Optional unified cache for read/write buffering
// - Access control rules (IP-based, authentication)
// - Identity mapping rules (squashing)
//
// Multiple shares can reference the same store instances.
//
// Caching:
// The unified cache serves both reads and writes:
// - Writes accumulate in cache (StateBuffering)
// - COMMIT flushes to content store (StateUploading → StateCached)
// - Reads check cache first, populate on miss
// - Cache is optional (empty = sync writes, no read caching)
type Share struct {
	Name          string
	MetadataStore string              // Name of the metadata store
	ContentStore  string              // Name of the content store
	Cache         string              // Name of the unified cache (optional, empty = no caching)
	RootHandle    metadata.FileHandle // Encoded file handle for the root directory
	ReadOnly      bool

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
	PrefetchConfig PrefetchConfig // Read prefetch settings
	FlusherConfig  FlusherConfig  // Background flusher settings

	// Background flusher for this share (nil if no cache configured)
	Flusher *flusher.BackgroundFlusher
}

// ShareConfig contains all configuration needed to create a share.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ContentStore  string
	Cache         string // Unified cache name (optional, empty = no caching)
	ReadOnly      bool

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
	PrefetchConfig PrefetchConfig // Read prefetch settings
	FlusherConfig  FlusherConfig  // Background flusher settings
}
