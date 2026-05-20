package acl

import (
	"fmt"
	"log/slog"
)

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
// CREATOR dual-emission on directory children (mirrors Samba
// libcli/security/create_descriptor.c::calculate_inherited_from_parent):
// when the parent ACE has CONTAINER_INHERIT (CI) and a CREATOR_OWNER /
// CREATOR_GROUP principal, two child ACEs are emitted to the directory:
//  1. The resolved ACE (principal substituted to the new directory's
//     owner/group, inheritance flags propagated per the rules above).
//  2. A preserved CREATOR ACE with CI|INHERIT_ONLY (plus OI if the parent
//     had it) so grandchild directories still substitute against their
//     own owner/group rather than receiving an already-resolved ACE.
//
// File children are leaves: no dual-emission — the resolved ACE is the
// only one emitted (any inheritance to deeper levels is moot).
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

		// Cap enforcement (FIFO truncation, mirrors Samba behavior under
		// pressure): never produce a child ACL exceeding MaxACECount.
		// Check BEFORE appending the resolved ACE because the prior
		// iteration may have already dual-emitted, leaving the result at
		// capacity. Earlier parent ACEs take precedence over later ones.
		if len(inherited) >= MaxACECount {
			slog.Debug("acl.ComputeInheritedACL: MaxACECount reached — truncating remaining parent ACEs",
				"max", MaxACECount, "produced", len(inherited),
				"remaining_parent_aces", len(parentACL.ACEs)-i)
			break
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
		originalWho := ace.Who
		isCreator := originalWho == SpecialCreatorOwner || originalWho == SpecialCreatorGroup
		newACE.Who = substituteCreator(originalWho, creator)

		inherited = append(inherited, newACE)

		// Dual-emit on directory children when parent ACE carried a
		// CREATOR principal AND CONTAINER_INHERIT. Mirrors Samba
		// calculate_inherited_from_parent: keep the original CREATOR ACE
		// with CI|INHERIT_ONLY (preserving OI when present) so grandchild
		// directories continue to substitute against THEIR own creator
		// instead of inheriting the already-resolved owner/group.
		//
		// Skipped on:
		//   - files (leaves)
		//   - parent ACE without CI (no need to reach grandchild dirs)
		//   - NO_PROPAGATE (Samba stops propagation in that branch)
		if isDirectory && isCreator && hasCI && !hasNP {
			// Budget check: dual-emit needs one extra slot. The resolved
			// ACE was already appended above; we now want to also add the
			// preserved CREATOR. If there is no room left, drop the
			// preserved one (resolved already inherits — losing the
			// preserved version only weakens grandchild substitution, not
			// the immediate child's effective permissions). FIFO
			// truncation: prefer earlier parent ACEs over later ones.
			if len(inherited) >= MaxACECount {
				slog.Debug("acl.ComputeInheritedACL: dropping preserved CREATOR ACE — MaxACECount reached",
					"max", MaxACECount, "produced", len(inherited),
					"principal", originalWho)
				continue
			}
			preserved := *ace
			preserved.Who = originalWho
			preserved.Flag &^= inheritanceMask
			preserved.Flag |= ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE
			if hasOI {
				preserved.Flag |= ACE4_FILE_INHERIT_ACE
			}
			// INHERITED_ACE conditional: same rule as the resolved ACE
			// (Bug A — PR 2). The preserved ACE was produced by the
			// inheritance computation; it must carry the bit whenever
			// the resolved sibling does.
			preserved.Flag &^= ACE4_INHERITED_ACE
			if (ace.Flag&ACE4_INHERITED_ACE) != 0 || parentACL.AutoInherited {
				preserved.Flag |= ACE4_INHERITED_ACE
			}
			inherited = append(inherited, preserved)
		}
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
