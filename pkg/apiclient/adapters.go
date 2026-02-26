package apiclient

import (
	"encoding/json"
)

// Adapter represents a protocol adapter configuration.
type Adapter struct {
	Type    string          `json:"type"`
	Port    int             `json:"port"`
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config,omitempty"`
}

// CreateAdapterRequest is the request to create an adapter.
type CreateAdapterRequest struct {
	Type    string `json:"type"`
	Port    int    `json:"port,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	Config  any    `json:"config,omitempty"`
}

// UpdateAdapterRequest is the request to update an adapter.
type UpdateAdapterRequest struct {
	Port    *int  `json:"port,omitempty"`
	Enabled *bool `json:"enabled,omitempty"`
	Config  any   `json:"config,omitempty"`
}

// ListAdapters returns all adapters.
func (c *Client) ListAdapters() ([]Adapter, error) {
	return listResources[Adapter](c, "/api/v1/adapters")
}

// GetAdapter returns an adapter by type.
func (c *Client) GetAdapter(adapterType string) (*Adapter, error) {
	return getResource[Adapter](c, resourcePath("/api/v1/adapters/%s", adapterType))
}

// CreateAdapter creates a new adapter.
func (c *Client) CreateAdapter(req *CreateAdapterRequest) (*Adapter, error) {
	return createResource[Adapter](c, "/api/v1/adapters", req)
}

// UpdateAdapter updates an existing adapter.
func (c *Client) UpdateAdapter(adapterType string, req *UpdateAdapterRequest) (*Adapter, error) {
	return updateResource[Adapter](c, resourcePath("/api/v1/adapters/%s", adapterType), req)
}

// DeleteAdapter deletes an adapter.
func (c *Client) DeleteAdapter(adapterType string) error {
	return deleteResource(c, resourcePath("/api/v1/adapters/%s", adapterType))
}
