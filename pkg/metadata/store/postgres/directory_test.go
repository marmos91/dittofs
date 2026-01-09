package postgres

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestReadDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create some files and directories
	_ = createTestFile(t, store, ctx, rootHandle, "file1.txt")
	_ = createTestFile(t, store, ctx, rootHandle, "file2.txt")
	_ = createTestDirectory(t, store, ctx, rootHandle, "subdir1")
	_ = createTestDirectory(t, store, ctx, rootHandle, "subdir2")

	// Read directory
	page, err := store.ReadDirectory(ctx, rootHandle, "", 0)
	if err != nil {
		t.Fatalf("ReadDirectory failed: %v", err)
	}

	// Verify we got all entries
	if len(page.Entries) != 4 {
		t.Errorf("expected 4 entries, got %d", len(page.Entries))
	}

	// Verify entries are sorted by name
	expectedNames := []string{"file1.txt", "file2.txt", "subdir1", "subdir2"}
	for i, entry := range page.Entries {
		if entry.Name != expectedNames[i] {
			t.Errorf("entry %d: expected name %s, got %s", i, expectedNames[i], entry.Name)
		}
	}

	// Verify no more pages
	if page.HasMore {
		t.Error("expected no more pages")
	}
	if page.NextToken != "" {
		t.Errorf("expected empty next token, got %s", page.NextToken)
	}
}

func TestReadDirectory_Pagination(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create 20 files (more than minimum page size of 10)
	for i := 0; i < 20; i++ {
		name := generateUniqueName("file")
		_ = createTestFile(t, store, ctx, rootHandle, name)
	}

	// Read first page (small limit to force pagination)
	page1, err := store.ReadDirectory(ctx, rootHandle, "", 200)
	if err != nil {
		t.Fatalf("ReadDirectory page 1 failed: %v", err)
	}

	// Should have some entries but not all (implementation has min 10 entries per page)
	if len(page1.Entries) == 0 {
		t.Fatal("expected some entries in page 1")
	}
	if len(page1.Entries) >= 20 {
		t.Fatal("expected pagination to limit entries")
	}

	// Should have more pages
	if !page1.HasMore {
		t.Error("expected more pages")
	}
	if page1.NextToken == "" {
		t.Error("expected non-empty next token")
	}

	// Read second page
	page2, err := store.ReadDirectory(ctx, rootHandle, page1.NextToken, 200)
	if err != nil {
		t.Fatalf("ReadDirectory page 2 failed: %v", err)
	}

	// Should have remaining entries
	if len(page2.Entries) == 0 {
		t.Fatal("expected entries in page 2")
	}

	// Total should be 20
	totalEntries := len(page1.Entries) + len(page2.Entries)
	if totalEntries != 20 {
		t.Errorf("expected 20 total entries, got %d", totalEntries)
	}

	// Page 2 should not have more pages
	if page2.HasMore {
		t.Error("expected no more pages after page 2")
	}
}

func TestReadDirectory_Empty(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Read empty directory
	page, err := store.ReadDirectory(ctx, rootHandle, "", 0)
	if err != nil {
		t.Fatalf("ReadDirectory failed: %v", err)
	}

	// Should have no entries
	if len(page.Entries) != 0 {
		t.Errorf("expected no entries, got %d", len(page.Entries))
	}
	if page.HasMore {
		t.Error("expected no more pages")
	}
}

func TestReadDirectory_NotDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create a file
	file := createTestFile(t, store, ctx, rootHandle, "regularfile.txt")
	fileHandle := getFileHandle(file)

	// Try to read directory on a file
	_, err := store.ReadDirectory(ctx, fileHandle, "", 0)
	assertError(t, err, metadata.ErrNotDirectory, "read directory on file")
}

func TestReadDirectory_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create directory with root permissions (mode 0700)
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0700, // Only root can read
		UID:  0,
		GID:  0,
	}
	restrictedDir, err := store.Create(rootCtx, rootHandle, "restricted", attr)
	if err != nil {
		t.Fatalf("failed to create restricted directory: %v", err)
	}

	// Try to read as non-root user
	userCtx := createTestAuthContext()
	restrictedHandle := getFileHandle(restrictedDir)

	_, err = store.ReadDirectory(userCtx, restrictedHandle, "", 0)
	assertError(t, err, metadata.ErrPermissionDenied, "permission denied")
}

func TestNestedDirectories(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create nested structure: /a/b/c/file.txt
	dirA := createTestDirectory(t, store, ctx, rootHandle, "a")
	dirAHandle := getFileHandle(dirA)

	dirB := createTestDirectory(t, store, ctx, dirAHandle, "b")
	dirBHandle := getFileHandle(dirB)

	dirC := createTestDirectory(t, store, ctx, dirBHandle, "c")
	dirCHandle := getFileHandle(dirC)

	file := createTestFile(t, store, ctx, dirCHandle, "file.txt")

	// Verify paths
	if dirA.Path != "/a" {
		t.Errorf("expected path '/a', got '%s'", dirA.Path)
	}
	if dirB.Path != "/a/b" {
		t.Errorf("expected path '/a/b', got '%s'", dirB.Path)
	}
	if dirC.Path != "/a/b/c" {
		t.Errorf("expected path '/a/b/c', got '%s'", dirC.Path)
	}
	if file.Path != "/a/b/c/file.txt" {
		t.Errorf("expected path '/a/b/c/file.txt', got '%s'", file.Path)
	}

	// Lookup file through nested path
	foundB, err := store.Lookup(ctx, dirAHandle, "b")
	if err != nil {
		t.Fatalf("Lookup b failed: %v", err)
	}
	foundBHandle := getFileHandle(foundB)

	foundC, err := store.Lookup(ctx, foundBHandle, "c")
	if err != nil {
		t.Fatalf("Lookup c failed: %v", err)
	}
	foundCHandle := getFileHandle(foundC)

	foundFile, err := store.Lookup(ctx, foundCHandle, "file.txt")
	if err != nil {
		t.Fatalf("Lookup file.txt failed: %v", err)
	}

	// Verify found file matches created file
	assertFileEqual(t, file, foundFile, "nested lookup")
}
