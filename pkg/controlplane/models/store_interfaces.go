package models

import "context"

// UserStore provides read-only user and group operations for protocol handlers.
//
// This is the minimal interface needed by SMB protocol handlers for authentication
// and authorization. Thread-safe implementations are required.
type UserStore interface {
	// GetUser returns a user by username.
	// Returns ErrUserNotFound if the user doesn't exist.
	GetUser(ctx context.Context, username string) (*User, error)

	// ValidateCredentials verifies username/password credentials.
	// Returns the user if credentials are valid.
	// Returns ErrInvalidCredentials if the credentials are invalid.
	// Returns ErrUserDisabled if the user account is disabled.
	ValidateCredentials(ctx context.Context, username, password string) (*User, error)

	// ListUsers returns all users.
	ListUsers(ctx context.Context) ([]*User, error)

	// GetGuestUser returns the guest user for a specific share if guest access is enabled.
	// Returns ErrGuestDisabled if guest access is not configured for the share.
	GetGuestUser(ctx context.Context, shareName string) (*User, error)

	// GetGroup returns a group by name.
	// Returns ErrGroupNotFound if the group doesn't exist.
	GetGroup(ctx context.Context, name string) (*Group, error)

	// ListGroups returns all groups.
	ListGroups(ctx context.Context) ([]*Group, error)

	// GetUserGroups returns all groups a user belongs to.
	GetUserGroups(ctx context.Context, username string) ([]*Group, error)

	// ResolveSharePermission returns the effective permission for a user on a share.
	// Resolution order: user explicit > group permissions (highest wins) > default
	ResolveSharePermission(ctx context.Context, user *User, shareName string) (SharePermission, error)
}

// IdentityStore extends UserStore with additional operations needed by NFS protocol handlers.
//
// NFS handlers need GetUserByUID for reverse UID lookup since NFS AUTH_UNIX provides
// only numeric UIDs, not usernames.
type IdentityStore interface {
	UserStore

	// GetUserByUID returns a user by their Unix UID.
	// Used for NFS reverse lookup from AUTH_UNIX credentials.
	// Returns ErrUserNotFound if no user has this UID.
	GetUserByUID(ctx context.Context, uid uint32) (*User, error)

	// GetUserByID returns a user by their unique ID (UUID).
	// Returns ErrUserNotFound if no user has this ID.
	GetUserByID(ctx context.Context, id string) (*User, error)

	// IsGuestEnabled returns whether guest access is enabled for the share.
	IsGuestEnabled(ctx context.Context, shareName string) bool
}
