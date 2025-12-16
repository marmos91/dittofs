package identity

import (
	"testing"
)

func TestParseSharePermission(t *testing.T) {
	tests := []struct {
		input    string
		expected SharePermission
	}{
		{"none", PermissionNone},
		{"read", PermissionRead},
		{"read-write", PermissionReadWrite},
		{"admin", PermissionAdmin},
		// Note: ParseSharePermission only accepts lowercase values
		{"NONE", PermissionNone}, // Uppercase "NONE" happens to match because it's the same as lowercase
		{"READ", PermissionNone}, // Uppercase not supported
		{"READ-WRITE", PermissionNone},
		{"ADMIN", PermissionNone},
		{"None", PermissionNone}, // Mixed case not supported
		{"Read", PermissionNone},
		{"", PermissionNone},
		{"invalid", PermissionNone},
		{"readwrite", PermissionNone}, // Not valid without hyphen
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := ParseSharePermission(tc.input)
			if result != tc.expected {
				t.Errorf("ParseSharePermission(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestSharePermission_Level(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		expected int
	}{
		{PermissionNone, 0},
		{PermissionRead, 1},
		{PermissionReadWrite, 2},
		{PermissionAdmin, 3},
		{SharePermission("invalid"), 0},
	}

	for _, tc := range tests {
		t.Run(string(tc.perm), func(t *testing.T) {
			result := tc.perm.Level()
			if result != tc.expected {
				t.Errorf("%q.Level() = %d, want %d", tc.perm, result, tc.expected)
			}
		})
	}
}

func TestSharePermission_IsValid(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		expected bool
	}{
		{PermissionNone, true},
		{PermissionRead, true},
		{PermissionReadWrite, true},
		{PermissionAdmin, true},
		{SharePermission("invalid"), false},
		{SharePermission(""), false},
	}

	for _, tc := range tests {
		t.Run(string(tc.perm), func(t *testing.T) {
			result := tc.perm.IsValid()
			if result != tc.expected {
				t.Errorf("%q.IsValid() = %v, want %v", tc.perm, result, tc.expected)
			}
		})
	}
}

func TestSharePermission_CanRead(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		expected bool
	}{
		{PermissionNone, false},
		{PermissionRead, true},
		{PermissionReadWrite, true},
		{PermissionAdmin, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.perm), func(t *testing.T) {
			result := tc.perm.CanRead()
			if result != tc.expected {
				t.Errorf("%q.CanRead() = %v, want %v", tc.perm, result, tc.expected)
			}
		})
	}
}

func TestSharePermission_CanWrite(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		expected bool
	}{
		{PermissionNone, false},
		{PermissionRead, false},
		{PermissionReadWrite, true},
		{PermissionAdmin, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.perm), func(t *testing.T) {
			result := tc.perm.CanWrite()
			if result != tc.expected {
				t.Errorf("%q.CanWrite() = %v, want %v", tc.perm, result, tc.expected)
			}
		})
	}
}

func TestSharePermission_CanAdmin(t *testing.T) {
	tests := []struct {
		perm     SharePermission
		expected bool
	}{
		{PermissionNone, false},
		{PermissionRead, false},
		{PermissionReadWrite, false},
		{PermissionAdmin, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.perm), func(t *testing.T) {
			result := tc.perm.CanAdmin()
			if result != tc.expected {
				t.Errorf("%q.CanAdmin() = %v, want %v", tc.perm, result, tc.expected)
			}
		})
	}
}

func TestMaxPermission(t *testing.T) {
	tests := []struct {
		a, b     SharePermission
		expected SharePermission
	}{
		{PermissionNone, PermissionNone, PermissionNone},
		{PermissionNone, PermissionRead, PermissionRead},
		{PermissionRead, PermissionNone, PermissionRead},
		{PermissionRead, PermissionReadWrite, PermissionReadWrite},
		{PermissionReadWrite, PermissionRead, PermissionReadWrite},
		{PermissionAdmin, PermissionRead, PermissionAdmin},
		{PermissionRead, PermissionAdmin, PermissionAdmin},
	}

	for _, tc := range tests {
		t.Run(string(tc.a)+"_"+string(tc.b), func(t *testing.T) {
			result := MaxPermission(tc.a, tc.b)
			if result != tc.expected {
				t.Errorf("MaxPermission(%q, %q) = %q, want %q", tc.a, tc.b, result, tc.expected)
			}
		})
	}
}
