package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// runDirOpsTests runs all directory operation conformance tests.
func runDirOpsTests(t *testing.T, factory StoreFactory) {
	t.Run("CreateDirectory", func(t *testing.T) { testCreateDirectory(t, factory) })
	t.Run("ListDirectory", func(t *testing.T) { testListDirectory(t, factory) })
	t.Run("ListDirectoryHydratesACL", func(t *testing.T) { testListDirectoryHydratesACL(t, factory) })
	t.Run("RemoveEmptyDirectory", func(t *testing.T) { testRemoveEmptyDirectory(t, factory) })
	t.Run("NestedDirectories", func(t *testing.T) { testNestedDirectories(t, factory) })
	t.Run("RootDirectoryIdempotent", func(t *testing.T) { testRootDirectoryIdempotent(t, factory) })
	t.Run("RootDirectoryReconcilesAttrs", func(t *testing.T) { testRootDirectoryReconcilesAttrs(t, factory) })
	t.Run("LinkCountAgreesWithGetFile", func(t *testing.T) { testLinkCountAgreesWithGetFile(t, factory) })
	t.Run("DeleteChildIsIdempotent", func(t *testing.T) { testDeleteChildIsIdempotent(t, factory) })
	t.Run("NamesOnlyMatchesWithAttrs", func(t *testing.T) { testNamesOnlyMatchesWithAttrs(t, factory) })
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
	entries, nextCursor, err := store.ListChildren(ctx, rootHandle, "", 100, metadata.WithAttrs)
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

// testListDirectoryHydratesACL verifies that ListChildren populates
// DirEntry.Attr.ACL so callers (notably SMB access-based enumeration, refs
// PR #536) can make ACL-aware decisions without a follow-up GetFile per
// entry. All non-trivial backends (Memory, Badger, Postgres) must satisfy
// this contract.
func testListDirectoryHydratesACL(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "with-acl.txt", 0o600)

	ctx := t.Context()

	// Attach an ACL by reading + putting back the file with ACL set.
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.ACL = &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: acl.ACE4_READ_DATA,
				Who:        acl.SpecialOwner,
			},
		},
	}
	if err := store.UpdateAttrs(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs with ACL: %v", err)
	}

	entries, _, err := store.ListChildren(ctx, rootHandle, "", 100, metadata.WithAttrs)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}

	var found *metadata.DirEntry
	for i := range entries {
		if entries[i].Name == "with-acl.txt" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("with-acl.txt missing from listing")
	}
	if found.Attr == nil {
		t.Fatalf("DirEntry.Attr nil — backend must hydrate attributes on ListChildren")
	}
	if found.Attr.ACL == nil {
		t.Fatalf("DirEntry.Attr.ACL nil — backend must hydrate ACL on ListChildren so ABE can evaluate without per-entry GetFile")
	}
	if len(found.Attr.ACL.ACEs) != 1 {
		t.Fatalf("expected 1 ACE on hydrated ACL, got %d", len(found.Attr.ACL.ACEs))
	}
	got := found.Attr.ACL.ACEs[0]
	if got.Type != acl.ACE4_ACCESS_ALLOWED_ACE_TYPE || got.AccessMask != acl.ACE4_READ_DATA || got.Who != acl.SpecialOwner {
		t.Fatalf("hydrated ACE differs from stored: %+v", got)
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

// testRootDirectoryReconcilesAttrs pins what CreateRootDirectory does when the
// share already has a root: the configured attributes win, and the stored
// inode is rewritten to match.
//
// This is what makes re-attaching a share with changed root ownership or mode
// take effect. It is asserted through the transaction as well as through the
// store because the two are separate entry points — a backend can reconcile on
// one and return the stored root untouched on the other, and then whether an
// operator's config change lands depends on which call site reached it.
func testRootDirectoryReconcilesAttrs(t *testing.T, factory StoreFactory) {
	const (
		shareName = "/reconcile"
		wantMode  = 0o700
		wantUID   = 4242
		wantGID   = 4343
	)

	// check creates a root, re-creates it with different attributes through the
	// caller's entry point, and asserts the returned root AND the stored one: a
	// body that reports the new attributes without writing them would pass on
	// the returned root alone.
	check := func(t *testing.T, create func(context.Context, metadata.Store, *metadata.FileAttr) (*metadata.File, error)) {
		t.Helper()
		store := factory(t)
		ctx := t.Context()

		first := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755, UID: 1000, GID: 1000}
		if _, err := create(ctx, store, first); err != nil {
			t.Fatalf("first CreateRootDirectory() failed: %v", err)
		}

		// Read the root before reconciling so any per-inode cache holds the
		// pre-reconcile value. A backend that writes the new attrs but does not
		// drop that entry then keeps serving the old ones, which a cold read
		// after the write would not notice. The read has to go through the
		// read fast path where one exists, because that is where the cache is.
		warm, err := store.GetRootHandle(ctx, shareName)
		if err != nil {
			t.Fatalf("GetRootHandle() before reconcile failed: %v", err)
		}
		if _, err := readRootForCache(ctx, store, warm); err != nil {
			t.Fatalf("read of root before reconcile failed: %v", err)
		}

		changed := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: wantMode, UID: wantUID, GID: wantGID}
		got, err := create(ctx, store, changed)
		if err != nil {
			t.Fatalf("second CreateRootDirectory() failed: %v", err)
		}
		if got.Mode != wantMode || got.UID != wantUID || got.GID != wantGID {
			t.Errorf("returned root not reconciled: mode=%o uid=%d gid=%d, want mode=%o uid=%d gid=%d",
				got.Mode, got.UID, got.GID, wantMode, wantUID, wantGID)
		}

		handle, err := store.GetRootHandle(ctx, shareName)
		if err != nil {
			t.Fatalf("GetRootHandle() failed: %v", err)
		}
		stored, err := readRootForCache(ctx, store, handle)
		if err != nil {
			t.Fatalf("read of root failed: %v", err)
		}
		if stored.Mode != wantMode || stored.UID != wantUID || stored.GID != wantGID {
			t.Errorf("stored root not reconciled: mode=%o uid=%d gid=%d, want mode=%o uid=%d gid=%d",
				stored.Mode, stored.UID, stored.GID, wantMode, wantUID, wantGID)
		}
	}

	t.Run("StorePath", func(t *testing.T) {
		check(t, func(ctx context.Context, store metadata.Store, attr *metadata.FileAttr) (*metadata.File, error) {
			return store.CreateRootDirectory(ctx, shareName, attr)
		})
	})

	t.Run("TransactionPath", func(t *testing.T) {
		check(t, txCreateRoot(shareName))
	})

	// A zero mode means "use the default". Both entry points have to pick the
	// SAME default, so the create and the re-create go through DIFFERENT ones:
	// two self-consistent but different defaults would each rewrite what the
	// other wrote, and running one path twice could not tell.
	t.Run("ZeroModeIsIdempotentAcrossEntryPoints", func(t *testing.T) {
		storeCreate := func(ctx context.Context, store metadata.Store, attr *metadata.FileAttr) (*metadata.File, error) {
			return store.CreateRootDirectory(ctx, shareName, attr)
		}
		txCreate := txCreateRoot(shareName)

		for _, tc := range []struct {
			name          string
			first, second func(context.Context, metadata.Store, *metadata.FileAttr) (*metadata.File, error)
		}{
			{"StoreThenTransaction", storeCreate, txCreate},
			{"TransactionThenStore", txCreate, storeCreate},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store := factory(t)
				ctx := t.Context()

				zero := &metadata.FileAttr{Type: metadata.FileTypeDirectory}
				created, err := tc.first(ctx, store, zero)
				if err != nil {
					t.Fatalf("first CreateRootDirectory() failed: %v", err)
				}
				recreated, err := tc.second(ctx, store, zero)
				if err != nil {
					t.Fatalf("second CreateRootDirectory() failed: %v", err)
				}
				if created.Mode != recreated.Mode {
					t.Errorf("zero-mode root changed when re-created through the other entry point: %o then %o",
						created.Mode, recreated.Mode)
				}
			})
		}
	})

	// A reconcile is a write like any other, so a transaction that fails after
	// one must leave the root as it was.
	t.Run("ReconcileRollsBack", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		first := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755, UID: 1000, GID: 1000}
		if _, err := store.CreateRootDirectory(ctx, shareName, first); err != nil {
			t.Fatalf("CreateRootDirectory() failed: %v", err)
		}

		errRollback := errors.New("abandon the transaction")
		changed := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: wantMode, UID: wantUID, GID: wantGID}
		err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			if _, txErr := tx.CreateRootDirectory(ctx, shareName, changed); txErr != nil {
				return txErr
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("WithTransaction() error = %v, want %v", err, errRollback)
		}

		handle, err := store.GetRootHandle(ctx, shareName)
		if err != nil {
			t.Fatalf("GetRootHandle() failed: %v", err)
		}
		stored, err := store.GetFile(ctx, handle)
		if err != nil {
			t.Fatalf("GetFile(root) failed: %v", err)
		}
		if stored.Mode != first.Mode || stored.UID != first.UID || stored.GID != first.GID {
			t.Errorf("reconcile survived rollback: mode=%o uid=%d gid=%d, want mode=%o uid=%d gid=%d",
				stored.Mode, stored.UID, stored.GID, first.Mode, first.UID, first.GID)
		}
	})
}

// txCreateRoot returns a create func that runs CreateRootDirectory through a
// transaction, so a caller can drive both entry points from one body.
func txCreateRoot(shareName string) func(context.Context, metadata.Store, *metadata.FileAttr) (*metadata.File, error) {
	return func(ctx context.Context, store metadata.Store, attr *metadata.FileAttr) (*metadata.File, error) {
		var root *metadata.File
		err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			var txErr error
			root, txErr = tx.CreateRootDirectory(ctx, shareName, attr)
			return txErr
		})
		return root, err
	}
}

// testLinkCountAgreesWithGetFile verifies that GetLinkCount reports the same
// hard-link count as GetFile for an inode whose count was never explicitly
// stored. Backends keep the count outside the inode record (a separate key or
// a nullable column), so "never written" is a state both surfaces must resolve
// the same way: a GetLinkCount answering 0 while GetFile answers 2 makes
// read-modify-write callers persist a wrong count, and 0 reads as "fully
// unlinked" to anything treating the count as a liveness signal.
func testLinkCountAgreesWithGetFile(t *testing.T, factory StoreFactory) {
	cases := []struct {
		name  string
		ftype metadata.FileType
	}{
		{"Directory", metadata.FileTypeDirectory},
		{"RegularFile", metadata.FileTypeRegular},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := factory(t)
			rootHandle := createTestShare(t, store, "/test")
			ctx := t.Context()

			const name = "nolinkcount"
			fullPath := childFullPath(t, store, rootHandle, name)

			handle, err := store.GenerateHandle(ctx, "/test", fullPath)
			if err != nil {
				t.Fatalf("GenerateHandle() failed: %v", err)
			}
			_, id, err := metadata.DecodeFileHandle(handle)
			if err != nil {
				t.Fatalf("DecodeFileHandle() failed: %v", err)
			}

			// SetLinkCount is deliberately never called, which is what leaves
			// the count unwritten.
			entry := &metadata.File{
				ShareName: "/test",
				Path:      fullPath,
				FileAttr: metadata.FileAttr{
					Type: tc.ftype,
					Mode: 0755,
					UID:  1000,
					GID:  1000,
				},
			}
			entry.ID = id
			if err := store.UpdateAttrs(ctx, entry); err != nil {
				t.Fatalf("UpdateAttrs() failed: %v", err)
			}
			// Both namespace edges, so the only thing left unset is the link
			// count. Backends that derive File.Path from parent edges otherwise
			// see an entry that is reachable by name but has no parent.
			if err := store.SetParent(ctx, handle, rootHandle); err != nil {
				t.Fatalf("SetParent() failed: %v", err)
			}
			if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
				t.Fatalf("SetChild() failed: %v", err)
			}

			file, err := store.GetFile(ctx, handle)
			if err != nil {
				t.Fatalf("GetFile() failed: %v", err)
			}
			count, err := store.GetLinkCount(ctx, handle)
			if err != nil {
				t.Fatalf("GetLinkCount() failed: %v", err)
			}

			if count == 0 {
				t.Errorf("GetLinkCount() = 0 for a live %v; a live entry is never fully unlinked", tc.ftype)
			}
			if count != file.Nlink {
				t.Errorf("GetLinkCount() = %d, GetFile().Nlink = %d; both surfaces must agree", count, file.Nlink)
			}
		})
	}
}

// testDeleteChildIsIdempotent pins DeleteChild's contract: removing a name the
// directory does not hold succeeds and reports no error.
//
// Both halves matter and they fail differently. A backend that resolves the
// name before deleting fails "NeverExisted"; a backend that reports how many
// rows the delete matched fails "AlreadyRemoved", because the first call is
// what leaves the second with nothing to do. The transaction path is checked
// separately from the pool path — the two are distinct bodies in every backend
// that has not yet collapsed them, and only the transaction path is what a
// rename or an rmdir actually runs.
func testDeleteChildIsIdempotent(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	ctx := t.Context()

	if err := store.DeleteChild(ctx, rootHandle, "never-existed"); err != nil {
		t.Errorf("DeleteChild(absent name) = %v, want nil", err)
	}

	createTestDir(t, store, "/test", rootHandle, "victim")
	if err := store.DeleteChild(ctx, rootHandle, "victim"); err != nil {
		t.Fatalf("DeleteChild(present name) failed: %v", err)
	}
	if err := store.DeleteChild(ctx, rootHandle, "victim"); err != nil {
		t.Errorf("DeleteChild(already removed) = %v, want nil", err)
	}

	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		if txErr := tx.DeleteChild(ctx, rootHandle, "never-existed"); txErr != nil {
			t.Errorf("tx.DeleteChild(absent name) = %v, want nil", txErr)
		}
		if txErr := tx.DeleteChild(ctx, rootHandle, "victim"); txErr != nil {
			t.Errorf("tx.DeleteChild(already removed) = %v, want nil", txErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTransaction() failed: %v", err)
	}

	// The directory still has to be usable afterwards: a delete that found
	// nothing must not have torn down state the next create depends on.
	createTestDir(t, store, "/test", rootHandle, "after")
	entries, _, err := store.ListChildren(ctx, rootHandle, "", 0, metadata.WithAttrs)
	if err != nil {
		t.Fatalf("ListChildren() failed: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "after" {
		t.Errorf("children after idempotent deletes = %v, want [after]", names)
	}
}

// testNamesOnlyMatchesWithAttrs pins the one guarantee that makes NamesOnly
// safe to substitute at a call site: the two modes differ in Attr and in
// nothing else. Same entries, same order, same ids and handles, same cursor.
//
// Both modes are read twice over the same directory — once whole, once paged —
// because the SQL backends answer NamesOnly with a different query than
// WithAttrs, and a page boundary is where two queries that were meant to agree
// stop agreeing. A cursor handed out by one mode has to resume the other in
// the same place or a caller that switched modes would silently skip or repeat
// a name.
func testNamesOnlyMatchesWithAttrs(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	const total = 5
	for i := range total {
		createTestFile(t, store, "/test", rootHandle, fmt.Sprintf("file%d.txt", i), 0644)
	}

	ctx := t.Context()

	withAttrs, withCursor, err := store.ListChildren(ctx, rootHandle, "", 100, metadata.WithAttrs)
	if err != nil {
		t.Fatalf("ListChildren(WithAttrs) failed: %v", err)
	}
	namesOnly, namesCursor, err := store.ListChildren(ctx, rootHandle, "", 100, metadata.NamesOnly)
	if err != nil {
		t.Fatalf("ListChildren(NamesOnly) failed: %v", err)
	}

	if len(namesOnly) != len(withAttrs) {
		t.Fatalf("NamesOnly returned %d entries, WithAttrs returned %d", len(namesOnly), len(withAttrs))
	}
	if namesCursor != withCursor {
		t.Errorf("nextCursor = %q under NamesOnly, %q under WithAttrs", namesCursor, withCursor)
	}

	for i := range withAttrs {
		want, got := withAttrs[i], namesOnly[i]
		if got.Name != want.Name {
			t.Errorf("entry %d: name = %q under NamesOnly, %q under WithAttrs", i, got.Name, want.Name)
		}
		if got.ID != want.ID {
			t.Errorf("entry %d (%s): ID = %d under NamesOnly, %d under WithAttrs", i, want.Name, got.ID, want.ID)
		}
		if !bytes.Equal(got.Handle, want.Handle) {
			t.Errorf("entry %d (%s): handle differs between modes", i, want.Name)
		}
		if want.Attr == nil {
			t.Errorf("entry %d (%s): WithAttrs left Attr nil", i, want.Name)
		}
		if got.Attr != nil {
			t.Errorf("entry %d (%s): NamesOnly populated Attr; callers are told it is nil and may not pay for it",
				i, want.Name)
		}
	}

	// Page both modes two at a time, following each mode's own cursor.
	pageNames := func(attrs metadata.ChildAttrs) []string {
		t.Helper()
		var names []string
		cursor := ""
		for range total + 1 {
			page, next, err := store.ListChildren(ctx, rootHandle, cursor, 2, attrs)
			if err != nil {
				t.Fatalf("ListChildren(cursor=%q, %v) failed: %v", cursor, attrs, err)
			}
			for _, e := range page {
				names = append(names, e.Name)
			}
			if next == "" {
				break
			}
			cursor = next
		}
		return names
	}

	paged, pagedNamesOnly := pageNames(metadata.WithAttrs), pageNames(metadata.NamesOnly)
	if !slices.Equal(paged, pagedNamesOnly) {
		t.Errorf("paging disagrees between modes:\n WithAttrs: %v\n NamesOnly: %v", paged, pagedNamesOnly)
	}
	if len(paged) != total {
		t.Errorf("paging WithAttrs yielded %d names, want %d: %v", len(paged), total, paged)
	}

	// Resume each mode from the other's cursor. Paging each mode against
	// itself would still pass if the two modes agreed internally and only
	// with each other's cursors disagreed, which is the case a caller hits
	// the moment it switches modes mid-listing.
	crossed := func(first, second metadata.ChildAttrs) []string {
		t.Helper()
		head, cursor, err := store.ListChildren(ctx, rootHandle, "", 2, first)
		if err != nil {
			t.Fatalf("ListChildren(%v) failed: %v", first, err)
		}
		if cursor == "" {
			t.Fatalf("ListChildren(%v, limit 2) over %d entries returned no cursor", first, total)
		}
		tail, _, err := store.ListChildren(ctx, rootHandle, cursor, 100, second)
		if err != nil {
			t.Fatalf("ListChildren(%v, cursor from %v) failed: %v", second, first, err)
		}
		var names []string
		for _, e := range head {
			names = append(names, e.Name)
		}
		for _, e := range tail {
			names = append(names, e.Name)
		}
		return names
	}

	for _, c := range []struct {
		first, second metadata.ChildAttrs
	}{{metadata.WithAttrs, metadata.NamesOnly}, {metadata.NamesOnly, metadata.WithAttrs}} {
		if got := crossed(c.first, c.second); !slices.Equal(got, paged) {
			t.Errorf("resuming %v from a %v cursor yielded %v, want %v", c.second, c.first, got, paged)
		}
	}
}

// readRootForCache reads through the backend's cached path when it has one,
// falling back to GetFile when it does not.
func readRootForCache(ctx context.Context, store metadata.Store, handle metadata.FileHandle) (*metadata.File, error) {
	if fr, ok := store.(fileForReadStore); ok {
		return fr.GetFileForRead(ctx, handle)
	}
	return store.GetFile(ctx, handle)
}
