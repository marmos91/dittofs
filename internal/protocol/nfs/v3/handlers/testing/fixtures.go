// Package testing provides test fixtures for NFS v3 handler behavioral tests.
//
// This package uses real memory stores (not mocks) to test handlers against
// RFC 1813 behavioral requirements without testing implementation details.
package testing

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload"
	storemem "github.com/marmos91/dittofs/pkg/payload/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
)

// DefaultShareName is the default share name used in test fixtures.
const DefaultShareName = "/export"

// DefaultUID is the default user ID for test contexts.
const DefaultUID = uint32(1000)

// DefaultGID is the default group ID for test contexts.
const DefaultGID = uint32(1000)

// HandlerTestFixture provides a complete test environment for NFS v3 handlers.
//
// It sets up:
//   - A real memory metadata store (owned by MetadataService)
//   - A Cache for content (via ContentService)
//   - A registry with a configured share
//   - A Handler instance ready for testing
//
// Use NewHandlerFixture to create a fixture for each test.
type HandlerTestFixture struct {
	t *testing.T

	// Handler is the NFS v3 handler under test.
	Handler *handlers.Handler

	// Registry manages stores and shares.
	Registry *runtime.Runtime

	// MetadataService provides high-level metadata operations.
	// It owns the memory-backed metadata store.
	MetadataService *metadata.MetadataService

	// ContentService provides high-level content operations.
	// It uses Cache for content storage.
	ContentService *payload.PayloadService

	// ShareName is the name of the test share.
	ShareName string

	// RootHandle is the file handle for the share's root directory.
	RootHandle metadata.FileHandle
}

// NewHandlerFixture creates a new test fixture with default configuration.
//
// The fixture includes:
//   - Memory metadata store with default capabilities
//   - Cache for content (auto-created by registry)
//   - A share named "/export"
//   - Handler with the registry configured
//
// The fixture automatically cleans up on test completion.
func NewHandlerFixture(t *testing.T) *HandlerTestFixture {
	t.Helper()

	ctx := context.Background()

	// Create stores
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Create cache, block store, and transfer manager for content operations
	testCache := cache.New(0) // 0 = unlimited size
	blockStore := storemem.New()
	// Use metaStore as ObjectStore - MemoryMetadataStore implements ObjectStore
	transferMgr := transfer.New(testCache, blockStore, metaStore, transfer.DefaultConfig())

	// Create PayloadService with cache and transfer manager
	payloadSvc, err := payload.New(testCache, transferMgr)
	if err != nil {
		t.Fatalf("Failed to create payload service: %v", err)
	}

	// Create registry and set up payload service
	reg := runtime.New(nil)
	reg.SetPayloadService(payloadSvc)

	if err := reg.RegisterMetadataStore("test-metaSvc", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	// Add share
	shareConfig := &runtime.ShareConfig{
		Name:          DefaultShareName,
		MetadataStore: "test-metaSvc",
		RootAttr:      &metadata.FileAttr{}, // Empty attr, AddShare will apply defaults
	}
	if err := reg.AddShare(ctx, shareConfig); err != nil {
		t.Fatalf("Failed to add share: %v", err)
	}

	// Get root handle
	share, err := reg.GetShare(DefaultShareName)
	if err != nil {
		t.Fatalf("Failed to get share: %v", err)
	}

	// Create handler
	handler := &handlers.Handler{
		Registry: reg,
	}

	return &HandlerTestFixture{
		t:               t,
		Handler:         handler,
		Registry:        reg,
		MetadataService: reg.GetMetadataService(),
		ContentService:  reg.GetBlockService(),
		ShareName:       DefaultShareName,
		RootHandle:      share.RootHandle,
	}
}

// Context returns a new NFSHandlerContext with default credentials.
//
// The context has:
//   - AUTH_UNIX flavor (1)
//   - UID 1000
//   - GID 1000
//   - Client address "127.0.0.1:12345"
func (f *HandlerTestFixture) Context() *handlers.NFSHandlerContext {
	uid := DefaultUID
	gid := DefaultGID
	return &handlers.NFSHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:12345",
		Share:      f.ShareName,
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{gid},
	}
}

// ContextWithUID returns a context with a custom UID/GID.
func (f *HandlerTestFixture) ContextWithUID(uid, gid uint32) *handlers.NFSHandlerContext {
	return &handlers.NFSHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:12345",
		Share:      f.ShareName,
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{gid},
	}
}

// ContextWithCancellation returns a context that is already cancelled.
func (f *HandlerTestFixture) ContextWithCancellation() *handlers.NFSHandlerContext {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	uid := DefaultUID
	gid := DefaultGID
	return &handlers.NFSHandlerContext{
		Context:    ctx,
		ClientAddr: "127.0.0.1:12345",
		Share:      f.ShareName,
		AuthFlavor: 1,
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{gid},
	}
}

// CreateDirectory creates a directory at the given path.
//
// The path should be relative to the share root (e.g., "subdir" or "a/b/c").
// Parent directories are created automatically.
//
// Returns the file handle for the created directory.
func (f *HandlerTestFixture) CreateDirectory(path string) metadata.FileHandle {
	f.t.Helper()

	authCtx := f.authContext()

	// Split path into components
	components := splitPath(path)
	if len(components) == 0 {
		return f.RootHandle
	}

	// Create each component
	parentHandle := f.RootHandle
	for _, name := range components {
		// Check if already exists
		existing, err := f.MetadataService.Lookup(authCtx, parentHandle, name)
		if err == nil {
			handle, err := metadata.EncodeFileHandle(existing)
			if err != nil {
				f.t.Fatalf("Failed to encode handle: %v", err)
			}
			parentHandle = handle
			continue
		}

		// Create directory
		dir, err := f.MetadataService.CreateDirectory(authCtx, parentHandle, name, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  DefaultUID,
			GID:  DefaultGID,
		})
		if err != nil {
			f.t.Fatalf("Failed to create directory %q: %v", name, err)
		}

		handle, err := metadata.EncodeFileHandle(dir)
		if err != nil {
			f.t.Fatalf("Failed to encode handle: %v", err)
		}
		parentHandle = handle
	}

	return parentHandle
}

// CreateFile creates a file at the given path with the specified content.
//
// The path should be relative to the share root.
// Parent directories are created automatically.
//
// Returns the file handle for the created file.
func (f *HandlerTestFixture) CreateFile(path string, content []byte) metadata.FileHandle {
	f.t.Helper()

	authCtx := f.authContext()
	ctx := context.Background()

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	var parentHandle metadata.FileHandle
	if dir == "." || dir == "" {
		parentHandle = f.RootHandle
	} else {
		parentHandle = f.CreateDirectory(dir)
	}

	// Create the file
	name := filepath.Base(path)
	file, err := f.MetadataService.CreateFile(authCtx, parentHandle, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  DefaultUID,
		GID:  DefaultGID,
	})
	if err != nil {
		f.t.Fatalf("Failed to create file %q: %v", path, err)
	}

	// Write content if provided (using ContentService with Cache)
	if len(content) > 0 {
		if err := f.ContentService.WriteAt(ctx, file.PayloadID, content, 0); err != nil {
			f.t.Fatalf("Failed to write content to file %q: %v", path, err)
		}

		// Update file size in metadata
		newSize := uint64(len(content))
		err := f.MetadataService.SetFileAttributes(authCtx, mustEncodeHandle(f.t, file), &metadata.SetAttrs{
			Size: &newSize,
		})
		if err != nil {
			f.t.Fatalf("Failed to update file size for %q: %v", path, err)
		}
	}

	return mustEncodeHandle(f.t, file)
}

// CreateSymlink creates a symbolic link at the given path pointing to target.
//
// Returns the file handle for the created symlink.
func (f *HandlerTestFixture) CreateSymlink(path, target string) metadata.FileHandle {
	f.t.Helper()

	authCtx := f.authContext()

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	var parentHandle metadata.FileHandle
	if dir == "." || dir == "" {
		parentHandle = f.RootHandle
	} else {
		parentHandle = f.CreateDirectory(dir)
	}

	// Create the symlink
	name := filepath.Base(path)
	symlink, err := f.MetadataService.CreateSymlink(authCtx, parentHandle, name, target, &metadata.FileAttr{
		Mode: 0777,
		UID:  DefaultUID,
		GID:  DefaultGID,
	})
	if err != nil {
		f.t.Fatalf("Failed to create symlink %q -> %q: %v", path, target, err)
	}

	return mustEncodeHandle(f.t, symlink)
}

// GetHandle returns the file handle for the given path.
//
// The path should be relative to the share root.
// Returns nil if the path doesn't exist.
func (f *HandlerTestFixture) GetHandle(path string) metadata.FileHandle {
	f.t.Helper()

	if path == "" || path == "/" || path == "." {
		return f.RootHandle
	}

	authCtx := f.authContext()
	components := splitPath(path)

	currentHandle := f.RootHandle
	for _, name := range components {
		file, err := f.MetadataService.Lookup(authCtx, currentHandle, name)
		if err != nil {
			return nil
		}
		handle, err := metadata.EncodeFileHandle(file)
		if err != nil {
			return nil
		}
		currentHandle = handle
	}

	return currentHandle
}

// MustGetHandle returns the file handle for the given path.
// Fails the test if the path doesn't exist.
func (f *HandlerTestFixture) MustGetHandle(path string) metadata.FileHandle {
	f.t.Helper()

	handle := f.GetHandle(path)
	if handle == nil {
		f.t.Fatalf("Path %q does not exist", path)
	}
	return handle
}

// GetFile returns the File for the given path.
// Returns nil if the path doesn't exist.
func (f *HandlerTestFixture) GetFile(path string) *metadata.File {
	f.t.Helper()

	handle := f.GetHandle(path)
	if handle == nil {
		return nil
	}

	file, err := f.MetadataService.GetFile(context.Background(), handle)
	if err != nil {
		return nil
	}
	return file
}

// ReadContent reads the content of a file at the given path.
func (f *HandlerTestFixture) ReadContent(path string) []byte {
	f.t.Helper()

	file := f.GetFile(path)
	if file == nil {
		f.t.Fatalf("File %q does not exist", path)
	}

	ctx := context.Background()

	// Get content size
	size, err := f.ContentService.GetSize(ctx, file.PayloadID)
	if err != nil {
		f.t.Fatalf("Failed to get content size for %q: %v", path, err)
	}

	// Read content using ContentService (backed by Cache)
	content := make([]byte, size)
	n, err := f.ContentService.ReadAt(ctx, file.PayloadID, content, 0)
	if err != nil {
		f.t.Fatalf("Failed to read content from %q: %v", path, err)
	}

	return content[:n]
}

// authContext creates a metadata.AuthContext for store operations.
// Uses root credentials (UID 0) to ensure write permissions for setup operations.
func (f *HandlerTestFixture) authContext() *metadata.AuthContext {
	uid := uint32(0) // root for setup operations
	gid := uint32(0)
	return &metadata.AuthContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:12345",
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  &uid,
			GID:  &gid,
			GIDs: []uint32{gid},
		},
	}
}

// mustEncodeHandle encodes a file to a handle, failing the test on error.
func mustEncodeHandle(t *testing.T, file *metadata.File) metadata.FileHandle {
	t.Helper()
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("Failed to encode file handle: %v", err)
	}
	return handle
}

// splitPath splits a path into components, handling empty paths.
func splitPath(path string) []string {
	if path == "" || path == "/" || path == "." {
		return nil
	}

	// Clean the path
	path = filepath.Clean(path)
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	// Split by separator
	var components []string
	for path != "" && path != "." {
		dir, file := filepath.Split(path)
		if file != "" {
			components = append([]string{file}, components...)
		}
		path = filepath.Clean(dir)
		if path == "/" || path == "." {
			break
		}
	}

	return components
}
