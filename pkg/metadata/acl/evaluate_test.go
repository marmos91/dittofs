package acl

import "testing"

// ownerCtx returns an EvaluateContext where the requestor IS the file owner.
func ownerCtx() *EvaluateContext {
	return &EvaluateContext{
		Who:          "alice@example.com",
		UID:          1000,
		GID:          1000,
		GIDs:         nil,
		FileOwnerUID: 1000,
		FileOwnerGID: 1000,
	}
}

// nonOwnerCtx returns an EvaluateContext where the requestor is NOT the owner.
func nonOwnerCtx() *EvaluateContext {
	return &EvaluateContext{
		Who:          "bob@example.com",
		UID:          1001,
		GID:          1001,
		GIDs:         nil,
		FileOwnerUID: 1000,
		FileOwnerGID: 1000,
	}
}

// groupMemberCtx returns an EvaluateContext where the requestor is a member
// of the file's owning group via supplementary GIDs.
func groupMemberCtx() *EvaluateContext {
	return &EvaluateContext{
		Who:          "charlie@example.com",
		UID:          1002,
		GID:          1002,
		GIDs:         []uint32{1000}, // member of file's group
		FileOwnerUID: 1000,
		FileOwnerGID: 1000,
	}
}

func TestEvaluate_AllowOnly(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialEveryone},
		},
	}

	ctx := ownerCtx()

	// All requested bits are allowed.
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected READ_DATA to be allowed")
	}
	if !Evaluate(a, ctx, ACE4_EXECUTE) {
		t.Error("expected EXECUTE to be allowed")
	}
	if !Evaluate(a, ctx, ACE4_READ_DATA|ACE4_EXECUTE) {
		t.Error("expected READ_DATA|EXECUTE to be allowed")
	}

	// Request a bit that is not in the ALLOW mask.
	if Evaluate(a, ctx, ACE4_WRITE_DATA) {
		t.Error("expected WRITE_DATA to be denied (not in ACL)")
	}
}

func TestEvaluate_DenyBeforeAllow(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialEveryone},
		},
	}

	ctx := ownerCtx()

	// READ_DATA should be allowed (DENY doesn't cover it).
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected READ_DATA to be allowed")
	}

	// WRITE_DATA should be denied (DENY comes first).
	if Evaluate(a, ctx, ACE4_WRITE_DATA) {
		t.Error("expected WRITE_DATA to be denied")
	}

	// Combined: READ + WRITE should be denied because WRITE is denied.
	if Evaluate(a, ctx, ACE4_READ_DATA|ACE4_WRITE_DATA) {
		t.Error("expected READ_DATA|WRITE_DATA to be denied")
	}
}

func TestEvaluate_InheritOnlySkipped(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			// This ACE has INHERIT_ONLY: should not affect evaluation on THIS object.
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_INHERIT_ONLY_ACE | ACE4_FILE_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialEveryone,
			},
			// This ACE does apply.
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: ACE4_EXECUTE,
				Who:        SpecialEveryone,
			},
		},
	}

	ctx := ownerCtx()

	// READ_DATA comes from an INHERIT_ONLY ACE: should be denied on this object.
	if Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected READ_DATA to be denied (INHERIT_ONLY ACE should be skipped)")
	}

	// EXECUTE is allowed by the second ACE.
	if !Evaluate(a, ctx, ACE4_EXECUTE) {
		t.Error("expected EXECUTE to be allowed")
	}
}

func TestEvaluate_OwnerDynamic(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		},
	}

	// Owner should match.
	if !Evaluate(a, ownerCtx(), ACE4_READ_DATA) {
		t.Error("expected OWNER@ to match the file owner")
	}

	// Non-owner should not match.
	if Evaluate(a, nonOwnerCtx(), ACE4_READ_DATA) {
		t.Error("expected OWNER@ to NOT match a non-owner")
	}
}

func TestEvaluate_GroupDynamic(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		},
	}

	// Primary GID matches file group.
	ctx := &EvaluateContext{
		UID: 1001, GID: 1000, FileOwnerUID: 1000, FileOwnerGID: 1000,
	}
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected GROUP@ to match via primary GID")
	}

	// Supplementary GID matches file group.
	if !Evaluate(a, groupMemberCtx(), ACE4_READ_DATA) {
		t.Error("expected GROUP@ to match via supplementary GID")
	}

	// No group match.
	ctx = &EvaluateContext{
		UID: 1001, GID: 2000, GIDs: []uint32{3000, 4000},
		FileOwnerUID: 1000, FileOwnerGID: 1000,
	}
	if Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected GROUP@ to NOT match when not in file group")
	}
}

func TestEvaluate_EveryoneAlwaysMatches(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	// Any requestor should match EVERYONE@.
	ctx := &EvaluateContext{UID: 99999, GID: 99999, FileOwnerUID: 0, FileOwnerGID: 0}
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected EVERYONE@ to match any requestor")
	}
}

func TestEvaluate_EmptyACLDeniesAll(t *testing.T) {
	a := &ACL{ACEs: []ACE{}}

	if Evaluate(a, ownerCtx(), ACE4_READ_DATA) {
		t.Error("expected empty ACL to deny all access")
	}
}

func TestEvaluate_NilACLDeniesAll(t *testing.T) {
	if Evaluate(nil, ownerCtx(), ACE4_READ_DATA) {
		t.Error("expected nil ACL to deny all access")
	}
}

func TestEvaluate_ZeroMaskAllowed(t *testing.T) {
	a := &ACL{ACEs: []ACE{}}

	// Requesting zero bits should succeed (vacuously true).
	if !Evaluate(a, ownerCtx(), 0) {
		t.Error("expected zero mask to be trivially allowed")
	}
}

func TestEvaluate_MultipleMaskBits(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone},
		},
	}

	ctx := ownerCtx()

	// Both bits should be allowed from separate ACEs.
	if !Evaluate(a, ctx, ACE4_READ_DATA|ACE4_WRITE_DATA) {
		t.Error("expected READ_DATA|WRITE_DATA to be allowed from two ACEs")
	}
}

func TestEvaluate_EarlyTermination(t *testing.T) {
	// All requested bits are decided by the first ACE; the second ACE
	// (which would deny) should never be reached in effect.
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	ctx := ownerCtx()

	// The ALLOW comes first, so READ_DATA should be allowed.
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected READ_DATA to be allowed (early termination after first ACE)")
	}
}

func TestEvaluate_AuditAlarmSkipped(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			{Type: ACE4_SYSTEM_ALARM_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	ctx := ownerCtx()

	// AUDIT and ALARM ACEs should be skipped; the ALLOW ACE should grant access.
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected AUDIT/ALARM ACEs to be skipped and READ_DATA allowed")
	}
}

func TestEvaluate_NamedPrincipal(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: "alice@example.com"},
		},
	}

	// Exact match.
	ctx := &EvaluateContext{
		Who: "alice@example.com", UID: 1000, GID: 1000,
		FileOwnerUID: 0, FileOwnerGID: 0,
	}
	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected named principal to match")
	}

	// Different principal.
	ctx.Who = "bob@example.com"
	if Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected different named principal to NOT match")
	}
}

func TestEvaluate_GroupMembershipViaGIDs(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialGroup},
		},
	}

	// Requestor is in the file's group via supplementary GIDs.
	ctx := &EvaluateContext{
		UID: 2000, GID: 2000, GIDs: []uint32{3000, 1000},
		FileOwnerUID: 1000, FileOwnerGID: 1000,
	}
	if !Evaluate(a, ctx, ACE4_WRITE_DATA) {
		t.Error("expected GROUP@ to match via supplementary GIDs")
	}
}

func TestEvaluate_ComplexACL(t *testing.T) {
	// A realistic ACL: deny alice write, allow owner read/write, allow group read, allow everyone read.
	a := &ACL{
		ACEs: []ACE{
			// Explicit deny for alice.
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
			// Explicit allow for owner.
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialOwner},
			// Explicit allow for group.
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
			// Explicit allow for everyone.
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	// Alice (who IS the owner): WRITE_DATA should be denied by the first ACE.
	aliceCtx := &EvaluateContext{
		Who: "alice@example.com", UID: 1000, GID: 1000,
		FileOwnerUID: 1000, FileOwnerGID: 1000,
	}
	if Evaluate(a, aliceCtx, ACE4_WRITE_DATA) {
		t.Error("expected alice's WRITE to be denied by explicit DENY")
	}
	// Alice can still read (DENY only covers WRITE).
	if !Evaluate(a, aliceCtx, ACE4_READ_DATA) {
		t.Error("expected alice's READ to be allowed via OWNER@")
	}

	// Bob (owner UID=1000): WRITE should be allowed via OWNER@ ACE.
	bobOwnerCtx := &EvaluateContext{
		Who: "bob@example.com", UID: 1000, GID: 1000,
		FileOwnerUID: 1000, FileOwnerGID: 1000,
	}
	if !Evaluate(a, bobOwnerCtx, ACE4_WRITE_DATA) {
		t.Error("expected bob (owner) WRITE to be allowed via OWNER@")
	}

	// Random user not in group: only READ via EVERYONE@.
	randomCtx := &EvaluateContext{
		Who: "random@example.com", UID: 9999, GID: 9999,
		FileOwnerUID: 1000, FileOwnerGID: 1000,
	}
	if !Evaluate(a, randomCtx, ACE4_READ_DATA) {
		t.Error("expected random user READ allowed via EVERYONE@")
	}
	if Evaluate(a, randomCtx, ACE4_WRITE_DATA) {
		t.Error("expected random user WRITE denied (no matching ALLOW)")
	}
}

func TestEvaluate_DenyDoesNotAffectAlreadyDecidedBits(t *testing.T) {
	// ALLOW read first, then DENY read: the bit is already decided as allowed.
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	if !Evaluate(a, ownerCtx(), ACE4_READ_DATA) {
		t.Error("expected READ_DATA to be allowed (decided by first ACE)")
	}
}
