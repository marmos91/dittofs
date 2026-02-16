package acl

import (
	"encoding/json"
	"testing"
)

func TestACLJSONRoundTrip(t *testing.T) {
	original := &ACL{
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_DENIED_ACE_TYPE,
				Flag:       ACE4_INHERITED_ACE,
				AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA,
				Who:        "alice@example.com",
			},
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_EXECUTE | ACE4_READ_ACL,
				Who:        SpecialOwner,
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ACL
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.ACEs) != len(original.ACEs) {
		t.Fatalf("ACE count mismatch: got %d, want %d", len(decoded.ACEs), len(original.ACEs))
	}

	for i, ace := range decoded.ACEs {
		orig := original.ACEs[i]
		if ace.Type != orig.Type {
			t.Errorf("ACE %d Type: got %d, want %d", i, ace.Type, orig.Type)
		}
		if ace.Flag != orig.Flag {
			t.Errorf("ACE %d Flag: got %d, want %d", i, ace.Flag, orig.Flag)
		}
		if ace.AccessMask != orig.AccessMask {
			t.Errorf("ACE %d AccessMask: got 0x%x, want 0x%x", i, ace.AccessMask, orig.AccessMask)
		}
		if ace.Who != orig.Who {
			t.Errorf("ACE %d Who: got %q, want %q", i, ace.Who, orig.Who)
		}
	}
}

func TestACLJSONOmitEmpty(t *testing.T) {
	// When ACL is nil, json:"omitempty" should omit it.
	type wrapper struct {
		ACL *ACL `json:"acl,omitempty"`
	}

	w := wrapper{ACL: nil}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// nil pointer should be omitted.
	expected := `{}`
	if string(data) != expected {
		t.Errorf("got %s, want %s", string(data), expected)
	}
}

func TestIsSpecialWho(t *testing.T) {
	tests := []struct {
		who  string
		want bool
	}{
		{SpecialOwner, true},
		{SpecialGroup, true},
		{SpecialEveryone, true},
		{"alice@example.com", false},
		{"OWNER", false},
		{"owner@", false}, // case-sensitive
		{"", false},
	}

	for _, tt := range tests {
		if got := IsSpecialWho(tt.who); got != tt.want {
			t.Errorf("IsSpecialWho(%q) = %v, want %v", tt.who, got, tt.want)
		}
	}
}

func TestACEIsInheritOnly(t *testing.T) {
	tests := []struct {
		name string
		flag uint32
		want bool
	}{
		{"no flags", 0, false},
		{"inherit only set", ACE4_INHERIT_ONLY_ACE, true},
		{"inherit only with others", ACE4_INHERIT_ONLY_ACE | ACE4_FILE_INHERIT_ACE, true},
		{"file inherit only", ACE4_FILE_INHERIT_ACE, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ace := &ACE{Flag: tt.flag}
			if got := ace.IsInheritOnly(); got != tt.want {
				t.Errorf("IsInheritOnly() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestACEIsInherited(t *testing.T) {
	tests := []struct {
		name string
		flag uint32
		want bool
	}{
		{"no flags", 0, false},
		{"inherited set", ACE4_INHERITED_ACE, true},
		{"inherited with others", ACE4_INHERITED_ACE | ACE4_FILE_INHERIT_ACE, true},
		{"file inherit only", ACE4_FILE_INHERIT_ACE, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ace := &ACE{Flag: tt.flag}
			if got := ace.IsInherited(); got != tt.want {
				t.Errorf("IsInherited() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestACETypeString(t *testing.T) {
	tests := []struct {
		typ  uint32
		want string
	}{
		{ACE4_ACCESS_ALLOWED_ACE_TYPE, "ALLOW"},
		{ACE4_ACCESS_DENIED_ACE_TYPE, "DENY"},
		{ACE4_SYSTEM_AUDIT_ACE_TYPE, "AUDIT"},
		{ACE4_SYSTEM_ALARM_ACE_TYPE, "ALARM"},
		{99, "UNKNOWN(99)"},
	}

	for _, tt := range tests {
		ace := &ACE{Type: tt.typ, Who: SpecialEveryone}
		if got := ace.TypeString(); got != tt.want {
			t.Errorf("TypeString() for type %d = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

func TestConstants(t *testing.T) {
	// Verify directory aliases match their file counterparts.
	if ACE4_LIST_DIRECTORY != ACE4_READ_DATA {
		t.Error("ACE4_LIST_DIRECTORY should equal ACE4_READ_DATA")
	}
	if ACE4_ADD_FILE != ACE4_WRITE_DATA {
		t.Error("ACE4_ADD_FILE should equal ACE4_WRITE_DATA")
	}
	if ACE4_ADD_SUBDIRECTORY != ACE4_APPEND_DATA {
		t.Error("ACE4_ADD_SUBDIRECTORY should equal ACE4_APPEND_DATA")
	}

	// Verify ACL support constants.
	if FullACLSupport != 0x0F {
		t.Errorf("FullACLSupport = 0x%x, want 0x0F", FullACLSupport)
	}

	// Verify FATTR4 bit numbers.
	if FATTR4_ACL != 12 {
		t.Errorf("FATTR4_ACL = %d, want 12", FATTR4_ACL)
	}
	if FATTR4_ACLSUPPORT != 13 {
		t.Errorf("FATTR4_ACLSUPPORT = %d, want 13", FATTR4_ACLSUPPORT)
	}
}
