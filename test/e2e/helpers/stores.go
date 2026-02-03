//go:build e2e

package helpers

import (
	"encoding/json"
	"fmt"
)

// =============================================================================
// Metadata Store Types
// =============================================================================

// MetadataStore represents a metadata store returned from the API.
type MetadataStore struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// MetadataStoreOption is a functional option for metadata store operations.
type MetadataStoreOption func(*metadataStoreOptions)

type metadataStoreOptions struct {
	// BadgerDB specific
	dbPath string
	// PostgreSQL specific (for raw config)
	rawConfig string
}

// WithMetaDBPath sets the BadgerDB database path.
func WithMetaDBPath(path string) MetadataStoreOption {
	return func(o *metadataStoreOptions) {
		o.dbPath = path
	}
}

// WithMetaRawConfig sets the raw JSON config for the store.
// Use this for complex configurations like PostgreSQL.
func WithMetaRawConfig(config string) MetadataStoreOption {
	return func(o *metadataStoreOptions) {
		o.rawConfig = config
	}
}

// =============================================================================
// Metadata Store CRUD Methods
// =============================================================================

// CreateMetadataStore creates a new metadata store via the CLI.
// storeType should be "memory", "badger", or "postgres".
func (r *CLIRunner) CreateMetadataStore(name, storeType string, opts ...MetadataStoreOption) (*MetadataStore, error) {
	options := &metadataStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "metadata", "add", "--name", name, "--type", storeType}

	// Add type-specific options
	if options.dbPath != "" {
		args = append(args, "--db-path", options.dbPath)
	}
	if options.rawConfig != "" {
		args = append(args, "--config", options.rawConfig)
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store MetadataStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// ListMetadataStores lists all metadata stores via the CLI.
func (r *CLIRunner) ListMetadataStores() ([]*MetadataStore, error) {
	output, err := r.Run("store", "metadata", "list")
	if err != nil {
		return nil, err
	}

	var stores []*MetadataStore
	if err := ParseJSONResponse(output, &stores); err != nil {
		return nil, err
	}

	return stores, nil
}

// GetMetadataStore retrieves a metadata store by name.
// Since there's no dedicated 'store metadata get' command, this lists all
// stores and filters by name.
func (r *CLIRunner) GetMetadataStore(name string) (*MetadataStore, error) {
	stores, err := r.ListMetadataStores()
	if err != nil {
		return nil, err
	}

	for _, s := range stores {
		if s.Name == name {
			return s, nil
		}
	}

	return nil, fmt.Errorf("metadata store not found: %s", name)
}

// EditMetadataStore edits an existing metadata store via the CLI.
func (r *CLIRunner) EditMetadataStore(name string, opts ...MetadataStoreOption) (*MetadataStore, error) {
	options := &metadataStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "metadata", "edit", name}

	// Add options that were set
	if options.dbPath != "" {
		args = append(args, "--db-path", options.dbPath)
	}
	if options.rawConfig != "" {
		args = append(args, "--config", options.rawConfig)
	}

	// If no options were provided, the CLI might enter interactive mode
	if options.dbPath == "" && options.rawConfig == "" {
		return nil, fmt.Errorf("at least one option (WithMetaDBPath or WithMetaRawConfig) is required for EditMetadataStore")
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store MetadataStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// DeleteMetadataStore deletes a metadata store via the CLI.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteMetadataStore(name string) error {
	_, err := r.Run("store", "metadata", "remove", name, "--force")
	return err
}

// =============================================================================
// Payload Store Types
// =============================================================================

// PayloadStore represents a payload store returned from the API.
type PayloadStore struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// PayloadStoreOption is a functional option for payload store operations.
type PayloadStoreOption func(*payloadStoreOptions)

type payloadStoreOptions struct {
	// Filesystem specific
	path string
	// S3 specific
	bucket    string
	region    string
	endpoint  string
	accessKey string
	secretKey string
	// Generic JSON config
	rawConfig string
}

// WithPayloadPath sets the filesystem storage path.
func WithPayloadPath(path string) PayloadStoreOption {
	return func(o *payloadStoreOptions) {
		o.path = path
	}
}

// WithPayloadS3Config sets S3 configuration.
func WithPayloadS3Config(bucket, region, endpoint, accessKey, secretKey string) PayloadStoreOption {
	return func(o *payloadStoreOptions) {
		o.bucket = bucket
		o.region = region
		o.endpoint = endpoint
		o.accessKey = accessKey
		o.secretKey = secretKey
	}
}

// WithPayloadRawConfig sets raw JSON config for advanced use cases.
func WithPayloadRawConfig(config string) PayloadStoreOption {
	return func(o *payloadStoreOptions) {
		o.rawConfig = config
	}
}

// =============================================================================
// Payload Store CRUD Methods
// =============================================================================

// CreatePayloadStore creates a new payload store via the CLI.
// Supports memory, filesystem, and s3 store types.
func (r *CLIRunner) CreatePayloadStore(name, storeType string, opts ...PayloadStoreOption) (*PayloadStore, error) {
	options := &payloadStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "payload", "add", "--name", name, "--type", storeType}

	// Add type-specific options
	if options.rawConfig != "" {
		args = append(args, "--config", options.rawConfig)
	} else if options.path != "" {
		args = append(args, "--path", options.path)
	} else if options.bucket != "" {
		// S3 config
		args = append(args, "--bucket", options.bucket)
		if options.region != "" {
			args = append(args, "--region", options.region)
		}
		if options.endpoint != "" {
			args = append(args, "--endpoint", options.endpoint)
		}
		if options.accessKey != "" {
			args = append(args, "--access-key", options.accessKey)
		}
		if options.secretKey != "" {
			args = append(args, "--secret-key", options.secretKey)
		}
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store PayloadStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// ListPayloadStores lists all payload stores via the CLI.
func (r *CLIRunner) ListPayloadStores() ([]*PayloadStore, error) {
	output, err := r.Run("store", "payload", "list")
	if err != nil {
		return nil, err
	}

	var stores []*PayloadStore
	if err := ParseJSONResponse(output, &stores); err != nil {
		return nil, err
	}

	return stores, nil
}

// GetPayloadStore retrieves a payload store by name.
// Since there's no dedicated 'store payload get' command, this lists all stores and filters.
func (r *CLIRunner) GetPayloadStore(name string) (*PayloadStore, error) {
	stores, err := r.ListPayloadStores()
	if err != nil {
		return nil, err
	}

	for _, s := range stores {
		if s.Name == name {
			return s, nil
		}
	}

	return nil, fmt.Errorf("payload store not found: %s", name)
}

// EditPayloadStore edits an existing payload store via the CLI.
func (r *CLIRunner) EditPayloadStore(name string, opts ...PayloadStoreOption) (*PayloadStore, error) {
	options := &payloadStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "payload", "edit", name}
	hasUpdate := false

	// Add type-specific options
	if options.rawConfig != "" {
		args = append(args, "--config", options.rawConfig)
		hasUpdate = true
	} else if options.path != "" {
		args = append(args, "--path", options.path)
		hasUpdate = true
	} else if options.bucket != "" || options.region != "" || options.endpoint != "" || options.accessKey != "" || options.secretKey != "" {
		// S3 config - at least one field was set
		if options.bucket != "" {
			args = append(args, "--bucket", options.bucket)
		}
		if options.region != "" {
			args = append(args, "--region", options.region)
		}
		if options.endpoint != "" {
			args = append(args, "--endpoint", options.endpoint)
		}
		if options.accessKey != "" {
			args = append(args, "--access-key", options.accessKey)
		}
		if options.secretKey != "" {
			args = append(args, "--secret-key", options.secretKey)
		}
		hasUpdate = true
	}

	if !hasUpdate {
		return nil, fmt.Errorf("at least one option is required for EditPayloadStore")
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store PayloadStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// DeletePayloadStore deletes a payload store via the CLI.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeletePayloadStore(name string) error {
	_, err := r.Run("store", "payload", "remove", name, "--force")
	return err
}
