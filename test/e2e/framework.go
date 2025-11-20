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
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/server"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// TestContext provides a complete testing environment with:
// - Running DittoFS server
// - Mounted NFS share
// - Cleanup mechanisms
type TestContext struct {
	T             *testing.T
	Config        *TestConfig
	Server        *server.DittoServer
	Registry      *registry.Registry
	MetadataStore metadata.MetadataStore
	ContentStore  content.WritableContentStore
	MountPath     string
	Port          int
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	tempDirs      []string
	mounted       bool
}

// NewTestContext creates a new test environment with the specified configuration.
// It starts the DittoFS server and mounts the NFS share.
func NewTestContext(t *testing.T, config *TestConfig) *TestContext {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	tc := &TestContext{
		T:      t,
		Config: config,
		ctx:    ctx,
		cancel: cancel,
		Port:   findFreePort(t),
	}

	// Setup stores based on configuration
	tc.setupStores()

	// Start DittoFS server
	tc.startServer()

	// Mount NFS share
	tc.mountNFS()

	return tc
}

// setupStores initializes metadata and content stores based on the test configuration
func (tc *TestContext) setupStores() {
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
}

// startServer starts the DittoFS server with the configured stores
func (tc *TestContext) startServer() {
	tc.T.Helper()

	// Initialize logger
	// Always use ERROR level to keep test output clean
	// These are functional tests, not debugging sessions
	logger.SetLevel("ERROR")

	// Create Registry
	tc.Registry = registry.NewRegistry()

	// Register stores with unique names
	storeName := fmt.Sprintf("test-metadata-%d", tc.Port)
	if err := tc.Registry.RegisterMetadataStore(storeName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	contentStoreName := fmt.Sprintf("test-content-%d", tc.Port)
	if err := tc.Registry.RegisterContentStore(contentStoreName, tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	// Create share
	shareConfig := &registry.ShareConfig{
		Name:          "/export",
		MetadataStore: storeName,
		ContentStore:  contentStoreName,
		ReadOnly:      false,
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
		Port:           tc.Port,
		MaxConnections: 0,
		Timeouts: nfs.NFSTimeoutsConfig{
			Read:     5 * time.Minute,
			Write:    30 * time.Second,
			Idle:     5 * time.Minute,
			Shutdown: 30 * time.Second,
		},
	}
	nfsAdapter := nfs.New(nfsConfig, nil) // nil = no metrics

	// Create DittoServer
	tc.Server = server.New(tc.Registry, 30*time.Second)

	// Add adapter (server will call SetRegistry automatically)
	if err := tc.Server.AddAdapter(nfsAdapter); err != nil {
		tc.T.Fatalf("Failed to add NFS adapter: %v", err)
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

// waitForServer waits for the NFS server to be ready to accept connections
func (tc *TestContext) waitForServer() {
	tc.T.Helper()

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			tc.T.Fatal("Timeout waiting for server to start")
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", tc.Port), time.Second)
			if err == nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// mountNFS mounts the NFS share at a temporary directory
func (tc *TestContext) mountNFS() {
	tc.T.Helper()

	// Give the NFS server a moment to fully initialize
	// This is especially important for mount protocol to be ready
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-e2e-mount-*")
	if err != nil {
		tc.T.Fatalf("Failed to create mount directory: %v", err)
	}
	tc.MountPath = mountPath
	tc.tempDirs = append(tc.tempDirs, mountPath)

	// Build mount command parameters based on platform
	// Both NFS and MOUNT protocols run on the same port in DittoFS
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d", tc.Port, tc.Port)
	var mountArgs []string

	switch runtime.GOOS {
	case "darwin":
		// macOS mount options
		mountOptions += ",resvport"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, "localhost:/export", mountPath}
	case "linux":
		// Linux mount
		mountOptions += ",nolock"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, "localhost:/export", mountPath}
	default:
		tc.T.Fatalf("Unsupported platform: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		// Create a fresh command for each attempt
		cmd := exec.Command("mount", mountArgs...)
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			tc.T.Logf("NFS share mounted successfully at %s", mountPath)
			break
		}

		if i < maxRetries-1 {
			tc.T.Logf("Mount attempt %d failed (error: %v), retrying in 1 second...", i+1, lastErr)
			time.Sleep(time.Second)
		}
	}

	if lastErr != nil {
		tc.T.Fatalf("Failed to mount NFS share after %d attempts: %v\nOutput: %s\nMount command: mount %v",
			maxRetries, lastErr, string(output), mountArgs)
	}

	tc.mounted = true
}

// Cleanup unmounts the NFS share, stops the server, and removes temporary files
func (tc *TestContext) Cleanup() {
	tc.T.Helper()

	// Unmount NFS share
	if tc.mounted {
		tc.unmountNFS()
	}

	// Stop server
	if tc.cancel != nil {
		tc.cancel()
	}

	// Wait for server to stop
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

	// Remove temporary directories
	for _, dir := range tc.tempDirs {
		_ = os.RemoveAll(dir)
	}
}

// unmountNFS unmounts the NFS share
func (tc *TestContext) unmountNFS() {
	tc.T.Helper()

	if tc.MountPath == "" {
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin", "linux":
		cmd = exec.Command("umount", tc.MountPath)
	default:
		tc.T.Logf("Unsupported platform for unmount: %s", runtime.GOOS)
		return
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		tc.T.Logf("Failed to unmount NFS share: %v\nOutput: %s", err, string(output))
		// Try force unmount
		cmd = exec.Command("umount", "-f", tc.MountPath)
		_ = cmd.Run()
	}

	tc.mounted = false
}

// Path returns the absolute path for a relative path within the mount
func (tc *TestContext) Path(relativePath string) string {
	return filepath.Join(tc.MountPath, relativePath)
}

// CreateTempDir creates a temporary directory and registers it for cleanup
func (tc *TestContext) CreateTempDir(prefix string) string {
	tc.T.Helper()

	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		tc.T.Fatalf("Failed to create temp directory: %v", err)
	}
	tc.tempDirs = append(tc.tempDirs, dir)
	return dir
}

// GetConfig returns the test configuration
func (tc *TestContext) GetConfig() *TestConfig {
	return tc.Config
}

// GetPort returns the server port
func (tc *TestContext) GetPort() int {
	return tc.Port
}

// findFreePort finds an available TCP port
func findFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	return listener.Addr().(*net.TCPAddr).Port
}
