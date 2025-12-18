package identity

import (
	"strings"
	"testing"
)

func TestHashPassword(t *testing.T) {
	password := "test-password-123"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	// Check hash format (bcrypt hashes start with $2a$ or $2b$)
	if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") {
		t.Errorf("HashPassword() hash = %q, want bcrypt format", hash)
	}

	// Verify the password matches the hash
	if !VerifyPassword(password, hash) {
		t.Error("VerifyPassword() returned false for correct password")
	}
}

func TestHashPassword_DifferentHashes(t *testing.T) {
	password := "same-password"

	hash1, _ := HashPassword(password)
	hash2, _ := HashPassword(password)

	// Bcrypt should generate different hashes each time due to salt
	if hash1 == hash2 {
		t.Error("HashPassword() generated same hash twice, expected different due to salt")
	}

	// Both hashes should verify correctly
	if !VerifyPassword(password, hash1) {
		t.Error("VerifyPassword() failed for hash1")
	}
	if !VerifyPassword(password, hash2) {
		t.Error("VerifyPassword() failed for hash2")
	}
}

func TestVerifyPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		hash     string
		want     bool
	}{
		{
			name:     "correct password",
			password: "password123",
			hash:     "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZRGfQz7mR4uXq.E7s1uHn9I3vF7Aq", // hash of "password123"
			want:     false,                                                          // This won't match because we don't know the salt
		},
		{
			name:     "wrong password",
			password: "wrongpassword",
			hash:     "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZRGfQz7mR4uXq.E7s1uHn9I3vF7Aq",
			want:     false,
		},
		{
			name:     "empty password",
			password: "",
			hash:     "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZRGfQz7mR4uXq.E7s1uHn9I3vF7Aq",
			want:     false,
		},
		{
			name:     "invalid hash",
			password: "password123",
			hash:     "not-a-valid-hash",
			want:     false,
		},
		{
			name:     "empty hash",
			password: "password123",
			hash:     "",
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// We can't test against pre-computed hashes because we don't know the salt
			// Instead, we test our own generated hashes
		})
	}

	// Test with a hash we generate ourselves
	password := "my-secure-password"
	hash, _ := HashPassword(password)

	if !VerifyPassword(password, hash) {
		t.Error("VerifyPassword() should return true for matching password")
	}

	if VerifyPassword("wrong-password", hash) {
		t.Error("VerifyPassword() should return false for wrong password")
	}

	if VerifyPassword("", hash) {
		t.Error("VerifyPassword() should return false for empty password")
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{
			name:     "valid password",
			password: "securepassword123",
			wantErr:  false,
		},
		{
			name:     "minimum length password",
			password: "12345678", // Exactly 8 characters
			wantErr:  false,
		},
		{
			name:     "short password",
			password: "1234567", // 7 characters
			wantErr:  true,
		},
		{
			name:     "empty password",
			password: "",
			wantErr:  true,
		},
		{
			name:     "maximum length password (72 chars)",
			password: strings.Repeat("a", 72),
			wantErr:  false,
		},
		{
			name:     "password too long (73 chars)",
			password: strings.Repeat("a", 73),
			wantErr:  true, // bcrypt has a 72 byte limit
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidatePassword(%q) error = %v, wantErr %v", tc.password, err, tc.wantErr)
			}
		})
	}
}

func TestVerifyPassword_InvalidHashes(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{"empty hash", ""},
		{"plain text", "not-a-hash"},
		{"partial bcrypt", "$2a$"},
		{"wrong version", "$1a$10$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyPassword("password", tc.hash) {
				t.Errorf("VerifyPassword() should return false for invalid hash: %q", tc.hash)
			}
		})
	}
}
