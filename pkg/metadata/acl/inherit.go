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
// The implementation mirrors Samba libcli/security/create_descriptor.c
// (calculate_inherited_from_parent + desc_expand_generic). For each parent
// ACE, we compute two booleans:
//
//	applies     := (isDir && hasCI) || (!isDir && hasOI)
//	expand_ace  := principal is CREATOR_OWNER/CREATOR_GROUP (today we do
//	               not expand generic-mask bits; that is a separate fix)
//
// Cases:
//
//  1. applies && expand_ace
//     Emit two ACEs (only on dir children — files are leaves):
//     a) Resolved sibling: principal substituted, ALL inheritance flags
//     cleared, then INHERITED_ACE added per the conditional rule.
//     The resolved sibling applies at THIS object only — it does not
//     propagate further.
//     b) Preserved CREATOR: principal kept (CREATOR_OWNER /
//     CREATOR_GROUP stays), flags = parent's OI|CI bits + INHERIT_ONLY,
//     then INHERITED_ACE per the conditional rule. This preserves the
//     placeholder for substitution at grandchild create time.
//     On file children: only the resolved sibling is emitted (no need
//     to preserve for grandchildren).
//
//  2. applies && !expand_ace
//     Emit a single ACE with the original principal. For files, all
//     inheritance flags are cleared. For directories, OI/CI bits are
//     preserved (so grandchildren still inherit), and INHERIT_ONLY is
//     cleared so the ACE is effective at this dir. NO_PROPAGATE clears
//     OI/CI to stop further propagation.
//
//  3. !applies, but parent has bits that propagate to grandchildren of
//     THIS object's type (e.g. parent OI on a dir child): emit a single
//     "preserved" ACE — original principal (CREATOR stays CREATOR), parent's
//     OI/CI bits preserved, INHERIT_ONLY added so it does not apply here.
//     This is the OI-only-on-dir-child case (smbtorture row 1).
//
// The ACE4_INHERITED_ACE bit on every emitted child ACE is conditional,
// per MS-DTYP §2.5.3.4.2 and Samba calculate_inherited_from_parent: set
// iff parentACL.AutoInherited (after canonicalization). The parent ACE's
// own pre-existing INHERITED_ACE bit is NOT propagated independently —
// that was over-broad and broke smbtorture INHERITFLAGS rows where
// parent.AutoInherited is false but the parent ACE pre-carried the bit.
//
// NO_PROPAGATE on the parent strips OI/CI/NP from any emitted ACE so
// further inheritance stops at this child.
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
		// Check BEFORE appending so prior dual-emission is honored.
		// Earlier parent ACEs take precedence over later ones.
		if len(inherited) >= MaxACECount {
			slog.Debug("acl.ComputeInheritedACL: MaxACECount reached — truncating remaining parent ACEs",
				"max", MaxACECount, "produced", len(inherited),
				"remaining_parent_aces", len(parentACL.ACEs)-i)
			break
		}

		// MS-DTYP §2.5.3.4.2 / Samba calculate_inherited_from_parent
		// (libcli/security/create_descriptor.c): the INHERITED_ACE bit on
		// the child is set ONLY when the parent SD has AUTO_INHERITED set
		// (after canonicalization). Samba's torture tflags table makes
		// this explicit — the child gets INHERITED_ACE iff both
		// AUTO_INHERITED and AUTO_INHERIT_REQ were set on the parent SD,
		// which canonicalize down to parent.AutoInherited. The parent
		// ACE's pre-existing INHERITED_ACE bit is NOT propagated
		// independently; that was over-broad and caused smbtorture
		// INHERITFLAGS rows i ∈ {8,9,10,12,13,14} to fail.
		inheritedBit := uint32(0)
		if parentACL.AutoInherited {
			inheritedBit = ACE4_INHERITED_ACE
		}

		applies := (isDirectory && hasCI) || (!isDirectory && hasOI)
		isCreator := ace.Who == SpecialCreatorOwner || ace.Who == SpecialCreatorGroup
		expandACE := isCreator

		if applies && expandACE {
			// Case 1: emit resolved sibling, plus preserved CREATOR on dir
			// children (file children are leaves; no preserved emission).
			resolved := *ace
			resolved.Who = substituteCreator(ace.Who, creator)
			// Resolved sibling applies AT THIS object only; clear all
			// inheritance flags then add INHERITED_ACE per the rule.
			resolved.Flag = inheritedBit
			inherited = append(inherited, resolved)

			if !isDirectory {
				continue
			}
			// Cap check before preserved emission.
			if len(inherited) >= MaxACECount {
				slog.Debug("acl.ComputeInheritedACL: dropping preserved CREATOR ACE — MaxACECount reached",
					"max", MaxACECount, "produced", len(inherited),
					"principal", ace.Who)
				continue
			}
			preserved := *ace
			// Principal stays CREATOR_OWNER / CREATOR_GROUP — substitution
			// happens at grandchild create time.
			preserved.Flag &^= inheritanceMask
			if hasOI {
				preserved.Flag |= ACE4_FILE_INHERIT_ACE
			}
			if hasCI {
				preserved.Flag |= ACE4_DIRECTORY_INHERIT_ACE
			}
			preserved.Flag |= ACE4_INHERIT_ONLY_ACE
			// NO_PROPAGATE: stop further propagation by clearing OI/CI/NP.
			// (NB: hasNP && hasCI is the only NP path that reaches here for
			// dir children; resolved-only emission below handles
			// NP|OI-only and other shapes.)
			if hasNP {
				preserved.Flag &^= inheritanceMask
			}
			preserved.Flag &^= ACE4_INHERITED_ACE
			preserved.Flag |= inheritedBit
			inherited = append(inherited, preserved)
			continue
		}

		if applies && !expandACE {
			// Case 2: single ACE with original principal.
			//
			// Samba `calculate_inherited_from_parent`:
			//   - NO_PROPAGATE on parent ACE → child gets ALL inheritance
			//     bits (OI/CI/NP/IO) stripped. NP says "do not propagate
			//     further", which is enforced by removing every
			//     inheritance flag from the child ACE. smbtorture
			//     INHERITANCE rows 6/7/14/15 (NP+CI shapes on dir child)
			//     require flag=0 (only INHERITED_ACE remains via
			//     inheritedBit below).
			//   - File child → no propagation possible; strip all
			//     inheritance bits.
			//   - Otherwise (dir child, NP unset) → preserve OI/CI/NP on
			//     parent ACE so grandchildren still inherit; clear
			//     INHERIT_ONLY because the ACE is effective at this dir.
			newACE := *ace
			switch {
			case hasNP:
				newACE.Flag &^= inheritanceMask
			case !isDirectory:
				newACE.Flag &^= inheritanceMask
			default:
				newACE.Flag &^= ACE4_INHERIT_ONLY_ACE
			}
			newACE.Flag &^= ACE4_INHERITED_ACE
			newACE.Flag |= inheritedBit
			inherited = append(inherited, newACE)
			continue
		}

		// Case 3: !applies — parent ACE does not apply at this child, but
		// must propagate to grandchildren. Emit a preserved ACE with
		// original principal (CREATOR_OWNER stays CREATOR_OWNER for
		// future grandchild substitution).
		//
		// Only reachable when isDirectory && hasOI && !hasCI (parent OI
		// only on dir child), since other !applies shapes were filtered
		// out at the top of the loop. NO_PROPAGATE drops this entirely
		// (no grandchildren to propagate to).
		if hasNP {
			continue
		}
		preserved := *ace
		// Principal stays as-is — including CREATOR placeholders (not
		// reached by isCreator branch above because applies==false here).
		preserved.Flag &^= inheritanceMask
		preserved.Flag |= ACE4_FILE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE
		preserved.Flag |= inheritedBit
		inherited = append(inherited, preserved)
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
