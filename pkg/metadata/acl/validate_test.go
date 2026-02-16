package acl

import (
	"errors"
	"testing"
)

func TestValidateACL_ValidCanonicalOrder(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			// Bucket 1: Explicit DENY.
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
			// Bucket 2: Explicit ALLOW.
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			// Bucket 3: Inherited DENY.
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_WRITE_DATA, Who: "bob@example.com"},
			// Bucket 4: Inherited ALLOW.
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		},
	}

	if err := ValidateACL(a); err != nil {
		t.Errorf("expected valid canonical order, got: %v", err)
	}
}

func TestValidateACL_OutOfOrder(t *testing.T) {
	tests := []struct {
		name string
		aces []ACE
	}{
		{
			name: "explicit allow before explicit deny",
			aces: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
				{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
			},
		},
		{
			name: "inherited deny before explicit allow",
			aces: []ACE{
				{Type: ACE4_ACCESS_DENIED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
			},
		},
		{
			name: "inherited allow before inherited deny",
			aces: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
				{Type: ACE4_ACCESS_DENIED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &ACL{ACEs: tt.aces}
			err := ValidateACL(a)
			if err == nil {
				t.Error("expected validation error for out-of-order ACL")
			}
			if !errors.Is(err, ErrACLNotCanonical) {
				t.Errorf("expected ErrACLNotCanonical, got: %v", err)
			}
		})
	}
}

func TestValidateACL_MaxACECount(t *testing.T) {
	// Exactly 128 ACEs should be valid.
	aces := make([]ACE, MaxACECount)
	for i := range aces {
		aces[i] = ACE{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone}
	}
	a := &ACL{ACEs: aces}
	if err := ValidateACL(a); err != nil {
		t.Errorf("expected 128 ACEs to be valid, got: %v", err)
	}

	// 129 ACEs should fail.
	aces = make([]ACE, MaxACECount+1)
	for i := range aces {
		aces[i] = ACE{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone}
	}
	a = &ACL{ACEs: aces}
	err := ValidateACL(a)
	if err == nil {
		t.Error("expected validation error for >128 ACEs")
	}
	if !errors.Is(err, ErrACETooMany) {
		t.Errorf("expected ErrACETooMany, got: %v", err)
	}
}

func TestValidateACL_EmptyACL(t *testing.T) {
	a := &ACL{ACEs: []ACE{}}
	if err := ValidateACL(a); err != nil {
		t.Errorf("expected empty ACL to be valid, got: %v", err)
	}
}

func TestValidateACL_Nil(t *testing.T) {
	if err := ValidateACL(nil); err != nil {
		t.Errorf("expected nil ACL to be valid, got: %v", err)
	}
}

func TestValidateACL_AuditAlarmAnywhere(t *testing.T) {
	// AUDIT and ALARM ACEs can appear anywhere without breaking canonical order.
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
			{Type: ACE4_SYSTEM_ALARM_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
			{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_DELETE, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_WRITE_DATA, Who: "bob@example.com"},
			{Type: ACE4_SYSTEM_ALARM_ACE_TYPE, AccessMask: ACE4_DELETE, Who: SpecialEveryone},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		},
	}

	if err := ValidateACL(a); err != nil {
		t.Errorf("expected AUDIT/ALARM ACEs anywhere to be valid, got: %v", err)
	}
}

func TestValidateACE_InvalidType(t *testing.T) {
	ace := &ACE{Type: 4, Who: SpecialEveryone}
	err := ValidateACE(ace)
	if err == nil {
		t.Error("expected validation error for invalid ACE type")
	}
	if !errors.Is(err, ErrACEInvalidType) {
		t.Errorf("expected ErrACEInvalidType, got: %v", err)
	}
}

func TestValidateACE_EmptyWho(t *testing.T) {
	ace := &ACE{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: ""}
	err := ValidateACE(ace)
	if err == nil {
		t.Error("expected validation error for empty Who")
	}
	if !errors.Is(err, ErrACEEmptyWho) {
		t.Errorf("expected ErrACEEmptyWho, got: %v", err)
	}
}

func TestValidateACE_Valid(t *testing.T) {
	tests := []struct {
		name string
		ace  ACE
	}{
		{"allow with owner", ACE{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: SpecialOwner}},
		{"deny with named user", ACE{Type: ACE4_ACCESS_DENIED_ACE_TYPE, Who: "alice@example.com"}},
		{"audit with everyone", ACE{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, Who: SpecialEveryone}},
		{"alarm with group", ACE{Type: ACE4_SYSTEM_ALARM_ACE_TYPE, Who: SpecialGroup}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateACE(&tt.ace); err != nil {
				t.Errorf("expected valid ACE, got: %v", err)
			}
		})
	}
}

func TestValidateACL_InvalidACEInList(t *testing.T) {
	a := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
			{Type: 99, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone}, // invalid type
		},
	}

	err := ValidateACL(a)
	if err == nil {
		t.Error("expected validation error for ACL containing invalid ACE")
	}
	if !errors.Is(err, ErrACEInvalidType) {
		t.Errorf("expected ErrACEInvalidType, got: %v", err)
	}
}
