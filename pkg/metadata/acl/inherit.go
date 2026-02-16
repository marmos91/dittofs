package acl

// inheritanceMask covers all inheritance-related ACE flags.
const inheritanceMask = ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE |
	ACE4_NO_PROPAGATE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE

// ComputeInheritedACL computes the ACL to set on a newly created file or
// directory based on its parent's ACL per RFC 7530 Section 6.4.3.
//
// For files:
//   - Include ACEs with FILE_INHERIT flag
//   - Clear ALL inheritance flags (files don't propagate further)
//   - Add INHERITED_ACE flag
//
// For directories:
//   - Include ACEs with DIRECTORY_INHERIT flag
//   - Add INHERITED_ACE flag
//   - If NO_PROPAGATE_INHERIT: clear all inheritance flags (stop propagation)
//   - If INHERIT_ONLY on parent: clear INHERIT_ONLY on child (ACE now applies)
//
// Returns nil if parentACL is nil or no ACEs are inheritable.
func ComputeInheritedACL(parentACL *ACL, isDirectory bool) *ACL {
	if parentACL == nil {
		return nil
	}

	inheritFlag := uint32(ACE4_FILE_INHERIT_ACE)
	if isDirectory {
		inheritFlag = ACE4_DIRECTORY_INHERIT_ACE
	}

	var inherited []ACE

	for i := range parentACL.ACEs {
		ace := &parentACL.ACEs[i]

		if ace.Flag&inheritFlag == 0 {
			continue
		}

		newACE := *ace
		newACE.Flag |= ACE4_INHERITED_ACE

		if !isDirectory {
			// Files don't propagate further: clear all inheritance flags.
			newACE.Flag &^= inheritanceMask
		} else if ace.Flag&ACE4_NO_PROPAGATE_INHERIT_ACE != 0 {
			// NO_PROPAGATE: stop propagation to grandchildren.
			newACE.Flag &^= inheritanceMask
		} else if ace.Flag&ACE4_INHERIT_ONLY_ACE != 0 {
			// INHERIT_ONLY on parent: clear so ACE applies on this child.
			newACE.Flag &^= ACE4_INHERIT_ONLY_ACE
		}

		inherited = append(inherited, newACE)
	}

	if len(inherited) == 0 {
		return nil
	}

	return &ACL{ACEs: inherited}
}

// PropagateACL replaces the inherited ACEs of an existing ACL with newly
// computed inherited ACEs from a parent, while preserving explicit ACEs.
//
// This is used for recursive ACL propagation: when a parent's ACL changes,
// each descendant's inherited ACEs are recomputed but its explicit ACEs
// (those without the INHERITED_ACE flag) are kept intact.
//
// The result maintains canonical ordering: explicit ACEs first, then inherited.
// Returns nil if both newly computed and existing explicit ACEs are empty.
func PropagateACL(parentACL *ACL, existingACL *ACL, isDirectory bool) *ACL {
	newInherited := ComputeInheritedACL(parentACL, isDirectory)

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

	return &ACL{ACEs: combined}
}
