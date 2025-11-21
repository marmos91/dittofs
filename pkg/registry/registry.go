package registry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Registry manages all named resources: metadata stores, content stores, and shares.
// It provides thread-safe registration and lookup of all server resources.
//
// The Registry also tracks active mounts (NFS clients that have mounted shares).
// Mount information is ephemeral and kept in-memory only.
//
// Example usage:
//
//	reg := NewRegistry()
//	reg.RegisterMetadataStore("badger-main", badgerStore)
//	reg.RegisterContentStore("local-disk", fsStore)
//	reg.AddShare("export", "badger-main", "local-disk", false)
//
//	share, _ := reg.GetShare("/export")
//	metaStore, _ := reg.GetMetadataStoreForShare("/export")
type Registry struct {
	mu       sync.RWMutex
	metadata map[string]metadata.MetadataStore
	content  map[string]content.ContentStore
	shares   map[string]*Share
	mounts   map[string]*MountInfo // key: clientAddr, value: mount info
}

// MountInfo represents an active NFS mount from a client.
type MountInfo struct {
	ClientAddr string // Client IP address
	ShareName  string // Name of the mounted share
	MountTime  int64  // Unix timestamp when mounted
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		metadata: make(map[string]metadata.MetadataStore),
		content:  make(map[string]content.ContentStore),
		shares:   make(map[string]*Share),
		mounts:   make(map[string]*MountInfo),
	}
}

// RegisterMetadataStore adds a named metadata store to the registry.
// Returns an error if a store with the same name already exists.
func (r *Registry) RegisterMetadataStore(name string, store metadata.MetadataStore) error {
	if store == nil {
		return fmt.Errorf("cannot register nil metadata store")
	}
	if name == "" {
		return fmt.Errorf("cannot register metadata store with empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.metadata[name]; exists {
		return fmt.Errorf("metadata store %q already registered", name)
	}

	r.metadata[name] = store
	return nil
}

// RegisterContentStore adds a named content store to the registry.
// Returns an error if a store with the same name already exists.
func (r *Registry) RegisterContentStore(name string, store content.ContentStore) error {
	if store == nil {
		return fmt.Errorf("cannot register nil content store")
	}
	if name == "" {
		return fmt.Errorf("cannot register content store with empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.content[name]; exists {
		return fmt.Errorf("content store %q already registered", name)
	}

	r.content[name] = store
	return nil
}

// AddShare creates and registers a new share with the given configuration.
// This method:
//  1. Validates that the share doesn't already exist
//  2. Validates that the referenced stores exist
//  3. Creates the root directory in the metadata store
//  4. Registers the share in the registry with full configuration
//
// Returns an error if:
// - A share with the same name already exists
// - The referenced metadata or content stores don't exist
// - The metadata store fails to create the root directory
func (r *Registry) AddShare(ctx context.Context, config *ShareConfig) error {
	if config.Name == "" {
		return fmt.Errorf("cannot add share with empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if share already exists
	if _, exists := r.shares[config.Name]; exists {
		return fmt.Errorf("share %q already exists", config.Name)
	}

	// Validate that stores exist
	metadataStore, exists := r.metadata[config.MetadataStore]
	if !exists {
		return fmt.Errorf("metadata store %q not found", config.MetadataStore)
	}
	if _, exists := r.content[config.ContentStore]; !exists {
		return fmt.Errorf("content store %q not found", config.ContentStore)
	}

	// Create root directory in metadata store
	// Complete attributes with defaults if needed
	rootAttr := config.RootAttr
	if rootAttr.Type == 0 {
		rootAttr.Type = metadata.FileTypeDirectory
	}
	if rootAttr.Mode == 0 {
		rootAttr.Mode = 0755
	}
	if rootAttr.Atime.IsZero() {
		now := time.Now()
		rootAttr.Atime = now
		rootAttr.Mtime = now
		rootAttr.Ctime = now
	}

	// Create the root directory
	rootFile, err := metadataStore.CreateRootDirectory(ctx, config.Name, rootAttr)
	if err != nil {
		return fmt.Errorf("failed to create root directory: %w", err)
	}

	// Encode the root file handle
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return fmt.Errorf("failed to encode root handle: %w", err)
	}

	// Register share in registry with full configuration
	r.shares[config.Name] = &Share{
		Name:                     config.Name,
		MetadataStore:            config.MetadataStore,
		ContentStore:             config.ContentStore,
		RootHandle:               rootHandle,
		ReadOnly:                 config.ReadOnly,
		AllowedClients:           config.AllowedClients,
		DeniedClients:            config.DeniedClients,
		RequireAuth:              config.RequireAuth,
		AllowedAuthMethods:       config.AllowedAuthMethods,
		MapAllToAnonymous:        config.MapAllToAnonymous,
		MapPrivilegedToAnonymous: config.MapPrivilegedToAnonymous,
		AnonymousUID:             config.AnonymousUID,
		AnonymousGID:             config.AnonymousGID,
	}

	return nil
}

// RemoveShare removes a share from the registry.
// Returns an error if the share doesn't exist.
// Note: This does NOT close the underlying stores, as they may be used by other shares.
func (r *Registry) RemoveShare(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.shares[name]; !exists {
		return fmt.Errorf("share %q not found", name)
	}

	delete(r.shares, name)
	return nil
}

// GetShare retrieves a share by name.
// Returns nil, error if the share doesn't exist.
func (r *Registry) GetShare(name string) (*Share, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return share, nil
}

// GetRootHandle retrieves the root file handle for a share by name.
// This is used by mount handlers to get the root file handle for a mounted share.
// Returns an error if the share doesn't exist.
func (r *Registry) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return share.RootHandle, nil
}

// GetMetadataStore retrieves a metadata store by name.
// Returns nil, error if not found.
func (r *Registry) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	store, exists := r.metadata[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", name)
	}
	return store, nil
}

// GetContentStore retrieves a content store by name.
// Returns nil, error if not found.
func (r *Registry) GetContentStore(name string) (content.ContentStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	store, exists := r.content[name]
	if !exists {
		return nil, fmt.Errorf("content store %q not found", name)
	}
	return store, nil
}

// GetMetadataStoreForShare retrieves the metadata store used by the specified share.
// Returns nil, error if the share or store doesn't exist.
func (r *Registry) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	store, exists := r.metadata[share.MetadataStore]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found for share %q", share.MetadataStore, shareName)
	}

	return store, nil
}

// GetContentStoreForShare retrieves the content store used by the specified share.
// Returns nil, error if the share or store doesn't exist.
func (r *Registry) GetContentStoreForShare(shareName string) (content.ContentStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	store, exists := r.content[share.ContentStore]
	if !exists {
		return nil, fmt.Errorf("content store %q not found for share %q", share.ContentStore, shareName)
	}

	return store, nil
}

// ListShares returns all registered share names.
// The returned slice is a copy and safe to modify.
func (r *Registry) ListShares() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.shares))
	for name := range r.shares {
		names = append(names, name)
	}
	return names
}

// ListMetadataStores returns all registered metadata store names.
// The returned slice is a copy and safe to modify.
func (r *Registry) ListMetadataStores() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.metadata))
	for name := range r.metadata {
		names = append(names, name)
	}
	return names
}

// ListContentStores returns all registered content store names.
// The returned slice is a copy and safe to modify.
func (r *Registry) ListContentStores() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.content))
	for name := range r.content {
		names = append(names, name)
	}
	return names
}

// ListSharesUsingMetadataStore returns all shares that use the specified metadata store.
// The returned slice is a copy and safe to modify.
func (r *Registry) ListSharesUsingMetadataStore(storeName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var shares []string
	for _, share := range r.shares {
		if share.MetadataStore == storeName {
			shares = append(shares, share.Name)
		}
	}
	return shares
}

// ListSharesUsingContentStore returns all shares that use the specified content store.
// The returned slice is a copy and safe to modify.
func (r *Registry) ListSharesUsingContentStore(storeName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var shares []string
	for _, share := range r.shares {
		if share.ContentStore == storeName {
			shares = append(shares, share.Name)
		}
	}
	return shares
}

// CountShares returns the number of registered shares.
func (r *Registry) CountShares() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.shares)
}

// CountMetadataStores returns the number of registered metadata stores.
func (r *Registry) CountMetadataStores() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.metadata)
}

// CountContentStores returns the number of registered content stores.
func (r *Registry) CountContentStores() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.content)
}

// ShareExists checks if a share with the given name exists in the registry.
// This is useful for validating share names decoded from file handles.
func (r *Registry) ShareExists(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.shares[name]
	return exists
}

// ============================================================================
// Mount Tracking
// ============================================================================

// RecordMount registers that a client has mounted a share.
// The clientAddr should be the client's IP address or IP:port.
func (r *Registry) RecordMount(clientAddr, shareName string, mountTime int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mounts[clientAddr] = &MountInfo{
		ClientAddr: clientAddr,
		ShareName:  shareName,
		MountTime:  mountTime,
	}
}

// RemoveMount removes a mount record for the given client.
// Returns true if a mount was removed, false if no mount existed.
func (r *Registry) RemoveMount(clientAddr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.mounts[clientAddr]; exists {
		delete(r.mounts, clientAddr)
		return true
	}
	return false
}

// RemoveAllMounts removes all mount records. Used for UMNTALL.
func (r *Registry) RemoveAllMounts() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := len(r.mounts)
	r.mounts = make(map[string]*MountInfo)
	return count
}

// ListMounts returns all active mount records.
// The returned slice is a copy and safe to modify.
func (r *Registry) ListMounts() []*MountInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	mounts := make([]*MountInfo, 0, len(r.mounts))
	for _, mount := range r.mounts {
		mounts = append(mounts, &MountInfo{
			ClientAddr: mount.ClientAddr,
			ShareName:  mount.ShareName,
			MountTime:  mount.MountTime,
		})
	}
	return mounts
}
