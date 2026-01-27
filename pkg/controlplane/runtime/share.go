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
	"github.com/marmos91/dittofs/pkg/metadata"
)

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

	// Identity Mapping (Squashing)
	MapAllToAnonymous        bool   // Map all users to anonymous (all_squash)
	MapPrivilegedToAnonymous bool   // Map root to anonymous (root_squash)
	AnonymousUID             uint32 // UID for anonymous users
	AnonymousGID             uint32 // GID for anonymous users

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

	// Identity Mapping
	MapAllToAnonymous        bool
	MapPrivilegedToAnonymous bool
	AnonymousUID             uint32
	AnonymousGID             uint32

	// Root directory attributes
	RootAttr *metadata.FileAttr

	// NFS-specific options
	DisableReaddirplus bool
}

// MountInfo represents an active NFS mount from a client.
type MountInfo struct {
	ClientAddr string // Client IP address
	ShareName  string // Name of the mounted share
	MountTime  int64  // Unix timestamp when mounted
}
