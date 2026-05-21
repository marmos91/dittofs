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

func TestAceMatchesWho_SIDForm(t *testing.T) {
	cases := []struct {
		name     string
		aceWho   string
		ctxSID   string
		ctxGSIDs []string
		want     bool
	}{
		{
			name:   "exact_user_sid_match",
			aceWho: "sid:S-1-5-21-1-2-3-1001",
			ctxSID: "S-1-5-21-1-2-3-1001",
			want:   true,
		},
		{
			name:   "user_sid_mismatch",
			aceWho: "sid:S-1-5-21-1-2-3-1001",
			ctxSID: "S-1-5-21-1-2-3-9999",
			want:   false,
		},
		{
			name:     "group_sid_match_via_groupSIDs",
			aceWho:   "sid:S-1-5-21-1-2-3-513",
			ctxSID:   "S-1-5-21-1-2-3-1001",
			ctxGSIDs: []string{"S-1-5-21-1-2-3-513"},
			want:     true,
		},
		{
			name:   "missing_ctx_sid",
			aceWho: "sid:S-1-5-21-1-2-3-1001",
			ctxSID: "",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ace := &ACE{Who: tc.aceWho, Type: ACE4_ACCESS_ALLOWED_ACE_TYPE}
			ctx := &EvaluateContext{SID: tc.ctxSID, GroupSIDs: tc.ctxGSIDs}
			got := aceMatchesWho(ace, ctx)
			if got != tc.want {
				t.Errorf("aceMatchesWho(%q, SID=%q, GroupSIDs=%v) = %v, want %v",
					tc.aceWho, tc.ctxSID, tc.ctxGSIDs, got, tc.want)
			}
		})
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

func TestHasExplicitDeny(t *testing.T) {
	t.Run("nil ACL returns false", func(t *testing.T) {
		if HasExplicitDeny(nil) {
			t.Error("expected HasExplicitDeny(nil) == false")
		}
	})

	t.Run("allow-only ACL returns false", func(t *testing.T) {
		a := &ACL{
			ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: SpecialEveryone, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: SpecialOwner, AccessMask: 0xFFFFFFFF},
			},
		}
		if HasExplicitDeny(a) {
			t.Error("expected HasExplicitDeny on allow-only ACL == false")
		}
	})

	t.Run("ACL with one DENY ACE returns true", func(t *testing.T) {
		a := &ACL{
			ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: SpecialEveryone, AccessMask: ACE4_READ_DATA},
				{Type: ACE4_ACCESS_DENIED_ACE_TYPE, Who: "sid:S-1-5-21-1-2-3-2001", AccessMask: ACE4_WRITE_DATA},
			},
		}
		if !HasExplicitDeny(a) {
			t.Error("expected HasExplicitDeny on ACL containing a DENY ACE == true")
		}
	})
}

func TestAceMatchesWho_OwnerRights(t *testing.T) {
	owner := uint32(1000)
	other := uint32(2000)
	cases := []struct {
		name         string
		requesterUID uint32
		fileOwnerUID uint32
		want         bool
	}{
		{"requester_is_owner", owner, owner, true},
		{"requester_not_owner", other, owner, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ace := &ACE{Who: SpecialOwnerRights, Type: ACE4_ACCESS_ALLOWED_ACE_TYPE}
			ctx := &EvaluateContext{
				UID:          tc.requesterUID,
				FileOwnerUID: tc.fileOwnerUID,
			}
			if got := aceMatchesWho(ace, ctx); got != tc.want {
				t.Errorf("aceMatchesWho(OwnerRights@, requester=%d, owner=%d) = %v, want %v",
					tc.requesterUID, tc.fileOwnerUID, got, tc.want)
			}
		})
	}
}

// TestEvaluate_OwnerRights_SuppressesOwnerAce covers MS-DTYP §2.5.3 OWNER_RIGHTS
// (S-1-3-4) semantics: when an OWNER_RIGHTS ACE is present in the DACL, the
// OWNER@ ACEs are no longer authoritative for the file owner. Only the
// OWNER_RIGHTS ACEs (Allow and Deny) decide what the owner is granted.
func TestEvaluate_OwnerRights_SuppressesOwnerAce(t *testing.T) {
	// OWNER_RIGHTS Allow READ + OWNER@ Allow WRITE → owner gets READ only.
	// Without the override, OWNER@ would grant WRITE first; with it, OWNER@
	// is ignored for owner identity and only OWNER_RIGHTS speaks.
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwnerRights},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialOwner},
		},
	}
	ctx := ownerCtx()

	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected owner to be allowed READ_DATA via OWNER_RIGHTS")
	}
	if Evaluate(a, ctx, ACE4_WRITE_DATA) {
		t.Error("expected owner to be denied WRITE_DATA (OWNER@ must be ignored when OWNER_RIGHTS is present)")
	}
	if Evaluate(a, ctx, ACE4_READ_DATA|ACE4_WRITE_DATA) {
		t.Error("expected owner to be denied combined READ_DATA|WRITE_DATA")
	}
}

// TestEvaluate_OwnerRights_DenyOverridesOwnerAllow covers the DENY case from
// MS-DTYP §2.5.3 and the smb2.acls.OWNER-RIGHTS-DENY family of tests: an
// OWNER_RIGHTS DENY ACE must take effect even when an OWNER@ ACE earlier in
// (or simply elsewhere in) the DACL would grant the same bits.
func TestEvaluate_OwnerRights_DenyOverridesOwnerAllow(t *testing.T) {
	// Order matters under first-match-wins: place OWNER_RIGHTS Allow READ
	// before the OWNER_RIGHTS Deny WRITE so that READ is granted via the
	// OWNER_RIGHTS authority. OWNER@ Allow FULL must be ignored entirely.
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwnerRights},
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialOwnerRights},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0xFFFFFFFF, Who: SpecialOwner},
		},
	}
	ctx := ownerCtx()

	if !Evaluate(a, ctx, ACE4_READ_DATA) {
		t.Error("expected owner to be allowed READ_DATA via OWNER_RIGHTS Allow")
	}
	if Evaluate(a, ctx, ACE4_WRITE_DATA) {
		t.Error("expected owner to be denied WRITE_DATA via OWNER_RIGHTS Deny")
	}
}

// TestEvaluate_NoOwnerRights_OwnerAceStillAuthoritative is the regression
// check: when no OWNER_RIGHTS ACE is in the DACL, OWNER@ continues to handle
// the file owner exactly as before. This guards against accidentally
// breaking the common case.
func TestEvaluate_NoOwnerRights_OwnerAceStillAuthoritative(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0xFFFFFFFF, Who: SpecialOwner},
		},
	}
	ctx := ownerCtx()

	if !Evaluate(a, ctx, ACE4_READ_DATA|ACE4_WRITE_DATA) {
		t.Error("expected owner to retain OWNER@ Allow FULL when no OWNER_RIGHTS present")
	}
}

// TestEvaluate_OwnerRights_NonOwnerUnaffected verifies that the OWNER_RIGHTS
// override only suppresses OWNER@ matching for the file owner. A non-owner
// requester observes normal OWNER@ semantics — i.e. OWNER@ never matched
// them anyway, and OWNER_RIGHTS also never matches them — so other ACEs
// (e.g. EVERYONE@) decide their effective access.
func TestEvaluate_OwnerRights_NonOwnerUnaffected(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwnerRights},
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialOwnerRights},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialEveryone},
		},
	}

	// Non-owner: OWNER_RIGHTS ACEs don't match them, so EVERYONE@ grants
	// both READ_DATA and WRITE_DATA.
	nonOwner := nonOwnerCtx()
	if !Evaluate(a, nonOwner, ACE4_READ_DATA|ACE4_WRITE_DATA) {
		t.Error("expected non-owner to be allowed READ_DATA|WRITE_DATA via EVERYONE@")
	}

	// Owner: OWNER_RIGHTS speaks for them — READ allowed, WRITE denied —
	// and EVERYONE@'s WRITE grant is shadowed by OWNER_RIGHTS Deny WRITE.
	owner := ownerCtx()
	if !Evaluate(a, owner, ACE4_READ_DATA) {
		t.Error("expected owner to be allowed READ_DATA via OWNER_RIGHTS Allow")
	}
	if Evaluate(a, owner, ACE4_WRITE_DATA) {
		t.Error("expected owner to be denied WRITE_DATA via OWNER_RIGHTS Deny (first-match-wins)")
	}
}

// TestEvaluate_OwnerRights_AuditAceDoesNotSuppressOwner verifies that an
// AUDIT (or ALARM) ACE for OwnerRights@ does NOT trigger the OWNER_RIGHTS
// override. AUDIT/ALARM ACEs are evaluation no-ops per RFC 7530 / MS-DTYP;
// only ACCESS_ALLOWED / ACCESS_DENIED ACEs for OwnerRights@ are authoritative.
// Without this guard, an audit-only OwnerRights@ entry would incorrectly
// strip OWNER@ identity from the file owner and silently deny owner access.
func TestEvaluate_OwnerRights_AuditAceDoesNotSuppressOwner(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			// AUDIT ACE for OwnerRights@ — must NOT be treated as an
			// OWNER_RIGHTS authority signal.
			{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwnerRights},
			// OWNER@ Allow READ — must remain authoritative for the owner.
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		},
	}

	owner := ownerCtx()
	if !Evaluate(a, owner, ACE4_READ_DATA) {
		t.Error("expected owner to retain OWNER@ READ_DATA when only an AUDIT OwnerRights@ ACE is present")
	}
}

// TestEvaluate_OwnerRights_AlarmAceDoesNotSuppressOwner mirrors the AUDIT
// case for ALARM ACEs — both are non-access ACE types and must not engage
// the OWNER_RIGHTS override.
func TestEvaluate_OwnerRights_AlarmAceDoesNotSuppressOwner(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_SYSTEM_ALARM_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialOwnerRights},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialOwner},
		},
	}

	owner := ownerCtx()
	if !Evaluate(a, owner, ACE4_READ_DATA|ACE4_WRITE_DATA) {
		t.Error("expected owner to retain OWNER@ rights when only an ALARM OwnerRights@ ACE is present")
	}
}

// TestEvaluate_OwnerImplicitGrants_DataPlusImplicit verifies MS-DTYP §2.5.3.2:
// owner gets READ_CONTROL / WRITE_DAC / WRITE_OWNER on top of explicit DACL
// grants when no OWNER_RIGHTS ACE is present. The DACL here grants OWNER@
// only READ_DATA; the owner must still be able to open for READ_DATA + the
// three implicit standard rights together.
func TestEvaluate_OwnerImplicitGrants_DataPlusImplicit(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		},
	}

	requested := uint32(ACE4_READ_DATA | ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER)
	if !Evaluate(a, ownerCtx(), requested) {
		t.Error("expected owner to receive READ_DATA + implicit READ_CONTROL|WRITE_DAC|WRITE_OWNER")
	}
}

// TestEvaluate_OwnerImplicitGrants_NoOwnerAceAtAll verifies that the owner
// gets the §2.5.3.2 implicit grants even when the DACL has no OWNER@ ACE at
// all — implicit grants are not gated on the owner being mentioned.
func TestEvaluate_OwnerImplicitGrants_NoOwnerAceAtAll(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	requested := uint32(ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER)
	if !Evaluate(a, ownerCtx(), requested) {
		t.Error("expected owner to receive implicit READ_CONTROL|WRITE_DAC|WRITE_OWNER even with no OWNER@ ACE")
	}
}

// TestEvaluate_OwnerImplicitGrants_OwnerRightsSuppresses verifies §2.5.3:
// when OWNER_RIGHTS ACE is present, the §2.5.3.2 implicit owner grants are
// suppressed — OWNER_RIGHTS is the sole authority for the owner's rights.
func TestEvaluate_OwnerImplicitGrants_OwnerRightsSuppresses(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwnerRights},
		},
	}

	// Owner requests READ_DATA — granted via OWNER_RIGHTS Allow.
	if !Evaluate(a, ownerCtx(), ACE4_READ_DATA) {
		t.Error("expected owner to receive READ_DATA from OWNER_RIGHTS Allow")
	}
	// Owner requests READ_CONTROL — should be DENIED because OWNER_RIGHTS
	// suppresses the implicit grant and only Allows READ_DATA.
	if Evaluate(a, ownerCtx(), ACE4_READ_ACL) {
		t.Error("expected owner to be denied implicit READ_CONTROL when OWNER_RIGHTS is present")
	}
}

// TestEvaluate_OwnerImplicitGrants_ExplicitDenyWins verifies that an
// explicit DENY ACE on a standard right (e.g., WRITE_DAC) overrides the
// §2.5.3.2 implicit grant for that bit. Explicit DENY always wins.
func TestEvaluate_OwnerImplicitGrants_ExplicitDenyWins(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			// DENY WRITE_DAC for owner — first-match-wins.
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_ACL, Who: SpecialOwner},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		},
	}

	// READ_CONTROL + WRITE_OWNER (implicit) should still work.
	if !Evaluate(a, ownerCtx(), ACE4_READ_ACL|ACE4_WRITE_OWNER) {
		t.Error("expected owner to receive implicit READ_CONTROL|WRITE_OWNER")
	}
	// WRITE_DAC must be denied by explicit DENY.
	if Evaluate(a, ownerCtx(), ACE4_WRITE_ACL) {
		t.Error("expected owner WRITE_DAC denied by explicit DENY ACE")
	}
}

// TestEvaluate_OwnerImplicitGrants_NonOwnerUnaffected verifies non-owner
// requesters do NOT receive the implicit standard rights — those are
// owner-only per §2.5.3.2.
func TestEvaluate_OwnerImplicitGrants_NonOwnerUnaffected(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
		},
	}

	// Non-owner requesting READ_CONTROL — must be denied (no implicit grant).
	if Evaluate(a, nonOwnerCtx(), ACE4_READ_ACL) {
		t.Error("expected non-owner to be denied READ_CONTROL (no implicit grant)")
	}
}
