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
	// SID, when non-empty, is a direct Active Directory grant (#1528): it
	// projects an additional "sid:<SID>" ACE so a Kerberos/PAC login is matched
	// by its Windows user/group SID (the SMB path). Such a grant may ALSO carry
	// a non-zero ID — a Unix GID allocated for the SID — so NFS, which has no
	// SID on the wire, matches via the "{id}@localdomain" numeric form.
	SID string
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
// The result is allow-only and applies to the root directory ONLY — the ACEs
// carry no inheritance flags. Share grants are the outer gate (the per-request
// ResolveSharePermission check already enforces read vs read-write); the inner,
// per-file access is governed by each entry's own POSIX mode / ACL. Projecting
// inheritable grant ACEs onto every descendant would make a grantee bypass
// those per-file bits (and breaks POSIX-conformance suites that assert mode-bit
// enforcement). The share root owner always retains full control via the
// dynamic OWNER@ principal. defaultLevel projects the share's default-permission
// onto EVERYONE@ (omitted when none). Per-principal grants project onto
// "{id}@localdomain" ACEs.
func BuildShareRootACL(defaultLevel GrantLevel, grants []RootGrant) *ACL {
	allow := func(who string, mask uint32) ACE {
		return ACE{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: mask, Who: who}
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

	// Direct AD/SID grants (#1528): project a "sid:<SID>" ACE matched against a
	// login's PAC user/group SIDs (the SMB path). Dedup by SID keeping the
	// highest level; deterministic order by SID string.
	sidMerged := make(map[string]GrantLevel, len(grants))
	for _, g := range grants {
		if g.SID == "" {
			continue
		}
		if lvl, ok := sidMerged[g.SID]; !ok || g.Level > lvl {
			sidMerged[g.SID] = g.Level
		}
	}
	sidKeys := make([]string, 0, len(sidMerged))
	for s := range sidMerged {
		sidKeys = append(sidKeys, s)
	}
	sort.Strings(sidKeys)
	for _, s := range sidKeys {
		if mask := maskForLevel(sidMerged[s]); mask != 0 {
			aces = append(aces, allow("sid:"+s, mask))
		}
	}

	// Merge grants that project to the same ACE, keeping the highest level.
	// The merge key is the numeric id, not (id, isGroup): LocalDomainPrincipal
	// encodes only the id, so a user UID and a group GID with the same value
	// collapse to one Who (the known local-domain uid/gid collision) and must
	// not emit duplicate ACEs. Distinct users without an explicit UID also
	// collapse here (all to the same fallback id). A user grant takes
	// precedence over a group grant for ordering when ids collide, so the order
	// stays deterministic.
	//
	// A pure SID grant (SID set, no allocated Unix id) is excluded — it projects
	// only the "sid:" ACE above. A SID grant that carries a numeric GID for NFS
	// (#1528) has ID != 0 and IS merged here so it also gets a numeric ACE.
	merged := make(map[uint32]RootGrant, len(grants))
	for _, g := range grants {
		if g.SID != "" && g.ID == 0 {
			continue
		}
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
