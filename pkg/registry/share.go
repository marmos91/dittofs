package registry

import "github.com/marmos91/dittofs/pkg/store/metadata"

// Share represents a configured share that binds together:
// - A share name (export path for NFS, share name for SMB)
// - A metadata store instance (for file/directory structure)
// - A content store instance (for file data)
// - Optional caches for read/write buffering
// - Access control rules (IP-based, authentication)
// - Identity mapping rules (squashing)
//
// Multiple shares can reference the same store instances.
//
// Caching Modes:
// - Write Cache: If specified, enables async writes (WRITE → cache, COMMIT → flush to store)
// - Read Cache: If specified, caches content reads for better performance
// - Both caches are optional and independent
type Share struct {
	Name          string
	MetadataStore string              // Name of the metadata store
	ContentStore  string              // Name of the content store
	WriteCache    string              // Name of the write cache (optional, empty = sync writes)
	ReadCache     string              // Name of the read cache (optional, empty = no read caching)
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
}

// ShareConfig contains all configuration needed to create a share.
type ShareConfig struct {
	Name               string
	MetadataStore      string
	ContentStore       string
	WriteCache         string   // Optional write cache name (empty = sync writes)
	ReadCache          string   // Optional read cache name (empty = no read caching)
	ReadOnly           bool
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
