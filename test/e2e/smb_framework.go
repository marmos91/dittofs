//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/server"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// SMBTestContext provides a complete SMB testing environment with:
// - Running DittoFS server with SMB adapter
// - Mounted SMB share
// - Test user credentials
// - Cleanup mechanisms
type SMBTestContext struct {
	T             *testing.T
	Config        *TestConfig
	Server        *server.DittoServer
	Registry      *registry.Registry
	MetadataStore metadata.MetadataStore
	ContentStore  content.ContentStore
	Cache         cache.Cache
	MountPath     string
	Port          int
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	tempDirs      []string
	mounted       bool

	// SMB-specific
	Username string
	Password string
}

// Default test credentials
const (
	SMBTestUsername = "testuser"
	SMBTestPassword = "testpass123"
)

// NewSMBTestContext creates a new SMB test environment with the specified configuration.
func NewSMBTestContext(t *testing.T, config *TestConfig) *SMBTestContext {
	t.Helper()

	// For PostgreSQL configurations, check availability and setup
	if config.MetadataStore == MetadataPostgres {
		if !CheckPostgresAvailable(t) {
			t.Skip("PostgreSQL not available, skipping PostgreSQL test")
		}
		helper := NewPostgresHelper(t)
		SetupPostgresConfig(t, config, helper)
	}

	// For S3 configurations, check Localstack and setup S3 client
	if config.ContentStore == ContentS3 {
		if !CheckLocalstackAvailable(t) {
			t.Skip("Localstack not available, skipping S3 test")
		}
		helper := NewLocalstackHelper(t)
		SetupS3Config(t, config, helper)
	}

	ctx, cancel := context.WithCancel(context.Background())

	tc := &SMBTestContext{
		T:        t,
		Config:   config,
		ctx:      ctx,
		cancel:   cancel,
		Port:     findFreeSMBPort(t),
		Username: SMBTestUsername,
		Password: SMBTestPassword,
	}

	// Setup stores based on configuration
	tc.setupStores()

	// Start DittoFS server with SMB adapter
	tc.startServer()

	// Mount SMB share
	tc.mountSMB()

	return tc
}

// setupStores initializes metadata and content stores based on the test configuration
func (tc *SMBTestContext) setupStores() {
	tc.T.Helper()

	var err error

	// Create metadata store
	tc.MetadataStore, err = tc.Config.CreateMetadataStore(tc.ctx, tc)
	if err != nil {
		tc.T.Fatalf("Failed to create metadata store: %v", err)
	}

	// Create content store
	tc.ContentStore, err = tc.Config.CreateContentStore(tc.ctx, tc)
	if err != nil {
		tc.T.Fatalf("Failed to create content store: %v", err)
	}

	// Create cache if enabled
	tc.Cache = tc.Config.CreateCache()
}

// startServer starts the DittoFS server with SMB adapter
func (tc *SMBTestContext) startServer() {
	tc.T.Helper()

	// Initialize logger - use ERROR level for clean test output
	logger.SetLevel("ERROR")

	// Create Registry
	tc.Registry = registry.NewRegistry()

	// Create and set user store with test user
	userStore := tc.createUserStore()
	tc.Registry.SetUserStore(userStore)

	// Register stores with unique names
	storeName := fmt.Sprintf("test-metadata-smb-%d", tc.Port)
	if err := tc.Registry.RegisterMetadataStore(storeName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	contentStoreName := fmt.Sprintf("test-content-smb-%d", tc.Port)
	if err := tc.Registry.RegisterContentStore(contentStoreName, tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	// Register cache if enabled
	cacheName := ""
	if tc.Cache != nil {
		cacheName = fmt.Sprintf("test-cache-smb-%d", tc.Port)
		if err := tc.Registry.RegisterCache(cacheName, tc.Cache); err != nil {
			tc.T.Fatalf("Failed to register cache: %v", err)
		}
	}

	// Create share with read-write permission for test user
	// Note: Root directory must be writable by the test user (UID 1000, GID 1000)
	// Using mode 0777 to allow all users to write
	shareConfig := &registry.ShareConfig{
		Name:              "/export",
		MetadataStore:     storeName,
		ContentStore:      contentStoreName,
		Cache:             cacheName,
		ReadOnly:          false,
		AllowGuest:        false,
		DefaultPermission: string(identity.PermissionReadWrite),
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0777,
			UID:  0,
			GID:  0,
		},
	}

	if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share: %v", err)
	}

	// Create SMB adapter
	smbConfig := smb.SMBConfig{
		Enabled:        true,
		Port:           tc.Port,
		MaxConnections: 0,
		Timeouts: smb.SMBTimeoutsConfig{
			Read:     5 * time.Minute,
			Write:    30 * time.Second,
			Idle:     5 * time.Minute,
			Shutdown: 30 * time.Second,
		},
	}
	smbAdapter := smb.New(smbConfig)

	// Create DittoServer
	tc.Server = server.New(tc.Registry, 30*time.Second)

	// Add adapter
	if err := tc.Server.AddAdapter(smbAdapter); err != nil {
		tc.T.Fatalf("Failed to add SMB adapter: %v", err)
	}

	// Start server in background
	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()
		if err := tc.Server.Serve(tc.ctx); err != nil && err != context.Canceled {
			tc.T.Logf("Server error: %v", err)
		}
	}()

	// Wait for server to be ready
	tc.waitForServer()
}

// createUserStore creates a user store with test credentials
func (tc *SMBTestContext) createUserStore() identity.UserStore {
	// Hash password for test user
	hash, err := identity.HashPassword(tc.Password)
	if err != nil {
		tc.T.Fatalf("Failed to hash password: %v", err)
	}

	testUser := &identity.User{
		Username:     tc.Username,
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

// waitForServer waits for the SMB server to be ready to accept connections
func (tc *SMBTestContext) waitForServer() {
	tc.T.Helper()

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			tc.T.Fatal("Timeout waiting for SMB server to start")
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tc.Port), time.Second)
			if err == nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// mountSMB mounts the SMB share at a temporary directory
func (tc *SMBTestContext) mountSMB() {
	tc.T.Helper()

	// Give the SMB server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-smb-e2e-mount-*")
	if err != nil {
		tc.T.Fatalf("Failed to create mount directory: %v", err)
	}
	tc.MountPath = mountPath
	tc.tempDirs = append(tc.tempDirs, mountPath)

	// Build mount command based on platform
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// macOS: mount_smbfs
		// Format: //user:password@host:port/share
		smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", tc.Username, tc.Password, tc.Port)
		cmd = exec.Command("mount_smbfs", smbURL, mountPath)
	case "linux":
		// Linux: mount -t cifs
		cmd = exec.Command("mount", "-t", "cifs",
			fmt.Sprintf("//localhost/export"),
			mountPath,
			"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.0",
				tc.Port, tc.Username, tc.Password))
	default:
		tc.T.Fatalf("Unsupported platform for SMB: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			tc.T.Logf("SMB share mounted successfully at %s", mountPath)
			break
		}

		if i < maxRetries-1 {
			tc.T.Logf("SMB mount attempt %d failed (error: %v), retrying in 1 second...", i+1, lastErr)
			time.Sleep(time.Second)

			// Rebuild command for next attempt (cmd can only be run once)
			switch runtime.GOOS {
			case "darwin":
				smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", tc.Username, tc.Password, tc.Port)
				cmd = exec.Command("mount_smbfs", smbURL, mountPath)
			case "linux":
				cmd = exec.Command("mount", "-t", "cifs",
					fmt.Sprintf("//localhost/export"),
					mountPath,
					"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.0",
						tc.Port, tc.Username, tc.Password))
			}
		}
	}

	if lastErr != nil {
		tc.T.Fatalf("Failed to mount SMB share after %d attempts: %v\nOutput: %s",
			maxRetries, lastErr, string(output))
	}

	tc.mounted = true
}

// Cleanup unmounts the SMB share, stops the server, and removes temporary files
func (tc *SMBTestContext) Cleanup() {
	tc.T.Helper()

	// Unmount SMB share
	if tc.mounted {
		tc.unmountSMB()
	}

	// Stop server
	if tc.cancel != nil {
		tc.cancel()
	}

	// Wait for server to stop
	tc.wg.Wait()

	// Close stores and cache
	if tc.MetadataStore != nil {
		if closer, ok := tc.MetadataStore.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	if tc.ContentStore != nil {
		if closer, ok := tc.ContentStore.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	if tc.Cache != nil {
		_ = tc.Cache.Close()
	}

	// Remove temporary directories
	for _, dir := range tc.tempDirs {
		_ = os.RemoveAll(dir)
	}
}

// unmountSMB unmounts the SMB share
func (tc *SMBTestContext) unmountSMB() {
	tc.T.Helper()

	if tc.MountPath == "" {
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("umount", tc.MountPath)
	case "linux":
		cmd = exec.Command("umount", tc.MountPath)
	default:
		tc.T.Logf("Unsupported platform for unmount: %s", runtime.GOOS)
		return
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		tc.T.Logf("Failed to unmount SMB share: %v\nOutput: %s", err, string(output))
		// Try force unmount
		cmd = exec.Command("umount", "-f", tc.MountPath)
		_ = cmd.Run()
	}

	tc.mounted = false
}

// Path returns the absolute path for a relative path within the mount
func (tc *SMBTestContext) Path(relativePath string) string {
	return filepath.Join(tc.MountPath, relativePath)
}

// CreateTempDir creates a temporary directory and registers it for cleanup
func (tc *SMBTestContext) CreateTempDir(prefix string) string {
	tc.T.Helper()

	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		tc.T.Fatalf("Failed to create temp directory: %v", err)
	}
	tc.tempDirs = append(tc.tempDirs, dir)
	return dir
}

// GetConfig returns the test configuration
func (tc *SMBTestContext) GetConfig() *TestConfig {
	return tc.Config
}

// GetPort returns the server port
func (tc *SMBTestContext) GetPort() int {
	return tc.Port
}

// findFreeSMBPort finds an available TCP port for SMB
func findFreeSMBPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	return listener.Addr().(*net.TCPAddr).Port
}
