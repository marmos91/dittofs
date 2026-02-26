// Package store provides the control plane persistence layer.
//
// This package implements the Store interface for managing control plane data
// including users, groups, shares, store configurations, and adapters.
//
// The Store interface is composed of focused sub-interfaces, each grouping
// related operations by entity. Consumers should accept the narrowest
// sub-interface they need for improved testability and explicit dependencies.
//
// Two backends are supported:
//   - SQLite (single-node, default)
//   - PostgreSQL (HA-capable)
package store

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// UserStore provides user CRUD and credential operations.
//
// All methods are safe for concurrent use. Username lookups are
// case-sensitive. UID lookups support NFS AUTH_UNIX reverse mapping.
type UserStore interface {
	// GetUser returns a user by username.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	GetUser(ctx context.Context, username string) (*models.User, error)

	// GetUserByID returns a user by their unique ID (UUID).
	// Returns models.ErrUserNotFound if no user has this ID.
	GetUserByID(ctx context.Context, id string) (*models.User, error)

	// GetUserByUID returns a user by their Unix UID.
	// Used for NFS reverse lookup from AUTH_UNIX credentials.
	// Returns models.ErrUserNotFound if no user has this UID.
	GetUserByUID(ctx context.Context, uid uint32) (*models.User, error)

	// ListUsers returns all users.
	// Use with caution for large user counts.
	ListUsers(ctx context.Context) ([]*models.User, error)

	// CreateUser creates a new user.
	// The user ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateUser if a user with the same username exists.
	CreateUser(ctx context.Context, user *models.User) (string, error)

	// UpdateUser updates an existing user.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	UpdateUser(ctx context.Context, user *models.User) error

	// DeleteUser deletes a user by username.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	// Also deletes all share permissions for the user.
	DeleteUser(ctx context.Context, username string) error

	// UpdatePassword updates a user's password hash and NT hash.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	UpdatePassword(ctx context.Context, username, passwordHash, ntHash string) error

	// UpdateLastLogin updates the user's last login timestamp.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	UpdateLastLogin(ctx context.Context, username string, timestamp time.Time) error

	// ValidateCredentials verifies username/password credentials.
	// Returns the user if credentials are valid.
	// Returns models.ErrInvalidCredentials if the credentials are invalid.
	// Returns models.ErrUserDisabled if the user account is disabled.
	ValidateCredentials(ctx context.Context, username, password string) (*models.User, error)

	// GetGuestUser returns the guest user for a specific share if guest access is enabled.
	// Returns models.ErrGuestDisabled if guest access is not configured for the share.
	GetGuestUser(ctx context.Context, shareName string) (*models.User, error)

	// IsGuestEnabled returns whether guest access is enabled for the share.
	IsGuestEnabled(ctx context.Context, shareName string) bool
}

// GroupStore provides group CRUD and membership operations.
//
// Groups are used for permission resolution and Unix GID mapping.
// Default groups (admins, users) are created during server startup.
type GroupStore interface {
	// GetGroup returns a group by name.
	// Returns models.ErrGroupNotFound if the group doesn't exist.
	GetGroup(ctx context.Context, name string) (*models.Group, error)

	// GetGroupByID returns a group by its unique ID.
	// Returns models.ErrGroupNotFound if no group has this ID.
	GetGroupByID(ctx context.Context, id string) (*models.Group, error)

	// ListGroups returns all groups.
	ListGroups(ctx context.Context) ([]*models.Group, error)

	// CreateGroup creates a new group.
	// The group ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateGroup if a group with the same name exists.
	CreateGroup(ctx context.Context, group *models.Group) (string, error)

	// UpdateGroup updates an existing group.
	// Returns models.ErrGroupNotFound if the group doesn't exist.
	UpdateGroup(ctx context.Context, group *models.Group) error

	// DeleteGroup deletes a group by name.
	// Returns models.ErrGroupNotFound if the group doesn't exist.
	// Users belonging to the group are updated to remove the group reference.
	DeleteGroup(ctx context.Context, name string) error

	// GetUserGroups returns all groups a user belongs to.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	GetUserGroups(ctx context.Context, username string) ([]*models.Group, error)

	// AddUserToGroup adds a user to a group.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	// Returns models.ErrGroupNotFound if the group doesn't exist.
	// No error if the user is already in the group.
	AddUserToGroup(ctx context.Context, username, groupName string) error

	// RemoveUserFromGroup removes a user from a group.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	// No error if the user was not in the group.
	RemoveUserFromGroup(ctx context.Context, username, groupName string) error

	// GetGroupMembers returns all users who are members of a group.
	// Returns models.ErrGroupNotFound if the group doesn't exist.
	GetGroupMembers(ctx context.Context, groupName string) ([]*models.User, error)

	// EnsureDefaultGroups creates the default groups (admins, users) if they don't exist.
	// Also adds the admin user to the admins group if both exist.
	// Returns true if any groups were created.
	// This should be called during server startup after EnsureAdminUser.
	EnsureDefaultGroups(ctx context.Context) (created bool, err error)
}

// ShareStore provides share CRUD, access rules, and per-share adapter config operations.
//
// Shares define NFS/SMB exports. Each share references a metadata store and
// payload store by ID. ShareAdapterConfig methods manage per-share,
// per-protocol configuration (e.g., NFS export options, SMB share options).
type ShareStore interface {
	// GetShare returns a share by name.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	GetShare(ctx context.Context, name string) (*models.Share, error)

	// GetShareByID returns a share by its unique ID.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	GetShareByID(ctx context.Context, id string) (*models.Share, error)

	// ListShares returns all shares.
	ListShares(ctx context.Context) ([]*models.Share, error)

	// CreateShare creates a new share.
	// The ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateShare if a share with the same name exists.
	CreateShare(ctx context.Context, share *models.Share) (string, error)

	// UpdateShare updates an existing share.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	UpdateShare(ctx context.Context, share *models.Share) error

	// DeleteShare deletes a share by name.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	// Also deletes all access rules and permissions for the share.
	DeleteShare(ctx context.Context, name string) error

	// GetUserAccessibleShares returns all shares a user can access.
	// This includes shares with explicit user permission or via group membership.
	GetUserAccessibleShares(ctx context.Context, username string) ([]*models.Share, error)

	// GetShareAccessRules returns all access rules for a share.
	GetShareAccessRules(ctx context.Context, shareName string) ([]*models.ShareAccessRule, error)

	// SetShareAccessRules replaces all access rules for a share.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	SetShareAccessRules(ctx context.Context, shareName string, rules []*models.ShareAccessRule) error

	// AddShareAccessRule adds a single access rule to a share.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	AddShareAccessRule(ctx context.Context, shareName string, rule *models.ShareAccessRule) error

	// RemoveShareAccessRule removes a single access rule from a share.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	// No error if the rule doesn't exist.
	RemoveShareAccessRule(ctx context.Context, shareName, ruleID string) error

	// GetShareAdapterConfig returns the adapter config for a share and adapter type.
	// Returns nil (no error) if no config exists.
	GetShareAdapterConfig(ctx context.Context, shareID, adapterType string) (*models.ShareAdapterConfig, error)

	// SetShareAdapterConfig creates or updates an adapter config for a share.
	// Uses upsert semantics: creates if not found, updates if exists.
	SetShareAdapterConfig(ctx context.Context, config *models.ShareAdapterConfig) error

	// DeleteShareAdapterConfig deletes an adapter config for a share and adapter type.
	// No error if the config didn't exist.
	DeleteShareAdapterConfig(ctx context.Context, shareID, adapterType string) error

	// ListShareAdapterConfigs returns all adapter configs for a share.
	ListShareAdapterConfigs(ctx context.Context, shareID string) ([]models.ShareAdapterConfig, error)
}

// PermissionStore provides user and group share permission operations.
//
// Permission resolution follows the order: user explicit > group permissions
// (highest wins) > share default. This interface is separated from ShareStore
// to allow handlers that only need permission checks to accept a narrow interface.
type PermissionStore interface {
	// GetUserSharePermission returns the user's permission for a share.
	// Returns nil (no error) if no permission is set.
	GetUserSharePermission(ctx context.Context, username, shareName string) (*models.UserSharePermission, error)

	// SetUserSharePermission sets a user's permission for a share.
	// Creates or updates the permission.
	// Returns models.ErrUserNotFound if the user doesn't exist.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	SetUserSharePermission(ctx context.Context, perm *models.UserSharePermission) error

	// DeleteUserSharePermission removes a user's permission for a share.
	// No error if the permission didn't exist.
	DeleteUserSharePermission(ctx context.Context, username, shareName string) error

	// GetUserSharePermissions returns all share permissions for a user.
	GetUserSharePermissions(ctx context.Context, username string) ([]*models.UserSharePermission, error)

	// GetGroupSharePermission returns the group's permission for a share.
	// Returns nil (no error) if no permission is set.
	GetGroupSharePermission(ctx context.Context, groupName, shareName string) (*models.GroupSharePermission, error)

	// SetGroupSharePermission sets a group's permission for a share.
	// Creates or updates the permission.
	// Returns models.ErrGroupNotFound if the group doesn't exist.
	// Returns models.ErrShareNotFound if the share doesn't exist.
	SetGroupSharePermission(ctx context.Context, perm *models.GroupSharePermission) error

	// DeleteGroupSharePermission removes a group's permission for a share.
	// No error if the permission didn't exist.
	DeleteGroupSharePermission(ctx context.Context, groupName, shareName string) error

	// GetGroupSharePermissions returns all share permissions for a group.
	GetGroupSharePermissions(ctx context.Context, groupName string) ([]*models.GroupSharePermission, error)

	// ResolveSharePermission returns the effective permission for a user on a share.
	// Resolution order: user explicit > group permissions (highest wins) > share default
	// Fetches the share's default permission internally.
	ResolveSharePermission(ctx context.Context, user *models.User, shareName string) (models.SharePermission, error)
}

// MetadataStoreConfigStore provides metadata store configuration CRUD.
//
// These operations manage the configuration records for metadata store backends
// (memory, BadgerDB, PostgreSQL). The actual metadata store instances are
// created and managed by the Runtime.
type MetadataStoreConfigStore interface {
	// GetMetadataStore returns a metadata store configuration by name.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	GetMetadataStore(ctx context.Context, name string) (*models.MetadataStoreConfig, error)

	// GetMetadataStoreByID returns a metadata store configuration by ID.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	GetMetadataStoreByID(ctx context.Context, id string) (*models.MetadataStoreConfig, error)

	// ListMetadataStores returns all metadata store configurations.
	ListMetadataStores(ctx context.Context) ([]*models.MetadataStoreConfig, error)

	// CreateMetadataStore creates a new metadata store configuration.
	// The ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateStore if a store with the same name exists.
	CreateMetadataStore(ctx context.Context, store *models.MetadataStoreConfig) (string, error)

	// UpdateMetadataStore updates an existing metadata store configuration.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	UpdateMetadataStore(ctx context.Context, store *models.MetadataStoreConfig) error

	// DeleteMetadataStore deletes a metadata store configuration by name.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	// Returns models.ErrStoreInUse if the store is referenced by any shares.
	DeleteMetadataStore(ctx context.Context, name string) error

	// GetSharesByMetadataStore returns all shares using the given metadata store.
	GetSharesByMetadataStore(ctx context.Context, storeName string) ([]*models.Share, error)
}

// PayloadStoreConfigStore provides payload store configuration CRUD.
//
// These operations manage the configuration records for payload store backends
// (memory, S3). The actual payload store instances are created and managed
// by the Runtime.
type PayloadStoreConfigStore interface {
	// GetPayloadStore returns a payload store configuration by name.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	GetPayloadStore(ctx context.Context, name string) (*models.PayloadStoreConfig, error)

	// GetPayloadStoreByID returns a payload store configuration by ID.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	GetPayloadStoreByID(ctx context.Context, id string) (*models.PayloadStoreConfig, error)

	// ListPayloadStores returns all payload store configurations.
	ListPayloadStores(ctx context.Context) ([]*models.PayloadStoreConfig, error)

	// CreatePayloadStore creates a new payload store configuration.
	// The ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateStore if a store with the same name exists.
	CreatePayloadStore(ctx context.Context, store *models.PayloadStoreConfig) (string, error)

	// UpdatePayloadStore updates an existing payload store configuration.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	UpdatePayloadStore(ctx context.Context, store *models.PayloadStoreConfig) error

	// DeletePayloadStore deletes a payload store configuration by name.
	// Returns models.ErrStoreNotFound if the store doesn't exist.
	// Returns models.ErrStoreInUse if the store is referenced by any shares.
	DeletePayloadStore(ctx context.Context, name string) error

	// GetSharesByPayloadStore returns all shares using the given payload store.
	GetSharesByPayloadStore(ctx context.Context, storeName string) ([]*models.Share, error)
}

// AdapterStore provides adapter configuration CRUD and protocol-specific settings.
//
// Adapter settings (NFS/SMB) are managed alongside adapter CRUD because they
// are tightly coupled: settings cannot exist without an adapter, and adapter
// creation automatically provisions default settings.
type AdapterStore interface {
	// GetAdapter returns an adapter configuration by type.
	// Returns models.ErrAdapterNotFound if the adapter doesn't exist.
	GetAdapter(ctx context.Context, adapterType string) (*models.AdapterConfig, error)

	// ListAdapters returns all adapter configurations.
	ListAdapters(ctx context.Context) ([]*models.AdapterConfig, error)

	// CreateAdapter creates a new adapter configuration.
	// The ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateAdapter if an adapter with the same type exists.
	CreateAdapter(ctx context.Context, adapter *models.AdapterConfig) (string, error)

	// UpdateAdapter updates an existing adapter configuration.
	// Returns models.ErrAdapterNotFound if the adapter doesn't exist.
	UpdateAdapter(ctx context.Context, adapter *models.AdapterConfig) error

	// DeleteAdapter deletes an adapter configuration by type.
	// Returns models.ErrAdapterNotFound if the adapter doesn't exist.
	DeleteAdapter(ctx context.Context, adapterType string) error

	// EnsureDefaultAdapters creates the default NFS and SMB adapters if they don't exist.
	// Returns true if any adapters were created.
	// This should be called during server startup.
	EnsureDefaultAdapters(ctx context.Context) (created bool, err error)

	// GetNFSAdapterSettings returns the NFS adapter settings by adapter ID.
	// Returns models.ErrAdapterNotFound if no settings exist for this adapter.
	GetNFSAdapterSettings(ctx context.Context, adapterID string) (*models.NFSAdapterSettings, error)

	// UpdateNFSAdapterSettings updates the NFS adapter settings.
	// The Version field is incremented atomically for change detection.
	// Returns models.ErrAdapterNotFound if no settings exist for this adapter.
	UpdateNFSAdapterSettings(ctx context.Context, settings *models.NFSAdapterSettings) error

	// ResetNFSAdapterSettings resets NFS adapter settings to defaults.
	// Deletes the existing record and creates a new one with default values.
	// Returns models.ErrAdapterNotFound if the adapter doesn't exist.
	ResetNFSAdapterSettings(ctx context.Context, adapterID string) error

	// GetSMBAdapterSettings returns the SMB adapter settings by adapter ID.
	// Returns models.ErrAdapterNotFound if no settings exist for this adapter.
	GetSMBAdapterSettings(ctx context.Context, adapterID string) (*models.SMBAdapterSettings, error)

	// UpdateSMBAdapterSettings updates the SMB adapter settings.
	// The Version field is incremented atomically for change detection.
	// Returns models.ErrAdapterNotFound if no settings exist for this adapter.
	UpdateSMBAdapterSettings(ctx context.Context, settings *models.SMBAdapterSettings) error

	// ResetSMBAdapterSettings resets SMB adapter settings to defaults.
	// Deletes the existing record and creates a new one with default values.
	// Returns models.ErrAdapterNotFound if the adapter doesn't exist.
	ResetSMBAdapterSettings(ctx context.Context, adapterID string) error

	// EnsureAdapterSettings creates default settings records for adapters that lack them.
	// Called during startup and migration to populate settings for existing adapters.
	EnsureAdapterSettings(ctx context.Context) error
}

// SettingsStore provides generic key-value settings operations.
//
// Settings are used for server-wide configuration that can be changed at
// runtime without restart (e.g., feature flags, tuning parameters).
type SettingsStore interface {
	// GetSetting returns a setting value by key.
	// Returns empty string if the setting doesn't exist.
	GetSetting(ctx context.Context, key string) (string, error)

	// SetSetting creates or updates a setting.
	SetSetting(ctx context.Context, key, value string) error

	// DeleteSetting removes a setting.
	// No error if the setting didn't exist.
	DeleteSetting(ctx context.Context, key string) error

	// ListSettings returns all settings.
	ListSettings(ctx context.Context) ([]*models.Setting, error)
}

// AdminStore provides admin user initialization operations.
//
// These methods are called during server startup to ensure an admin user
// exists. They are separated from UserStore because they have different
// access patterns (startup-only vs request-time).
type AdminStore interface {
	// EnsureAdminUser ensures an admin user exists.
	// If no admin user exists, creates one with a generated password.
	// Returns the initial password if a new admin was created, empty string otherwise.
	// This should be called during server startup.
	EnsureAdminUser(ctx context.Context) (initialPassword string, err error)

	// IsAdminInitialized returns whether the admin user has been initialized.
	IsAdminInitialized(ctx context.Context) (bool, error)
}

// HealthStore provides store health check and lifecycle operations.
//
// These methods are used by health check endpoints and graceful shutdown.
type HealthStore interface {
	// Healthcheck verifies the store is operational.
	// Returns an error if the store is not healthy.
	Healthcheck(ctx context.Context) error

	// Close closes the store and releases resources.
	Close() error
}

// NetgroupStore provides netgroup operations.
// This is a separate interface from Store so that only NFS-aware components
// need to depend on netgroup functionality. Use type assertion to check:
//
//	if ns, ok := store.(NetgroupStore); ok { ... }
type NetgroupStore interface {
	// GetNetgroup returns a netgroup by name with members preloaded.
	// Returns models.ErrNetgroupNotFound if the netgroup doesn't exist.
	GetNetgroup(ctx context.Context, name string) (*models.Netgroup, error)

	// GetNetgroupByID returns a netgroup by its unique ID with members preloaded.
	// Returns models.ErrNetgroupNotFound if the netgroup doesn't exist.
	GetNetgroupByID(ctx context.Context, id string) (*models.Netgroup, error)

	// ListNetgroups returns all netgroups with members preloaded.
	ListNetgroups(ctx context.Context) ([]*models.Netgroup, error)

	// CreateNetgroup creates a new netgroup.
	// The ID will be generated if empty.
	// Returns the generated ID.
	// Returns models.ErrDuplicateNetgroup if a netgroup with the same name exists.
	CreateNetgroup(ctx context.Context, netgroup *models.Netgroup) (string, error)

	// DeleteNetgroup deletes a netgroup by name.
	// Returns models.ErrNetgroupNotFound if the netgroup doesn't exist.
	// Returns models.ErrNetgroupInUse if the netgroup is referenced by any shares.
	DeleteNetgroup(ctx context.Context, name string) error

	// AddNetgroupMember adds a member to a netgroup.
	// Validates the member type and value before adding.
	// Returns models.ErrNetgroupNotFound if the netgroup doesn't exist.
	AddNetgroupMember(ctx context.Context, netgroupName string, member *models.NetgroupMember) error

	// RemoveNetgroupMember removes a member from a netgroup by member ID.
	// Returns models.ErrNetgroupNotFound if the netgroup doesn't exist.
	RemoveNetgroupMember(ctx context.Context, netgroupName, memberID string) error

	// GetNetgroupMembers returns all members of a netgroup.
	// Returns models.ErrNetgroupNotFound if the netgroup doesn't exist.
	GetNetgroupMembers(ctx context.Context, netgroupName string) ([]*models.NetgroupMember, error)

	// GetSharesByNetgroup returns all shares referencing a netgroup.
	// Used to check ErrNetgroupInUse before deletion.
	GetSharesByNetgroup(ctx context.Context, netgroupName string) ([]*models.Share, error)
}

// IdentityMappingStore provides identity mapping operations.
// This is a separate interface from Store so that only NFS/Kerberos-aware
// components need to depend on identity mapping functionality.
type IdentityMappingStore interface {
	// GetIdentityMapping returns an identity mapping by principal.
	// Returns models.ErrMappingNotFound if the mapping doesn't exist.
	GetIdentityMapping(ctx context.Context, principal string) (*models.IdentityMapping, error)

	// ListIdentityMappings returns all identity mappings.
	ListIdentityMappings(ctx context.Context) ([]*models.IdentityMapping, error)

	// CreateIdentityMapping creates a new identity mapping.
	// Returns models.ErrDuplicateMapping if a mapping for this principal already exists.
	CreateIdentityMapping(ctx context.Context, mapping *models.IdentityMapping) error

	// DeleteIdentityMapping deletes an identity mapping by principal.
	// Returns models.ErrMappingNotFound if the mapping doesn't exist.
	DeleteIdentityMapping(ctx context.Context, principal string) error
}

// Store is the composite control plane persistence interface.
//
// It embeds all sub-interfaces to provide the full set of operations.
// Callers that need everything (Runtime, tests) accept Store; individual
// handlers and services accept only the narrowest sub-interface they need.
//
// Thread Safety: Implementations must be safe for concurrent use from multiple
// goroutines.
//
// The Store interface supports both SQLite (single-node) and PostgreSQL (HA) backends.
type Store interface {
	UserStore
	GroupStore
	ShareStore
	PermissionStore
	MetadataStoreConfigStore
	PayloadStoreConfigStore
	AdapterStore
	SettingsStore
	AdminStore
	HealthStore
}
