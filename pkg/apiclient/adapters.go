package apiclient

import (
	"encoding/json"
	"fmt"
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
	var adapters []Adapter
	if err := c.get("/api/v1/adapters", &adapters); err != nil {
		return nil, err
	}
	return adapters, nil
}

// GetAdapter returns an adapter by type.
func (c *Client) GetAdapter(adapterType string) (*Adapter, error) {
	var adapter Adapter
	if err := c.get(fmt.Sprintf("/api/v1/adapters/%s", adapterType), &adapter); err != nil {
		return nil, err
	}
	return &adapter, nil
}

// CreateAdapter creates a new adapter.
func (c *Client) CreateAdapter(req *CreateAdapterRequest) (*Adapter, error) {
	var adapter Adapter
	if err := c.post("/api/v1/adapters", req, &adapter); err != nil {
		return nil, err
	}
	return &adapter, nil
}

// UpdateAdapter updates an existing adapter.
func (c *Client) UpdateAdapter(adapterType string, req *UpdateAdapterRequest) (*Adapter, error) {
	var adapter Adapter
	if err := c.put(fmt.Sprintf("/api/v1/adapters/%s", adapterType), req, &adapter); err != nil {
		return nil, err
	}
	return &adapter, nil
}

// DeleteAdapter deletes an adapter.
func (c *Client) DeleteAdapter(adapterType string) error {
	return c.delete(fmt.Sprintf("/api/v1/adapters/%s", adapterType), nil)
}
