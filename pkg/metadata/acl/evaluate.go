package acl

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// builtinAdministratorsSID is BUILTIN\Administrators — the SID Windows
// uses to denote the local administrators group. Members of this group
// hold SeTakeOwnershipPrivilege by default per MS-DTYP §2.5.3.2.
const builtinAdministratorsSID = "S-1-5-32-544"

// domainAdminSIDPattern matches the local/domain Administrator account SID
// (S-1-5-21-{auth1}-{auth2}-{auth3}-500). Mirrors the pattern in
// pkg/metadata/auth_identity.go::IsAdministratorSID. Kept local to the
// acl package to avoid a cyclic import from pkg/metadata.
var domainAdminSIDPattern = regexp.MustCompile(`^S-1-5-21-\d+-\d+-\d+-500$`)

// HasTakeOwnershipPrivilege reports whether the given requester SID +
// group SIDs imply SeTakeOwnershipPrivilege. By default this is granted
// only to BUILTIN\Administrators (S-1-5-32-544) members and the
// local/domain Administrator (RID 500) account.
//
// Used by callers building EvaluateContext.RequesterHasTakeOwnership so
// the MS-DTYP §2.5.3.2 owner-implicit WRITE_OWNER grant is restricted
// to admins, matching Samba access_check.c::se_access_check_implicit_owner.
func HasTakeOwnershipPrivilege(requesterSID string, groupSIDs []string) bool {
	if isAdminSID(requesterSID) {
		return true
	}
	for _, g := range groupSIDs {
		if isAdminSID(g) {
			return true
		}
	}
	return false
}

func isAdminSID(sid string) bool {
	if sid == "" {
		return false
	}
	if sid == builtinAdministratorsSID {
		return true
	}
	return domainAdminSIDPattern.MatchString(sid)
}

// localDomainSuffix is the synthetic domain string SIDMapper.SIDToPrincipal
// produces for machine-domain user/group SIDs ("{uid}@localdomain"). When an
// SD round-trips through parse → store → evaluate, ACE.Who is in this form
// for any SID that maps to a Unix UID/GID. Matching it against the requester
// is by numeric UID/GID, not the username-form evalCtx.Who.
const localDomainSuffix = "@localdomain"

// AnonymousFileOwnerUID is the sentinel callers must use for
// EvaluateContext.FileOwnerUID when building a context for an anonymous /
// unauthenticated requester. It guarantees the OWNER@ identity arm
// (UID == FileOwnerUID) and the MS-DTYP §2.5.3.2 owner-implicit RC|WRITE_DAC
// pass both miss, regardless of the file's real owner.
//
// Without this sentinel, an anonymous requester (Go zero-value UID == 0)
// matches OWNER@ on every root-owned file and receives the implicit
// READ_CONTROL|WRITE_DAC grant on top — handing privileged DACL-edit rights
// to unauthenticated sessions. 0xFFFFFFFF mirrors the "nobody"/invalid-UID
// convention already used in file_modify.go and validation.go and is outside
// any realistic POSIX UID range. See #540.
const AnonymousFileOwnerUID uint32 = ^uint32(0)

// EvaluateContext carries the requestor's identity and the file's
// owner/group for dynamic OWNER@/GROUP@/EVERYONE@ resolution.
type EvaluateContext struct {
	Who          string   // Named principal (e.g., "alice@example.com")
	UID          uint32   // Requestor's effective UID
	GID          uint32   // Requestor's effective primary GID
	GIDs         []uint32 // Requestor's supplementary GIDs
	FileOwnerUID uint32   // File's owner UID (for OWNER@ resolution)
	FileOwnerGID uint32   // File's owning group GID (for GROUP@ resolution)

	// SID is the requester's Windows SID in canonical string form
	// (e.g. "S-1-5-21-A-B-C-RID"). Empty when the session is POSIX-only.
	SID string

	// GroupSIDs is the requester's group SIDs.
	// Empty when the session is POSIX-only.
	GroupSIDs []string

	// RequesterHasTakeOwnership indicates whether the requester holds the
	// SeTakeOwnershipPrivilege (MS-DTYP §2.5.3.2). Only privilege holders
	// receive WRITE_OWNER implicitly when they own the file. On Windows
	// this privilege is granted to BUILTIN\Administrators by default.
	// Callers should set this from IsAdministratorSID(SID) or equivalent
	// admin-group membership detection. POSIX-only sessions default to
	// false; UID==0 callers are usually short-circuited above acl.Evaluate.
	RequesterHasTakeOwnership bool
}

// HasExplicitDeny reports whether the ACL contains any explicit DENY ACE.
// Allow-only ACLs are additive and may safely coexist with a share-level
// write grant; an ACL with a DENY ACE encodes intent that POSIX bits
// cannot express and must take precedence.
func HasExplicitDeny(a *ACL) bool {
	if a == nil {
		return false
	}
	for i := range a.ACEs {
		if a.ACEs[i].Type == ACE4_ACCESS_DENIED_ACE_TYPE {
			return true
		}
	}
	return false
}

// ownerImplicitRights is the set of access rights MS-DTYP §2.5.3.2 grants
// implicitly to the file owner: READ_CONTROL and WRITE_DAC. These are
// layered on top of the explicit DACL grants unless suppressed by an
// effective OWNER_RIGHTS (S-1-3-4) ACE in the DACL. Explicit DENY ACEs in
// the DACL still win over the implicit grant for any specific bit.
//
// WRITE_OWNER is NOT in this base set per MS-DTYP §2.5.3.2: it requires
// SeTakeOwnershipPrivilege (only Administrators by default). Callers grant
// it by setting EvaluateContext.RequesterHasTakeOwnership; see the
// owner-implicit pass in Evaluate. This mirrors Samba
// libcli/security/access_check.c::se_access_check_implicit_owner.
//
// NFSv4 mask bits intentionally share positions with Windows MS-DTYP rights
// (RFC 7530 §6.2.1), so the same constants describe both layers.
const ownerImplicitRights = ACE4_READ_ACL | ACE4_WRITE_ACL

// takeOwnershipImplicitRight is the additional implicit grant a requester
// receives when they own the file AND hold SeTakeOwnershipPrivilege per
// MS-DTYP §2.5.3.2. Layered on top of ownerImplicitRights, masked by
// deniedBits like the base owner-implicit grants.
const takeOwnershipImplicitRight = ACE4_WRITE_OWNER

// Evaluate implements the NFSv4 ACL evaluation algorithm per RFC 7530
// Section 6.2.1, with the OWNER_RIGHTS (S-1-3-4) override and the owner
// implicit-grant rule from MS-DTYP §2.5.3 / §2.4.10 / §2.5.3.2. It
// processes ACEs sequentially and returns true only if ALL requested
// permission bits are explicitly allowed (or implicitly granted to the
// owner per §2.5.3.2).
//
// The algorithm:
//  1. Pre-scan (owner only): detect whether any effective OWNER_RIGHTS
//     ACCESS_ALLOWED/ACCESS_DENIED ACE is present. When present AND the
//     requester is the file owner, the OWNER_RIGHTS ACEs become the sole
//     authority for the owner: OWNER@ ACEs are ignored for matching
//     purposes (they would otherwise supersede the OWNER_RIGHTS decision
//     under first-match-wins) AND the §2.5.3.2 implicit owner grants are
//     suppressed. This mirrors Samba
//     `libcli/security/access_check.c::se_access_check`. AUDIT/ALARM
//     ACEs do not affect access decisions and therefore do not trigger
//     the override; non-owner requesters skip the pre-scan entirely.
//  2. Process ACEs in order.
//  3. Skip ACEs with INHERIT_ONLY flag (they only apply to children).
//  4. For each matching ACE, record newly decided bits:
//     - ALLOW: mark undecided bits as allowed
//     - DENY: mark undecided bits as denied
//     - AUDIT/ALARM: skip (store-only per project decision)
//  5. Once all requested bits are decided, stop early.
//  6. Owner-implicit pass (MS-DTYP §2.5.3.2): if the requester is the file
//     owner AND no OWNER_RIGHTS ACE is present, add `ownerImplicitRights`
//     (READ_CONTROL|WRITE_DAC) to `allowedBits`, masked by the bits not
//     already denied. WRITE_OWNER is added on top ONLY when the requester
//     holds SeTakeOwnershipPrivilege (evalCtx.RequesterHasTakeOwnership).
//     Explicit DENY in the DACL still wins per-bit.
//  7. Return true if and only if ALL requested bits are in allowedBits.
func Evaluate(a *ACL, evalCtx *EvaluateContext, requestedMask uint32) bool {
	if requestedMask == 0 {
		return true
	}
	// A nil ACL means "no DACL at all" — distinct from an empty DACL.
	// An empty DACL (len(a.ACEs) == 0) is the MS-DTYP §2.5.3 "deny all"
	// case but §2.5.3.2 still grants the file owner READ_CONTROL|
	// WRITE_DAC|WRITE_OWNER, so we must fall through to the owner-implicit
	// pass below rather than short-circuiting here.
	if a == nil {
		return false
	}

	// Null DACL (SE_DACL_PRESENT but no DACL body) grants everyone full access.
	if a.NullDACL {
		return true
	}

	// First pass: does this DACL contain any effective OWNER_RIGHTS ACE
	// that affects access decisions? The override only matters when the
	// requester is the file owner, so non-owners skip the scan entirely.
	// AUDIT/ALARM ACEs are evaluation no-ops and must not trigger the
	// override — only ACCESS_ALLOWED/ACCESS_DENIED count. Inherit-only
	// entries don't apply to access checks, so they don't count either.
	requesterIsOwner := evalCtx.UID == evalCtx.FileOwnerUID
	var ownerRightsPresent bool
	if requesterIsOwner {
		for i := range a.ACEs {
			ace := &a.ACEs[i]
			if ace.IsInheritOnly() {
				continue
			}
			if ace.Who != SpecialOwnerRights {
				continue
			}
			if ace.Type != ACE4_ACCESS_ALLOWED_ACE_TYPE &&
				ace.Type != ACE4_ACCESS_DENIED_ACE_TYPE {
				continue
			}
			ownerRightsPresent = true
			break
		}
	}

	var allowedBits uint32
	var deniedBits uint32

	for i := range a.ACEs {
		ace := &a.ACEs[i]

		// Skip inherit-only ACEs during evaluation (Pitfall 2).
		if ace.IsInheritOnly() {
			continue
		}

		// Check if this ACE applies to the requestor.
		if !aceMatchesWhoWithOwnerRights(ace, evalCtx, ownerRightsPresent) {
			continue
		}

		switch ace.Type {
		case ACE4_ACCESS_ALLOWED_ACE_TYPE:
			// Only consider bits not yet decided.
			newBits := ace.AccessMask &^ (allowedBits | deniedBits)
			allowedBits |= newBits

		case ACE4_ACCESS_DENIED_ACE_TYPE:
			// Only consider bits not yet decided.
			newBits := ace.AccessMask &^ (allowedBits | deniedBits)
			deniedBits |= newBits

		case ACE4_SYSTEM_AUDIT_ACE_TYPE, ACE4_SYSTEM_ALARM_ACE_TYPE:
			// AUDIT/ALARM ACEs are store-only: skip during evaluation.
			continue
		}

		// Early termination: all requested bits have been decided.
		if (allowedBits|deniedBits)&requestedMask == requestedMask {
			break
		}
	}

	// MS-DTYP §2.5.3.2 owner implicit grants: layered after the DACL walk
	// so explicit DENY ACEs still win per-bit. Suppressed when OWNER_RIGHTS
	// is present (§2.5.3 — OWNER_RIGHTS is the sole authority for the owner).
	//
	// The base set is READ_CONTROL|WRITE_DAC; WRITE_OWNER is layered in
	// only when the requester holds SeTakeOwnershipPrivilege (admins).
	// This mirrors Samba access_check.c::se_access_check_implicit_owner:
	// non-privileged owners cannot reassign ownership via the implicit
	// grant — they need either an explicit WRITE_OWNER ACE or the
	// privilege. Tests under smb2.acls (DENY1, MXAC-NOT-GRANTED, OWNER)
	// rely on this split for the MxAc reply.
	if requesterIsOwner && !ownerRightsPresent {
		allowedBits |= ownerImplicitRights &^ deniedBits
		if evalCtx.RequesterHasTakeOwnership {
			allowedBits |= takeOwnershipImplicitRight &^ deniedBits
		}
	}

	// Access is granted only if ALL requested bits are in allowedBits.
	return (allowedBits & requestedMask) == requestedMask
}

// aceMatchesWhoWithOwnerRights checks if an ACE applies to the given
// evaluation context. When ownerRightsPresent is true AND the requester is the
// file owner, OWNER@ ACEs are made to not-match and OWNER_RIGHTS ACEs are
// made to match — implementing MS-DTYP §2.5.3 / §2.4.10 (S-1-3-4): an
// explicit OWNER_RIGHTS entry supersedes the implicit owner grants and
// becomes the sole authority for the owner's effective rights. Pass false to
// treat OWNER_RIGHTS as an ordinary owner-matching arm.
//
// Resolution rules for special identifiers (dynamic resolution):
//   - "OWNER@": matches when requestor UID == file owner UID
//   - "GROUP@": matches when requestor primary GID == file group GID,
//     or file group GID is in requestor's supplementary GIDs
//   - "EVERYONE@": always matches
//   - Otherwise: exact string match on named principal
//
// Mirrors Samba `libcli/security/access_check.c::se_access_check` handling
// of `SID_OWNER_RIGHTS` / `SEC_RIGHTS_OWNER_RIGHTS`.
func aceMatchesWhoWithOwnerRights(ace *ACE, evalCtx *EvaluateContext, ownerRightsPresent bool) bool {
	requesterIsOwner := evalCtx.UID == evalCtx.FileOwnerUID

	switch ace.Who {
	case SpecialOwner:
		// OWNER_RIGHTS, when present, strips the file owner of the
		// implicit OWNER@ identity for the purposes of this DACL walk.
		// Non-owners are unaffected.
		if ownerRightsPresent && requesterIsOwner {
			return false
		}
		return requesterIsOwner

	case SpecialGroup:
		if evalCtx.GID == evalCtx.FileOwnerGID {
			return true
		}
		for _, gid := range evalCtx.GIDs {
			if gid == evalCtx.FileOwnerGID {
				return true
			}
		}
		return false

	case SpecialEveryone:
		return true

	case SpecialOwnerRights:
		// MS-DTYP §2.5.3 / §2.4.10: OWNER_RIGHTS (S-1-3-4) ACEs match the
		// file owner only. They have no meaning for non-owner requesters.
		return requesterIsOwner

	default:
		// SID-form ACE (set by SD parse): "sid:<canonical SID>".
		if strings.HasPrefix(ace.Who, "sid:") {
			target := ace.Who[len("sid:"):]
			if evalCtx.SID != "" && evalCtx.SID == target {
				return true
			}
			return slices.Contains(evalCtx.GroupSIDs, target)
		}
		// Machine-domain SIDs round-trip through SIDMapper.SIDToPrincipal as
		// "{uid}@localdomain" (users) or "{gid}@localdomain" (groups). Match
		// numerically against the requester's UID/GIDs so an ACE written with
		// a raw machine-domain SID resolves the same as an OWNER@/GROUP@ ACE.
		if id, ok := parseLocalDomainID(ace.Who); ok {
			if id == evalCtx.UID {
				return true
			}
			if id == evalCtx.GID {
				return true
			}
			return slices.Contains(evalCtx.GIDs, id)
		}
		// Legacy/string match (named principal).
		return ace.Who == evalCtx.Who
	}
}

// parseLocalDomainID extracts the numeric prefix from a "{N}@localdomain"
// ACE Who string. Returns (id, true) for valid forms only — non-numeric
// prefixes and any other suffix yield (0, false).
func parseLocalDomainID(who string) (uint32, bool) {
	prefix, ok := strings.CutSuffix(who, localDomainSuffix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseUint(prefix, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(id), true
}
