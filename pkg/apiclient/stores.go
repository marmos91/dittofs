package apiclient

import (
	"encoding/json"
	"fmt"
)

// MetadataStore represents a metadata store configuration.
type MetadataStore struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// PayloadStore represents a payload/content store configuration.
type PayloadStore struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// CreateStoreRequest is the request to create a metadata or payload store.
type CreateStoreRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config any    `json:"-"` // Config is serialized separately as a JSON string
}

// createStoreAPIRequest is the actual API request format.
type createStoreAPIRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config,omitempty"`
}

// UpdateStoreRequest is the request to update a store.
type UpdateStoreRequest struct {
	Type   *string `json:"type,omitempty"`
	Config any     `json:"-"` // Config is serialized separately as a JSON string
}

// updateStoreAPIRequest is the actual API request format.
type updateStoreAPIRequest struct {
	Type   *string `json:"type,omitempty"`
	Config *string `json:"config,omitempty"`
}

// serializeConfig converts a config value to a JSON string for the API.
func serializeConfig(config any) (string, error) {
	if config == nil {
		return "", nil
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("failed to serialize config: %w", err)
	}
	return string(configBytes), nil
}

// ListMetadataStores returns all metadata stores.
func (c *Client) ListMetadataStores() ([]MetadataStore, error) {
	return listResources[MetadataStore](c, "/api/v1/metadata-stores")
}

// GetMetadataStore returns a metadata store by name.
func (c *Client) GetMetadataStore(name string) (*MetadataStore, error) {
	return getResource[MetadataStore](c, resourcePath("/api/v1/metadata-stores/%s", name))
}

// CreateMetadataStore creates a new metadata store.
func (c *Client) CreateMetadataStore(req *CreateStoreRequest) (*MetadataStore, error) {
	configStr, err := serializeConfig(req.Config)
	if err != nil {
		return nil, err
	}
	apiReq := createStoreAPIRequest{Name: req.Name, Type: req.Type, Config: configStr}
	var store MetadataStore
	if err := c.post("/api/v1/metadata-stores", apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// UpdateMetadataStore updates an existing metadata store.
func (c *Client) UpdateMetadataStore(name string, req *UpdateStoreRequest) (*MetadataStore, error) {
	apiReq := updateStoreAPIRequest{Type: req.Type}
	if req.Config != nil {
		configStr, err := serializeConfig(req.Config)
		if err != nil {
			return nil, err
		}
		apiReq.Config = &configStr
	}
	var store MetadataStore
	if err := c.put(fmt.Sprintf("/api/v1/metadata-stores/%s", name), apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// DeleteMetadataStore deletes a metadata store.
func (c *Client) DeleteMetadataStore(name string) error {
	return deleteResource(c, resourcePath("/api/v1/metadata-stores/%s", name))
}

// ListPayloadStores returns all payload stores.
func (c *Client) ListPayloadStores() ([]PayloadStore, error) {
	return listResources[PayloadStore](c, "/api/v1/payload-stores")
}

// GetPayloadStore returns a payload store by name.
func (c *Client) GetPayloadStore(name string) (*PayloadStore, error) {
	return getResource[PayloadStore](c, resourcePath("/api/v1/payload-stores/%s", name))
}

// CreatePayloadStore creates a new payload store.
func (c *Client) CreatePayloadStore(req *CreateStoreRequest) (*PayloadStore, error) {
	configStr, err := serializeConfig(req.Config)
	if err != nil {
		return nil, err
	}
	apiReq := createStoreAPIRequest{Name: req.Name, Type: req.Type, Config: configStr}
	var store PayloadStore
	if err := c.post("/api/v1/payload-stores", apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// UpdatePayloadStore updates an existing payload store.
func (c *Client) UpdatePayloadStore(name string, req *UpdateStoreRequest) (*PayloadStore, error) {
	apiReq := updateStoreAPIRequest{Type: req.Type}
	if req.Config != nil {
		configStr, err := serializeConfig(req.Config)
		if err != nil {
			return nil, err
		}
		apiReq.Config = &configStr
	}
	var store PayloadStore
	if err := c.put(fmt.Sprintf("/api/v1/payload-stores/%s", name), apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// DeletePayloadStore deletes a payload store.
func (c *Client) DeletePayloadStore(name string) error {
	return deleteResource(c, resourcePath("/api/v1/payload-stores/%s", name))
}
