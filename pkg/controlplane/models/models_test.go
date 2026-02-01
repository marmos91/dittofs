package models

import (
	"testing"
)

func TestUserRole_IsValid(t *testing.T) {
	tests := []struct {
		role  UserRole
		valid bool
	}{
		{RoleUser, true},
		{RoleAdmin, true},
		{"invalid", false},
		{"", false},
		{"USER", false}, // case sensitive
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := tt.role.IsValid(); got != tt.valid {
				t.Errorf("UserRole(%q).IsValid() = %v, want %v", tt.role, got, tt.valid)
			}
		})
	}
}

func TestUser_GetDisplayName(t *testing.T) {
	tests := []struct {
		name        string
		user        User
		wantDisplay string
	}{
		{"with display name", User{Username: "john", DisplayName: "John Doe"}, "John Doe"},
		{"without display name", User{Username: "john"}, "john"},
		{"empty display name", User{Username: "john", DisplayName: ""}, "john"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.user.GetDisplayName(); got != tt.wantDisplay {
				t.Errorf("GetDisplayName() = %q, want %q", got, tt.wantDisplay)
			}
		})
	}
}

func TestUser_HasGroup(t *testing.T) {
	user := User{
		Username: "john",
		Groups: []Group{
			{Name: "developers"},
			{Name: "admins"},
		},
	}

	tests := []struct {
		groupName string
		expected  bool
	}{
		{"developers", true},
		{"admins", true},
		{"users", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.groupName, func(t *testing.T) {
			if got := user.HasGroup(tt.groupName); got != tt.expected {
				t.Errorf("HasGroup(%q) = %v, want %v", tt.groupName, got, tt.expected)
			}
		})
	}
}

func TestUser_GetGroupNames(t *testing.T) {
	t.Run("with groups", func(t *testing.T) {
		user := User{
			Groups: []Group{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		}
		names := user.GetGroupNames()
		if len(names) != 3 {
			t.Fatalf("expected 3 names, got %d", len(names))
		}
		expected := []string{"a", "b", "c"}
		for i, name := range names {
			if name != expected[i] {
				t.Errorf("names[%d] = %q, want %q", i, name, expected[i])
			}
		}
	})

	t.Run("no groups", func(t *testing.T) {
		user := User{}
		names := user.GetGroupNames()
		if len(names) != 0 {
			t.Errorf("expected empty slice, got %d names", len(names))
		}
	})
}

func TestUser_Validate(t *testing.T) {
	tests := []struct {
		name    string
		user    User
		wantErr bool
	}{
		{"valid user", User{Username: "john", Role: "user"}, false},
		{"valid admin", User{Username: "admin", Role: "admin"}, false},
		{"empty role", User{Username: "john"}, false}, // empty role is allowed
		{"missing username", User{Role: "user"}, true},
		{"invalid role", User{Username: "john", Role: "superuser"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.user.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUser_IsAdmin(t *testing.T) {
	tests := []struct {
		role    string
		isAdmin bool
	}{
		{"admin", true},
		{"user", false},
		{"", false},
		{"ADMIN", false}, // case sensitive
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			user := User{Role: tt.role}
			if got := user.IsAdmin(); got != tt.isAdmin {
				t.Errorf("IsAdmin() = %v, want %v", got, tt.isAdmin)
			}
		})
	}
}

func TestUser_GetExplicitSharePermission(t *testing.T) {
	user := User{
		SharePermissions: []UserSharePermission{
			{ShareName: "/data", Permission: "read-write"},
			{ShareName: "/backup", Permission: "read"},
		},
	}

	t.Run("existing permission", func(t *testing.T) {
		perm, ok := user.GetExplicitSharePermission("/data")
		if !ok {
			t.Fatal("expected permission to be found")
		}
		if perm != PermissionReadWrite {
			t.Errorf("expected %q, got %q", PermissionReadWrite, perm)
		}
	})

	t.Run("non-existing permission", func(t *testing.T) {
		perm, ok := user.GetExplicitSharePermission("/other")
		if ok {
			t.Error("expected permission not to be found")
		}
		if perm != PermissionNone {
			t.Errorf("expected %q, got %q", PermissionNone, perm)
		}
	})
}

func TestUser_NTHash(t *testing.T) {
	user := User{Username: "test"}

	t.Run("empty NT hash", func(t *testing.T) {
		_, ok := user.GetNTHash()
		if ok {
			t.Error("expected false for empty NT hash")
		}
	})

	t.Run("set and get NT hash", func(t *testing.T) {
		user.SetNTHashFromPassword("password123")
		ntHash, ok := user.GetNTHash()
		if !ok {
			t.Fatal("expected NT hash to be set")
		}
		// NT hash should be 16 bytes and non-zero
		if len(ntHash) != 16 {
			t.Errorf("expected 16-byte hash, got %d bytes", len(ntHash))
		}
		// Verify the hash is not all zeros (password was set)
		allZeros := true
		for _, b := range ntHash {
			if b != 0 {
				allZeros = false
				break
			}
		}
		if allZeros {
			t.Error("expected non-zero NT hash")
		}
	})

	t.Run("invalid hex NT hash", func(t *testing.T) {
		user.NTHash = "invalid-hex"
		_, ok := user.GetNTHash()
		if ok {
			t.Error("expected false for invalid hex")
		}
	})

	t.Run("wrong length NT hash", func(t *testing.T) {
		user.NTHash = "aabbccdd" // only 4 bytes
		_, ok := user.GetNTHash()
		if ok {
			t.Error("expected false for wrong length")
		}
	})
}

func TestSharePermission_Level(t *testing.T) {
	tests := []struct {
		perm  SharePermission
		level int
	}{
		{PermissionNone, 0},
		{PermissionRead, 1},
		{PermissionReadWrite, 2},
		{PermissionAdmin, 3},
		{"unknown", 0},
	}

	for _, tt := range tests {
		t.Run(string(tt.perm), func(t *testing.T) {
			if got := tt.perm.Level(); got != tt.level {
				t.Errorf("Level() = %d, want %d", got, tt.level)
			}
		})
	}
}

func TestSharePermission_CanRead(t *testing.T) {
	tests := []struct {
		perm    SharePermission
		canRead bool
	}{
		{PermissionNone, false},
		{PermissionRead, true},
		{PermissionReadWrite, true},
		{PermissionAdmin, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.perm), func(t *testing.T) {
			if got := tt.perm.CanRead(); got != tt.canRead {
				t.Errorf("CanRead() = %v, want %v", got, tt.canRead)
			}
		})
	}
}

func TestSharePermission_CanWrite(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		canWrite bool
	}{
		{PermissionNone, false},
		{PermissionRead, false},
		{PermissionReadWrite, true},
		{PermissionAdmin, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.perm), func(t *testing.T) {
			if got := tt.perm.CanWrite(); got != tt.canWrite {
				t.Errorf("CanWrite() = %v, want %v", got, tt.canWrite)
			}
		})
	}
}

func TestSharePermission_CanAdmin(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		canAdmin bool
	}{
		{PermissionNone, false},
		{PermissionRead, false},
		{PermissionReadWrite, false},
		{PermissionAdmin, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.perm), func(t *testing.T) {
			if got := tt.perm.CanAdmin(); got != tt.canAdmin {
				t.Errorf("CanAdmin() = %v, want %v", got, tt.canAdmin)
			}
		})
	}
}

func TestSharePermission_IsValid(t *testing.T) {
	tests := []struct {
		perm  SharePermission
		valid bool
	}{
		{PermissionNone, true},
		{PermissionRead, true},
		{PermissionReadWrite, true},
		{PermissionAdmin, true},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.perm), func(t *testing.T) {
			if got := tt.perm.IsValid(); got != tt.valid {
				t.Errorf("IsValid() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestParseSharePermission(t *testing.T) {
	tests := []struct {
		input    string
		expected SharePermission
	}{
		{"none", PermissionNone},
		{"read", PermissionRead},
		{"read-write", PermissionReadWrite},
		{"admin", PermissionAdmin},
		{"invalid", PermissionNone},
		{"", PermissionNone},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ParseSharePermission(tt.input); got != tt.expected {
				t.Errorf("ParseSharePermission(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMaxPermission(t *testing.T) {
	tests := []struct {
		a, b     SharePermission
		expected SharePermission
	}{
		{PermissionNone, PermissionRead, PermissionRead},
		{PermissionRead, PermissionNone, PermissionRead},
		{PermissionRead, PermissionReadWrite, PermissionReadWrite},
		{PermissionAdmin, PermissionRead, PermissionAdmin},
		{PermissionNone, PermissionNone, PermissionNone},
		{PermissionAdmin, PermissionAdmin, PermissionAdmin},
	}

	for _, tt := range tests {
		t.Run(string(tt.a)+"_"+string(tt.b), func(t *testing.T) {
			if got := MaxPermission(tt.a, tt.b); got != tt.expected {
				t.Errorf("MaxPermission(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}
