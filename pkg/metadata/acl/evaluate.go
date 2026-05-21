package acl

import (
	"slices"
	"strings"
)

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
// implicitly to the file owner: READ_CONTROL, WRITE_DAC, and WRITE_OWNER.
// These are layered on top of the explicit DACL grants unless suppressed by
// an effective OWNER_RIGHTS (S-1-3-4) ACE in the DACL. Explicit DENY ACEs
// in the DACL still win over the implicit grant for any specific bit.
//
// NFSv4 mask bits intentionally share positions with Windows MS-DTYP rights
// (RFC 7530 §6.2.1), so the same constants describe both layers.
const ownerImplicitRights = ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER

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
//     owner AND no OWNER_RIGHTS ACE is present, OR `ownerImplicitRights`
//     into `allowedBits` for any of those bits not already denied. Explicit
//     DENY in the DACL still wins per-bit.
//  7. Return true if and only if ALL requested bits are in allowedBits.
func Evaluate(a *ACL, evalCtx *EvaluateContext, requestedMask uint32) bool {
	if requestedMask == 0 {
		return true
	}
	if a == nil {
		return false
	}
	// Empty DACL: per MS-DTYP §2.5.3 the explicit grant set is empty
	// ("deny all"). MS-DTYP §2.5.3.2 still applies — the file owner
	// receives the implicit READ_CONTROL|WRITE_DAC|WRITE_OWNER grants
	// even when no ACE is present. Fall through to the owner-implicit
	// pass below; non-owners terminate with an empty allowedBits.

	// First pass: does this DACL contain any effective OWNER_RIGHTS ACE
	// that affects access decisions? The override only matters when the
	// requester is the file owner, so non-owners skip the scan entirely.
	// AUDIT/ALARM ACEs are evaluation no-ops and must not trigger the
	// override — only ACCESS_ALLOWED/ACCESS_DENIED count. Inherit-only
	// entries don't apply to access checks, so they don't count either.
	var ownerRightsPresent bool
	if evalCtx.UID == evalCtx.FileOwnerUID {
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
	if evalCtx.UID == evalCtx.FileOwnerUID && !ownerRightsPresent {
		allowedBits |= ownerImplicitRights &^ deniedBits
	}

	// Access is granted only if ALL requested bits are in allowedBits.
	return (allowedBits & requestedMask) == requestedMask
}

// aceMatchesWho checks if an ACE applies to the given evaluation context,
// with OWNER_RIGHTS treated as an ordinary owner-matching arm. Callers that
// need MS-DTYP §2.5.3 OWNER_RIGHTS-vs-OWNER@ arbitration should use
// aceMatchesWhoWithOwnerRights instead.
//
// Resolution rules for special identifiers (Pitfall 3 - dynamic resolution):
//   - "OWNER@": matches when requestor UID == file owner UID
//   - "GROUP@": matches when requestor primary GID == file group GID,
//     or file group GID is in requestor's supplementary GIDs
//   - "EVERYONE@": always matches
//   - Otherwise: exact string match on named principal
func aceMatchesWho(ace *ACE, evalCtx *EvaluateContext) bool {
	return aceMatchesWhoWithOwnerRights(ace, evalCtx, false)
}

// aceMatchesWhoWithOwnerRights is the OWNER_RIGHTS-aware variant of
// aceMatchesWho. When ownerRightsPresent is true AND the requester is the
// file owner, OWNER@ ACEs are made to not-match and OWNER_RIGHTS ACEs are
// made to match — implementing MS-DTYP §2.5.3 / §2.4.10 (S-1-3-4): an
// explicit OWNER_RIGHTS entry supersedes the implicit owner grants and
// becomes the sole authority for the owner's effective rights.
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
		// Legacy/string match (numeric uid, named principal).
		return ace.Who == evalCtx.Who
	}
}
