// Package acl implements NFSv4 Access Control List types, evaluation,
// validation, mode synchronization, and inheritance per RFC 7530 Section 6.
//
// This package is protocol-agnostic: it has no dependencies on NFS/SMB wire
// formats or internal protocol packages. All types use Go primitives and
// are JSON-serializable for storage in metadata backends.
package acl

import "fmt"

// ACE types (acetype4) per RFC 7530 Section 6.2.1.
const (
	ACE4_ACCESS_ALLOWED_ACE_TYPE = 0x00000000
	ACE4_ACCESS_DENIED_ACE_TYPE  = 0x00000001
	ACE4_SYSTEM_AUDIT_ACE_TYPE   = 0x00000002
	ACE4_SYSTEM_ALARM_ACE_TYPE   = 0x00000003
)

// ACE flags (aceflag4) per RFC 7530 Section 6.2.1.
const (
	ACE4_FILE_INHERIT_ACE           = 0x00000001
	ACE4_DIRECTORY_INHERIT_ACE      = 0x00000002
	ACE4_NO_PROPAGATE_INHERIT_ACE   = 0x00000004
	ACE4_INHERIT_ONLY_ACE           = 0x00000008
	ACE4_SUCCESSFUL_ACCESS_ACE_FLAG = 0x00000010
	ACE4_FAILED_ACCESS_ACE_FLAG     = 0x00000020
	ACE4_INHERITED_ACE              = 0x00000080
)

// ACE access mask bits (acemask4) per RFC 7530 Section 6.2.1.
// File-specific permissions.
const (
	ACE4_READ_DATA    = 0x00000001
	ACE4_WRITE_DATA   = 0x00000002
	ACE4_APPEND_DATA  = 0x00000004
	ACE4_EXECUTE      = 0x00000020
	ACE4_DELETE_CHILD = 0x00000040
)

// Directory-specific aliases (same bit positions as file permissions).
const (
	ACE4_LIST_DIRECTORY   = ACE4_READ_DATA   // 0x00000001
	ACE4_ADD_FILE         = ACE4_WRITE_DATA  // 0x00000002
	ACE4_ADD_SUBDIRECTORY = ACE4_APPEND_DATA // 0x00000004
)

// Common permissions (apply to both files and directories).
const (
	ACE4_READ_NAMED_ATTRS     = 0x00000008
	ACE4_WRITE_NAMED_ATTRS    = 0x00000010
	ACE4_READ_ATTRIBUTES      = 0x00000080
	ACE4_WRITE_ATTRIBUTES     = 0x00000100
	ACE4_WRITE_RETENTION      = 0x00000200
	ACE4_WRITE_RETENTION_HOLD = 0x00000400
	ACE4_DELETE               = 0x00010000
	ACE4_READ_ACL             = 0x00020000
	ACE4_WRITE_ACL            = 0x00040000
	ACE4_WRITE_OWNER          = 0x00080000
	ACE4_SYNCHRONIZE          = 0x00100000
)

// ACL support constants for FATTR4_ACLSUPPORT.
const (
	ACL4_SUPPORT_ALLOW_ACL = 0x00000001
	ACL4_SUPPORT_DENY_ACL  = 0x00000002
	ACL4_SUPPORT_AUDIT_ACL = 0x00000004
	ACL4_SUPPORT_ALARM_ACL = 0x00000008

	// FullACLSupport is the union of all four ACL support bits.
	FullACLSupport = ACL4_SUPPORT_ALLOW_ACL | ACL4_SUPPORT_DENY_ACL | ACL4_SUPPORT_AUDIT_ACL | ACL4_SUPPORT_ALARM_ACL
)

// FATTR4 bit numbers for ACL-related attributes.
const (
	FATTR4_ACL        = 12
	FATTR4_ACLSUPPORT = 13
)

// Special identifiers per RFC 7530 Section 6.2.1.5.
const (
	SpecialOwner    = "OWNER@"
	SpecialGroup    = "GROUP@"
	SpecialEveryone = "EVERYONE@"
)

// Well-known SID special identifiers for Windows interop.
// The SMB translator converts these to binary SIDs (S-1-5-18 and S-1-5-32-544).
const (
	SpecialSystem         = "SYSTEM@"         // NT AUTHORITY\SYSTEM (S-1-5-18)
	SpecialAdministrators = "ADMINISTRATORS@" // BUILTIN\Administrators (S-1-5-32-544)
)

// MaxACECount is the maximum number of ACEs per file.
const MaxACECount = 128

// MaxDACLSize is the maximum DACL size in bytes (64KB Windows default MAX_ACL_SIZE).
const MaxDACLSize = 65536

// ACE represents a single NFSv4 Access Control Entry.
type ACE struct {
	Type       uint32 `json:"type"`        // acetype4
	Flag       uint32 `json:"flag"`        // aceflag4
	AccessMask uint32 `json:"access_mask"` // acemask4
	Who        string `json:"who"`         // utf8str_mixed: "user@domain", "OWNER@", etc.
}

// ACLSource indicates how an ACL was created. The zero value (empty string)
// means "unknown/legacy" which is backward compatible with existing data.
type ACLSource string

const (
	// ACLSourcePOSIXDerived indicates the ACL was synthesized from POSIX mode bits.
	ACLSourcePOSIXDerived ACLSource = "posix-derived"

	// ACLSourceSMBExplicit indicates the ACL was set explicitly via SMB/CIFS.
	ACLSourceSMBExplicit ACLSource = "smb-explicit"

	// ACLSourceNFSExplicit indicates the ACL was set explicitly via NFSv4.
	ACLSourceNFSExplicit ACLSource = "nfs-explicit"
)

// ACL represents an NFSv4 Access Control List.
type ACL struct {
	ACEs      []ACE     `json:"aces"`
	Source    ACLSource `json:"source,omitempty"`    // How this ACL was created
	Protected bool      `json:"protected,omitempty"` // SE_DACL_PROTECTED - blocks inheritance
}

// IsSpecialWho reports whether who is one of the three special identifiers:
// OWNER@, GROUP@, or EVERYONE@.
func IsSpecialWho(who string) bool {
	return who == SpecialOwner || who == SpecialGroup || who == SpecialEveryone
}

// IsInheritOnly reports whether this ACE has the INHERIT_ONLY flag set.
// An inherit-only ACE is skipped during access evaluation but inherited by children.
func (a *ACE) IsInheritOnly() bool {
	return a.Flag&ACE4_INHERIT_ONLY_ACE != 0
}

// IsInherited reports whether this ACE has the INHERITED_ACE flag set.
// Inherited ACEs were propagated from a parent directory.
func (a *ACE) IsInherited() bool {
	return a.Flag&ACE4_INHERITED_ACE != 0
}

// TypeString returns a human-readable representation of the ACE type.
func (a *ACE) TypeString() string {
	switch a.Type {
	case ACE4_ACCESS_ALLOWED_ACE_TYPE:
		return "ALLOW"
	case ACE4_ACCESS_DENIED_ACE_TYPE:
		return "DENY"
	case ACE4_SYSTEM_AUDIT_ACE_TYPE:
		return "AUDIT"
	case ACE4_SYSTEM_ALARM_ACE_TYPE:
		return "ALARM"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", a.Type)
	}
}
