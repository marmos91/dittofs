package acl

// FullAccessMask is the union of all read, write, execute, and admin rights.
// This matches the Windows GENERIC_ALL expansion for file objects.
const FullAccessMask = ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA |
	ACE4_READ_NAMED_ATTRS | ACE4_WRITE_NAMED_ATTRS | ACE4_EXECUTE |
	ACE4_DELETE_CHILD | ACE4_READ_ATTRIBUTES | ACE4_WRITE_ATTRIBUTES |
	ACE4_DELETE | ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER |
	ACE4_SYNCHRONIZE

// alwaysGrantedMask contains the rights always granted to the file owner
// beyond the rwx bits (admin rights).
const alwaysGrantedMask = ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER |
	ACE4_DELETE | ACE4_SYNCHRONIZE

// SynthesizeFromMode creates a Windows-compatible DACL from POSIX mode bits.
// The resulting ACL follows canonical Windows ordering (deny before allow) and
// includes well-known SID ACEs for SYSTEM and Administrators.
//
// For directories, all ACEs include CONTAINER_INHERIT and OBJECT_INHERIT flags.
//
// Parameters:
//   - mode: POSIX 9-bit permission mode (e.g., 0755)
//   - ownerUID: file owner UID (reserved for future SID mapping)
//   - ownerGID: file owner GID (reserved for future SID mapping)
//   - isDirectory: whether the target is a directory
//
// The ownerUID and ownerGID parameters are accepted for future use when
// mapping Unix credentials to Windows SIDs. Currently unused but included
// in the signature to avoid breaking changes when SID mapping is added.
func SynthesizeFromMode(mode uint32, ownerUID, ownerGID uint32, isDirectory bool) *ACL {
	ownerRWX := (mode >> 6) & 7
	groupRWX := (mode >> 3) & 7
	otherRWX := mode & 7

	// Compute directory inheritance flags applied to ALL ACEs.
	var inheritFlags uint32
	if isDirectory {
		inheritFlags = ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE
	}

	var aces []ACE

	// Step 1: DENY ACEs (canonical order: deny before allow).
	// If group has fewer rights than owner, deny GROUP@ the difference.
	if groupDiff := ownerRWX &^ groupRWX; groupDiff != 0 {
		aces = append(aces, ACE{
			Type:       ACE4_ACCESS_DENIED_ACE_TYPE,
			Flag:       inheritFlags,
			AccessMask: rwxToFullMask(groupDiff, isDirectory),
			Who:        SpecialGroup,
		})
	}

	// If other has fewer rights than owner, deny EVERYONE@ the difference.
	if otherDiff := ownerRWX &^ otherRWX; otherDiff != 0 {
		aces = append(aces, ACE{
			Type:       ACE4_ACCESS_DENIED_ACE_TYPE,
			Flag:       inheritFlags,
			AccessMask: rwxToFullMask(otherDiff, isDirectory),
			Who:        SpecialEveryone,
		})
	}

	// Step 2: ALLOW ACEs.
	// Owner always gets at least admin rights plus their rwx permissions.
	aces = append(aces, ACE{
		Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
		Flag:       inheritFlags,
		AccessMask: rwxToFullMask(ownerRWX, isDirectory) | alwaysGrantedMask,
		Who:        SpecialOwner,
	})

	// Group gets their permissions (only if non-zero).
	if groupRWX != 0 {
		aces = append(aces, ACE{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       inheritFlags,
			AccessMask: rwxToFullMask(groupRWX, isDirectory),
			Who:        SpecialGroup,
		})
	}

	// Everyone gets their permissions (only if non-zero).
	if otherRWX != 0 {
		aces = append(aces, ACE{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       inheritFlags,
			AccessMask: rwxToFullMask(otherRWX, isDirectory),
			Who:        SpecialEveryone,
		})
	}

	// Step 3: Well-known SID ACEs (always present in synthesized DACLs).
	aces = append(aces,
		ACE{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       inheritFlags,
			AccessMask: FullAccessMask,
			Who:        SpecialSystem,
		},
		ACE{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       inheritFlags,
			AccessMask: FullAccessMask,
			Who:        SpecialAdministrators,
		},
	)

	return &ACL{
		ACEs:   aces,
		Source: ACLSourcePOSIXDerived,
	}
}

// rwxToFullMask maps a 3-bit rwx value to the full set of Windows access
// rights associated with each permission. This provides fine-grained rights
// mapping that Windows Explorer and icacls can display meaningfully.
//
// Mapping:
//
//	Read (4):    READ_DATA | READ_ATTRIBUTES | READ_NAMED_ATTRS | READ_ACL | SYNCHRONIZE
//	Write (2):   WRITE_DATA | APPEND_DATA | WRITE_ATTRIBUTES | WRITE_NAMED_ATTRS (+ DELETE_CHILD for dirs)
//	Execute (1): EXECUTE | READ_ATTRIBUTES | SYNCHRONIZE
func rwxToFullMask(rwx uint32, isDirectory bool) uint32 {
	var mask uint32

	if rwx&modeRead != 0 {
		mask |= ACE4_READ_DATA | ACE4_READ_ATTRIBUTES | ACE4_READ_NAMED_ATTRS |
			ACE4_READ_ACL | ACE4_SYNCHRONIZE
	}

	if rwx&modeWrite != 0 {
		mask |= ACE4_WRITE_DATA | ACE4_APPEND_DATA |
			ACE4_WRITE_ATTRIBUTES | ACE4_WRITE_NAMED_ATTRS
		if isDirectory {
			mask |= ACE4_DELETE_CHILD
		}
	}

	if rwx&modeExecute != 0 {
		mask |= ACE4_EXECUTE | ACE4_READ_ATTRIBUTES | ACE4_SYNCHRONIZE
	}

	return mask
}
