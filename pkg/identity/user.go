package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/md4"
)

// User represents a DittoFS user with cross-protocol identity mapping.
//
// Users can authenticate via different protocols (NFS, SMB, API) and have
// their identity mapped to the appropriate format. Share-level permissions
// can be assigned directly to users or inherited from groups.
type User struct {
	// Username is the unique identifier for the user.
	// Used for SMB authentication and display purposes.
	Username string `yaml:"username" mapstructure:"username"`

	// PasswordHash is the bcrypt hash of the user's password.
	// Used for dashboard login and password verification.
	PasswordHash string `yaml:"password_hash" mapstructure:"password_hash"`

	// NTHash is the hex-encoded NT hash of the user's password.
	// Used for SMB NTLM authentication. This is MD4(UTF16LE(password)).
	// Must be computed when the password is set and stored alongside bcrypt hash.
	// If empty, NTLM authentication will fall back to guest access.
	NTHash string `yaml:"nt_hash,omitempty" mapstructure:"nt_hash"`

	// Enabled indicates whether the user account is active.
	// Disabled users cannot authenticate.
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`

	// Unix identity mapping
	// Used for NFS authentication and file ownership.

	// UID is the Unix user ID.
	// NFS clients with this UID will be mapped to this user.
	UID uint32 `yaml:"uid" mapstructure:"uid"`

	// GID is the primary Unix group ID.
	GID uint32 `yaml:"gid" mapstructure:"gid"`

	// GIDs is a list of supplementary Unix group IDs.
	GIDs []uint32 `yaml:"gids,omitempty" mapstructure:"gids"`

	// Windows identity mapping
	// Used for SMB authentication.

	// SID is the Windows Security Identifier.
	// If empty, a SID will be auto-generated from the UID.
	SID string `yaml:"sid,omitempty" mapstructure:"sid"`

	// GroupSIDs is a list of Windows group Security Identifiers.
	GroupSIDs []string `yaml:"group_sids,omitempty" mapstructure:"group_sids"`

	// DittoFS group membership

	// Groups is a list of DittoFS group names this user belongs to.
	// Permissions are inherited from these groups.
	Groups []string `yaml:"groups,omitempty" mapstructure:"groups"`

	// Per-share permissions

	// SharePermissions maps share names to explicit permission levels.
	// These take precedence over group permissions.
	SharePermissions map[string]SharePermission `yaml:"share_permissions,omitempty" mapstructure:"share_permissions"`

	// Metadata

	// DisplayName is the human-readable name for the user.
	DisplayName string `yaml:"display_name,omitempty" mapstructure:"display_name"`

	// Email is the user's email address.
	Email string `yaml:"email,omitempty" mapstructure:"email"`

	// CreatedAt is when the user was created.
	CreatedAt time.Time `yaml:"created_at,omitempty" mapstructure:"created_at"`

	// LastLogin is when the user last authenticated.
	LastLogin time.Time `yaml:"last_login,omitempty" mapstructure:"last_login"`
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
// This is a standalone function for computing NT hashes without a User instance.
func ComputeNTHash(password string) [16]byte {
	// Convert password to UTF-16LE
	utf16Password := utf16.Encode([]rune(password))
	passwordBytes := make([]byte, len(utf16Password)*2)
	for i, r := range utf16Password {
		passwordBytes[i*2] = byte(r)
		passwordBytes[i*2+1] = byte(r >> 8)
	}

	// Compute MD4 hash
	h := newMD4()
	h.Write(passwordBytes)
	var ntHash [16]byte
	copy(ntHash[:], h.Sum(nil))
	return ntHash
}

// newMD4 returns a new MD4 hash.
func newMD4() hash.Hash {
	return md4.New()
}

// GetSID returns the Windows SID, auto-generating one if not set.
//
// The auto-generated SID follows the format:
// S-1-5-21-dittofs-{hash(uid)}
//
// This ensures consistent SID generation across restarts.
func (u *User) GetSID() string {
	if u.SID != "" {
		return u.SID
	}
	return GenerateSIDFromUID(u.UID)
}

// HasGroup checks if the user belongs to the specified group.
func (u *User) HasGroup(groupName string) bool {
	return slices.Contains(u.Groups, groupName)
}

// HasGID checks if the user has the specified GID in their supplementary groups.
func (u *User) HasGID(gid uint32) bool {
	if u.GID == gid {
		return true
	}
	return slices.Contains(u.GIDs, gid)
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
	if u.UID == 0 && u.Username != "root" {
		return fmt.Errorf("UID 0 is reserved for root")
	}
	for shareName, perm := range u.SharePermissions {
		if !perm.IsValid() {
			return fmt.Errorf("invalid permission %q for share %q", perm, shareName)
		}
	}
	return nil
}

// GenerateSIDFromUID creates a deterministic Windows SID from a Unix UID.
//
// Format: S-1-5-21-{dittofs-hash}-{uid}
// The dittofs-hash provides a unique identifier authority.
func GenerateSIDFromUID(uid uint32) string {
	// Use a hash of "dittofs" as the identifier authority
	// This ensures our SIDs don't conflict with real Windows SIDs
	h := sha256.Sum256([]byte("dittofs"))
	hashStr := hex.EncodeToString(h[:4])

	// Parse first 4 bytes as uint32 for the sub-authority
	// Note: Sscanf cannot fail here because hashStr is always 8 hex chars from sha256
	var subAuth1 uint32
	_, _ = fmt.Sscanf(hashStr, "%08x", &subAuth1)

	// S-1-5-21-{subAuth1}-0-{uid}
	// 1 = NT Authority
	// 5 = NT Authority
	// 21 = Non-unique domain
	return fmt.Sprintf("S-1-5-21-%d-0-%d", subAuth1, uid)
}

// GuestUser creates a guest user with the specified UID/GID.
func GuestUser(uid, gid uint32) *User {
	return &User{
		Username:    "guest",
		Enabled:     true,
		UID:         uid,
		GID:         gid,
		DisplayName: "Guest",
	}
}
