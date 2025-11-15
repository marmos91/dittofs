package framework

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/content"
	contentfs "github.com/marmos91/dittofs/pkg/content/fs"
	contentmemory "github.com/marmos91/dittofs/pkg/content/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/memory"
	"github.com/marmos91/dittofs/pkg/server"
)

// StoreType represents the type of backend store to use
type StoreType string

const (
	StoreTypeMemory        StoreType = "memory"
	StoreTypeMemoryChunked StoreType = "memory-chunked"
	StoreTypeFilesystem    StoreType = "filesystem"
)

// TestServerConfig holds configuration for the test server.
// This is distinct from pkg/config.ServerConfig (application-level server settings)
// and pkg/metadata.MetadataServerConfig (repository-level settings).
type TestServerConfig struct {
	Port           int
	ContentStore   StoreType
	ContentPath    string // Only used for filesystem store
	ShareName      string
	LogLevel       string
	StartupTimeout time.Duration
}

// TestServer wraps a DittoFS server for testing
type TestServer struct {
	t             testing.TB
	config        TestServerConfig
	server        *server.DittoServer
	metadataStore metadata.MetadataStore
	contentStore  content.WritableContentStore
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	started       bool
	mu            sync.Mutex
	tempDir       string // Temporary directory for this server instance
}

// NewTestServer creates a new test server instance
func NewTestServer(t testing.TB, config TestServerConfig) *TestServer {
	t.Helper()

	// Set defaults
	if config.Port == 0 {
		config.Port = findFreePort(t)
	}
	if config.ShareName == "" {
		config.ShareName = "/export"
	}
	if config.LogLevel == "" {
		config.LogLevel = "ERROR" // Keep tests quiet by default
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = 10 * time.Second
	}

	// Create temporary directory for this server instance
	tempDir, err := os.MkdirTemp("", "dittofs-e2e-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}

	// Set content path for filesystem store
	if config.ContentStore == StoreTypeFilesystem && config.ContentPath == "" {
		config.ContentPath = filepath.Join(tempDir, "content")
		if err := os.MkdirAll(config.ContentPath, 0755); err != nil {
			_ = os.RemoveAll(tempDir)
			t.Fatalf("Failed to create content directory: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &TestServer{
		t:       t,
		config:  config,
		ctx:     ctx,
		cancel:  cancel,
		tempDir: tempDir,
	}
}

// Start starts the test server
func (ts *TestServer) Start() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.started {
		return fmt.Errorf("server already started")
	}

	ts.t.Helper()

	// Set log level
	logger.SetLevel(ts.config.LogLevel)

	// Create content store based on type
	var err error
	switch ts.config.ContentStore {
	case StoreTypeMemory:
		ts.contentStore, err = contentmemory.NewMemoryContentStore(ts.ctx)
		if err != nil {
			return fmt.Errorf("failed to create memory content store: %w", err)
		}
		ts.t.Logf("Using memory content store")
	case StoreTypeMemoryChunked:
		ts.contentStore, err = contentmemory.NewChunkedMemoryContentStore(ts.ctx, 64*1024)
		if err != nil {
			return fmt.Errorf("failed to create chunked memory content store: %w", err)
		}
		ts.t.Logf("Using chunked memory content store")
	case StoreTypeFilesystem:
		ts.contentStore, err = contentfs.NewFSContentStore(ts.ctx, ts.config.ContentPath)
		if err != nil {
			return fmt.Errorf("failed to create filesystem content store: %w", err)
		}
		ts.t.Logf("Using filesystem content store at %s", ts.config.ContentPath)
	default:
		return fmt.Errorf("unknown content store type: %s", ts.config.ContentStore)
	}

	// Create metadata store (always memory for now)
	ts.metadataStore = metadatamemory.NewMemoryMetadataStoreWithDefaults()
	ts.t.Logf("Using memory metadata store")

	// Add share/export
	shareOpts := metadata.ShareOptions{
		ReadOnly: false,
	}

	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  uint32(os.Getuid()),
		GID:  uint32(os.Getgid()),
	}

	if err := ts.metadataStore.AddShare(ts.ctx, ts.config.ShareName, shareOpts, rootAttr); err != nil {
		return fmt.Errorf("failed to add share: %w", err)
	}
	ts.t.Logf("Added share %s", ts.config.ShareName)

	// Create NFS adapter
	nfsConfig := nfs.NFSConfig{
		Port: ts.config.Port,
	}
	nfsAdapter := nfs.New(nfsConfig, nil) // nil = no metrics for tests

	// Create DittoServer
	ts.server = server.New(ts.metadataStore, ts.contentStore)
	_ = ts.server.AddAdapter(nfsAdapter)

	// Start server in background
	ts.wg.Add(1)
	go func() {
		defer ts.wg.Done()
		if err := ts.server.Serve(ts.ctx); err != nil && err != context.Canceled {
			ts.t.Logf("Server error: %v", err)
		}
	}()

	// Wait for server to be ready
	ts.t.Logf("Waiting for server to start on port %d...", ts.config.Port)
	if err := ts.waitForServer(); err != nil {
		ts.cancel()
		ts.wg.Wait()
		return fmt.Errorf("server failed to start: %w", err)
	}

	ts.started = true
	ts.t.Logf("Server started successfully on port %d", ts.config.Port)
	return nil
}

// Stop stops the test server and cleans up resources
func (ts *TestServer) Stop() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if !ts.started {
		return nil
	}

	ts.t.Helper()
	ts.t.Logf("Stopping server...")

	// Cancel context to stop server
	ts.cancel()

	// Wait for server to stop with timeout
	done := make(chan struct{})
	go func() {
		ts.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		ts.t.Logf("Server stopped gracefully")
	case <-time.After(5 * time.Second):
		ts.t.Logf("Server stop timeout - forcing shutdown")
	}

	// Clean up temporary directory
	if ts.tempDir != "" {
		if err := os.RemoveAll(ts.tempDir); err != nil {
			ts.t.Logf("Warning: failed to remove temp directory %s: %v", ts.tempDir, err)
		}
	}

	ts.started = false
	return nil
}

// Port returns the port the server is listening on
func (ts *TestServer) Port() int {
	return ts.config.Port
}

// ShareName returns the share/export name
func (ts *TestServer) ShareName() string {
	return ts.config.ShareName
}

// ContentStore returns the content store instance
func (ts *TestServer) ContentStore() content.WritableContentStore {
	return ts.contentStore
}

// MetadataStore returns the metadata store instance
func (ts *TestServer) MetadataStore() metadata.MetadataStore {
	return ts.metadataStore
}

// waitForServer waits for the server to be ready by attempting to connect
func (ts *TestServer) waitForServer() error {
	deadline := time.Now().Add(ts.config.StartupTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", ts.config.Port), 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			// Give the NFS service more time to fully initialize and bind
			// This is especially important for macOS
			time.Sleep(1 * time.Second)
			ts.t.Logf("Server is accepting connections on port %d", ts.config.Port)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for server to start")
}

// findFreePort finds an available port
func findFreePort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	// Give the OS time to release the port
	time.Sleep(100 * time.Millisecond)
	return port
}
