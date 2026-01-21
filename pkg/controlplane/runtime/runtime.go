package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// Runtime manages all runtime state for shares and protocol adapters.
// It provides thread-safe registration and lookup of all server resources.
//
// The Runtime combines:
//   - Configuration from the persistent Store
//   - Live metadata store instances
//   - Share runtime state (root handles)
//   - Active mounts from NFS clients (ephemeral)
//
// Content Model:
// All content operations go through the PayloadService which includes
// caching and transfer management.
type Runtime struct {
	mu              sync.RWMutex
	metadata        map[string]metadata.MetadataStore // Named metadata store instances
	shares          map[string]*Share                 // Share runtime state
	mounts          map[string]*MountInfo             // Active NFS mounts (key: clientAddr)
	store           store.Store                       // Persistent configuration store
	metadataService *metadata.MetadataService         // High-level metadata operations
	payloadService  *payload.PayloadService           // High-level content operations
}

// New creates a new Runtime with the given persistent store.
func New(s store.Store) *Runtime {
	return &Runtime{
		metadata:        make(map[string]metadata.MetadataStore),
		shares:          make(map[string]*Share),
		mounts:          make(map[string]*MountInfo),
		store:           s,
		metadataService: metadata.New(),
	}
}

// SetPayloadService sets the PayloadService for content operations.
// This should be called during initialization after creating the cache
// and transfer manager.
func (r *Runtime) SetPayloadService(ps *payload.PayloadService) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloadService = ps
}

// Store returns the persistent configuration store.
func (r *Runtime) Store() store.Store {
	return r.store
}

// ============================================================================
// Metadata Store Management
// ============================================================================

// RegisterMetadataStore adds a named metadata store instance to the runtime.
// Returns an error if a store with the same name already exists.
func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	if metaStore == nil {
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

	r.metadata[name] = metaStore
	return nil
}

// GetMetadataStore retrieves a metadata store instance by name.
func (r *Runtime) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	metaStore, exists := r.metadata[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", name)
	}
	return metaStore, nil
}

// GetMetadataStoreForShare retrieves the metadata store used by the specified share.
func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	metaStore, exists := r.metadata[share.MetadataStore]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found for share %q", share.MetadataStore, shareName)
	}

	return metaStore, nil
}

// ListMetadataStores returns all registered metadata store names.
func (r *Runtime) ListMetadataStores() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.metadata))
	for name := range r.metadata {
		names = append(names, name)
	}
	return names
}

// ============================================================================
// Share Management
// ============================================================================

// AddShare creates and registers a new share with the given configuration.
// This method:
//  1. Validates that the share doesn't already exist
//  2. Validates that the referenced metadata store exists
//  3. Creates the root directory in the metadata store
//  4. Registers the share in the runtime
func (r *Runtime) AddShare(ctx context.Context, config *ShareConfig) error {
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
	rootAttr := config.RootAttr
	if rootAttr == nil {
		rootAttr = &metadata.FileAttr{}
	}
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

	return nil
}

// RemoveShare removes a share from the runtime.
// Note: This does NOT close the underlying metadata store.
func (r *Runtime) RemoveShare(name string) error {
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
func (r *Runtime) GetShare(name string) (*Share, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return share, nil
}

// GetRootHandle retrieves the root file handle for a share.
func (r *Runtime) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	share, exists := r.shares[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return share.RootHandle, nil
}

// ListShares returns all registered share names.
func (r *Runtime) ListShares() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.shares))
	for name := range r.shares {
		names = append(names, name)
	}
	return names
}

// ShareExists checks if a share exists in the runtime.
func (r *Runtime) ShareExists(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.shares[name]
	return exists
}

// ============================================================================
// Service Access
// ============================================================================

// GetMetadataService returns the MetadataService for high-level operations.
func (r *Runtime) GetMetadataService() *metadata.MetadataService {
	return r.metadataService
}

// GetPayloadService returns the PayloadService for high-level content operations.
func (r *Runtime) GetPayloadService() *payload.PayloadService {
	return r.payloadService
}

// ============================================================================
// Mount Tracking (Ephemeral)
// ============================================================================

// RecordMount registers that a client has mounted a share.
func (r *Runtime) RecordMount(clientAddr, shareName string, mountTime int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mounts[clientAddr] = &MountInfo{
		ClientAddr: clientAddr,
		ShareName:  shareName,
		MountTime:  mountTime,
	}
}

// RemoveMount removes a mount record for the given client.
func (r *Runtime) RemoveMount(clientAddr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.mounts[clientAddr]; exists {
		delete(r.mounts, clientAddr)
		return true
	}
	return false
}

// RemoveAllMounts removes all mount records.
func (r *Runtime) RemoveAllMounts() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := len(r.mounts)
	r.mounts = make(map[string]*MountInfo)
	return count
}

// ListMounts returns all active mount records.
func (r *Runtime) ListMounts() []*MountInfo {
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

// ============================================================================
// Identity Mapping
// ============================================================================

// ApplyIdentityMapping applies share-level identity mapping rules.
//
// This implements:
//   - anonymous access: Maps nil UID/GID (AUTH_NULL) to anonymous credentials
//   - all_squash: Maps all users to anonymous
//   - root_squash: Maps root (UID 0) to anonymous
func (r *Runtime) ApplyIdentityMapping(shareName string, identity *metadata.Identity) (*metadata.Identity, error) {
	r.mu.RLock()
	share, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	// Create effective identity (copy of original)
	effective := &metadata.Identity{
		UID:      identity.UID,
		GID:      identity.GID,
		GIDs:     identity.GIDs,
		Username: identity.Username,
	}

	// Handle anonymous access (AUTH_NULL - nil UID/GID)
	if identity.UID == nil {
		anonUID := share.AnonymousUID
		anonGID := share.AnonymousGID
		effective.UID = &anonUID
		effective.GID = &anonGID
		effective.GIDs = []uint32{anonGID}
		effective.Username = fmt.Sprintf("anonymous(%d)", anonUID)
		return effective, nil
	}

	// Apply all_squash (map all users to anonymous)
	if share.MapAllToAnonymous {
		anonUID := share.AnonymousUID
		anonGID := share.AnonymousGID
		effective.UID = &anonUID
		effective.GID = &anonGID
		effective.GIDs = []uint32{anonGID}
		effective.Username = fmt.Sprintf("anonymous(%d)", anonUID)
		return effective, nil
	}

	// Apply root_squash (map root to anonymous)
	if share.MapPrivilegedToAnonymous && identity.UID != nil && *identity.UID == 0 {
		anonUID := share.AnonymousUID
		anonGID := share.AnonymousGID
		effective.UID = &anonUID
		effective.GID = &anonGID
		effective.GIDs = []uint32{anonGID}
		effective.Username = fmt.Sprintf("anonymous(%d)", anonUID)
		return effective, nil
	}

	// No mapping applied, return original identity
	return effective, nil
}

// GetShareNameForHandle extracts the share name from a file handle.
func (r *Runtime) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", fmt.Errorf("failed to decode share handle: %w", err)
	}

	// Verify share exists in runtime
	r.mu.RLock()
	_, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("share %q not found in runtime", shareName)
	}

	return shareName, nil
}

// ============================================================================
// Store Interfaces (for protocol handlers)
// ============================================================================

// GetUserStore returns the store as a UserStore for SMB handlers.
func (r *Runtime) GetUserStore() models.UserStore {
	return r.store
}

// GetIdentityStore returns the store as an IdentityStore for NFS handlers.
func (r *Runtime) GetIdentityStore() models.IdentityStore {
	return r.store
}

// SetIdentityStore is a no-op for backwards compatibility.
// Runtime uses the Store directly for identity operations.
// Deprecated: Use runtime.Store() instead.
func (r *Runtime) SetIdentityStore(_ models.IdentityStore) {
	// No-op: Runtime uses r.store directly
}

// SetUserStore is a no-op for backwards compatibility.
// Runtime uses the Store directly for user operations.
// Deprecated: Use runtime.Store() instead.
func (r *Runtime) SetUserStore(_ models.UserStore) {
	// No-op: Runtime uses r.store directly
}

// GetBlockService returns the PayloadService for backwards compatibility.
// Deprecated: Use GetPayloadService instead.
func (r *Runtime) GetBlockService() *payload.PayloadService {
	return r.payloadService
}

// CountMetadataStores returns the number of registered metadata stores.
func (r *Runtime) CountMetadataStores() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.metadata)
}

// CountShares returns the number of registered shares.
func (r *Runtime) CountShares() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.shares)
}

// ListSharesUsingMetadataStore returns all shares that use the specified metadata store.
func (r *Runtime) ListSharesUsingMetadataStore(storeName string) []string {
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
