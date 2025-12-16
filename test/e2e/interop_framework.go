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
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/server"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// InteropTestContext provides a testing environment with both NFS and SMB protocols
// sharing the same metadata and content stores. This enables cross-protocol testing
// such as writing via NFS and reading via SMB.
type InteropTestContext struct {
	T             *testing.T
	Config        *TestConfig
	Server        *server.DittoServer
	Registry      *registry.Registry
	MetadataStore metadata.MetadataStore
	ContentStore  content.ContentStore
	Cache         cache.Cache

	// NFS access
	NFSMountPath string
	NFSPort      int
	nfsMounted   bool

	// SMB access
	SMBMountPath string
	SMBPort      int
	SMBUsername  string
	SMBPassword  string
	smbMounted   bool

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	tempDirs []string
}

// NewInteropTestContext creates a test environment with both NFS and SMB protocols
// sharing the same underlying stores.
func NewInteropTestContext(t *testing.T, config *TestConfig) *InteropTestContext {
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

	tc := &InteropTestContext{
		T:           t,
		Config:      config,
		ctx:         ctx,
		cancel:      cancel,
		NFSPort:     findFreePort(t),
		SMBPort:     findFreePort(t),
		SMBUsername: SMBTestUsername,
		SMBPassword: SMBTestPassword,
	}

	// Setup stores
	tc.setupStores()

	// Start server with both adapters
	tc.startServer()

	// Mount both protocols
	tc.mountNFS()
	tc.mountSMB()

	return tc
}

// setupStores initializes shared metadata and content stores
func (tc *InteropTestContext) setupStores() {
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

// startServer starts the DittoFS server with both NFS and SMB adapters
func (tc *InteropTestContext) startServer() {
	tc.T.Helper()

	// Initialize logger
	logger.SetLevel("ERROR")

	// Create Registry
	tc.Registry = registry.NewRegistry()

	// Create and set user store for SMB authentication
	userStore := tc.createUserStore()
	tc.Registry.SetUserStore(userStore)

	// Register shared stores
	storeName := fmt.Sprintf("interop-metadata-%d", tc.NFSPort)
	if err := tc.Registry.RegisterMetadataStore(storeName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	contentStoreName := fmt.Sprintf("interop-content-%d", tc.NFSPort)
	if err := tc.Registry.RegisterContentStore(contentStoreName, tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	// Register cache if enabled
	cacheName := ""
	if tc.Cache != nil {
		cacheName = fmt.Sprintf("interop-cache-%d", tc.NFSPort)
		if err := tc.Registry.RegisterCache(cacheName, tc.Cache); err != nil {
			tc.T.Fatalf("Failed to register cache: %v", err)
		}
	}

	// Create share accessible by both protocols
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
			Mode: 0755,
			UID:  0,
			GID:  0,
		},
	}

	if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share: %v", err)
	}

	// Create NFS adapter
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

	// Create SMB adapter
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

	// Create server
	tc.Server = server.New(tc.Registry, 30*time.Second)

	// Add both adapters
	if err := tc.Server.AddAdapter(nfsAdapter); err != nil {
		tc.T.Fatalf("Failed to add NFS adapter: %v", err)
	}
	if err := tc.Server.AddAdapter(smbAdapter); err != nil {
		tc.T.Fatalf("Failed to add SMB adapter: %v", err)
	}

	// Start server
	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()
		if err := tc.Server.Serve(tc.ctx); err != nil && err != context.Canceled {
			tc.T.Logf("Server error: %v", err)
		}
	}()

	// Wait for both protocols to be ready
	tc.waitForProtocols()
}

// createUserStore creates a user store with test credentials
func (tc *InteropTestContext) createUserStore() identity.UserStore {
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

// waitForProtocols waits for both NFS and SMB to be ready
func (tc *InteropTestContext) waitForProtocols() {
	tc.T.Helper()

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	nfsReady := false
	smbReady := false

	for !nfsReady || !smbReady {
		select {
		case <-timeout:
			tc.T.Fatalf("Timeout waiting for protocols (NFS ready: %v, SMB ready: %v)", nfsReady, smbReady)
		case <-ticker.C:
			if !nfsReady {
				if conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tc.NFSPort), time.Second); err == nil {
					conn.Close()
					nfsReady = true
				}
			}
			if !smbReady {
				if conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tc.SMBPort), time.Second); err == nil {
					conn.Close()
					smbReady = true
				}
			}
		}
	}
}

// mountNFS mounts the NFS share
func (tc *InteropTestContext) mountNFS() {
	tc.T.Helper()

	time.Sleep(500 * time.Millisecond)

	mountPath, err := os.MkdirTemp("", "dittofs-interop-nfs-*")
	if err != nil {
		tc.T.Fatalf("Failed to create NFS mount directory: %v", err)
	}
	tc.NFSMountPath = mountPath
	tc.tempDirs = append(tc.tempDirs, mountPath)

	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d", tc.NFSPort, tc.NFSPort)
	var mountArgs []string

	switch runtime.GOOS {
	case "darwin":
		mountOptions += ",resvport"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, "localhost:/export", mountPath}
	case "linux":
		mountOptions += ",nolock"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, "localhost:/export", mountPath}
	default:
		tc.T.Fatalf("Unsupported platform: %s", runtime.GOOS)
	}

	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		cmd := exec.Command("mount", mountArgs...)
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			tc.T.Logf("NFS share mounted at %s", mountPath)
			break
		}

		if i < maxRetries-1 {
			tc.T.Logf("NFS mount attempt %d failed, retrying...", i+1)
			time.Sleep(time.Second)
		}
	}

	if lastErr != nil {
		tc.T.Fatalf("Failed to mount NFS share: %v\nOutput: %s", lastErr, string(output))
	}

	tc.nfsMounted = true
}

// mountSMB mounts the SMB share
func (tc *InteropTestContext) mountSMB() {
	tc.T.Helper()

	time.Sleep(500 * time.Millisecond)

	mountPath, err := os.MkdirTemp("", "dittofs-interop-smb-*")
	if err != nil {
		tc.T.Fatalf("Failed to create SMB mount directory: %v", err)
	}
	tc.SMBMountPath = mountPath
	tc.tempDirs = append(tc.tempDirs, mountPath)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", tc.SMBUsername, tc.SMBPassword, tc.SMBPort)
		cmd = exec.Command("mount_smbfs", smbURL, mountPath)
	case "linux":
		cmd = exec.Command("mount", "-t", "cifs",
			"//localhost/export",
			mountPath,
			"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.0",
				tc.SMBPort, tc.SMBUsername, tc.SMBPassword))
	default:
		tc.T.Fatalf("Unsupported platform for SMB: %s", runtime.GOOS)
	}

	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			tc.T.Logf("SMB share mounted at %s", mountPath)
			break
		}

		if i < maxRetries-1 {
			tc.T.Logf("SMB mount attempt %d failed, retrying...", i+1)
			time.Sleep(time.Second)

			// Rebuild command
			switch runtime.GOOS {
			case "darwin":
				smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", tc.SMBUsername, tc.SMBPassword, tc.SMBPort)
				cmd = exec.Command("mount_smbfs", smbURL, mountPath)
			case "linux":
				cmd = exec.Command("mount", "-t", "cifs",
					"//localhost/export",
					mountPath,
					"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.0",
						tc.SMBPort, tc.SMBUsername, tc.SMBPassword))
			}
		}
	}

	if lastErr != nil {
		tc.T.Fatalf("Failed to mount SMB share: %v\nOutput: %s", lastErr, string(output))
	}

	tc.smbMounted = true
}

// NFSPath returns absolute path within NFS mount
func (tc *InteropTestContext) NFSPath(relativePath string) string {
	return filepath.Join(tc.NFSMountPath, relativePath)
}

// SMBPath returns absolute path within SMB mount
func (tc *InteropTestContext) SMBPath(relativePath string) string {
	return filepath.Join(tc.SMBMountPath, relativePath)
}

// Cleanup unmounts both shares and cleans up resources
func (tc *InteropTestContext) Cleanup() {
	tc.T.Helper()

	// Unmount both protocols
	if tc.smbMounted {
		tc.unmountSMB()
	}
	if tc.nfsMounted {
		tc.unmountNFS()
	}

	// Stop server
	if tc.cancel != nil {
		tc.cancel()
	}
	tc.wg.Wait()

	// Close stores
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

	// Remove temp directories
	for _, dir := range tc.tempDirs {
		_ = os.RemoveAll(dir)
	}
}

func (tc *InteropTestContext) unmountNFS() {
	cmd := exec.Command("umount", tc.NFSMountPath)
	if _, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("umount", "-f", tc.NFSMountPath)
		_ = cmd.Run()
	}
	tc.nfsMounted = false
}

func (tc *InteropTestContext) unmountSMB() {
	cmd := exec.Command("umount", tc.SMBMountPath)
	if _, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("umount", "-f", tc.SMBMountPath)
		_ = cmd.Run()
	}
	tc.smbMounted = false
}

// Implement TestContextProvider interface
func (tc *InteropTestContext) CreateTempDir(prefix string) string {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		tc.T.Fatalf("Failed to create temp directory: %v", err)
	}
	tc.tempDirs = append(tc.tempDirs, dir)
	return dir
}

func (tc *InteropTestContext) GetConfig() *TestConfig {
	return tc.Config
}

func (tc *InteropTestContext) GetPort() int {
	return tc.NFSPort
}
