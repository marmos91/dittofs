package acl

import "fmt"

// inheritanceMask covers all inheritance-related ACE flags.
const inheritanceMask = ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE |
	ACE4_NO_PROPAGATE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE

// Creator captures the identity of the principal creating a new file or
// directory at inherit time. It is used to substitute CREATOR_OWNER /
// CREATOR_GROUP placeholders in a parent's inheritable ACEs with the
// concrete creator identity (frozen at creation time, per MS-DTYP §2.5.3.4).
//
// SID may be empty when no Windows identity is known (e.g. anonymous
// NFS-only create): in that case the substitution falls back to the
// POSIX form "<id>@localdomain" produced by SIDMapper.SIDToPrincipal.
type Creator struct {
	UID uint32
	GID uint32
	// SID is the creator's Windows user SID. Used for BOTH CreatorOwner@
	// and CreatorGroup@ substitution because the AuthContext carries a
	// single per-creator identifier today — no separate primary-group SID
	// is plumbed. If a future phase introduces a group SID, add a
	// GroupSID field and split the CreatorGroup branch in
	// substituteCreator accordingly.
	SID string
}

// substituteCreator returns the resolved Who string for an ACE whose
// principal is one of SpecialCreatorOwner / SpecialCreatorGroup.
// Returns the original who unchanged for any other principal.
func substituteCreator(who string, creator Creator) string {
	switch who {
	case SpecialCreatorOwner:
		if creator.SID != "" {
			return "sid:" + creator.SID
		}
		return fmt.Sprintf("%d@localdomain", creator.UID)
	case SpecialCreatorGroup:
		if creator.SID != "" {
			return "sid:" + creator.SID
		}
		return fmt.Sprintf("%d@localdomain", creator.GID)
	default:
		return who
	}
}

// ComputeInheritedACL computes the ACL to set on a newly created file or
// directory based on its parent's ACL per RFC 7530 Section 6.4.3 and
// MS-DTYP §2.5.3.4 (CREATOR_OWNER / CREATOR_GROUP substitution).
//
// For files:
//   - Include ACEs with FILE_INHERIT (OI) flag
//   - Clear ALL inheritance flags (files don't propagate further)
//
// For directories (matches Samba libcli/security/create_descriptor.c
// calculate_inherited_from_parent and MS-DTYP §2.5.3.4.1):
//   - Drop ACEs with no OI and no CI (not inheritable to anything).
//   - With NO_PROPAGATE_INHERIT: clear all inheritance flags so the ACE
//     applies at this dir but does not propagate further; if the parent
//     ACE has OI only (no CI), the ACE does not apply at the directory
//     itself and is dropped entirely.
//   - With CI: ACE applies at the dir; clear INHERIT_ONLY.
//   - With OI only: ACE does NOT apply at this dir but must propagate to
//     file grandchildren — emit on the child as OI|INHERIT_ONLY.
//
// The ACE4_INHERITED_ACE bit on the resulting child ACE is conditional, per
// MS-DTYP §2.5.3.4.2 and Samba calculate_inherited_from_parent: the bit
// is set iff the parent SD has SE_DACL_AUTO_INHERITED (i.e.
// parentACL.AutoInherited) OR the source parent ACE itself already
// carries ACE4_INHERITED_ACE (meaning it was inherited from upstream —
// that fact survives propagation to the child).
//
// In both cases, any ACE whose Who is SpecialCreatorOwner or
// SpecialCreatorGroup is rewritten in place with the creator's frozen
// identity (sid:<SID> when known, otherwise "<uid|gid>@localdomain").
//
// Per MS-DTYP §2.5.3.4.2, when the parent SD has SE_DACL_AUTO_INHERITED set,
// the computed child SD also has SE_DACL_AUTO_INHERITED set (mirrors Samba
// source3/smbd/posix_acls.c::set_inherited_sd). SE_DACL_PROTECTED is a
// per-SD property that BLOCKS inheritance from upstream and is never itself
// inherited onto the child.
//
// Returns nil if parentACL is nil or no ACEs are inheritable.
func ComputeInheritedACL(parentACL *ACL, isDirectory bool, creator Creator) *ACL {
	if parentACL == nil {
		return nil
	}

	var inherited []ACE

	for i := range parentACL.ACEs {
		ace := &parentACL.ACEs[i]

		hasOI := ace.Flag&ACE4_FILE_INHERIT_ACE != 0
		hasCI := ace.Flag&ACE4_DIRECTORY_INHERIT_ACE != 0
		hasNP := ace.Flag&ACE4_NO_PROPAGATE_INHERIT_ACE != 0

		if !isDirectory && !hasOI {
			// Files only inherit OI-bearing ACEs.
			continue
		}
		if isDirectory && !hasOI && !hasCI {
			// Dir children only inherit OI- or CI-bearing ACEs.
			continue
		}

		newACE := *ace
		// MS-DTYP §2.5.3.4.2 / Samba calculate_inherited_from_parent: the
		// INHERITED_ACE bit on the child is conditional. It is set when
		// the parent ACE already carries it (already-inherited fact
		// survives propagation) OR the parent SD has AUTO_INHERITED set
		// (parent is configured to mark its inherited children). Strip
		// any pre-existing bit first so we apply the rule cleanly.
		preservedInheritedACE := newACE.Flag & ACE4_INHERITED_ACE
		newACE.Flag &^= ACE4_INHERITED_ACE

		if !isDirectory {
			// Files are leaves: clear ALL inheritance flags.
			newACE.Flag &^= inheritanceMask
		} else if hasNP {
			// NO_PROPAGATE: stop propagation to grandchildren. ACE
			// applies at this dir only when it carries CI; an
			// OI-only-with-NP parent ACE has no effect on the dir
			// child (per Samba calculate_inherited_from_parent /
			// smbtorture test_inheritance row 5).
			if !hasCI {
				continue
			}
			newACE.Flag &^= inheritanceMask
		} else if hasCI {
			// CI (with or without OI) applies at this dir; clear
			// INHERIT_ONLY so the ACE is effective here. OI is
			// preserved when present so file grandchildren still
			// inherit.
			newACE.Flag &^= ACE4_INHERIT_ONLY_ACE
		} else {
			// OI only (no CI, no NP): ACE does not apply at this dir
			// but must propagate to file grandchildren. Emit with
			// OI|INHERIT_ONLY; clear NP/CI noise.
			newACE.Flag &^= inheritanceMask
			newACE.Flag |= ACE4_FILE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE
		}

		if preservedInheritedACE != 0 || parentACL.AutoInherited {
			newACE.Flag |= ACE4_INHERITED_ACE
		}

		// MS-DTYP §2.5.3.4: substitute CREATOR_OWNER / CREATOR_GROUP
		// placeholders with the creator's frozen identity.
		newACE.Who = substituteCreator(newACE.Who, creator)

		inherited = append(inherited, newACE)
	}

	if len(inherited) == 0 {
		return nil
	}

	result := &ACL{ACEs: inherited}
	// Per MS-DTYP §2.5.3.4.2, when a parent has SE_DACL_AUTO_INHERITED set,
	// children created under it inherit that bit on their own SD. Mirrors
	// Samba source3/smbd/posix_acls.c::set_inherited_sd. Protected is a
	// per-SD property that blocks inheritance from upstream; it is never
	// itself inherited.
	if parentACL.AutoInherited {
		result.AutoInherited = true
	}
	return result
}

// PropagateACL replaces the inherited ACEs of an existing ACL with newly
// computed inherited ACEs from a parent, while preserving explicit ACEs.
//
// This is used for recursive ACL propagation: when a parent's ACL changes,
// each descendant's inherited ACEs are recomputed but its explicit ACEs
// (those without the INHERITED_ACE flag) are kept intact.
//
// The result maintains canonical ordering: explicit ACEs first, then inherited.
// Per MS-DTYP §2.5.3.4.2 (matching ComputeInheritedACL), the parent's
// SE_DACL_AUTO_INHERITED bit propagates onto the recomputed child SD;
// SE_DACL_PROTECTED is never inherited. Mirrors Samba
// source3/smbd/posix_acls.c::set_inherited_sd.
//
// Returns nil if both newly computed and existing explicit ACEs are empty.
func PropagateACL(parentACL *ACL, existingACL *ACL, isDirectory bool, creator Creator) *ACL {
	newInherited := ComputeInheritedACL(parentACL, isDirectory, creator)

	if existingACL == nil {
		return newInherited
	}

	// Collect explicit ACEs (those without INHERITED_ACE flag).
	var explicit []ACE
	for i := range existingACL.ACEs {
		if !existingACL.ACEs[i].IsInherited() {
			explicit = append(explicit, existingACL.ACEs[i])
		}
	}

	if len(explicit) == 0 && newInherited == nil {
		return nil
	}

	// Combine: explicit first, then inherited (maintains canonical order).
	var inheritedACEs []ACE
	if newInherited != nil {
		inheritedACEs = newInherited.ACEs
	}
	combined := make([]ACE, 0, len(explicit)+len(inheritedACEs))
	combined = append(combined, explicit...)
	combined = append(combined, inheritedACEs...)

	result := &ACL{ACEs: combined}
	// MS-DTYP §2.5.3.4.2: SE_DACL_AUTO_INHERITED propagates from parent to
	// child on the child's recomputed SD. Protected is per-SD and never
	// inherited. See ComputeInheritedACL for spec/Samba references.
	if parentACL != nil && parentACL.AutoInherited {
		result.AutoInherited = true
	}
	return result
}
