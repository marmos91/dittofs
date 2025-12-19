// Package testing provides test fixtures for NFS v3 handler behavioral tests.
//
// This package uses real memory stores (not mocks) to test handlers against
// RFC 1813 behavioral requirements without testing implementation details.
package testing

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/pkg/registry"
	contentmemory "github.com/marmos91/dittofs/pkg/store/content/memory"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/store/metadata/memory"
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
//   - A real memory metadata store
//   - A real memory content store
//   - A registry with a configured share
//   - A Handler instance ready for testing
//
// Use NewHandlerFixture to create a fixture for each test.
type HandlerTestFixture struct {
	t *testing.T

	// Handler is the NFS v3 handler under test.
	Handler *handlers.Handler

	// Registry manages stores and shares.
	Registry *registry.Registry

	// MetadataStore is the memory-backed metadata store.
	MetadataStore *metadatamemory.MemoryMetadataStore

	// ContentStore is the memory-backed content store.
	ContentStore *contentmemory.MemoryContentStore

	// ShareName is the name of the test share.
	ShareName string

	// RootHandle is the file handle for the share's root directory.
	RootHandle metadata.FileHandle
}

// NewHandlerFixture creates a new test fixture with default configuration.
//
// The fixture includes:
//   - Memory metadata store with default capabilities
//   - Memory content store
//   - A share named "/export"
//   - Handler with the registry configured
//
// The fixture automatically cleans up on test completion.
func NewHandlerFixture(t *testing.T) *HandlerTestFixture {
	t.Helper()

	ctx := context.Background()

	// Create stores
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	contentStore, err := contentmemory.NewMemoryContentStore(ctx)
	if err != nil {
		t.Fatalf("Failed to create content store: %v", err)
	}

	// Create registry and register stores
	reg := registry.NewRegistry()
	if err := reg.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}
	if err := reg.RegisterContentStore("test-content", contentStore); err != nil {
		t.Fatalf("Failed to register content store: %v", err)
	}

	// Add share
	shareConfig := &registry.ShareConfig{
		Name:          DefaultShareName,
		MetadataStore: "test-meta",
		ContentStore:  "test-content",
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
		t:             t,
		Handler:       handler,
		Registry:      reg,
		MetadataStore: metaStore,
		ContentStore:  contentStore,
		ShareName:     DefaultShareName,
		RootHandle:    share.RootHandle,
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
		existing, err := f.MetadataStore.Lookup(authCtx, parentHandle, name)
		if err == nil {
			handle, err := metadata.EncodeFileHandle(existing)
			if err != nil {
				f.t.Fatalf("Failed to encode handle: %v", err)
			}
			parentHandle = handle
			continue
		}

		// Create directory
		dir, err := f.MetadataStore.Create(authCtx, parentHandle, name, &metadata.FileAttr{
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
	file, err := f.MetadataStore.Create(authCtx, parentHandle, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  DefaultUID,
		GID:  DefaultGID,
	})
	if err != nil {
		f.t.Fatalf("Failed to create file %q: %v", path, err)
	}

	// Write content if provided
	if len(content) > 0 {
		if err := f.ContentStore.WriteContent(ctx, file.ContentID, content); err != nil {
			f.t.Fatalf("Failed to write content to file %q: %v", path, err)
		}

		// Update file size in metadata
		newSize := uint64(len(content))
		err := f.MetadataStore.SetFileAttributes(authCtx, mustEncodeHandle(f.t, file), &metadata.SetAttrs{
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
	symlink, err := f.MetadataStore.CreateSymlink(authCtx, parentHandle, name, target, &metadata.FileAttr{
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
		file, err := f.MetadataStore.Lookup(authCtx, currentHandle, name)
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

	file, err := f.MetadataStore.GetFile(context.Background(), handle)
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

	reader, err := f.ContentStore.ReadContent(context.Background(), file.ContentID)
	if err != nil {
		f.t.Fatalf("Failed to read content from %q: %v", path, err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			f.t.Errorf("Failed to close reader for %q: %v", path, closeErr)
		}
	}()

	content, err := io.ReadAll(reader)
	if err != nil {
		f.t.Fatalf("Failed to read content from %q: %v", path, err)
	}

	return content
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
