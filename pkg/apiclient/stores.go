package apiclient

import (
	"encoding/json"
	"fmt"
	"time"
)

// MetadataStore represents a metadata store configuration.
type MetadataStore struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// BlockStore represents a block store configuration.
type BlockStore struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// CreateStoreRequest is the request to create a metadata or block store.
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
	return listResources[MetadataStore](c, "/api/v1/store/metadata")
}

// GetMetadataStore returns a metadata store by name.
func (c *Client) GetMetadataStore(name string) (*MetadataStore, error) {
	return getResource[MetadataStore](c, fmt.Sprintf("/api/v1/store/metadata/%s", name))
}

// CreateMetadataStore creates a new metadata store.
func (c *Client) CreateMetadataStore(req *CreateStoreRequest) (*MetadataStore, error) {
	configStr, err := serializeConfig(req.Config)
	if err != nil {
		return nil, err
	}
	apiReq := createStoreAPIRequest{Name: req.Name, Type: req.Type, Config: configStr}
	var store MetadataStore
	if err := c.post("/api/v1/store/metadata", apiReq, &store); err != nil {
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
	if err := c.put(fmt.Sprintf("/api/v1/store/metadata/%s", name), apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// DeleteMetadataStore deletes a metadata store.
func (c *Client) DeleteMetadataStore(name string) error {
	return deleteResource(c, fmt.Sprintf("/api/v1/store/metadata/%s", name))
}

// MetadataStoreHealthResult holds the result of a metadata store health check.
type MetadataStoreHealthResult struct {
	Healthy   bool   `json:"healthy"`
	LatencyMs int64  `json:"latency_ms"`
	CheckedAt string `json:"checked_at"`
	Details   string `json:"details,omitempty"`
}

// MetadataStoreHealth performs a health check on a metadata store.
func (c *Client) MetadataStoreHealth(name string) (*MetadataStoreHealthResult, error) {
	return getResource[MetadataStoreHealthResult](c, fmt.Sprintf("/api/v1/store/metadata/%s/health", name))
}

// ListBlockStores returns all block stores of a given kind.
func (c *Client) ListBlockStores(kind string) ([]BlockStore, error) {
	return listResources[BlockStore](c, fmt.Sprintf("/api/v1/store/block/%s", kind))
}

// GetBlockStore returns a block store by name and kind.
func (c *Client) GetBlockStore(kind, name string) (*BlockStore, error) {
	return getResource[BlockStore](c, fmt.Sprintf("/api/v1/store/block/%s/%s", kind, name))
}

// CreateBlockStore creates a new block store.
func (c *Client) CreateBlockStore(kind string, req *CreateStoreRequest) (*BlockStore, error) {
	configStr, err := serializeConfig(req.Config)
	if err != nil {
		return nil, err
	}
	apiReq := createStoreAPIRequest{Name: req.Name, Type: req.Type, Config: configStr}
	var store BlockStore
	if err := c.post(fmt.Sprintf("/api/v1/store/block/%s", kind), apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// UpdateBlockStore updates an existing block store.
func (c *Client) UpdateBlockStore(kind, name string, req *UpdateStoreRequest) (*BlockStore, error) {
	apiReq := updateStoreAPIRequest{Type: req.Type}
	if req.Config != nil {
		configStr, err := serializeConfig(req.Config)
		if err != nil {
			return nil, err
		}
		apiReq.Config = &configStr
	}
	var store BlockStore
	if err := c.put(fmt.Sprintf("/api/v1/store/block/%s/%s", kind, name), apiReq, &store); err != nil {
		return nil, err
	}
	return &store, nil
}

// DeleteBlockStore deletes a block store.
func (c *Client) DeleteBlockStore(kind, name string) error {
	return deleteResource(c, fmt.Sprintf("/api/v1/store/block/%s/%s", kind, name))
}

// BlockStoreHealthResult holds the result of a block store health check.
type BlockStoreHealthResult struct {
	Healthy   bool   `json:"healthy"`
	LatencyMs int64  `json:"latency_ms"`
	CheckedAt string `json:"checked_at"`
	Details   string `json:"details,omitempty"`
}

// BlockStoreHealth performs a health check on a block store.
// The server always returns 200 with the health status in the response body.
func (c *Client) BlockStoreHealth(kind, name string) (*BlockStoreHealthResult, error) {
	return getResource[BlockStoreHealthResult](c, fmt.Sprintf("/api/v1/store/block/%s/%s/health", kind, name))
}
