package identity

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/md4" //nolint:staticcheck // MD4 is required for NTLM protocol compatibility
)

// UserRole represents the role of a user in the system.
type UserRole string

const (
	// RoleUser is a regular user with limited permissions.
	RoleUser UserRole = "user"
	// RoleAdmin is an administrator with full permissions.
	RoleAdmin UserRole = "admin"
)

// IsValid checks if the role is a valid UserRole.
func (r UserRole) IsValid() bool {
	return r == RoleUser || r == RoleAdmin
}

// User represents a DittoFS user with abstract identity.
//
// Users can authenticate via different protocols (NFS, SMB, API) and have
// their identity mapped to the appropriate format. Protocol-specific identity
// (Unix UID/GID, Windows SID) is resolved per-share via ShareIdentityMapping.
// Share-level permissions can be assigned directly to users or inherited from groups.
type User struct {
	// ID is the unique identifier for the user (UUID).
	ID string `json:"id" yaml:"id" mapstructure:"id"`

	// Username is the unique human-readable identifier for the user.
	// Used for SMB authentication and display purposes.
	Username string `json:"username" yaml:"username" mapstructure:"username"`

	// PasswordHash is the bcrypt hash of the user's password.
	// Used for API login and password verification.
	PasswordHash string `json:"-" yaml:"password_hash" mapstructure:"password_hash"`

	// NTHash is the hex-encoded NT hash of the user's password.
	// Used for SMB NTLM authentication. This is MD4(UTF16LE(password)).
	// Must be computed when the password is set and stored alongside bcrypt hash.
	// If empty, NTLM authentication will allow access without password validation
	// (insecure transitional mode - should only be used for explicit guest accounts).
	//
	// SECURITY WARNING:
	//   - This value is highly sensitive and can be used for pass-the-hash attacks
	//     without knowing the original password.
	//   - Any configuration file or storage that contains NTHash MUST be treated as
	//     secret material and restricted to root/administrator access only.
	//   - Operators should ensure that on-disk config files are readable only by the
	//     service account (for example, chmod 600 on Unix-like systems).
	NTHash string `json:"-" yaml:"nt_hash,omitempty" mapstructure:"nt_hash"`

	// Enabled indicates whether the user account is active.
	// Disabled users cannot authenticate.
	Enabled bool `json:"enabled" yaml:"enabled" mapstructure:"enabled"`

	// MustChangePassword indicates whether the user must change their password
	// before performing other operations. Set to true for newly created users
	// or after admin password reset.
	MustChangePassword bool `json:"must_change_password" yaml:"must_change_password" mapstructure:"must_change_password"`

	// Role is the user's role in the system (admin or user).
	Role UserRole `json:"role" yaml:"role" mapstructure:"role"`

	// DittoFS group membership

	// Groups is a list of DittoFS group names this user belongs to.
	// Permissions are inherited from these groups.
	Groups []string `json:"groups,omitempty" yaml:"groups,omitempty" mapstructure:"groups"`

	// Per-share permissions

	// SharePermissions maps share names to explicit permission levels.
	// These take precedence over group permissions.
	SharePermissions map[string]SharePermission `json:"share_permissions,omitempty" yaml:"share_permissions,omitempty" mapstructure:"share_permissions"`

	// Metadata

	// DisplayName is the human-readable name for the user.
	DisplayName string `json:"display_name,omitempty" yaml:"display_name,omitempty" mapstructure:"display_name"`

	// Email is the user's email address.
	Email string `json:"email,omitempty" yaml:"email,omitempty" mapstructure:"email"`

	// CreatedAt is when the user was created.
	CreatedAt time.Time `json:"created_at,omitempty" yaml:"created_at,omitempty" mapstructure:"created_at"`

	// LastLogin is when the user last authenticated.
	LastLogin time.Time `json:"last_login,omitempty" yaml:"last_login,omitempty" mapstructure:"last_login"`
}

// GetDisplayName returns the display name, or username if display name is not set.
func (u *User) GetDisplayName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// GetNTHash returns the NT hash as a 16-byte array.
// Returns false if the NTHash field is empty or invalid.
func (u *User) GetNTHash() ([16]byte, bool) {
	var ntHash [16]byte
	if u.NTHash == "" {
		return ntHash, false
	}

	// Decode hex string to bytes
	decoded, err := hex.DecodeString(u.NTHash)
	if err != nil || len(decoded) != 16 {
		return ntHash, false
	}

	copy(ntHash[:], decoded)
	return ntHash, true
}

// SetNTHashFromPassword computes and sets the NT hash from a plaintext password.
// The password is not stored - only the NT hash is kept.
func (u *User) SetNTHashFromPassword(password string) {
	ntHash := ComputeNTHash(password)
	u.NTHash = hex.EncodeToString(ntHash[:])
}

// ComputeNTHash computes the NT hash from a password.
// The NT hash is: MD4(UTF16LE(password))
// This helper is exposed for callers that need the raw NT hash bytes without a User instance.
func ComputeNTHash(password string) [16]byte {
	// Convert password to UTF-16LE using binary.LittleEndian for consistency
	utf16Password := utf16.Encode([]rune(password))
	passwordBytes := make([]byte, len(utf16Password)*2)
	for i, r := range utf16Password {
		binary.LittleEndian.PutUint16(passwordBytes[i*2:], r)
	}

	// Compute MD4 hash
	h := md4.New()
	h.Write(passwordBytes)
	var ntHash [16]byte
	copy(ntHash[:], h.Sum(nil))
	return ntHash
}

// HasGroup checks if the user belongs to the specified group.
func (u *User) HasGroup(groupName string) bool {
	return slices.Contains(u.Groups, groupName)
}

// GetExplicitSharePermission returns the user's explicit permission for a share.
// Returns PermissionNone and false if no explicit permission is set.
func (u *User) GetExplicitSharePermission(shareName string) (SharePermission, bool) {
	if u.SharePermissions == nil {
		return PermissionNone, false
	}
	perm, ok := u.SharePermissions[shareName]
	if !ok {
		return PermissionNone, false
	}
	return perm, ok
}

// Validate checks if the user has valid configuration.
func (u *User) Validate() error {
	if u.Username == "" {
		return fmt.Errorf("username is required")
	}
	if u.Role != "" && !u.Role.IsValid() {
		return fmt.Errorf("invalid role %q", u.Role)
	}
	for shareName, perm := range u.SharePermissions {
		if !perm.IsValid() {
			return fmt.Errorf("invalid permission %q for share %q", perm, shareName)
		}
	}
	return nil
}

// IsAdmin checks if the user has admin role.
func (u *User) IsAdmin() bool {
	return u.Role == RoleAdmin
}
