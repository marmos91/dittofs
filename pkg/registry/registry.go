package registry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// Registry manages all named resources: metadata stores and shares.
// It provides thread-safe registration and lookup of all server resources.
//
// The Registry also tracks active mounts (NFS clients that have mounted shares).
// Mount information is ephemeral and kept in-memory only.
//
// Content Model:
// All content operations go through a mandatory Cache that is automatically
// created for each share. The Cache implements the Chunk/Slice/Block model
// for efficient random writes without read-modify-write overhead.
//
// Example usage:
//
//	reg := NewRegistry()
//	reg.SetPayloadService(payloadService)  // Created with cache + transfer manager
//	reg.RegisterMetadataStore("badger-main", badgerStore)
//	reg.AddShare(ctx, &ShareConfig{
//	    Name: "/export",
//	    MetadataStore: "badger-main",
//	})
//
//	share, _ := reg.GetShare("/export")
//	metaStore, _ := reg.GetMetadataStoreForShare("/export")
type Registry struct {
	mu              sync.RWMutex
	metadata        map[string]metadata.MetadataStore
	shares          map[string]*Share
	mounts          map[string]*MountInfo     // key: clientAddr, value: mount info
	userStore       identity.UserStore        // User/group management for authentication
	identityStore   identity.IdentityStore    // Full identity store for ShareIdentityMapping lookup
	metadataService *metadata.MetadataService // High-level metadata operations
	blockService    *payload.PayloadService   // High-level content operations (uses Cache)
}

// MountInfo represents an active NFS mount from a client.
type MountInfo struct {
	ClientAddr string // Client IP address
	ShareName  string // Name of the mounted share
	MountTime  int64  // Unix timestamp when mounted
}

// NewRegistry creates an empty registry.
//
// The PayloadService for content operations must be set separately
// via SetPayloadService() after creating the cache and transfer manager.
func NewRegistry() *Registry {
	return &Registry{
		metadata:        make(map[string]metadata.MetadataStore),
		shares:          make(map[string]*Share),
		mounts:          make(map[string]*MountInfo),
		metadataService: metadata.New(),
	}
}

// SetPayloadService sets the PayloadService for content operations.
// This should be called during initialization after creating the cache
// and transfer manager.
func (r *Registry) SetPayloadService(ps *payload.PayloadService) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockService = ps
}

// RegisterMetadataStore adds a named metadata store to the registry.
// Returns an error if a store with the same name already exists.
// Note: The store will be associated with shares via AddShare.
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

// AddShare creates and registers a new share with the given configuration.
// This method:
//  1. Validates that the share doesn't already exist
//  2. Validates that the referenced metadata store exists
//  3. Creates the root directory in the metadata store
//  4. Auto-creates a Cache for content operations
//  5. Registers the share in the registry with full configuration
//
// Returns an error if:
// - A share with the same name already exists
// - The referenced metadata store doesn't exist
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

	// Validate that metadata store exists
	metadataStore, exists := r.metadata[config.MetadataStore]
	if !exists {
		return fmt.Errorf("metadata store %q not found", config.MetadataStore)
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

	// Create share struct
	share := &Share{
		Name:                     config.Name,
		MetadataStore:            config.MetadataStore,
		RootHandle:               rootHandle,
		ReadOnly:                 config.ReadOnly,
		DefaultPermission:        config.DefaultPermission,
		AllowedClients:           config.AllowedClients,
		DeniedClients:            config.DeniedClients,
		RequireAuth:              config.RequireAuth,
		AllowedAuthMethods:       config.AllowedAuthMethods,
		MapAllToAnonymous:        config.MapAllToAnonymous,
		MapPrivilegedToAnonymous: config.MapPrivilegedToAnonymous,
		AnonymousUID:             config.AnonymousUID,
		AnonymousGID:             config.AnonymousGID,
		PrefetchConfig:           config.PrefetchConfig,
		WriteGatheringConfig:     config.WriteGatheringConfig,
		DisableReaddirplus:       config.DisableReaddirplus,
	}

	r.shares[config.Name] = share

	// Register the metadata store with the MetadataService for this share
	if err := r.metadataService.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		delete(r.shares, config.Name)
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	// Content storage is handled by the PayloadService set via SetPayloadService()
	// PayloadID uniqueness ensures data isolation between shares

	return nil
}

// RemoveShare removes a share from the registry.
// Returns an error if the share doesn't exist.
// Note: This does NOT close the underlying metadata store, as it may be used by other shares.
func (r *Registry) RemoveShare(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, exists := r.shares[name]
	if !exists {
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

// GetMetadataService returns the MetadataService instance for high-level operations.
// MetadataService provides methods like Lookup, CreateFile, RemoveFile, etc.
// that handle business logic and automatically route to the correct store based on share.
func (r *Registry) GetMetadataService() *metadata.MetadataService {
	return r.metadataService
}

// GetBlockService returns the BlockService instance for high-level content operations.
// BlockService provides methods like ReadAt, WriteAt, Flush, etc. that use
// the Cache and automatically route operations based on share.
func (r *Registry) GetBlockService() *payload.PayloadService {
	return r.blockService
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

// SetUserStore sets the user store for authentication and authorization.
// This should be called during server initialization before handling requests.
func (r *Registry) SetUserStore(store identity.UserStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userStore = store
}

// GetUserStore returns the user store for authentication and authorization.
// Returns nil if no user store has been configured.
func (r *Registry) GetUserStore() identity.UserStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.userStore
}

// SetIdentityStore sets the identity store for ShareIdentityMapping lookup.
// This should be called during server initialization before handling requests.
// The identity store provides per-share UID/GID/SID mappings for authenticated users.
func (r *Registry) SetIdentityStore(store identity.IdentityStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identityStore = store
}

// GetIdentityStore returns the identity store for ShareIdentityMapping lookup.
// Returns nil if no identity store has been configured.
func (r *Registry) GetIdentityStore() identity.IdentityStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.identityStore
}
