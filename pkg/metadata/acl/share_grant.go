package acl

import (
	"fmt"
	"sort"
)

// GrantLevel is the access a share-permission grant projects onto the share
// root directory. It mirrors the control-plane SharePermission levels without
// importing that package (the acl package stays protocol/control-plane
// agnostic).
type GrantLevel int

const (
	// GrantNone projects no ACE (the principal is not granted root access).
	GrantNone GrantLevel = iota
	// GrantRead projects read + traverse (r-x).
	GrantRead
	// GrantReadWrite projects read + write + traverse (rwx).
	GrantReadWrite
	// GrantAdmin projects full control, including WRITE_ACL / WRITE_OWNER.
	GrantAdmin
)

// RootGrant is a single principal's projected access on a share root.
type RootGrant struct {
	// ID is the principal's Unix UID (user grant) or GID (group grant).
	ID uint32
	// IsGroup distinguishes a group grant from a user grant. Both project to
	// the same "{id}@localdomain" Who form (see LocalDomainPrincipal); the
	// flag only affects deterministic ACE ordering.
	IsGroup bool
	// Level is the access level to grant.
	Level GrantLevel
}

// LocalDomainPrincipal formats a Unix UID/GID as the "{id}@localdomain" ACE
// Who string the ACL evaluator matches numerically against an AuthContext's
// UID/GID. It is the inverse of the evaluator's local-domain parser and the
// form SIDMapper.SIDToPrincipal emits for machine-domain SIDs, kept here so
// producers and the matcher never drift. Both NFS (AUTH_UNIX uid) and SMB
// (mapped user uid) populate the numeric identity, so this form is the
// cross-protocol common denominator (a "sid:" Who would not match NFS).
func LocalDomainPrincipal(id uint32) string {
	return fmt.Sprintf("%d%s", id, localDomainSuffix)
}

// maskForLevel maps a grant level to a directory access mask. Read includes
// EXECUTE so the principal can traverse and list the directory; ReadWrite adds
// the write bits (including DELETE_CHILD for directories); Admin is full
// control.
func maskForLevel(level GrantLevel) uint32 {
	switch level {
	case GrantRead:
		return rwxToFullMask(modeRead|modeExecute, true)
	case GrantReadWrite:
		return rwxToFullMask(modeRead|modeWrite|modeExecute, true)
	case GrantAdmin:
		return FullAccessMask
	default: // GrantNone or unknown
		return 0
	}
}

// BuildShareRootACL synthesizes the share root directory's DACL from the set of
// share-permission grants, so the filesystem permission layer agrees with the
// share-level access control plane. Without this, a user granted read-write at
// the share level is still denied by the root directory's POSIX mode bits
// (owner uid 0, mode 0755) because they are neither the owner nor in its group.
//
// The result is allow-only and carries FILE_INHERIT|DIRECTORY_INHERIT flags so
// entries created under the root inherit consistent access. The share root
// owner always retains full control via the dynamic OWNER@ principal.
// defaultLevel projects the share's default-permission onto EVERYONE@ (omitted
// when none). Per-principal grants project onto "{id}@localdomain" ACEs.
func BuildShareRootACL(defaultLevel GrantLevel, grants []RootGrant) *ACL {
	const inheritFlags = ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE
	allow := func(who string, mask uint32) ACE {
		return ACE{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: inheritFlags, AccessMask: mask, Who: who}
	}

	aces := []ACE{
		// The root owner always keeps full control so the share stays
		// manageable regardless of which grants exist.
		allow(SpecialOwner, FullAccessMask),
		// SYSTEM and Administrators full control: the dfs admin maps to uid 0
		// and bypasses checks as superuser, but the explicit ACEs keep the
		// Windows Security tab coherent (matches the Windows default shape).
		allow(SpecialSystem, FullAccessMask),
		allow(SpecialAdministrators, FullAccessMask),
	}

	// Merge grants that project to the same ACE, keeping the highest level.
	// The merge key is the numeric id, not (id, isGroup): LocalDomainPrincipal
	// encodes only the id, so a user UID and a group GID with the same value
	// collapse to one Who (the known local-domain uid/gid collision) and must
	// not emit duplicate ACEs. Distinct users without an explicit UID also
	// collapse here (all to the same fallback id). A user grant takes
	// precedence over a group grant for ordering when ids collide, so the order
	// stays deterministic.
	merged := make(map[uint32]RootGrant, len(grants))
	for _, g := range grants {
		cur, ok := merged[g.ID]
		if !ok {
			merged[g.ID] = g
			continue
		}
		if g.Level > cur.Level {
			cur.Level = g.Level
		}
		if !g.IsGroup { // a user grant wins the IsGroup flag for ordering
			cur.IsGroup = false
		}
		merged[g.ID] = cur
	}
	sorted := make([]RootGrant, 0, len(merged))
	for _, g := range merged {
		sorted = append(sorted, g)
	}

	// Deterministic ACE order so the projected ACL is stable across
	// reconciliations (users before groups, each ascending by id).
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].IsGroup != sorted[j].IsGroup {
			return !sorted[i].IsGroup
		}
		return sorted[i].ID < sorted[j].ID
	})
	for _, g := range sorted {
		if mask := maskForLevel(g.Level); mask != 0 { // GrantNone → no ACE
			aces = append(aces, allow(LocalDomainPrincipal(g.ID), mask))
		}
	}

	// Default-permission projects onto EVERYONE@ so an authenticated principal
	// that passed the share layer without an explicit grant gets the same
	// access at the filesystem layer. Omitted entirely when none, preserving
	// the secure default.
	if m := maskForLevel(defaultLevel); m != 0 {
		aces = append(aces, allow(SpecialEveryone, m))
	}

	return &ACL{
		ACEs:   aces,
		Source: ACLSourceShareGrant,
	}
}
