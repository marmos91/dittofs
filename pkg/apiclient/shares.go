package apiclient

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// normalizeShareNameForAPI strips all leading slashes from share names for API URLs.
// This removes all leading slashes (e.g., "///export" becomes "export") to ensure
// valid URL paths. The server will normalize them back to include the leading slash.
func normalizeShareNameForAPI(name string) string {
	return strings.TrimLeft(name, "/")
}

// Share represents a share in the system.
type Share struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	MetadataStoreID    string  `json:"metadata_store_id"`
	LocalBlockStoreID  string  `json:"local_block_store_id"`
	RemoteBlockStoreID *string `json:"remote_block_store_id"`
	ReadOnly           bool    `json:"read_only,omitempty"`
	// Enabled mirrors models.Share.Enabled. The tag is deliberately NOT
	// omitempty: `false` is semantically meaningful ("share is
	// disabled") whereas read_only:false is the inert default.
	Enabled           bool     `json:"enabled"`
	EncryptData       bool     `json:"encrypt_data,omitempty"`
	DefaultPermission string   `json:"default_permission,omitempty"`
	Description       string   `json:"description,omitempty"`
	BlockedOperations []string `json:"blocked_operations,omitempty"`
	RetentionPolicy   string   `json:"retention_policy,omitempty"`
	RetentionTTL      string   `json:"retention_ttl,omitempty"`
	LocalStoreSize    string   `json:"local_store_size,omitempty"`
	ReadBufferSize    string   `json:"read_buffer_size,omitempty"`
	QuotaBytes        string   `json:"quota_bytes,omitempty"`
	UsedBytes         int64    `json:"used_bytes"`
	PhysicalBytes     int64    `json:"physical_bytes"`
	UsagePercent      float64  `json:"usage_percent"`
	// AclFlagInheritedCanonicalization mirrors models.Share — Refs #514.
	// No omitempty: `false` is operator-meaningful (canonicalization
	// disabled) and consumers need to render the state explicitly.
	AclFlagInheritedCanonicalization bool `json:"acl_flag_inherited_canonicalization"`
	// AccessBasedEnumeration mirrors models.Share — Refs #532. No omitempty
	// for the same reason.
	AccessBasedEnumeration bool `json:"access_based_enumeration"`
	// ChangeNotifyDisabled mirrors models.Share. No omitempty: `false` is
	// the operator-meaningful "change notify enabled" state.
	ChangeNotifyDisabled bool `json:"change_notify_disabled"`
	// StreamsDisabled mirrors models.Share. No omitempty for the same
	// reason as ChangeNotifyDisabled.
	StreamsDisabled bool `json:"streams_disabled"`
	// ContinuousAvailability mirrors models.Share — Refs #739. No omitempty:
	// operators need to render the explicit CA state.
	ContinuousAvailability bool `json:"continuous_availability"`
	// Per-share recycle-bin policy (#190). Mirrors the server
	// ShareResponse so dfsctl share show can render the trash config.
	TrashEnabled         bool      `json:"trash_enabled"`
	TrashRetentionDays   int       `json:"trash_retention_days"`
	TrashRestrictToAdmin bool      `json:"trash_restrict_to_admin"`
	TrashMaxBytes        int64     `json:"trash_max_bytes"`
	TrashExcludePatterns []string  `json:"trash_exclude_patterns,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// CreateShareRequest is the request to create a share.
type CreateShareRequest struct {
	Name              string    `json:"name"`
	MetadataStoreID   string    `json:"metadata_store_id"`
	LocalBlockStore   string    `json:"local_block_store"`
	RemoteBlockStore  *string   `json:"remote_block_store,omitempty"`
	ReadOnly          bool      `json:"read_only,omitempty"`
	EncryptData       bool      `json:"encrypt_data,omitempty"`
	DefaultPermission string    `json:"default_permission,omitempty"`
	Description       string    `json:"description,omitempty"`
	BlockedOperations *[]string `json:"blocked_operations,omitempty"`
	RetentionPolicy   string    `json:"retention_policy,omitempty"`
	RetentionTTL      string    `json:"retention_ttl,omitempty"`
	LocalStoreSize    string    `json:"local_store_size,omitempty"`
	ReadBufferSize    string    `json:"read_buffer_size,omitempty"`
	QuotaBytes        string    `json:"quota_bytes,omitempty"`
	// AclFlagInheritedCanonicalization — Refs #514. Pointer so callers can
	// distinguish "unset → server default (true)" from "explicit false".
	AclFlagInheritedCanonicalization *bool `json:"acl_flag_inherited_canonicalization,omitempty"`
	// AccessBasedEnumeration — Refs #532. Pointer so callers can distinguish
	// "unset → server default (false)" from "explicit true".
	AccessBasedEnumeration *bool `json:"access_based_enumeration,omitempty"`
	// ChangeNotifyDisabled — pointer so callers can distinguish "unset →
	// server default (false)" from "explicit true".
	ChangeNotifyDisabled *bool `json:"change_notify_disabled,omitempty"`
	// StreamsDisabled — pointer so callers can distinguish "unset →
	// server default (false)" from "explicit true".
	StreamsDisabled *bool `json:"streams_disabled,omitempty"`
	// ContinuousAvailability — Refs #739. Pointer so callers can distinguish
	// "unset → server default (false)" from "explicit true".
	ContinuousAvailability *bool `json:"continuous_availability,omitempty"`
	// Per-share recycle-bin policy (#190). Pointers so nil keeps the
	// server default (trash disabled, zero limits).
	TrashEnabled         *bool    `json:"trash_enabled,omitempty"`
	TrashRetentionDays   *int     `json:"trash_retention_days,omitempty"`
	TrashRestrictToAdmin *bool    `json:"trash_restrict_to_admin,omitempty"`
	TrashMaxBytes        *int64   `json:"trash_max_bytes,omitempty"`
	TrashExcludePatterns []string `json:"trash_exclude_patterns,omitempty"`
}

// UpdateShareRequest is the request to update a share.
type UpdateShareRequest struct {
	LocalBlockStoreID  *string   `json:"local_block_store_id,omitempty"`
	RemoteBlockStoreID *string   `json:"remote_block_store_id,omitempty"`
	ReadOnly           *bool     `json:"read_only,omitempty"`
	EncryptData        *bool     `json:"encrypt_data,omitempty"`
	DefaultPermission  *string   `json:"default_permission,omitempty"`
	Description        *string   `json:"description,omitempty"`
	BlockedOperations  *[]string `json:"blocked_operations,omitempty"`
	RetentionPolicy    *string   `json:"retention_policy,omitempty"`
	RetentionTTL       *string   `json:"retention_ttl,omitempty"`
	LocalStoreSize     *string   `json:"local_store_size,omitempty"`
	ReadBufferSize     *string   `json:"read_buffer_size,omitempty"`
	QuotaBytes         *string   `json:"quota_bytes,omitempty"`
	// AclFlagInheritedCanonicalization — Refs #514. nil = no change;
	// non-nil = explicit set. Takes effect on adapter restart.
	AclFlagInheritedCanonicalization *bool `json:"acl_flag_inherited_canonicalization,omitempty"`
	// AccessBasedEnumeration — Refs #532. nil = no change; non-nil =
	// explicit set. Takes effect on adapter restart.
	AccessBasedEnumeration *bool `json:"access_based_enumeration,omitempty"`
	// ChangeNotifyDisabled — nil = no change; non-nil = explicit set. Takes
	// effect on adapter restart.
	ChangeNotifyDisabled *bool `json:"change_notify_disabled,omitempty"`
	// StreamsDisabled — nil = no change; non-nil = explicit set. Takes
	// effect on adapter restart.
	StreamsDisabled *bool `json:"streams_disabled,omitempty"`
	// ContinuousAvailability — Refs #739. nil = no change; non-nil = explicit
	// set. Takes effect on adapter restart.
	ContinuousAvailability *bool `json:"continuous_availability,omitempty"`
	// Per-share recycle-bin policy (#190). nil = no change; non-nil =
	// explicit set. Applied live by the server; turning trash off
	// auto-empties the bin.
	TrashEnabled         *bool    `json:"trash_enabled,omitempty"`
	TrashRetentionDays   *int     `json:"trash_retention_days,omitempty"`
	TrashRestrictToAdmin *bool    `json:"trash_restrict_to_admin,omitempty"`
	TrashMaxBytes        *int64   `json:"trash_max_bytes,omitempty"`
	TrashExcludePatterns []string `json:"trash_exclude_patterns,omitempty"`
}

// ShareNFSConfig represents the per-share NFS adapter configuration. Netgroup
// is exposed by name (empty = no association).
type ShareNFSConfig struct {
	Squash             string  `json:"squash"`
	AnonymousUID       *uint32 `json:"anonymous_uid,omitempty"`
	AnonymousGID       *uint32 `json:"anonymous_gid,omitempty"`
	AllowAuthSys       bool    `json:"allow_auth_sys"`
	RequireKerberos    bool    `json:"require_kerberos"`
	MinKerberosLevel   string  `json:"min_kerberos_level"`
	Netgroup           string  `json:"netgroup"`
	DisableReaddirplus bool    `json:"disable_readdirplus"`
}

// PatchShareNFSConfigRequest is the request to update a share's NFS adapter
// config. All fields are optional; nil leaves the field unchanged. Netgroup is
// a pointer-to-string: a non-nil empty string clears the association.
type PatchShareNFSConfigRequest struct {
	Squash             *string `json:"squash,omitempty"`
	AnonymousUID       *uint32 `json:"anonymous_uid,omitempty"`
	AnonymousGID       *uint32 `json:"anonymous_gid,omitempty"`
	AllowAuthSys       *bool   `json:"allow_auth_sys,omitempty"`
	RequireKerberos    *bool   `json:"require_kerberos,omitempty"`
	MinKerberosLevel   *string `json:"min_kerberos_level,omitempty"`
	Netgroup           *string `json:"netgroup,omitempty"`
	DisableReaddirplus *bool   `json:"disable_readdirplus,omitempty"`
}

// SharePermission represents a permission on a share.
type SharePermission struct {
	Type  string `json:"type"`  // "user" or "group"
	Name  string `json:"name"`  // username or group name
	Level string `json:"level"` // "none", "read", "read-write", "admin"
}

// ListShares returns all shares.
func (c *Client) ListShares() ([]Share, error) {
	return listResources[Share](c, "/api/v1/shares")
}

// GetShare returns a share by name.
func (c *Client) GetShare(name string) (*Share, error) {
	var share Share
	if err := c.get(fmt.Sprintf("/api/v1/shares/%s", url.PathEscape(normalizeShareNameForAPI(name))), &share); err != nil {
		return nil, err
	}
	return &share, nil
}

// CreateShare creates a new share.
func (c *Client) CreateShare(req *CreateShareRequest) (*Share, error) {
	var share Share
	if err := c.post("/api/v1/shares", req, &share); err != nil {
		return nil, err
	}
	return &share, nil
}

// UpdateShare updates an existing share.
func (c *Client) UpdateShare(name string, req *UpdateShareRequest) (*Share, error) {
	var share Share
	if err := c.put(fmt.Sprintf("/api/v1/shares/%s", url.PathEscape(normalizeShareNameForAPI(name))), req, &share); err != nil {
		return nil, err
	}
	return &share, nil
}

// DeleteShare deletes a share.
func (c *Client) DeleteShare(name string) error {
	return deleteResource(c, fmt.Sprintf("/api/v1/shares/%s", url.PathEscape(normalizeShareNameForAPI(name))))
}

// GetShareNFSConfig returns the per-share NFS adapter configuration.
func (c *Client) GetShareNFSConfig(name string) (*ShareNFSConfig, error) {
	var cfg ShareNFSConfig
	if err := c.get(fmt.Sprintf("/api/v1/shares/%s/adapters/nfs/config", url.PathEscape(normalizeShareNameForAPI(name))), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PatchShareNFSConfig updates the per-share NFS adapter configuration.
func (c *Client) PatchShareNFSConfig(name string, req *PatchShareNFSConfigRequest) (*ShareNFSConfig, error) {
	var cfg ShareNFSConfig
	if err := c.patch(fmt.Sprintf("/api/v1/shares/%s/adapters/nfs/config", url.PathEscape(normalizeShareNameForAPI(name))), req, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListSharePermissions returns permissions for a share.
func (c *Client) ListSharePermissions(shareName string) ([]SharePermission, error) {
	var perms []SharePermission
	if err := c.get(fmt.Sprintf("/api/v1/shares/%s/permissions", url.PathEscape(normalizeShareNameForAPI(shareName))), &perms); err != nil {
		return nil, err
	}
	return perms, nil
}

// SetUserSharePermission sets a user's permission on a share.
func (c *Client) SetUserSharePermission(shareName, username, level string) error {
	req := map[string]string{"level": level}
	return c.put(fmt.Sprintf("/api/v1/shares/%s/permissions/users/%s", url.PathEscape(normalizeShareNameForAPI(shareName)), username), req, nil)
}

// RemoveUserSharePermission removes a user's permission from a share.
func (c *Client) RemoveUserSharePermission(shareName, username string) error {
	return c.delete(fmt.Sprintf("/api/v1/shares/%s/permissions/users/%s", url.PathEscape(normalizeShareNameForAPI(shareName)), username), nil)
}

// SetGroupSharePermission sets a group's permission on a share.
func (c *Client) SetGroupSharePermission(shareName, groupName, level string) error {
	req := map[string]string{"level": level}
	return c.put(fmt.Sprintf("/api/v1/shares/%s/permissions/groups/%s", url.PathEscape(normalizeShareNameForAPI(shareName)), groupName), req, nil)
}

// RemoveGroupSharePermission removes a group's permission from a share.
func (c *Client) RemoveGroupSharePermission(shareName, groupName string) error {
	return c.delete(fmt.Sprintf("/api/v1/shares/%s/permissions/groups/%s", url.PathEscape(normalizeShareNameForAPI(shareName)), groupName), nil)
}

// DisableShare flips Enabled=false on the share. Returns the updated Share
// (with Enabled=false). Admin-only on the server side.
func (c *Client) DisableShare(name string) (*Share, error) {
	var share Share
	if err := c.post(fmt.Sprintf("/api/v1/shares/%s/disable",
		url.PathEscape(normalizeShareNameForAPI(name))), nil, &share); err != nil {
		return nil, err
	}
	return &share, nil
}

// EnableShare flips Enabled=true on the share. Idempotent server-side.
func (c *Client) EnableShare(name string) (*Share, error) {
	var share Share
	if err := c.post(fmt.Sprintf("/api/v1/shares/%s/enable",
		url.PathEscape(normalizeShareNameForAPI(name))), nil, &share); err != nil {
		return nil, err
	}
	return &share, nil
}
