package acl

// Mode bit positions for rwx triplets.
const (
	modeRead    = 0x4
	modeWrite   = 0x2
	modeExecute = 0x1
)

// DeriveMode derives Unix mode bits from an ACL for display purposes.
// It scans OWNER@, GROUP@, and EVERYONE@ ALLOW ACEs and maps their
// mask bits to the corresponding rwx triplets per RFC 7530 Section 6.4.1.
//
// Returns the composed 9-bit mode (e.g., 0755, 0644).
// If the ACL is nil, returns 0.
func DeriveMode(a *ACL) uint32 {
	if a == nil {
		return 0
	}

	var ownerBits, groupBits, otherBits uint32

	for i := range a.ACEs {
		ace := &a.ACEs[i]

		// Only consider non-inherit-only ALLOW ACEs for mode derivation.
		if ace.Type != ACE4_ACCESS_ALLOWED_ACE_TYPE || ace.IsInheritOnly() {
			continue
		}

		rwx := maskToRWX(ace.AccessMask)

		switch ace.Who {
		case SpecialOwner:
			ownerBits |= rwx
		case SpecialGroup:
			groupBits |= rwx
		case SpecialEveryone:
			otherBits |= rwx
		}
	}

	return (ownerBits << 6) | (groupBits << 3) | otherBits
}

// AdjustACLForMode adjusts an ACL when mode bits change (chmod).
// Per RFC 7530 Section 6.4.1, only OWNER@, GROUP@, and EVERYONE@ ACEs
// are modified. All other ACEs (explicit user/group) are preserved
// unchanged (Pitfall 4).
//
// For ALLOW ACEs of special identifiers:
//   - Recompute rwx mask bits from the corresponding mode triplet
//   - Preserve non-rwx bits (READ_ACL, WRITE_ACL, DELETE, etc.)
//
// For DENY ACEs of special identifiers:
//   - Adjust to deny bits NOT granted by the new mode
//
// Returns a new ACL; the original is not modified.
func AdjustACLForMode(a *ACL, newMode uint32) *ACL {
	if a == nil {
		return nil
	}

	ownerRWX := (newMode >> 6) & 0x7
	groupRWX := (newMode >> 3) & 0x7
	otherRWX := newMode & 0x7

	newACEs := make([]ACE, len(a.ACEs))
	copy(newACEs, a.ACEs)

	for i := range newACEs {
		ace := &newACEs[i]

		var triplet uint32
		switch ace.Who {
		case SpecialOwner:
			triplet = ownerRWX
		case SpecialGroup:
			triplet = groupRWX
		case SpecialEveryone:
			triplet = otherRWX
		default:
			// Non-special ACEs are preserved unchanged.
			continue
		}

		switch ace.Type {
		case ACE4_ACCESS_ALLOWED_ACE_TYPE:
			// Clear the rwx-related mask bits, then set from mode.
			ace.AccessMask = (ace.AccessMask &^ rwxMaskBits) | rwxToMask(triplet)

		case ACE4_ACCESS_DENIED_ACE_TYPE:
			// Deny bits NOT in the new mode.
			denyMask := rwxToMask(0x7 &^ triplet)
			ace.AccessMask = (ace.AccessMask &^ rwxMaskBits) | denyMask
		}
	}

	return &ACL{ACEs: newACEs}
}

// rwxMaskBits is the set of ACE mask bits that correspond to rwx permissions.
const rwxMaskBits = ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE

// maskToRWX converts ACE mask bits to a 3-bit rwx value.
func maskToRWX(mask uint32) uint32 {
	var rwx uint32
	if mask&ACE4_READ_DATA != 0 {
		rwx |= modeRead
	}
	if mask&(ACE4_WRITE_DATA|ACE4_APPEND_DATA) != 0 {
		rwx |= modeWrite
	}
	if mask&ACE4_EXECUTE != 0 {
		rwx |= modeExecute
	}
	return rwx
}

// rwxToMask converts a 3-bit rwx value to ACE mask bits.
func rwxToMask(rwx uint32) uint32 {
	var mask uint32
	if rwx&modeRead != 0 {
		mask |= ACE4_READ_DATA
	}
	if rwx&modeWrite != 0 {
		mask |= ACE4_WRITE_DATA | ACE4_APPEND_DATA
	}
	if rwx&modeExecute != 0 {
		mask |= ACE4_EXECUTE
	}
	return mask
}
