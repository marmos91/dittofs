//go:build e2e

package framework

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/server"
)

// TestContext provides a complete testing environment with:
// - Running DittoFS server
// - NFS mount (always available)
// - SMB mount (when EnableSMB is true, default: true)
// - Cleanup mechanisms
//
// Note: Content storage is handled by the Registry's auto-created SliceCache.
// The cache implements the Chunk/Slice/Block model for efficient writes.
type TestContext struct {
	T             *testing.T
	Config        *TestConfig
	Server        *server.DittoServer
	Registry      *registry.Registry
	MetadataStore metadata.MetadataStore

	// Protocol mounts
	NFS *Mount // Always available
	SMB *Mount // Available when EnableSMB=true (default: true)

	// Ports
	NFSPort int
	SMBPort int

	// SMB credentials
	SMBUsername string
	SMBPassword string

	// Internal
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	tempDirs []string
	options  TestContextOptions
}

// TestContextOptions configures the test context.
type TestContextOptions struct {
	EnableNFS  bool // default: true
	EnableSMB  bool // default: true
	ShareCount int  // default: 1 (for multi-share tests)
}

// DefaultOptions returns the default test context options.
func DefaultOptions() TestContextOptions {
	return TestContextOptions{
		EnableNFS:  true,
		EnableSMB:  true,
		ShareCount: 1,
	}
}

// NewTestContext creates a new test environment with default options (NFS + SMB).
func NewTestContext(t *testing.T, config *TestConfig) *TestContext {
	return NewTestContextWithOptions(t, config, DefaultOptions())
}

// NewNFSOnlyContext creates a test context with only NFS enabled.
func NewNFSOnlyContext(t *testing.T, config *TestConfig) *TestContext {
	return NewTestContextWithOptions(t, config, TestContextOptions{
		EnableNFS: true,
		EnableSMB: false,
	})
}

// NewTestContextWithOptions creates a new test environment with custom options.
func NewTestContextWithOptions(t *testing.T, config *TestConfig, opts TestContextOptions) *TestContext {
	t.Helper()

	// Setup external dependencies
	setupDependencies(t, config)

	ctx, cancel := context.WithCancel(context.Background())

	tc := &TestContext{
		T:           t,
		Config:      config,
		ctx:         ctx,
		cancel:      cancel,
		options:     opts,
		SMBUsername: DefaultSMBCredentials().Username,
		SMBPassword: DefaultSMBCredentials().Password,
	}

	// Allocate ports
	if opts.EnableNFS {
		tc.NFSPort = FindFreePort(t)
	}
	if opts.EnableSMB {
		tc.SMBPort = FindFreePort(t)
	}

	// Setup stores
	tc.setupStores()

	// Start server
	tc.startServer()

	// Mount filesystems
	tc.mountFilesystems()

	return tc
}

// setupDependencies sets up PostgreSQL and S3 if needed.
func setupDependencies(t *testing.T, config *TestConfig) {
	t.Helper()

	// PostgreSQL setup
	if config.RequiresPostgres() {
		if !CheckPostgresAvailable(t) {
			t.Skip("PostgreSQL not available, skipping PostgreSQL test")
		}
		helper := NewPostgresHelper(t)
		SetupPostgresConfig(t, config, helper)
	}

	// S3 setup - currently not used in cache-only model
	// Will be needed when block storage is implemented
	if config.RequiresS3() {
		if !CheckLocalstackAvailable(t) {
			t.Skip("Localstack not available, skipping S3 test")
		}
		helper := NewLocalstackHelper(t)
		SetupS3Config(t, config, helper)
	}
}

// setupStores initializes metadata store.
// Note: Content storage is handled by the Registry's auto-created SliceCache.
func (tc *TestContext) setupStores() {
	tc.T.Helper()

	var err error

	tc.MetadataStore, err = tc.Config.CreateMetadataStore(tc.ctx, tc)
	if err != nil {
		tc.T.Fatalf("Failed to create metadata store: %v", err)
	}
}

// startServer starts the DittoFS server with configured adapters.
func (tc *TestContext) startServer() {
	tc.T.Helper()

	// Always use ERROR level for server logs to keep test output clean.
	// Use DITTOFS_LOGGING_LEVEL env var to debug specific tests (e.g., DEBUG, INFO).
	if level := os.Getenv("DITTOFS_LOGGING_LEVEL"); level != "" {
		logger.SetLevel(level)
	} else {
		logger.SetLevel("ERROR")
	}

	// Create Registry (auto-creates global SliceCache for content storage)
	tc.Registry = registry.NewRegistry(nil)

	// Setup user store for SMB if enabled
	if tc.options.EnableSMB {
		userStore := tc.createUserStore()
		tc.Registry.SetUserStore(userStore)
	}

	// Register metadata store
	port := tc.NFSPort
	if port == 0 {
		port = tc.SMBPort
	}

	storeName := fmt.Sprintf("test-metadata-%d", port)
	if err := tc.Registry.RegisterMetadataStore(storeName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	// Create share with appropriate permissions
	// Note: No content store or cache registration needed - Registry handles it
	mode := uint32(0755)
	if tc.options.EnableSMB {
		mode = 0777 // Allow SMB test user to write
	}

	shareConfig := &registry.ShareConfig{
		Name:              "/export",
		MetadataStore:     storeName,
		ReadOnly:          false,
		AllowGuest:        tc.options.EnableSMB, // Allow guest for interop tests
		DefaultPermission: string(identity.PermissionReadWrite),
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: mode,
			UID:  0,
			GID:  0,
		},
	}

	if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share: %v", err)
	}

	// Create DittoServer
	tc.Server = server.New(tc.Registry, 30*time.Second)

	// Add NFS adapter if enabled
	if tc.options.EnableNFS {
		nfsConfig := nfs.NFSConfig{
			Enabled:        true,
			Port:           tc.NFSPort,
			MaxConnections: 0,
			Timeouts: nfs.NFSTimeoutsConfig{
				Read:     5 * time.Minute,
				Write:    30 * time.Second,
				Idle:     5 * time.Minute,
				Shutdown: 30 * time.Second,
			},
		}
		nfsAdapter := nfs.New(nfsConfig, nil)
		if err := tc.Server.AddAdapter(nfsAdapter); err != nil {
			tc.T.Fatalf("Failed to add NFS adapter: %v", err)
		}
	}

	// Add SMB adapter if enabled
	if tc.options.EnableSMB {
		smbConfig := smb.SMBConfig{
			Enabled:        true,
			Port:           tc.SMBPort,
			MaxConnections: 0,
			Timeouts: smb.SMBTimeoutsConfig{
				Read:     5 * time.Minute,
				Write:    30 * time.Second,
				Idle:     5 * time.Minute,
				Shutdown: 30 * time.Second,
			},
		}
		smbAdapter := smb.New(smbConfig)
		if err := tc.Server.AddAdapter(smbAdapter); err != nil {
			tc.T.Fatalf("Failed to add SMB adapter: %v", err)
		}
	}

	// Start server in background
	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()
		if err := tc.Server.Serve(tc.ctx); err != nil && err != context.Canceled {
			tc.T.Logf("Server error: %v", err)
		}
	}()

	// Wait for servers to be ready
	if tc.options.EnableNFS {
		WaitForServer(tc.T, tc.NFSPort, 10*time.Second)
	}
	if tc.options.EnableSMB {
		WaitForServer(tc.T, tc.SMBPort, 10*time.Second)
	}
}

// createUserStore creates a user store with test credentials.
func (tc *TestContext) createUserStore() identity.UserStore {
	hash, err := identity.HashPassword(tc.SMBPassword)
	if err != nil {
		tc.T.Fatalf("Failed to hash password: %v", err)
	}

	testUser := &identity.User{
		Username:     tc.SMBUsername,
		PasswordHash: hash,
		UID:          1000,
		GID:          1000,
		Enabled:      true,
		DisplayName:  "Test User",
		SharePermissions: map[string]identity.SharePermission{
			"/export": identity.PermissionReadWrite,
		},
	}

	store, err := identity.NewConfigUserStore([]*identity.User{testUser}, nil, nil)
	if err != nil {
		tc.T.Fatalf("Failed to create user store: %v", err)
	}

	return store
}

// mountFilesystems mounts NFS and/or SMB filesystems.
func (tc *TestContext) mountFilesystems() {
	tc.T.Helper()

	if tc.options.EnableNFS {
		tc.NFS = MountNFS(tc.T, tc.NFSPort)
	}

	if tc.options.EnableSMB {
		tc.SMB = MountSMB(tc.T, tc.SMBPort, SMBCredentials{
			Username: tc.SMBUsername,
			Password: tc.SMBPassword,
		})
	}
}

// Cleanup unmounts filesystems, stops the server, and cleans up resources.
func (tc *TestContext) Cleanup() {
	tc.T.Helper()

	// Unmount filesystems
	if tc.NFS != nil {
		tc.NFS.Cleanup()
	}
	if tc.SMB != nil {
		tc.SMB.Cleanup()
	}

	// Stop server
	if tc.cancel != nil {
		tc.cancel()
	}

	// Wait for server to stop
	tc.wg.Wait()

	// Close metadata store
	if tc.MetadataStore != nil {
		if closer, ok := tc.MetadataStore.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}

	// Remove temporary directories
	for _, dir := range tc.tempDirs {
		_ = os.RemoveAll(dir)
	}
}

// NFSPath returns the absolute path for a relative path within the NFS mount.
func (tc *TestContext) NFSPath(relativePath string) string {
	if tc.NFS == nil {
		tc.T.Fatal("NFS not enabled")
	}
	return tc.NFS.FilePath(relativePath)
}

// SMBPath returns the absolute path for a relative path within the SMB mount.
func (tc *TestContext) SMBPath(relativePath string) string {
	if tc.SMB == nil {
		tc.T.Fatal("SMB not enabled")
	}
	return tc.SMB.FilePath(relativePath)
}

// Path returns the absolute path for a relative path.
// Uses NFS if available, otherwise SMB.
func (tc *TestContext) Path(relativePath string) string {
	if tc.NFS != nil {
		return tc.NFS.FilePath(relativePath)
	}
	if tc.SMB != nil {
		return tc.SMB.FilePath(relativePath)
	}
	tc.T.Fatal("No filesystem mounted")
	return ""
}

// CreateTempDir creates a temporary directory and registers it for cleanup.
func (tc *TestContext) CreateTempDir(prefix string) string {
	tc.T.Helper()

	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		tc.T.Fatalf("Failed to create temp directory: %v", err)
	}
	tc.tempDirs = append(tc.tempDirs, dir)
	return dir
}

// GetConfig returns the test configuration.
func (tc *TestContext) GetConfig() *TestConfig {
	return tc.Config
}

// GetPort returns the primary server port.
func (tc *TestContext) GetPort() int {
	if tc.NFSPort != 0 {
		return tc.NFSPort
	}
	return tc.SMBPort
}

// HasNFS returns true if NFS is enabled.
func (tc *TestContext) HasNFS() bool {
	return tc.NFS != nil
}

// HasSMB returns true if SMB is enabled.
func (tc *TestContext) HasSMB() bool {
	return tc.SMB != nil
}

// CleanupAllContexts is a no-op kept for compatibility with TestMain.
// Each test now cleans up its own context via defer tc.Cleanup().
func CleanupAllContexts() {
	// No-op: contexts are now cleaned up individually after each test
}
