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
// Block Store Types
// =============================================================================

// BlockStore represents a block store returned from the API.
type BlockStore struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// BlockStoreOption is a functional option for block store operations.
type BlockStoreOption func(*blockStoreOptions)

type blockStoreOptions struct {
	// S3 specific
	bucket    string
	region    string
	endpoint  string
	accessKey string
	secretKey string
	// Generic JSON config
	rawConfig string
}

// WithBlockS3Config sets S3 configuration for block store creation.
func WithBlockS3Config(bucket, region, endpoint, accessKey, secretKey string) BlockStoreOption {
	return func(o *blockStoreOptions) {
		o.bucket = bucket
		o.region = region
		o.endpoint = endpoint
		o.accessKey = accessKey
		o.secretKey = secretKey
	}
}

// WithBlockRawConfig sets raw JSON config for advanced use cases.
func WithBlockRawConfig(config string) BlockStoreOption {
	return func(o *blockStoreOptions) {
		o.rawConfig = config
	}
}

// =============================================================================
// Block Store CRUD Methods
// =============================================================================

// appendBlockStoreConfigArgs appends config-related CLI arguments from blockStoreOptions.
// Shared by both local and remote block store creation.
func appendBlockStoreConfigArgs(args []string, options *blockStoreOptions) []string {
	if options.rawConfig != "" {
		return append(args, "--config", options.rawConfig)
	}
	if options.bucket != "" {
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
	return args
}

// CreateLocalBlockStore creates a new local block store via the CLI.
// Supports memory and fs store types.
func (r *CLIRunner) CreateLocalBlockStore(name, storeType string, opts ...BlockStoreOption) (*BlockStore, error) {
	options := &blockStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "block", "local", "add", "--name", name, "--type", storeType}
	args = appendBlockStoreConfigArgs(args, options)

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store BlockStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// CreateRemoteBlockStore creates a new remote block store via the CLI.
// Supports memory and s3 store types.
func (r *CLIRunner) CreateRemoteBlockStore(name, storeType string, opts ...BlockStoreOption) (*BlockStore, error) {
	options := &blockStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "block", "remote", "add", "--name", name, "--type", storeType}
	args = appendBlockStoreConfigArgs(args, options)

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store BlockStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// ListLocalBlockStores lists all local block stores via the CLI.
func (r *CLIRunner) ListLocalBlockStores() ([]*BlockStore, error) {
	output, err := r.Run("store", "block", "local", "list")
	if err != nil {
		return nil, err
	}

	var stores []*BlockStore
	if err := ParseJSONResponse(output, &stores); err != nil {
		return nil, err
	}

	return stores, nil
}

// GetLocalBlockStore retrieves a local block store by name.
func (r *CLIRunner) GetLocalBlockStore(name string) (*BlockStore, error) {
	stores, err := r.ListLocalBlockStores()
	if err != nil {
		return nil, err
	}

	for _, s := range stores {
		if s.Name == name {
			return s, nil
		}
	}

	return nil, fmt.Errorf("local block store not found: %s", name)
}

// EditLocalBlockStore edits an existing local block store via the CLI.
func (r *CLIRunner) EditLocalBlockStore(name string, opts ...BlockStoreOption) (*BlockStore, error) {
	options := &blockStoreOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"store", "block", "local", "edit", name}
	before := len(args)
	args = appendBlockStoreConfigArgs(args, options)

	if len(args) == before {
		return nil, fmt.Errorf("at least one option is required for EditLocalBlockStore")
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var store BlockStore
	if err := ParseJSONResponse(output, &store); err != nil {
		return nil, err
	}

	return &store, nil
}

// DeleteLocalBlockStore deletes a local block store via the CLI.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteLocalBlockStore(name string) error {
	_, err := r.Run("store", "block", "local", "remove", name, "--force")
	return err
}

// DeleteRemoteBlockStore deletes a remote block store via the CLI.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteRemoteBlockStore(name string) error {
	_, err := r.Run("store", "block", "remote", "remove", name, "--force")
	return err
}
