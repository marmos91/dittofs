package acl

import "testing"

// evaluateGrantedReference probes each set bit of probeMask through Evaluate
// individually and ORs the granted bits — the exact behavior EvaluateGranted
// replaces in a single pass. Used as the oracle for the equivalence tests.
func evaluateGrantedReference(a *ACL, evalCtx *EvaluateContext, probeMask uint32) uint32 {
	var granted uint32
	for bit := uint32(1); bit != 0; bit <<= 1 {
		if probeMask&bit == 0 {
			continue
		}
		if Evaluate(a, evalCtx, bit) {
			granted |= bit
		}
	}
	return granted
}

// TestEvaluateGranted_MatchesPerBitProbe asserts EvaluateGranted is
// bit-identical to the per-bit Evaluate loop it replaces, across a matrix of
// ACL shapes and requester contexts. This guards the single-pass MAXIMUM_ALLOWED
// / MxAc optimization in pkg/metadata/auth_permissions.go.
func TestEvaluateGranted_MatchesPerBitProbe(t *testing.T) {
	const fullProbe = ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA |
		ACE4_READ_NAMED_ATTRS | ACE4_WRITE_NAMED_ATTRS | ACE4_EXECUTE |
		ACE4_DELETE_CHILD | ACE4_READ_ATTRIBUTES | ACE4_WRITE_ATTRIBUTES |
		ACE4_DELETE | ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER |
		ACE4_SYNCHRONIZE

	admin := func(c *EvaluateContext) *EvaluateContext {
		c.RequesterHasTakeOwnership = true
		return c
	}

	acls := []struct {
		name string
		acl  *ACL
	}{
		{
			name: "allow-everyone-read-exec",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialEveryone},
			}},
		},
		{
			name: "deny-before-allow",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialEveryone},
			}},
		},
		{
			name: "owner-only",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialOwner},
			}},
		},
		{
			name: "group-allow",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
			}},
		},
		{
			name: "owner-rights-override",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwnerRights},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullProbe, Who: SpecialOwner},
			}},
		},
		{
			name: "audit-alarm-noise",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
				{Type: ACE4_SYSTEM_ALARM_ACE_TYPE, AccessMask: ACE4_DELETE, Who: SpecialEveryone},
			}},
		},
		{
			name: "empty-dacl",
			acl:  &ACL{ACEs: []ACE{}},
		},
		{
			name: "null-dacl",
			acl:  &ACL{NullDACL: true},
		},
		{
			name: "inherit-only-skipped",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullProbe, Who: SpecialEveryone, Flag: ACE4_INHERIT_ONLY_ACE},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			}},
		},
		{
			name: "complex-mixed",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_DELETE, Who: SpecialEveryone},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA, Who: SpecialOwner},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_EXECUTE, Who: SpecialEveryone},
			}},
		},
	}

	ctxs := []struct {
		name string
		ctx  *EvaluateContext
	}{
		{"owner", ownerCtx()},
		{"owner-admin", admin(ownerCtx())},
		{"non-owner", nonOwnerCtx()},
		{"group-member", groupMemberCtx()},
	}

	masks := []uint32{
		fullProbe,
		ACE4_READ_DATA | ACE4_WRITE_DATA,
		ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER,
		ACE4_DELETE,
		0,
	}

	for _, ac := range acls {
		for _, cc := range ctxs {
			for _, mask := range masks {
				want := evaluateGrantedReference(ac.acl, cc.ctx, mask)
				got := EvaluateGranted(ac.acl, cc.ctx, mask)
				if got != want {
					t.Errorf("acl=%s ctx=%s mask=%#x: EvaluateGranted=%#x, per-bit oracle=%#x",
						ac.name, cc.name, mask, got, want)
				}
			}
		}
	}
}

func TestEvaluateGranted_NilACL(t *testing.T) {
	if got := EvaluateGranted(nil, ownerCtx(), ACE4_READ_DATA); got != 0 {
		t.Errorf("nil ACL: want 0, got %#x", got)
	}
}

func TestEvaluateGranted_ZeroMask(t *testing.T) {
	a := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}
	if got := EvaluateGranted(a, ownerCtx(), 0); got != 0 {
		t.Errorf("zero mask: want 0, got %#x", got)
	}
}

func TestEvaluateGranted_NullDACLGrantsProbe(t *testing.T) {
	a := &ACL{NullDACL: true}
	mask := uint32(ACE4_READ_DATA | ACE4_WRITE_DATA)
	if got := EvaluateGranted(a, nonOwnerCtx(), mask); got != mask {
		t.Errorf("null DACL: want %#x, got %#x", mask, got)
	}
}
