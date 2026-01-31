package apiclient

import (
	"fmt"
	"net/url"
	"strings"
)

// normalizeShareNameForAPI strips all leading slashes from share names for API URLs.
// This removes all leading slashes (e.g., "///export" becomes "export") to ensure
// valid URL paths. The server will normalize them back to include the leading slash.
func normalizeShareNameForAPI(name string) string {
	return strings.TrimLeft(name, "/")
}

// Share represents a share in the system.
type Share struct {
	Name              string `json:"name"`
	MetadataStoreID   string `json:"metadata_store_id"`
	PayloadStoreID    string `json:"payload_store_id"`
	ReadOnly          bool   `json:"read_only,omitempty"`
	DefaultPermission string `json:"default_permission,omitempty"`
	Description       string `json:"description,omitempty"`
}

// CreateShareRequest is the request to create a share.
type CreateShareRequest struct {
	Name              string `json:"name"`
	MetadataStoreID   string `json:"metadata_store_id"`
	PayloadStoreID    string `json:"payload_store_id"`
	ReadOnly          bool   `json:"read_only,omitempty"`
	DefaultPermission string `json:"default_permission,omitempty"`
	Description       string `json:"description,omitempty"`
}

// UpdateShareRequest is the request to update a share.
type UpdateShareRequest struct {
	ReadOnly          *bool   `json:"read_only,omitempty"`
	DefaultPermission *string `json:"default_permission,omitempty"`
	Description       *string `json:"description,omitempty"`
}

// SharePermission represents a permission on a share.
type SharePermission struct {
	Type  string `json:"type"`  // "user" or "group"
	Name  string `json:"name"`  // username or group name
	Level string `json:"level"` // "none", "read", "read-write", "admin"
}

// ListShares returns all shares.
func (c *Client) ListShares() ([]Share, error) {
	var shares []Share
	if err := c.get("/api/v1/shares", &shares); err != nil {
		return nil, err
	}
	return shares, nil
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
	return c.delete(fmt.Sprintf("/api/v1/shares/%s", url.PathEscape(normalizeShareNameForAPI(name))), nil)
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
