package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// MultiShareTestContext provides a test environment with multiple shares
// backed by the same metadata and content stores
type MultiShareTestContext struct {
	T             *testing.T
	Config        *TestConfig
	Server        *server.DittoServer
	Registry      *registry.Registry
	MetadataStore metadata.MetadataStore
	ContentStore  content.WritableContentStore
	Shares        map[string]*ShareMount // share name -> mount info
	Port          int
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	tempDirs      []string
}

// ShareMount contains mount information for a single share
type ShareMount struct {
	Name      string
	MountPath string
	Mounted   bool
}

// NewMultiShareTestContext creates a test environment with multiple shares
func NewMultiShareTestContext(t *testing.T, config *TestConfig, shareNames []string) *MultiShareTestContext {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	tc := &MultiShareTestContext{
		T:      t,
		Config: config,
		Shares: make(map[string]*ShareMount),
		ctx:    ctx,
		cancel: cancel,
		Port:   findFreePort(t),
	}

	// Setup stores
	tc.setupStores()

	// Start server with multiple shares
	tc.startServer(shareNames)

	// Mount all shares
	tc.mountAllShares(shareNames)

	return tc
}

// setupStores initializes metadata and content stores
func (tc *MultiShareTestContext) setupStores() {
	tc.T.Helper()

	var err error

	// Create metadata store (shared by all shares)
	tc.MetadataStore, err = tc.Config.CreateMetadataStore(tc.ctx, tc)
	if err != nil {
		tc.T.Fatalf("Failed to create metadata store: %v", err)
	}

	// Create content store (shared by all shares)
	tc.ContentStore, err = tc.Config.CreateContentStore(tc.ctx, tc)
	if err != nil {
		tc.T.Fatalf("Failed to create content store: %v", err)
	}
}

// startServer starts the DittoFS server with multiple shares using the same stores
func (tc *MultiShareTestContext) startServer(shareNames []string) {
	tc.T.Helper()

	// Initialize logger
	logger.SetLevel("ERROR")

	// Create Registry
	tc.Registry = registry.NewRegistry()

	// Register the SAME metadata and content stores
	metadataStoreName := fmt.Sprintf("shared-metadata-%d", tc.Port)
	if err := tc.Registry.RegisterMetadataStore(metadataStoreName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	contentStoreName := fmt.Sprintf("shared-content-%d", tc.Port)
	if err := tc.Registry.RegisterContentStore(contentStoreName, tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	// Create multiple shares, all using the SAME stores
	for _, shareName := range shareNames {
		shareConfig := &registry.ShareConfig{
			Name:          shareName,
			MetadataStore: metadataStoreName, // Same store for all shares
			ContentStore:  contentStoreName,  // Same store for all shares
			ReadOnly:      false,
			RootAttr: &metadata.FileAttr{
				Type: metadata.FileTypeDirectory,
				Mode: 0755,
				UID:  0,
				GID:  0,
			},
		}

		if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
			tc.T.Fatalf("Failed to add share %s: %v", shareName, err)
		}

		// Initialize share mount info
		tc.Shares[shareName] = &ShareMount{
			Name:    shareName,
			Mounted: false,
		}
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
	nfsAdapter := nfs.New(nfsConfig, nil)

	// Create DittoServer
	tc.Server = server.New(tc.Registry, 30*time.Second)

	// Add adapter
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

// waitForServer waits for the server to be ready
func (tc *MultiShareTestContext) waitForServer() {
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

// mountAllShares mounts all shares
func (tc *MultiShareTestContext) mountAllShares(shareNames []string) {
	tc.T.Helper()

	// Give the NFS server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	for _, shareName := range shareNames {
		tc.mountShare(shareName)
	}
}

// mountShare mounts a single share
func (tc *MultiShareTestContext) mountShare(shareName string) {
	tc.T.Helper()

	share, exists := tc.Shares[shareName]
	if !exists {
		tc.T.Fatalf("Share %s not found", shareName)
	}

	// Create mount directory
	mountPath, err := os.MkdirTemp("", fmt.Sprintf("dittofs-e2e-mount-%s-*", filepath.Base(shareName)))
	if err != nil {
		tc.T.Fatalf("Failed to create mount directory for %s: %v", shareName, err)
	}
	share.MountPath = mountPath
	tc.tempDirs = append(tc.tempDirs, mountPath)

	// Build mount command
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d", tc.Port, tc.Port)
	var mountArgs []string

	switch runtime.GOOS {
	case "darwin":
		mountOptions += ",resvport"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, fmt.Sprintf("localhost:%s", shareName), mountPath}
	case "linux":
		mountOptions += ",nolock"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, fmt.Sprintf("localhost:%s", shareName), mountPath}
	default:
		tc.T.Fatalf("Unsupported platform: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		cmd := exec.Command("mount", mountArgs...)
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			tc.T.Logf("NFS share %s mounted at %s", shareName, mountPath)
			share.Mounted = true
			break
		}

		if i < maxRetries-1 {
			tc.T.Logf("Mount attempt %d for %s failed, retrying...", i+1, shareName)
			time.Sleep(time.Second)
		}
	}

	if lastErr != nil {
		tc.T.Fatalf("Failed to mount share %s: %v\nOutput: %s", shareName, lastErr, string(output))
	}
}

// Cleanup unmounts all shares and stops the server
func (tc *MultiShareTestContext) Cleanup() {
	tc.T.Helper()

	// Unmount all shares
	for _, share := range tc.Shares {
		if share.Mounted {
			tc.unmountShare(share)
		}
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

// unmountShare unmounts a single share
func (tc *MultiShareTestContext) unmountShare(share *ShareMount) {
	tc.T.Helper()

	if share.MountPath == "" {
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin", "linux":
		cmd = exec.Command("umount", share.MountPath)
	default:
		tc.T.Logf("Unsupported platform for unmount: %s", runtime.GOOS)
		return
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		tc.T.Logf("Failed to unmount %s: %v\nOutput: %s", share.Name, err, string(output))
		// Try force unmount
		cmd = exec.Command("umount", "-f", share.MountPath)
		_ = cmd.Run()
	}

	share.Mounted = false
}

// Path returns the absolute path for a filesystem item within a specific share
func (tc *MultiShareTestContext) Path(shareName, relativePath string) string {
	share, exists := tc.Shares[shareName]
	if !exists {
		tc.T.Fatalf("Share %s not found", shareName)
	}
	return filepath.Join(share.MountPath, relativePath)
}

// CreateTempDir creates a temporary directory and registers it for cleanup
func (tc *MultiShareTestContext) CreateTempDir(prefix string) string {
	tc.T.Helper()

	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		tc.T.Fatalf("Failed to create temp directory: %v", err)
	}
	tc.tempDirs = append(tc.tempDirs, dir)
	return dir
}

// GetConfig returns the test configuration
func (tc *MultiShareTestContext) GetConfig() *TestConfig {
	return tc.Config
}

// GetPort returns the server port
func (tc *MultiShareTestContext) GetPort() int {
	return tc.Port
}

// ============================================================================
// Tests
// ============================================================================

// TestSharedStoresIsolation verifies that two shares using the same metadata
// and content stores maintain complete data isolation
func TestSharedStoresIsolation(t *testing.T) {
	// Run on all non-S3 configurations
	configs := AllConfigurations()

	for _, config := range configs {
		config := config // capture for parallel tests
		t.Run(config.Name, func(t *testing.T) {
			testSharedStoresIsolation(t, config)
		})
	}
}

func testSharedStoresIsolation(t *testing.T, config *TestConfig) {
	// Create test context with two shares using the SAME stores
	shareNames := []string{"/share1", "/share2"}
	tc := NewMultiShareTestContext(t, config, shareNames)
	defer tc.Cleanup()

	share1 := tc.Shares["/share1"].MountPath
	share2 := tc.Shares["/share2"].MountPath

	t.Run("files_are_isolated", func(t *testing.T) {
		// Create file in share1
		file1 := filepath.Join(share1, "file_in_share1.txt")
		content1 := []byte("This file belongs to share1")
		if err := os.WriteFile(file1, content1, 0644); err != nil {
			t.Fatalf("Failed to create file in share1: %v", err)
		}

		// Verify file exists in share1
		if _, err := os.Stat(file1); err != nil {
			t.Fatalf("File should exist in share1: %v", err)
		}

		// Verify file does NOT exist in share2
		file2 := filepath.Join(share2, "file_in_share1.txt")
		if _, err := os.Stat(file2); !os.IsNotExist(err) {
			t.Errorf("File from share1 should NOT be visible in share2")
		}

		// Verify we can read the correct content from share1
		readContent, err := os.ReadFile(file1)
		if err != nil {
			t.Fatalf("Failed to read file from share1: %v", err)
		}
		if !bytes.Equal(readContent, content1) {
			t.Errorf("Content mismatch: got %q, want %q", readContent, content1)
		}
	})

	t.Run("directories_are_isolated", func(t *testing.T) {
		// Create directory in share2
		dir2 := filepath.Join(share2, "directory_in_share2")
		if err := os.Mkdir(dir2, 0755); err != nil {
			t.Fatalf("Failed to create directory in share2: %v", err)
		}

		// Verify directory exists in share2
		if info, err := os.Stat(dir2); err != nil || !info.IsDir() {
			t.Fatalf("Directory should exist in share2: %v", err)
		}

		// Verify directory does NOT exist in share1
		dir1 := filepath.Join(share1, "directory_in_share2")
		if _, err := os.Stat(dir1); !os.IsNotExist(err) {
			t.Errorf("Directory from share2 should NOT be visible in share1")
		}
	})

	t.Run("same_path_different_content", func(t *testing.T) {
		// Create file with same name in both shares but different content
		content1 := []byte("Content from share1")
		content2 := []byte("Content from share2")

		file1 := filepath.Join(share1, "same_name.txt")
		file2 := filepath.Join(share2, "same_name.txt")

		if err := os.WriteFile(file1, content1, 0644); err != nil {
			t.Fatalf("Failed to create file in share1: %v", err)
		}
		if err := os.WriteFile(file2, content2, 0644); err != nil {
			t.Fatalf("Failed to create file in share2: %v", err)
		}

		// Verify each share has its own content
		read1, err := os.ReadFile(file1)
		if err != nil {
			t.Fatalf("Failed to read from share1: %v", err)
		}
		read2, err := os.ReadFile(file2)
		if err != nil {
			t.Fatalf("Failed to read from share2: %v", err)
		}

		if !bytes.Equal(read1, content1) {
			t.Errorf("Share1 content mismatch: got %q, want %q", read1, content1)
		}
		if !bytes.Equal(read2, content2) {
			t.Errorf("Share2 content mismatch: got %q, want %q", read2, content2)
		}
		if bytes.Equal(read1, read2) {
			t.Errorf("Files should have different content, both have: %q", read1)
		}
	})

	t.Run("nested_directories_are_isolated", func(t *testing.T) {
		// Create nested structure in share1
		dir1 := filepath.Join(share1, "nested", "deep", "structure")
		if err := os.MkdirAll(dir1, 0755); err != nil {
			t.Fatalf("Failed to create nested directories in share1: %v", err)
		}

		file1 := filepath.Join(dir1, "nested_file.txt")
		if err := os.WriteFile(file1, []byte("nested content"), 0644); err != nil {
			t.Fatalf("Failed to create nested file in share1: %v", err)
		}

		// Verify the nested structure exists in share1
		if _, err := os.Stat(file1); err != nil {
			t.Fatalf("Nested file should exist in share1: %v", err)
		}

		// Verify the nested structure does NOT exist in share2
		file2 := filepath.Join(share2, "nested", "deep", "structure", "nested_file.txt")
		if _, err := os.Stat(file2); !os.IsNotExist(err) {
			t.Errorf("Nested structure from share1 should NOT be visible in share2")
		}
	})

	t.Run("readdir_only_shows_own_files", func(t *testing.T) {
		// Create files in share1
		for i := 0; i < 5; i++ {
			filename := fmt.Sprintf("share1_file_%d.txt", i)
			path := filepath.Join(share1, filename)
			if err := os.WriteFile(path, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
				t.Fatalf("Failed to create file in share1: %v", err)
			}
		}

		// Create files in share2
		for i := 0; i < 5; i++ {
			filename := fmt.Sprintf("share2_file_%d.txt", i)
			path := filepath.Join(share2, filename)
			if err := os.WriteFile(path, []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
				t.Fatalf("Failed to create file in share2: %v", err)
			}
		}

		// Read directory in share1
		entries1, err := os.ReadDir(share1)
		if err != nil {
			t.Fatalf("Failed to read directory in share1: %v", err)
		}

		// Read directory in share2
		entries2, err := os.ReadDir(share2)
		if err != nil {
			t.Fatalf("Failed to read directory in share2: %v", err)
		}

		// Verify share1 only has share1 files (plus any from previous tests)
		for _, entry := range entries1 {
			name := entry.Name()
			if strings.HasPrefix(name, "share2") {
				t.Errorf("Share1 directory listing contains file from share2: %s", name)
			}
		}

		// Verify share2 only has share2 files (plus any from previous tests)
		for _, entry := range entries2 {
			name := entry.Name()
			if strings.HasPrefix(name, "share1") {
				t.Errorf("Share2 directory listing contains file from share1: %s", name)
			}
		}
	})

	t.Run("delete_only_affects_own_share", func(t *testing.T) {
		// Create file with same name in both shares
		file1 := filepath.Join(share1, "delete_test.txt")
		file2 := filepath.Join(share2, "delete_test.txt")

		if err := os.WriteFile(file1, []byte("share1"), 0644); err != nil {
			t.Fatalf("Failed to create file in share1: %v", err)
		}
		if err := os.WriteFile(file2, []byte("share2"), 0644); err != nil {
			t.Fatalf("Failed to create file in share2: %v", err)
		}

		// Delete from share1
		if err := os.Remove(file1); err != nil {
			t.Fatalf("Failed to delete file from share1: %v", err)
		}

		// Verify deleted from share1
		if _, err := os.Stat(file1); !os.IsNotExist(err) {
			t.Errorf("File should be deleted from share1")
		}

		// Verify still exists in share2
		if _, err := os.Stat(file2); err != nil {
			t.Errorf("File should still exist in share2: %v", err)
		}

		// Verify content is still correct in share2
		content, err := os.ReadFile(file2)
		if err != nil {
			t.Fatalf("Failed to read file from share2: %v", err)
		}
		if !bytes.Equal(content, []byte("share2")) {
			t.Errorf("Content in share2 was affected by delete in share1")
		}
	})

	t.Run("rename_only_affects_own_share", func(t *testing.T) {
		// Create file with same name in both shares
		oldName1 := filepath.Join(share1, "rename_test.txt")
		oldName2 := filepath.Join(share2, "rename_test.txt")
		newName1 := filepath.Join(share1, "renamed.txt")

		if err := os.WriteFile(oldName1, []byte("share1"), 0644); err != nil {
			t.Fatalf("Failed to create file in share1: %v", err)
		}
		if err := os.WriteFile(oldName2, []byte("share2"), 0644); err != nil {
			t.Fatalf("Failed to create file in share2: %v", err)
		}

		// Rename in share1
		if err := os.Rename(oldName1, newName1); err != nil {
			t.Fatalf("Failed to rename file in share1: %v", err)
		}

		// Verify renamed in share1
		if _, err := os.Stat(newName1); err != nil {
			t.Errorf("Renamed file should exist in share1: %v", err)
		}
		if _, err := os.Stat(oldName1); !os.IsNotExist(err) {
			t.Errorf("Old file should not exist in share1")
		}

		// Verify share2 is unaffected
		if _, err := os.Stat(oldName2); err != nil {
			t.Errorf("Original file should still exist in share2: %v", err)
		}

		content, err := os.ReadFile(oldName2)
		if err != nil {
			t.Fatalf("Failed to read file from share2: %v", err)
		}
		if !bytes.Equal(content, []byte("share2")) {
			t.Errorf("Content in share2 was affected by rename in share1")
		}
	})
}
