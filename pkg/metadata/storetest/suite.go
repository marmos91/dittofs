package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// childFullPath derives the full share-relative path for a child entry,
// joining the parent directory's stored Path with the child name. Root
// children become "/name"; nested entries become "/parent/.../name". This
// mirrors production (file_create.go) so path-keyed backends (Postgres) see
// a unique, non-empty path per entry instead of all-"" collisions.
func childFullPath(t *testing.T, store metadata.Store, parentHandle metadata.FileHandle, name string) string {
	t.Helper()

	parent, err := store.GetFile(t.Context(), parentHandle)
	if err != nil {
		t.Fatalf("GetFile(parent) failed: %v", err)
	}
	parentPath := parent.Path
	if parentPath == "" {
		parentPath = "/"
	}
	if parentPath == "/" {
		return "/" + name
	}
	return parentPath + "/" + name
}

// StoreFactory creates a fresh MetadataStore instance for each test.
// The factory receives *testing.T so it can use t.TempDir() for stores
// that need filesystem paths and t.Cleanup() for teardown.
type StoreFactory func(t *testing.T) metadata.Store

// RunConformanceSuite runs the full conformance test suite against the provided
// store factory. Each test gets a fresh store instance to ensure isolation.
//
// The suite covers three categories:
//   - FileOps: file CRUD, hard links, attributes, read/write, rename, truncate
//   - DirOps: directory CRUD, listing, nesting, non-empty removal
//   - Permissions: access checking (requires auth context support)
func RunConformanceSuite(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("FileOps", func(t *testing.T) {
		runFileOpsTests(t, factory)
	})

	t.Run("DirOps", func(t *testing.T) {
		runDirOpsTests(t, factory)
	})

	t.Run("Permissions", func(t *testing.T) {
		runPermissionsTests(t, factory)
	})

	// CrossProtocolPermissions (AD-0) asserts every protocol family — NFSv3,
	// NFSv4.0, NFSv4.1, SMB — reaches the SAME allow/deny decision for the
	// SAME file + ACL on THIS backend. It is the cross-protocol regression net
	// for the AD/LDAP work: DENY ACEs and SID-only grants must enforce
	// identically across protocols (the bug class the NFSv4 ACCESS fix closes).
	t.Run("CrossProtocolPermissions", func(t *testing.T) {
		RunCrossProtocolPermissionMatrix(t, factory)
	})

	t.Run("DurableHandles", func(t *testing.T) {
		RunDurableHandleStoreTests(t, factory)
	})

	// ClientRecovery covers the server-global NFSv4 client-recovery store
	// (Put/List round-trip incl. BootVerifier bytes, upsert, Delete,
	// RecordReclaimComplete, empty-list). Cross-backend parity is the point:
	// the shared suite catches divergence (e.g. upsert producing dup rows,
	// or List erroring on empty). Stores lacking ClientRecoveryStore skip.
	t.Run("ClientRecovery", func(t *testing.T) {
		RunClientRecoveryStoreTests(t, factory)
	})

	// ACLAliasing asserts both directions of FileAttr.ACL deep-copy
	// discipline: PutFile must not alias the caller's ACE slice, and
	// GetFile must not hand back the store's backing slice. Pins the
	// cross-backend parity gap the area-6 audit found in the memory backend.
	t.Run("ACLAliasing", func(t *testing.T) {
		runACLAliasingTests(t, factory)
	})

	// EAOps covers FileAttr.EAs (SMB extended attributes, MS-FSCC §2.4.15):
	// PutFile/GetFile round-trip, zero-length values, deletion, case-
	// insensitive name resolution with set-case preservation, deep-copy
	// (aliasing) discipline, and persistence across unrelated writes. Pins
	// cross-backend parity the same way ACLAliasing does for ACLs.
	t.Run("EAOps", func(t *testing.T) {
		runEAOpsTests(t, factory)
	})

	t.Run("FileBlockOps", func(t *testing.T) {
		runFileBlockOpsTests(t, factory)
	})

	// BlockRefOps conformance for FileAttr.Blocks []BlockRef
	// round-trip across PutFile/GetFile, replace semantics, and the
	// Postgres-only FK-cascade behavior. Memory and Badger skip the
	// cascade scenario via FileBlockRefsAccessor type-assertion
	// failure.
	t.Run("BlockRefOps", func(t *testing.T) {
		runBlockRefOpsTests(t, factory)
	})

	// TruncateBlockRefOps asserts that a size-down SetAttr prunes
	// FileAttr.Blocks / file_block_refs past the new EOF, so the snapshot
	// manifest never over-references content past the current size (#817).
	t.Run("TruncateBlockRefOps", func(t *testing.T) {
		runTruncateBlockRefTests(t, factory)
	})

	// ObjectIDOps conformance for FileAttr.ObjectID round-trip,
	// FindByObjectID lookup, mutation lifecycle, and the first-
	// committer-wins concurrent-quiesce race. All
	// three backends implement ObjectIDIndexAccessor so the race
	// scenario asserts index-row counts directly rather than skipping.
	t.Run("ObjectIDOps", func(t *testing.T) {
		runObjectIDOpsTests(t, factory)
	})

	// StoreSurface covers MetadataStore interface methods that previously
	// had ZERO cross-backend conformance coverage (area-6 audit H1):
	// DeleteShare, GetUsedBytes, GetFileByPayloadID, filesystem
	// meta/stats/caps, server config, Healthcheck, plus a pagination
	// scenario and a duplicate-CreateShare scenario.
	t.Run("StoreSurface", func(t *testing.T) {
		runStoreSurfaceTests(t, factory)
	})

	// INV02Fuzz property-based fuzzer for the global
	// invariant ∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks). Runs
	// 10 concurrent goroutines × 10 ops each (create/delete/copy mix)
	// against every backend. The leak-injection scenario uses an
	// optional RefCountLeakInjector capability — backends that don't
	// implement it (Badger / Postgres today) skip cleanly.
	t.Run("INV02Fuzz", func(t *testing.T) {
		t.Run("PropertyFuzz", func(t *testing.T) {
			testINV02_PropertyFuzz(t, factory)
		})
		t.Run("LeakInjection", func(t *testing.T) {
			testINV02_LeakInjection(t, factory)
		})
	})

	// Trash exercises the recycle behavior (unlink-into-bin, in-bin permanent
	// delete, exclude-pattern bypass, collision uniquing, subtree-as-one-entry,
	// and overwrite-victim recycling) against every backend. The in-package
	// unit tests cover memory only; this is the cross-backend parity gate that
	// catches a backend dropping DeletedAt/OriginalPath or overwriting on a
	// name collision.
	t.Run("Trash", func(t *testing.T) {
		runTrashConformanceTests(t, factory)
	})
}

// createTestShare is a helper that creates a share and root directory for testing.
// Returns the root handle.
func createTestShare(t *testing.T, store metadata.Store, shareName string) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Create share
	share := &metadata.Share{
		Name: shareName,
	}
	if err := store.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare(%q) failed: %v", shareName, err)
	}

	// Create root directory
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("CreateRootDirectory(%q) failed: %v", shareName, err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle() failed: %v", err)
	}

	return rootHandle
}

// createTestFile is a helper that creates a regular file in a directory.
// Returns the file handle.
func createTestFile(t *testing.T, store metadata.Store, shareName string, dirHandle metadata.FileHandle, name string, mode uint32) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Derive the full path from the parent directory, mirroring production
	// (file_create.go), so path-keyed backends (Postgres) get a unique,
	// non-empty path per entry. Root children are "/name"; nested entries
	// join the parent's path.
	fullPath := childFullPath(t, store, dirHandle, name)

	// Generate handle
	handle, err := store.GenerateHandle(ctx, shareName, fullPath)
	if err != nil {
		t.Fatalf("GenerateHandle() failed: %v", err)
	}

	// Create file entry
	file := &metadata.File{
		ShareName: shareName,
		Path:      fullPath,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: mode,
			UID:  1000,
			GID:  1000,
		},
	}
	// Decode handle to set ID
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle() failed: %v", err)
	}
	file.ID = id

	// Put file
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}

	// Set parent
	if err := store.SetParent(ctx, handle, dirHandle); err != nil {
		t.Fatalf("SetParent() failed: %v", err)
	}

	// Set child
	if err := store.SetChild(ctx, dirHandle, name, handle); err != nil {
		t.Fatalf("SetChild() failed: %v", err)
	}

	// Set link count
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	return handle
}

// createTestDir is a helper that creates a directory within a parent directory.
// Returns the directory handle.
func createTestDir(t *testing.T, store metadata.Store, shareName string, parentHandle metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Derive the full path from the parent directory so path-keyed backends
	// (Postgres) get a unique, non-empty path per entry.
	fullPath := childFullPath(t, store, parentHandle, name)

	// Generate handle
	handle, err := store.GenerateHandle(ctx, shareName, fullPath)
	if err != nil {
		t.Fatalf("GenerateHandle() failed: %v", err)
	}

	// Create dir entry
	dir := &metadata.File{
		ShareName: shareName,
		Path:      fullPath,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  1000,
			GID:  1000,
		},
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle() failed: %v", err)
	}
	dir.ID = id

	// Put directory
	if err := store.PutFile(ctx, dir); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}

	// Set parent
	if err := store.SetParent(ctx, handle, parentHandle); err != nil {
		t.Fatalf("SetParent() failed: %v", err)
	}

	// Set child in parent
	if err := store.SetChild(ctx, parentHandle, name, handle); err != nil {
		t.Fatalf("SetChild() failed: %v", err)
	}

	// Set link count (2 for directories: . and parent entry)
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	return handle
}
