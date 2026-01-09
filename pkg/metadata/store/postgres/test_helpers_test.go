package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/testcontainers/testcontainers-go"
)

// testContainer manages a PostgreSQL container for testing
type testContainer struct {
	container testcontainers.Container
	connStr   string
}

// cleanup is a no-op when using shared test container
// The shared container is terminated in TestMain
func (tc *testContainer) cleanup(t *testing.T) {
	t.Helper()
	// No-op: container is shared and will be cleaned up by TestMain
}

// setupTestStore creates a PostgreSQL metadata store for testing
// Uses the shared test container to improve test performance
func setupTestStore(t *testing.T) (*PostgresMetadataStore, *testContainer) {
	t.Helper()

	// Use shared container instead of creating a new one
	if sharedTestContainer == nil {
		t.Fatal("shared test container not initialized - TestMain() not run?")
	}

	// Get host and port from shared container
	ctx := context.Background()
	host, err := sharedTestContainer.container.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get container host: %v", err)
	}

	port, err := sharedTestContainer.container.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatalf("failed to get container port: %v", err)
	}

	config := &PostgresMetadataStoreConfig{
		Host:        host,
		Port:        port.Int(),
		Database:    "dittofs_test",
		User:        "dittofs_test",
		Password:    "dittofs_test",
		SSLMode:     "disable",
		MaxConns:    10,
		MinConns:    2,
		AutoMigrate: true, // Enable auto-migration for tests
	}

	// Set reasonable default capabilities for testing
	capabilities := metadata.FilesystemCapabilities{
		// Transfer sizes
		MaxReadSize:        1024 * 1024, // 1 MB
		PreferredReadSize:  64 * 1024,   // 64 KB
		MaxWriteSize:       1024 * 1024, // 1 MB
		PreferredWriteSize: 64 * 1024,   // 64 KB

		// Limits
		MaxFileSize:      1024 * 1024 * 1024 * 100, // 100 GB
		MaxFilenameLen:   255,
		MaxPathLen:       4096,
		MaxHardLinkCount: 32767,

		// Features
		SupportsHardLinks: true,
		SupportsSymlinks:  true,
		CaseSensitive:     true,
		CasePreserving:    true,
		SupportsACLs:      false,

		// Time resolution
		TimestampResolution: time.Nanosecond,
	}

	store, err := NewPostgresMetadataStore(context.Background(), config, capabilities)
	if err != nil {
		t.Fatalf("failed to create postgres store: %v", err)
	}

	return store, sharedTestContainer
}

// createTestAuthContext creates an AuthContext for testing
func createTestAuthContext() *metadata.AuthContext {
	uid := uint32(1000)
	gid := uint32(1000)
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  &uid,
			GID:  &gid,
			GIDs: []uint32{1000},
		},
		ClientAddr: "127.0.0.1:12345",
	}
}

// createRootAuthContext creates a root AuthContext for testing
func createRootAuthContext() *metadata.AuthContext {
	uid := uint32(0)
	gid := uint32(0)
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  &uid,
			GID:  &gid,
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1:12345",
	}
}

// createTestFile creates a test file and returns its metadata
func createTestFile(t *testing.T, store *PostgresMetadataStore, ctx *metadata.AuthContext, parentHandle metadata.FileHandle, name string) *metadata.File {
	t.Helper()

	attr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	file, err := store.Create(ctx, parentHandle, name, attr)
	if err != nil {
		t.Fatalf("failed to create test file %s: %v", name, err)
	}

	return file
}

// createTestDirectory creates a test directory and returns its metadata
func createTestDirectory(t *testing.T, store *PostgresMetadataStore, ctx *metadata.AuthContext, parentHandle metadata.FileHandle, name string) *metadata.File {
	t.Helper()

	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	dir, err := store.Create(ctx, parentHandle, name, attr)
	if err != nil {
		t.Fatalf("failed to create test directory %s: %v", name, err)
	}

	return dir
}

// getFileHandle encodes a file handle for testing
func getFileHandle(file *metadata.File) metadata.FileHandle {
	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	if err != nil {
		panic(fmt.Sprintf("failed to encode file handle: %v", err))
	}
	return handle
}

// assertFileEqual compares two files for equality
func assertFileEqual(t *testing.T, expected, actual *metadata.File, msg string) {
	t.Helper()

	if expected.ID != actual.ID {
		t.Errorf("%s: ID mismatch: expected %v, got %v", msg, expected.ID, actual.ID)
	}
	if expected.ShareName != actual.ShareName {
		t.Errorf("%s: ShareName mismatch: expected %s, got %s", msg, expected.ShareName, actual.ShareName)
	}
	if expected.Path != actual.Path {
		t.Errorf("%s: Path mismatch: expected %s, got %s", msg, expected.Path, actual.Path)
	}
	if expected.Type != actual.Type {
		t.Errorf("%s: Type mismatch: expected %v, got %v", msg, expected.Type, actual.Type)
	}
	if expected.Mode != actual.Mode {
		t.Errorf("%s: Mode mismatch: expected %o, got %o", msg, expected.Mode, actual.Mode)
	}
	if expected.UID != actual.UID {
		t.Errorf("%s: UID mismatch: expected %d, got %d", msg, expected.UID, actual.UID)
	}
	if expected.GID != actual.GID {
		t.Errorf("%s: GID mismatch: expected %d, got %d", msg, expected.GID, actual.GID)
	}
}

// assertError checks if an error matches expected error code
func assertError(t *testing.T, err error, expectedCode metadata.ErrorCode, msg string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s: expected error with code %v, got nil", msg, expectedCode)
	}

	storeErr, ok := err.(*metadata.StoreError)
	if !ok {
		t.Fatalf("%s: expected StoreError, got %T: %v", msg, err, err)
	}

	if storeErr.Code != expectedCode {
		t.Errorf("%s: expected error code %v, got %v", msg, expectedCode, storeErr.Code)
	}
}

// mustGetRootHandle creates a root directory with a unique name and returns its handle and share name
func mustGetRootHandle(t *testing.T, store *PostgresMetadataStore) (metadata.FileHandle, string) {
	t.Helper()

	// Generate unique share name for this test to avoid conflicts with shared container
	shareName := generateUniqueName("share")

	attr := &metadata.FileAttr{
		Mode: 0777, // World-writable for testing
		UID:  0,
		GID:  0,
	}

	root, err := store.CreateRootDirectory(context.Background(), shareName, attr)
	if err != nil {
		t.Fatalf("failed to create root directory: %v", err)
	}

	return getFileHandle(root), shareName
}

// generateUniqueName generates a unique name for testing
func generateUniqueName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.New().String()[:8])
}
