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
type Share struct {
	Name          string
	MetadataStore string
	RootHandle    metadata.FileHandle
	ReadOnly      bool

	// DefaultPermission for users without explicit permission: "none", "read", "read-write", "admin".
	DefaultPermission string

	// Identity mapping (Synology-style squash modes)
	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	// NFS-specific options
	DisableReaddirplus bool

	// Security policy
	AllowAuthSys      bool
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string
}

// ShareConfig contains all configuration needed to create a share.
type ShareConfig struct {
	Name          string
	MetadataStore string
	ReadOnly      bool

	DefaultPermission string

	Squash       models.SquashMode
	AnonymousUID uint32
	AnonymousGID uint32

	RootAttr *metadata.FileAttr

	DisableReaddirplus bool

	AllowAuthSys      bool
	AllowAuthSysSet   bool // true when AllowAuthSys was explicitly set (distinguishes false from unset)
	RequireKerberos   bool
	MinKerberosLevel  string
	NetgroupName      string
	BlockedOperations []string
}

// LegacyMountInfo is the legacy NFS mount record format.
type LegacyMountInfo struct {
	ClientAddr string
	ShareName  string
	MountTime  int64
}

// MetadataStoreProvider looks up metadata stores by name.
type MetadataStoreProvider interface {
	GetMetadataStore(name string) (metadata.MetadataStore, error)
}

// MetadataServiceRegistrar registers metadata stores for shares.
type MetadataServiceRegistrar interface {
	RegisterStoreForShare(shareName string, store metadata.MetadataStore) error
}

// PayloadServiceEnsurer triggers lazy payload service initialization.
type PayloadServiceEnsurer interface {
	EnsurePayloadService(ctx context.Context) error
	HasPayloadService() bool
	HasStore() bool
}

// Service manages share registration, lookup, and configuration.
type Service struct {
	mu              sync.RWMutex
	registry        map[string]*Share
	changeCallbacks []func(shares []string)
}

func New() *Service {
	return &Service{
		registry: make(map[string]*Share),
	}
}

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

	if payloadEnsurer != nil && !payloadEnsurer.HasPayloadService() && payloadEnsurer.HasStore() {
		if err := payloadEnsurer.EnsurePayloadService(ctx); err != nil {
			return fmt.Errorf("failed to initialize payload service: %w", err)
		}
	}

	s.mu.Lock()

	if metadataSvc == nil {
		s.mu.Unlock()
		return fmt.Errorf("metadata service not initialized")
	}

	if _, exists := s.registry[config.Name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q already exists", config.Name)
	}

	metadataStore, err := storeProvider.GetMetadataStore(config.MetadataStore)
	if err != nil {
		s.mu.Unlock()
		return err
	}

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

	rootFile, err := metadataStore.CreateRootDirectory(ctx, config.Name, rootAttr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to create root directory: %w", err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to encode root handle: %w", err)
	}

	allowAuthSys := config.AllowAuthSys
	if !config.AllowAuthSysSet && !allowAuthSys {
		allowAuthSys = true
	}

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
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		delete(s.registry, config.Name)
		s.mu.Unlock()
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	s.mu.Unlock()
	s.notifyShareChange()

	return nil
}

// RemoveShare removes a share from the registry (does not close the underlying metadata store).
func (s *Service) RemoveShare(name string) error {
	s.mu.Lock()
	_, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q not found", name)
	}
	delete(s.registry, name)
	s.mu.Unlock()

	s.notifyShareChange()

	return nil
}

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

func (s *Service) GetShare(name string) (*Share, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return share, nil
}

func (s *Service) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return share.RootHandle, nil
}

func (s *Service) ListShares() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.registry))
	for name := range s.registry {
		names = append(names, name)
	}
	return names
}

func (s *Service) ShareExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.registry[name]
	return exists
}

func (s *Service) OnShareChange(callback func(shares []string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changeCallbacks = append(s.changeCallbacks, callback)
}

// notifyShareChange must NOT be called while holding s.mu.
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

func (s *Service) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", fmt.Errorf("failed to decode share handle: %w", err)
	}

	s.mu.RLock()
	_, exists := s.registry[shareName]
	s.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("share %q not found in runtime", shareName)
	}

	return shareName, nil
}

func (s *Service) CountShares() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}
