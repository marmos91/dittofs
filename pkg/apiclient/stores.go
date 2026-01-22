package apiclient

import (
	"encoding/json"
	"fmt"
)

// MetadataStore represents a metadata store configuration.
type MetadataStore struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// PayloadStore represents a payload/content store configuration.
type PayloadStore struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// CreateMetadataStoreRequest is the request to create a metadata store.
type CreateMetadataStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config any    `json:"config,omitempty"`
}

// CreatePayloadStoreRequest is the request to create a payload store.
type CreatePayloadStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config any    `json:"config,omitempty"`
}

// UpdateStoreRequest is the request to update a store.
type UpdateStoreRequest struct {
	Config any `json:"config,omitempty"`
}

// ListMetadataStores returns all metadata stores.
func (c *Client) ListMetadataStores() ([]MetadataStore, error) {
	var stores []MetadataStore
	if err := c.get("/api/v1/stores/metadata", &stores); err != nil {
		return nil, err
	}
	return stores, nil
}

// GetMetadataStore returns a metadata store by name.
func (c *Client) GetMetadataStore(name string) (*MetadataStore, error) {
	var store MetadataStore
	if err := c.get(fmt.Sprintf("/api/v1/stores/metadata/%s", name), &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// CreateMetadataStore creates a new metadata store.
func (c *Client) CreateMetadataStore(req *CreateMetadataStoreRequest) (*MetadataStore, error) {
	var store MetadataStore
	if err := c.post("/api/v1/stores/metadata", req, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// UpdateMetadataStore updates an existing metadata store.
func (c *Client) UpdateMetadataStore(name string, req *UpdateStoreRequest) (*MetadataStore, error) {
	var store MetadataStore
	if err := c.put(fmt.Sprintf("/api/v1/stores/metadata/%s", name), req, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// DeleteMetadataStore deletes a metadata store.
func (c *Client) DeleteMetadataStore(name string) error {
	return c.delete(fmt.Sprintf("/api/v1/stores/metadata/%s", name), nil)
}

// ListPayloadStores returns all payload stores.
func (c *Client) ListPayloadStores() ([]PayloadStore, error) {
	var stores []PayloadStore
	if err := c.get("/api/v1/stores/payload", &stores); err != nil {
		return nil, err
	}
	return stores, nil
}

// GetPayloadStore returns a payload store by name.
func (c *Client) GetPayloadStore(name string) (*PayloadStore, error) {
	var store PayloadStore
	if err := c.get(fmt.Sprintf("/api/v1/stores/payload/%s", name), &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// CreatePayloadStore creates a new payload store.
func (c *Client) CreatePayloadStore(req *CreatePayloadStoreRequest) (*PayloadStore, error) {
	var store PayloadStore
	if err := c.post("/api/v1/stores/payload", req, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// UpdatePayloadStore updates an existing payload store.
func (c *Client) UpdatePayloadStore(name string, req *UpdateStoreRequest) (*PayloadStore, error) {
	var store PayloadStore
	if err := c.put(fmt.Sprintf("/api/v1/stores/payload/%s", name), req, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// DeletePayloadStore deletes a payload store.
func (c *Client) DeletePayloadStore(name string) error {
	return c.delete(fmt.Sprintf("/api/v1/stores/payload/%s", name), nil)
}
