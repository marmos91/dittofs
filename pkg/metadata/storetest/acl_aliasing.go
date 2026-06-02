package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// runACLAliasingTests asserts that backends deep-copy FileAttr.ACL on both the
// store-from-caller (PutFile) and return-to-caller (GetFile) paths, so neither
// side can corrupt the other's view by mutating an ACE in place.
//
// This pins the cross-backend parity gap the area-6 audit found: the memory
// backend used to copy only FileAttr.Blocks and left FileAttr.ACL (a *acl.ACL
// holding an []ACE) shared by pointer, while Badger/Postgres round-trip through
// JSON and never alias. Identical inputs, divergent aliasing — an in-place ACE
// edit silently corrupted stored permissions on memory only.
func runACLAliasingTests(t *testing.T, factory StoreFactory) {
	t.Run("PutDoesNotAliasCallerACL", func(t *testing.T) { testPutDoesNotAliasCallerACL(t, factory) })
	t.Run("GetDoesNotAliasStoredACL", func(t *testing.T) { testGetDoesNotAliasStoredACL(t, factory) })
}

// putFileWithACL creates a regular file carrying the supplied ACL and returns
// its handle. The caller retains ownership of acl (it is set on the File passed
// to PutFile) so tests can mutate it afterwards to probe for aliasing.
func putFileWithACL(t *testing.T, store metadata.Store, handle metadata.FileHandle, a *acl.ACL) {
	t.Helper()

	ctx := t.Context()
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.ACL = a
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile with ACL: %v", err)
	}
}

// newTestACL builds a single-ACE ALLOW ACL granting READ_DATA to the owner.
func newTestACL() *acl.ACL {
	return &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: acl.ACE4_READ_DATA,
				Who:        acl.SpecialOwner,
			},
		},
	}
}

// testPutDoesNotAliasCallerACL: PutFile a file with a non-empty ACL, then mutate
// the caller's ACE slice in place; re-GetFile must return the ORIGINAL ACL.
// If the store aliased the caller's slice, the in-place edit would leak into
// the stored row.
func testPutDoesNotAliasCallerACL(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", rootHandle, "put-alias.txt", 0o600)

	callerACL := newTestACL()
	putFileWithACL(t, store, handle, callerACL)

	// Mutate the caller's ACL in place AFTER the store accepted it.
	callerACL.ACEs[0].AccessMask = acl.ACE4_WRITE_DATA
	callerACL.ACEs[0].Who = "evil@localdomain"
	callerACL.Protected = true

	got, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got.ACL == nil {
		t.Fatalf("stored ACL is nil after PutFile")
	}
	if len(got.ACL.ACEs) != 1 {
		t.Fatalf("expected 1 stored ACE, got %d", len(got.ACL.ACEs))
	}
	ace := got.ACL.ACEs[0]
	if ace.AccessMask != acl.ACE4_READ_DATA || ace.Who != acl.SpecialOwner || got.ACL.Protected {
		t.Fatalf("store aliased caller's ACL — a caller-side mutation corrupted the stored row: "+
			"got mask=%#x who=%q protected=%v, want mask=%#x who=%q protected=false",
			ace.AccessMask, ace.Who, got.ACL.Protected, acl.ACE4_READ_DATA, acl.SpecialOwner)
	}
}

// testGetDoesNotAliasStoredACL: GetFile, mutate the returned ACE slice in place,
// then re-GetFile; the second read must return the ORIGINAL ACL. If the store
// handed back its own backing slice, the caller's edit would corrupt it.
func testGetDoesNotAliasStoredACL(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", rootHandle, "get-alias.txt", 0o600)

	putFileWithACL(t, store, handle, newTestACL())

	first, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile (first): %v", err)
	}
	if first.ACL == nil || len(first.ACL.ACEs) != 1 {
		t.Fatalf("first GetFile returned unexpected ACL: %+v", first.ACL)
	}

	// Mutate the returned ACL in place.
	first.ACL.ACEs[0].AccessMask = acl.ACE4_WRITE_DATA
	first.ACL.ACEs[0].Who = "evil@localdomain"
	first.ACL.Protected = true

	second, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile (second): %v", err)
	}
	if second.ACL == nil || len(second.ACL.ACEs) != 1 {
		t.Fatalf("second GetFile returned unexpected ACL: %+v", second.ACL)
	}
	ace := second.ACL.ACEs[0]
	if ace.AccessMask != acl.ACE4_READ_DATA || ace.Who != acl.SpecialOwner || second.ACL.Protected {
		t.Fatalf("store aliased the returned ACL — a caller-side mutation corrupted the store: "+
			"got mask=%#x who=%q protected=%v, want mask=%#x who=%q protected=false",
			ace.AccessMask, ace.Who, second.ACL.Protected, acl.ACE4_READ_DATA, acl.SpecialOwner)
	}
}
