package registry

import "github.com/marmos91/dittofs/pkg/store/metadata"

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

	// Deprecated: Use Cache instead. These fields are kept for backward compatibility.
	// If Cache is empty but WriteCache or ReadCache are set, they will be used.
	WriteCache string // Deprecated: Name of the write cache
	ReadCache  string // Deprecated: Name of the read cache

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
}

// ShareConfig contains all configuration needed to create a share.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ContentStore  string
	Cache         string // Unified cache name (optional, empty = no caching)
	ReadOnly      bool

	// Deprecated: Use Cache instead. These are kept for backward compatibility.
	WriteCache string // Deprecated: Optional write cache name
	ReadCache  string // Deprecated: Optional read cache name

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
}
