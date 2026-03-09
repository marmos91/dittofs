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

// BlockStore represents a block store configuration.
type BlockStore struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
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

// createStore creates a store resource (metadata or block) via POST.
// Handles the config serialization from CreateStoreRequest to the wire format.
func createStore[T any](c *Client, path string, req *CreateStoreRequest) (*T, error) {
	configStr, err := serializeConfig(req.Config)
	if err != nil {
		return nil, err
	}
	apiReq := createStoreAPIRequest{Name: req.Name, Type: req.Type, Config: configStr}
	return createResource[T](c, path, apiReq)
}

// updateStore updates a store resource (metadata or block) via PUT.
// Handles the config serialization from UpdateStoreRequest to the wire format.
func updateStore[T any](c *Client, path string, req *UpdateStoreRequest) (*T, error) {
	apiReq := updateStoreAPIRequest{Type: req.Type}
	if req.Config != nil {
		configStr, err := serializeConfig(req.Config)
		if err != nil {
			return nil, err
		}
		apiReq.Config = &configStr
	}
	return updateResource[T](c, path, apiReq)
}

// ListMetadataStores returns all metadata stores.
func (c *Client) ListMetadataStores() ([]MetadataStore, error) {
	return listResources[MetadataStore](c, "/api/v1/store/metadata")
}

// GetMetadataStore returns a metadata store by name.
func (c *Client) GetMetadataStore(name string) (*MetadataStore, error) {
	return getResource[MetadataStore](c, resourcePath("/api/v1/store/metadata/%s", name))
}

// CreateMetadataStore creates a new metadata store.
func (c *Client) CreateMetadataStore(req *CreateStoreRequest) (*MetadataStore, error) {
	return createStore[MetadataStore](c, "/api/v1/store/metadata", req)
}

// UpdateMetadataStore updates an existing metadata store.
func (c *Client) UpdateMetadataStore(name string, req *UpdateStoreRequest) (*MetadataStore, error) {
	return updateStore[MetadataStore](c, resourcePath("/api/v1/store/metadata/%s", name), req)
}

// DeleteMetadataStore deletes a metadata store.
func (c *Client) DeleteMetadataStore(name string) error {
	return deleteResource(c, resourcePath("/api/v1/store/metadata/%s", name))
}

// ListBlockStores returns all block stores of a given kind.
func (c *Client) ListBlockStores(kind string) ([]BlockStore, error) {
	return listResources[BlockStore](c, resourcePath("/api/v1/store/block/%s", kind))
}

// GetBlockStore returns a block store by name and kind.
func (c *Client) GetBlockStore(kind, name string) (*BlockStore, error) {
	return getResource[BlockStore](c, resourcePath("/api/v1/store/block/%s/%s", kind, name))
}

// CreateBlockStore creates a new block store.
func (c *Client) CreateBlockStore(kind string, req *CreateStoreRequest) (*BlockStore, error) {
	return createStore[BlockStore](c, resourcePath("/api/v1/store/block/%s", kind), req)
}

// UpdateBlockStore updates an existing block store.
func (c *Client) UpdateBlockStore(kind, name string, req *UpdateStoreRequest) (*BlockStore, error) {
	return updateStore[BlockStore](c, resourcePath("/api/v1/store/block/%s/%s", kind, name), req)
}

// DeleteBlockStore deletes a block store.
func (c *Client) DeleteBlockStore(kind, name string) error {
	return deleteResource(c, resourcePath("/api/v1/store/block/%s/%s", kind, name))
}
