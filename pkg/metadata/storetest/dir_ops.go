package storetest

import (
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runDirOpsTests runs all directory operation conformance tests.
func runDirOpsTests(t *testing.T, factory StoreFactory) {
	t.Run("CreateDirectory", func(t *testing.T) { testCreateDirectory(t, factory) })
	t.Run("ListDirectory", func(t *testing.T) { testListDirectory(t, factory) })
	t.Run("RemoveEmptyDirectory", func(t *testing.T) { testRemoveEmptyDirectory(t, factory) })
	t.Run("NestedDirectories", func(t *testing.T) { testNestedDirectories(t, factory) })
	t.Run("RootDirectoryIdempotent", func(t *testing.T) { testRootDirectoryIdempotent(t, factory) })
}

// testCreateDirectory verifies that creating a directory results in the correct type and link count.
func testCreateDirectory(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	dirHandle := createTestDir(t, store, "/test", rootHandle, "subdir")

	ctx := t.Context()

	// Verify the directory exists
	file, err := store.GetFile(ctx, dirHandle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	if file.Type != metadata.FileTypeDirectory {
		t.Errorf("Type = %v, want FileTypeDirectory", file.Type)
	}
	if file.Mode != 0755 {
		t.Errorf("Mode = %o, want 0755", file.Mode)
	}

	// Verify link count is 2 (. and parent entry)
	count, err := store.GetLinkCount(ctx, dirHandle)
	if err != nil {
		t.Fatalf("GetLinkCount() failed: %v", err)
	}
	if count != 2 {
		t.Errorf("link count = %d, want 2", count)
	}

	// Verify parent
	parent, err := store.GetParent(ctx, dirHandle)
	if err != nil {
		t.Fatalf("GetParent() failed: %v", err)
	}
	if string(parent) != string(rootHandle) {
		t.Error("parent should be root handle")
	}
}

// testListDirectory verifies that listing a directory returns all children.
func testListDirectory(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	// Create several children
	createTestFile(t, store, "/test", rootHandle, "alpha.txt", 0644)
	createTestFile(t, store, "/test", rootHandle, "beta.txt", 0644)
	createTestDir(t, store, "/test", rootHandle, "gamma")

	ctx := t.Context()

	// List children
	entries, nextCursor, err := store.ListChildren(ctx, rootHandle, "", 100)
	if err != nil {
		t.Fatalf("ListChildren() failed: %v", err)
	}

	// Should have 3 entries
	if len(entries) != 3 {
		t.Fatalf("ListChildren() returned %d entries, want 3", len(entries))
	}

	// No more pages
	if nextCursor != "" {
		t.Errorf("nextCursor = %q, want empty (no more pages)", nextCursor)
	}

	// Verify all names present
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	sort.Strings(names)

	expected := []string{"alpha.txt", "beta.txt", "gamma"}
	for i, want := range expected {
		if names[i] != want {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want)
		}
	}
}

// testRemoveEmptyDirectory verifies that an empty directory can be removed.
func testRemoveEmptyDirectory(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	dirHandle := createTestDir(t, store, "/test", rootHandle, "emptydir")

	ctx := t.Context()

	// Remove directory
	if err := store.DeleteFile(ctx, dirHandle); err != nil {
		t.Fatalf("DeleteFile() failed: %v", err)
	}
	if err := store.DeleteChild(ctx, rootHandle, "emptydir"); err != nil {
		t.Fatalf("DeleteChild() failed: %v", err)
	}

	// Verify directory is gone
	_, err := store.GetFile(ctx, dirHandle)
	if err == nil {
		t.Error("GetFile() should fail after removal")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}

	// Verify child entry is gone
	_, err = store.GetChild(ctx, rootHandle, "emptydir")
	if err == nil {
		t.Error("GetChild() should fail after removal")
	}
}

// testNestedDirectories verifies parent/child relationships in nested directories.
func testNestedDirectories(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	// Create nested structure: /test/a/b/c
	dirA := createTestDir(t, store, "/test", rootHandle, "a")
	dirB := createTestDir(t, store, "/test", dirA, "b")
	dirC := createTestDir(t, store, "/test", dirB, "c")

	ctx := t.Context()

	// Verify parent chain
	parentB, err := store.GetParent(ctx, dirC)
	if err != nil {
		t.Fatalf("GetParent(c) failed: %v", err)
	}
	if string(parentB) != string(dirB) {
		t.Error("parent of c should be b")
	}

	parentA, err := store.GetParent(ctx, dirB)
	if err != nil {
		t.Fatalf("GetParent(b) failed: %v", err)
	}
	if string(parentA) != string(dirA) {
		t.Error("parent of b should be a")
	}

	parentRoot, err := store.GetParent(ctx, dirA)
	if err != nil {
		t.Fatalf("GetParent(a) failed: %v", err)
	}
	if string(parentRoot) != string(rootHandle) {
		t.Error("parent of a should be root")
	}

	// Verify child resolution at each level
	resolvedA, err := store.GetChild(ctx, rootHandle, "a")
	if err != nil {
		t.Fatalf("GetChild(root, a) failed: %v", err)
	}
	if string(resolvedA) != string(dirA) {
		t.Error("GetChild(root, a) returned wrong handle")
	}

	resolvedB, err := store.GetChild(ctx, dirA, "b")
	if err != nil {
		t.Fatalf("GetChild(a, b) failed: %v", err)
	}
	if string(resolvedB) != string(dirB) {
		t.Error("GetChild(a, b) returned wrong handle")
	}

	resolvedC, err := store.GetChild(ctx, dirB, "c")
	if err != nil {
		t.Fatalf("GetChild(b, c) failed: %v", err)
	}
	if string(resolvedC) != string(dirC) {
		t.Error("GetChild(b, c) returned wrong handle")
	}
}

// testRootDirectoryIdempotent verifies that creating a root directory is idempotent.
func testRootDirectoryIdempotent(t *testing.T, factory StoreFactory) {
	store := factory(t)

	ctx := t.Context()

	// Create share
	share := &metadata.Share{Name: "/idem"}
	if err := store.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare() failed: %v", err)
	}

	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
	}

	// Create root directory first time
	root1, err := store.CreateRootDirectory(ctx, "/idem", rootAttr)
	if err != nil {
		t.Fatalf("first CreateRootDirectory() failed: %v", err)
	}

	// Create root directory again (should be idempotent)
	root2, err := store.CreateRootDirectory(ctx, "/idem", rootAttr)
	if err != nil {
		t.Fatalf("second CreateRootDirectory() failed: %v", err)
	}

	// Both should return the same file (at least same share)
	if root1.ShareName != root2.ShareName {
		t.Errorf("ShareName mismatch: %q vs %q", root1.ShareName, root2.ShareName)
	}
}
