package apiclient

import "net/url"

// SIDMapping represents a durable foreign-SID -> Unix UID/GID allocation.
type SIDMapping struct {
	SID         string `json:"sid"`
	UnixID      uint32 `json:"unix_id"`
	IsGroup     bool   `json:"is_group"`
	DisplayName string `json:"display_name,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// ListSIDMappings returns all durable foreign-SID mappings.
func (c *Client) ListSIDMappings() ([]SIDMapping, error) {
	return listResources[SIDMapping](c, "/api/v1/sid-mappings")
}

// DeleteSIDMapping removes a foreign-SID mapping by SID.
func (c *Client) DeleteSIDMapping(sid string) error {
	return deleteResource(c, resourcePath("/api/v1/sid-mappings/%s", url.PathEscape(sid)))
}
