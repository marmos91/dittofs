package shares

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Share represents the runtime state of a configured share.
// This combines persisted configuration with live runtime state.
type Share struct {
	Name          string
	MetadataStore string              // Name of the metadata store
	RootHandle    metadata.FileHandle // Encoded file handle for the root directory
	ReadOnly      bool

	// User-based Access Control
	// DefaultPermission is the permission for users without explicit permission or unknown UIDs.
	// Values: "none" (block access), "read", "read-write", "admin"
	// Default is "read-write" for NFS compatibility.
	DefaultPermission string

	// Identity Mapping (Squashing) - matches Synology NFS options
	Squash       models.SquashMode
	AnonymousUID uint32 // UID for anonymous mapping (default: 65534)
	AnonymousGID uint32 // GID for anonymous mapping (default: 65534)

	// NFS-specific options
	DisableReaddirplus bool // Prevent READDIRPLUS on this share

	// Security Policy
	AllowAuthSys      bool     // Allow AUTH_SYS connections (default: true)
	RequireKerberos   bool     // Require Kerberos authentication (default: false)
	MinKerberosLevel  string   // Minimum Kerberos level: krb5, krb5i, krb5p (default: krb5)
	NetgroupName      string   // Netgroup name for IP-based access control (empty = allow all)
	BlockedOperations []string // Operations blocked on this share
}

// ShareConfig contains all configuration needed to create a share in the runtime.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ReadOnly      bool

	// User-based Access Control
	DefaultPermission string

	// Identity Mapping
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	// Root directory attributes
	RootAttr *metadata.FileAttr

	// NFS-specific options
	DisableReaddirplus bool

	// Security Policy
	AllowAuthSys      bool
	AllowAuthSysSet   bool // true when AllowAuthSys was explicitly set (distinguishes false from unset)
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string
}

// LegacyMountInfo represents a legacy NFS mount record.
// Deprecated: Use MountTracker and MountInfo from mounts package instead.
// Kept for backward compatibility with existing callers during migration.
type LegacyMountInfo struct {
	ClientAddr string // Client IP address
	ShareName  string // Name of the mounted share
	MountTime  int64  // Unix timestamp when mounted
}

// MetadataStoreProvider allows the shares service to look up metadata stores
// without importing the stores sub-package (avoids circular dependency).
type MetadataStoreProvider interface {
	GetMetadataStore(name string) (metadata.MetadataStore, error)
}

// MetadataServiceRegistrar allows the shares service to register stores
// with the metadata service without importing the metadata service directly.
type MetadataServiceRegistrar interface {
	RegisterStoreForShare(shareName string, store metadata.MetadataStore) error
}

// PayloadServiceEnsurer allows the shares service to trigger lazy
// initialization of the payload service.
type PayloadServiceEnsurer interface {
	EnsurePayloadService(ctx context.Context) error
	HasPayloadService() bool
	HasStore() bool
}

// Service manages share registration, lookup, and configuration.
type Service struct {
	mu       sync.RWMutex
	registry map[string]*Share

	// Share change callbacks for dynamic updates (e.g., pseudo-fs rebuild)
	changeCallbacks []func(shares []string)
}

// New creates a new share management service.
func New() *Service {
	return &Service{
		registry: make(map[string]*Share),
	}
}

// AddShare creates and registers a new share with the given configuration.
func (s *Service) AddShare(
	ctx context.Context,
	config *ShareConfig,
	storeProvider MetadataStoreProvider,
	metadataSvc MetadataServiceRegistrar,
	payloadEnsurer PayloadServiceEnsurer,
) error {
	if config.Name == "" {
		return fmt.Errorf("cannot add share with empty name")
	}

	// Ensure PayloadService is initialized before creating shares
	if payloadEnsurer != nil && !payloadEnsurer.HasPayloadService() && payloadEnsurer.HasStore() {
		if err := payloadEnsurer.EnsurePayloadService(ctx); err != nil {
			return fmt.Errorf("failed to initialize payload service: %w", err)
		}
	}

	s.mu.Lock()

	// Validate that metadata service registrar exists
	if metadataSvc == nil {
		s.mu.Unlock()
		return fmt.Errorf("metadata service not initialized")
	}

	// Check if share already exists
	if _, exists := s.registry[config.Name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q already exists", config.Name)
	}

	// Validate that metadata store exists
	metadataStore, err := storeProvider.GetMetadataStore(config.MetadataStore)
	if err != nil {
		s.mu.Unlock()
		return err
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
		rootAttr.Mode = 0777
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
		s.mu.Unlock()
		return fmt.Errorf("failed to create root directory: %w", err)
	}

	// Encode the root file handle
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to encode root handle: %w", err)
	}

	// Apply security policy defaults.
	allowAuthSys := config.AllowAuthSys
	if !config.AllowAuthSysSet && !allowAuthSys {
		allowAuthSys = true
	}

	// Create share struct
	share := &Share{
		Name:               config.Name,
		MetadataStore:      config.MetadataStore,
		RootHandle:         rootHandle,
		ReadOnly:           config.ReadOnly,
		DefaultPermission:  config.DefaultPermission,
		Squash:             config.Squash,
		AnonymousUID:       config.AnonymousUID,
		AnonymousGID:       config.AnonymousGID,
		DisableReaddirplus: config.DisableReaddirplus,
		AllowAuthSys:       allowAuthSys,
		RequireKerberos:    config.RequireKerberos,
		MinKerberosLevel:   config.MinKerberosLevel,
		NetgroupName:       config.NetgroupName,
		BlockedOperations:  config.BlockedOperations,
	}

	s.registry[config.Name] = share

	// Register the metadata store with the MetadataService for this share
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		delete(s.registry, config.Name)
		s.mu.Unlock()
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	s.mu.Unlock()

	// Notify after releasing lock (callbacks may call ListShares)
	s.notifyShareChange()

	return nil
}

// RemoveShare removes a share from the registry.
// Note: This does NOT close the underlying metadata store.
func (s *Service) RemoveShare(name string) error {
	s.mu.Lock()

	_, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q not found", name)
	}

	delete(s.registry, name)
	s.mu.Unlock()

	// Notify after releasing lock (callbacks may call ListShares)
	s.notifyShareChange()

	return nil
}

// UpdateShare updates a share's configuration.
// Only updates fields that can be changed without reloading (ReadOnly, DefaultPermission).
func (s *Service) UpdateShare(name string, readOnly *bool, defaultPermission *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share, exists := s.registry[name]
	if !exists {
		return fmt.Errorf("share %q not found", name)
	}

	if readOnly != nil {
		share.ReadOnly = *readOnly
	}
	if defaultPermission != nil {
		share.DefaultPermission = *defaultPermission
	}

	return nil
}

// GetShare retrieves a share by name.
func (s *Service) GetShare(name string) (*Share, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return share, nil
}

// GetRootHandle retrieves the root file handle for a share.
func (s *Service) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return share.RootHandle, nil
}

// ListShares returns all registered share names.
func (s *Service) ListShares() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.registry))
	for name := range s.registry {
		names = append(names, name)
	}
	return names
}

// ShareExists checks if a share exists.
func (s *Service) ShareExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.registry[name]
	return exists
}

// OnShareChange registers a callback to be invoked when shares are added,
// removed, or updated. The callback receives the current list of share names.
func (s *Service) OnShareChange(callback func(shares []string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changeCallbacks = append(s.changeCallbacks, callback)
}

// notifyShareChange invokes all registered share change callbacks.
// Must NOT be called while holding s.mu to avoid deadlock.
func (s *Service) notifyShareChange() {
	s.mu.RLock()
	callbacks := s.changeCallbacks
	shareNames := make([]string, 0, len(s.registry))
	for name := range s.registry {
		shareNames = append(shareNames, name)
	}
	s.mu.RUnlock()

	for _, cb := range callbacks {
		cb(shareNames)
	}
}

// GetShareNameForHandle extracts the share name from a file handle.
func (s *Service) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", fmt.Errorf("failed to decode share handle: %w", err)
	}

	// Verify share exists
	s.mu.RLock()
	_, exists := s.registry[shareName]
	s.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("share %q not found in runtime", shareName)
	}

	return shareName, nil
}

// CountShares returns the number of registered shares.
func (s *Service) CountShares() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}
