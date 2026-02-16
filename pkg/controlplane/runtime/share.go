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
	"github.com/marmos91/dittofs/pkg/controlplane/models"
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
	// Default is "read-write" for NFS compatibility.
	DefaultPermission string

	// Identity Mapping (Squashing) - matches Synology NFS options
	// Squash controls how UIDs are mapped:
	//   - none: No mapping, UIDs pass through
	//   - root_to_admin: Root (UID 0) retains admin privileges (default)
	//   - root_to_guest: Root mapped to anonymous (root_squash)
	//   - all_to_admin: All users mapped to root
	//   - all_to_guest: All users mapped to anonymous (all_squash)
	Squash       models.SquashMode
	AnonymousUID uint32 // UID for anonymous mapping (default: 65534)
	AnonymousGID uint32 // GID for anonymous mapping (default: 65534)

	// NFS-specific options
	DisableReaddirplus bool // Prevent READDIRPLUS on this share

	// Security Policy
	// These fields are populated when shares are loaded from the DB into the runtime.
	AllowAuthSys      bool     // Allow AUTH_SYS connections (default: true)
	RequireKerberos   bool     // Require Kerberos authentication (default: false)
	MinKerberosLevel  string   // Minimum Kerberos level: krb5, krb5i, krb5p (default: krb5)
	NetgroupName      string   // Netgroup name for IP-based access control (empty = allow all)
	BlockedOperations []string // Operations blocked on this share
}

// ShareConfig contains all configuration needed to create a share in the runtime.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ReadOnly      bool

	// User-based Access Control
	DefaultPermission string

	// Identity Mapping
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	// Root directory attributes
	RootAttr *metadata.FileAttr

	// NFS-specific options
	DisableReaddirplus bool

	// Security Policy
	AllowAuthSys      bool
	AllowAuthSysSet   bool // true when AllowAuthSys was explicitly set (distinguishes false from unset)
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string
}

// MountInfo represents an active NFS mount from a client.
type MountInfo struct {
	ClientAddr string // Client IP address
	ShareName  string // Name of the mounted share
	MountTime  int64  // Unix timestamp when mounted
}
