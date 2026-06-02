package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newWalkTestStore returns a fresh in-memory metadata store seeded with a
// share named "wshare" and an empty root directory. Returns the store +
// the share name + the root handle.
func newWalkTestStore(t *testing.T) (metadata.Store, string, metadata.FileHandle) {
	t.Helper()
	ctx := t.Context()
	store := memmeta.NewMemoryMetadataStoreWithDefaults()

	shareName := "wshare"
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	return store, shareName, rootHandle
}

// addFile materializes a regular file under parent.
func addFile(t *testing.T, store metadata.Store, shareName string, parent metadata.FileHandle, name, path string) metadata.FileHandle {
	t.Helper()
	ctx := t.Context()
	handle, err := store.GenerateHandle(ctx, shareName, path)
	if err != nil {
		t.Fatalf("GenerateHandle(%s): %v", path, err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	file := &metadata.File{
		ShareName: shareName,
		Path:      path,
		ID:        id,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile(%s): %v", path, err)
	}
	if err := store.SetParent(ctx, handle, parent); err != nil {
		t.Fatalf("SetParent(%s): %v", path, err)
	}
	if err := store.SetChild(ctx, parent, name, handle); err != nil {
		t.Fatalf("SetChild(%s): %v", path, err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount(%s): %v", path, err)
	}
	return handle
}

// addDir materializes a child directory under parent.
func addDir(t *testing.T, store metadata.Store, shareName string, parent metadata.FileHandle, name, path string) metadata.FileHandle {
	t.Helper()
	ctx := t.Context()
	handle, err := store.GenerateHandle(ctx, shareName, path)
	if err != nil {
		t.Fatalf("GenerateHandle(%s): %v", path, err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	dir := &metadata.File{
		ShareName: shareName,
		Path:      path,
		ID:        id,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}
	if err := store.PutFile(ctx, dir); err != nil {
		t.Fatalf("PutFile(dir %s): %v", path, err)
	}
	if err := store.SetParent(ctx, handle, parent); err != nil {
		t.Fatalf("SetParent(dir %s): %v", path, err)
	}
	if err := store.SetChild(ctx, parent, name, handle); err != nil {
		t.Fatalf("SetChild(dir %s): %v", path, err)
	}
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount(dir %s): %v", path, err)
	}
	return handle
}

// TestWalkShareFiles_Empty_W1 covers W1: walking a freshly created share
// with no children invokes the callback zero times.
func TestWalkShareFiles_Empty_W1(t *testing.T) {
	store, shareName, _ := newWalkTestStore(t)

	count := 0
	err := WalkShareFiles(t.Context(), store, shareName, func(handle metadata.FileHandle, file *metadata.File) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkShareFiles: %v", err)
	}
	if count != 0 {
		t.Errorf("empty share: callback invocations = %d, want 0", count)
	}
}

// TestWalkShareFiles_SingleFile_W2 covers W2: a single file at the root
// triggers exactly one callback invocation.
func TestWalkShareFiles_SingleFile_W2(t *testing.T) {
	store, shareName, root := newWalkTestStore(t)
	addFile(t, store, shareName, root, "a.txt", "/a.txt")

	var got []string
	err := WalkShareFiles(t.Context(), store, shareName, func(handle metadata.FileHandle, file *metadata.File) error {
		got = append(got, file.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkShareFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d invocations, want 1: %v", len(got), got)
	}
	if got[0] != "/a.txt" {
		t.Errorf("path = %q, want %q", got[0], "/a.txt")
	}
}

// TestWalkShareFiles_NestedTree_W3 covers W3: 5 files in a depth-3 tree
// → exactly 5 callback invocations, no directory delivery.
func TestWalkShareFiles_NestedTree_W3(t *testing.T) {
	store, shareName, root := newWalkTestStore(t)

	// /a.txt
	addFile(t, store, shareName, root, "a.txt", "/a.txt")
	// /sub/b.txt
	sub := addDir(t, store, shareName, root, "sub", "/sub")
	addFile(t, store, shareName, sub, "b.txt", "/sub/b.txt")
	// /sub/c.txt
	addFile(t, store, shareName, sub, "c.txt", "/sub/c.txt")
	// /sub/sub2/d.txt
	sub2 := addDir(t, store, shareName, sub, "sub2", "/sub/sub2")
	addFile(t, store, shareName, sub2, "d.txt", "/sub/sub2/d.txt")
	// /sub/sub2/e.txt
	addFile(t, store, shareName, sub2, "e.txt", "/sub/sub2/e.txt")

	var got []string
	err := WalkShareFiles(t.Context(), store, shareName, func(handle metadata.FileHandle, file *metadata.File) error {
		if file.Type != metadata.FileTypeRegular {
			t.Errorf("non-regular file delivered to callback: type=%v path=%q", file.Type, file.Path)
		}
		got = append(got, file.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkShareFiles: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d invocations, want 5: %v", len(got), got)
	}
	sort.Strings(got)
	want := []string{"/a.txt", "/sub/b.txt", "/sub/c.txt", "/sub/sub2/d.txt", "/sub/sub2/e.txt"}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("got[%d] = %q, want %q", i, got[i], p)
		}
	}
}

// TestWalkShareFiles_Pagination_W4 covers W4: a directory wider than the
// internal page size yields exactly one callback per child.
func TestWalkShareFiles_Pagination_W4(t *testing.T) {
	store, shareName, root := newWalkTestStore(t)

	// Add 600 files at the root — well beyond walkPageSize (256).
	const n = 600
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f-%04d.txt", i)
		addFile(t, store, shareName, root, name, "/"+name)
	}

	var count atomic.Int64
	err := WalkShareFiles(t.Context(), store, shareName, func(handle metadata.FileHandle, file *metadata.File) error {
		count.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkShareFiles: %v", err)
	}
	if got := count.Load(); got != n {
		t.Errorf("pagination walk: got %d invocations, want %d", got, n)
	}
}

// TestWalkShareFiles_ContextCancel_W5 covers W5: context cancellation
// aborts the walk and surfaces ctx.Err().
func TestWalkShareFiles_ContextCancel_W5(t *testing.T) {
	store, shareName, root := newWalkTestStore(t)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("f-%d.txt", i)
		addFile(t, store, shareName, root, name, "/"+name)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var seen int
	err := WalkShareFiles(ctx, store, shareName, func(handle metadata.FileHandle, file *metadata.File) error {
		seen++
		if seen == 3 {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Fatalf("expected context-cancel error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
}

// TestWalkShareFiles_CallbackError_W6 covers W6: a callback error aborts
// the walk and is returned wrapped.
func TestWalkShareFiles_CallbackError_W6(t *testing.T) {
	store, shareName, root := newWalkTestStore(t)
	addFile(t, store, shareName, root, "a.txt", "/a.txt")
	addFile(t, store, shareName, root, "b.txt", "/b.txt")

	sentinel := errors.New("walk: sentinel callback error")
	err := WalkShareFiles(t.Context(), store, shareName, func(handle metadata.FileHandle, file *metadata.File) error {
		return sentinel
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is(err, sentinel)", err)
	}
}
