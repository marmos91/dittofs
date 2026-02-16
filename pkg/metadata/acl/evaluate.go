package acl

// EvaluateContext carries the requestor's identity and the file's
// owner/group for dynamic OWNER@/GROUP@/EVERYONE@ resolution.
type EvaluateContext struct {
	Who          string   // Named principal (e.g., "alice@example.com")
	UID          uint32   // Requestor's effective UID
	GID          uint32   // Requestor's effective primary GID
	GIDs         []uint32 // Requestor's supplementary GIDs
	FileOwnerUID uint32   // File's owner UID (for OWNER@ resolution)
	FileOwnerGID uint32   // File's owning group GID (for GROUP@ resolution)
}

// Evaluate implements the NFSv4 ACL evaluation algorithm per RFC 7530
// Section 6.2.1. It processes ACEs sequentially and returns true only
// if ALL requested permission bits are explicitly allowed.
//
// The algorithm:
//  1. Process ACEs in order
//  2. Skip ACEs with INHERIT_ONLY flag (they only apply to children)
//  3. For each matching ACE, record newly decided bits:
//     - ALLOW: mark undecided bits as allowed
//     - DENY: mark undecided bits as denied
//     - AUDIT/ALARM: skip (store-only per project decision)
//  4. Once all requested bits are decided, stop early
//  5. Return true if and only if ALL requested bits are in allowedBits
func Evaluate(a *ACL, evalCtx *EvaluateContext, requestedMask uint32) bool {
	if requestedMask == 0 {
		return true
	}
	if a == nil || len(a.ACEs) == 0 {
		return false
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
		if !aceMatchesWho(ace, evalCtx) {
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

	// Access is granted only if ALL requested bits are in allowedBits.
	return (allowedBits & requestedMask) == requestedMask
}

// aceMatchesWho checks if an ACE applies to the given evaluation context.
//
// Resolution rules for special identifiers (Pitfall 3 - dynamic resolution):
//   - "OWNER@": matches when requestor UID == file owner UID
//   - "GROUP@": matches when requestor primary GID == file group GID,
//     or file group GID is in requestor's supplementary GIDs
//   - "EVERYONE@": always matches
//   - Otherwise: exact string match on named principal
func aceMatchesWho(ace *ACE, evalCtx *EvaluateContext) bool {
	switch ace.Who {
	case SpecialOwner:
		return evalCtx.UID == evalCtx.FileOwnerUID

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

	default:
		return ace.Who == evalCtx.Who
	}
}
