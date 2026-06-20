// Refs #532. Per-entry filter exercised by the SMB QUERY_DIRECTORY handler
// when the connected share has Windows access-based enumeration enabled
// (MS-SRVS SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM).
package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

func uint32Ptr(v uint32) *uint32 { return &v }

// abeChecker returns a metadata.FileAccessChecker for the access-based
// enumeration filter under test. CheckAttrReadAccess evaluates purely from the
// passed FileAttr + AuthContext and touches no store, so a bare service
// instance is sufficient.
func abeChecker() metadata.FileAccessChecker { return metadata.New() }

// authForUID builds an AuthContext for the given UID with a single primary GID.
// Context is wired to context.Background() so future hydrators that dereference
// authCtx.Context don't panic.
func authForUID(uid, gid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID:  uint32Ptr(uid),
			GID:  uint32Ptr(gid),
			GIDs: []uint32{gid},
		},
	}
}

// regularFileWithACL constructs a DirEntry for a regular file with the given
// owner/group and ACL.
func regularFileWithACL(name string, uid, gid uint32, mode uint32, a *acl.ACL) metadata.DirEntry {
	return metadata.DirEntry{
		Name: name,
		Attr: &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: mode,
			UID:  uid,
			GID:  gid,
			ACL:  a,
		},
	}
}

// directoryWithACL constructs a DirEntry for a directory.
func directoryWithACL(name string, uid, gid uint32, mode uint32, a *acl.ACL) metadata.DirEntry {
	return metadata.DirEntry{
		Name: name,
		Attr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: mode,
			UID:  uid,
			GID:  gid,
			ACL:  a,
		},
	}
}

// fullReadMask is the access-mask Samba (source3/smbd/dir.c::user_can_read_fsp)
// requires for ABE visibility: READ_DATA | READ_EA | READ_ATTRIBUTES |
// READ_CONTROL. Tests grant this exact mask whenever they want the entry to
// stay visible under the filter.
const fullReadMask = acl.ACE4_READ_DATA |
	acl.ACE4_READ_NAMED_ATTRS |
	acl.ACE4_READ_ATTRIBUTES |
	acl.ACE4_READ_ACL

// TestFilterByAccess_OwnerOnlyACL verifies that an ACL granting the full ABE
// read mask only to OWNER@ keeps the file visible to the owner and hides it
// from a non-owner.
func TestFilterByAccess_OwnerOnlyACL(t *testing.T) {
	ownerOnly := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullReadMask, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		regularFileWithACL("secret.txt", 1000, 1000, 0o000, ownerOnly),
		regularFileWithACL("public.txt", 1000, 1000, 0o000, &acl.ACL{
			ACEs: []acl.ACE{
				{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullReadMask, Who: acl.SpecialEveryone},
			},
		}),
	}

	owner := authForUID(1000, 1000)
	// Make a fresh copy each call since filterByAccess mutates in place.
	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), owner, nil)
	if len(got) != 2 {
		t.Fatalf("owner: want 2 entries, got %d (%v)", len(got), got)
	}

	other := authForUID(2000, 2000)
	got = filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), other, nil)
	if len(got) != 1 || got[0].Name != "public.txt" {
		t.Fatalf("non-owner: want only public.txt, got %v", got)
	}
}

// TestFilterByAccess_PartialReadMaskHidesFromOwner exercises the
// smb2.acls.ACCESSBASED iterations 2-4: an ACL granting the file owner some
// but not all of {READ_DATA, READ_EA, READ_ATTRIBUTES, READ_CONTROL} must hide
// the file from a directory listing — even though the requester IS the owner
// (the test enumerates as the same user that set the SD). Mirrors Samba
// source3/smbd/dir.c::user_can_read_fsp which requires the full mask.
func TestFilterByAccess_PartialReadMaskHidesFromOwner(t *testing.T) {
	cases := []struct {
		name string
		mask uint32
	}{
		{"missing_read_ea", acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL},
		{"missing_read_attributes", acl.ACE4_READ_DATA | acl.ACE4_READ_NAMED_ATTRS | acl.ACE4_READ_ACL},
		{"missing_read_data", acl.ACE4_READ_NAMED_ATTRS | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL},
	}
	owner := authForUID(1000, 1000)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &acl.ACL{
				ACEs: []acl.ACE{
					{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: tc.mask, Who: acl.SpecialOwner},
				},
			}
			entries := []metadata.DirEntry{regularFileWithACL("smb2-testsd", 1000, 1000, 0o000, a)}
			got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), owner, nil)
			if len(got) != 0 {
				t.Fatalf("owner with partial mask %#x: want hidden, got %d entries", tc.mask, len(got))
			}
		})
	}
}

// TestFilterByAccess_DirectoryListBit verifies that directories use the same
// 0x1 bit (ACE4_LIST_DIRECTORY) as files (ACE4_READ_DATA). The owner ACE
// carries the full ABE read mask so the owner remains visible.
func TestFilterByAccess_DirectoryListBit(t *testing.T) {
	denyOther := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullReadMask, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		directoryWithACL("private", 1000, 1000, 0o000, denyOther),
	}

	other := authForUID(2000, 2000)
	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), other, nil)
	if len(got) != 0 {
		t.Fatalf("non-owner directory: want hidden, got %v", got)
	}

	owner := authForUID(1000, 1000)
	got = filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), owner, nil)
	if len(got) != 1 {
		t.Fatalf("owner directory: want 1 entry, got %d", len(got))
	}
}

// TestFilterByAccess_POSIXFallback covers the path where file.ACL is nil and
// we fall back to mode bits, matching security.go::buildDACL's contract that
// server-side decisions keep POSIX semantics for nil ACL (refs #525).
func TestFilterByAccess_POSIXFallback(t *testing.T) {
	// owner=1000 group=1000, mode rw------- (0o600): owner read, no group/other.
	entries := []metadata.DirEntry{
		regularFileWithACL("owner-only.txt", 1000, 1000, 0o600, nil),
		regularFileWithACL("world.txt", 1000, 1000, 0o644, nil),
		regularFileWithACL("group.txt", 1000, 1000, 0o640, nil),
	}

	owner := authForUID(1000, 1000)
	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), owner, nil)
	if len(got) != 3 {
		t.Fatalf("owner: want all 3 entries, got %d (%v)", len(got), got)
	}

	groupMember := authForUID(2000, 1000) // not owner, in group
	got = filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), groupMember, nil)
	if len(got) != 2 {
		t.Fatalf("group member: want 2 entries (world.txt + group.txt), got %d (%v)", len(got), got)
	}

	stranger := authForUID(3000, 3000) // neither owner nor in group
	got = filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), stranger, nil)
	if len(got) != 1 || got[0].Name != "world.txt" {
		t.Fatalf("stranger: want only world.txt, got %v", got)
	}
}

// TestFilterByAccess_RootBypass verifies UID 0 sees everything regardless of
// per-file DACL or mode bits.
func TestFilterByAccess_RootBypass(t *testing.T) {
	denyEveryone := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialEveryone},
		},
	}
	entries := []metadata.DirEntry{
		regularFileWithACL("forbidden.txt", 1000, 1000, 0o000, denyEveryone),
		regularFileWithACL("mode-zero.txt", 1000, 1000, 0o000, nil),
	}

	root := authForUID(0, 0)
	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), root, nil)
	if len(got) != 2 {
		t.Fatalf("root: want all 2 entries, got %d (%v)", len(got), got)
	}
}

// TestFilterByAccess_HidesNonReadable exercises the pure filter on a
// non-owner identity against an owner-only ACL. The handler-side gate
// (whether the filter is called at all) is covered end-to-end by
// TestQueryDirectory_ABE in query_directory_test.go.
func TestFilterByAccess_HidesNonReadable(t *testing.T) {
	ownerOnly := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullReadMask, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		regularFileWithACL("secret.txt", 1000, 1000, 0o000, ownerOnly),
	}
	got := filterByAccess(abeChecker(), entries, authForUID(2000, 2000), nil)
	if len(got) != 0 {
		t.Fatalf("want 0 entries, got %d", len(got))
	}
}

// TestFilterByAccess_NilAttrFailsClosed verifies that an entry with no
// hydrated attributes is hidden under ABE when no hydrator is supplied.
// DirEntry.Attr is documented optional on the metadata layer
// (pkg/metadata/validation.go) and some backends (e.g. Badger) treat the
// hydration as best-effort, so this branch IS reachable. Returning the
// entry would leak files ABE is meant to suppress (refs #532 review).
func TestFilterByAccess_NilAttrFailsClosed(t *testing.T) {
	entries := []metadata.DirEntry{
		{Name: "noattr.txt", Attr: nil},
	}

	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), authForUID(2000, 2000), nil)
	if len(got) != 0 {
		t.Fatalf("nil-attr no-hydrator: want 0 entries (hidden), got %d (%v)", len(got), got)
	}
}

// TestFilterByAccess_NilAttrHydratorMisses verifies that a hydrator that
// returns nil keeps the fail-closed behaviour. Mirrors the case where
// GetFile races with a concurrent delete.
func TestFilterByAccess_NilAttrHydratorMisses(t *testing.T) {
	entries := []metadata.DirEntry{
		{Name: "noattr.txt", Attr: nil},
	}

	miss := func(_ *metadata.DirEntry) *metadata.FileAttr { return nil }
	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), authForUID(2000, 2000), miss)
	if len(got) != 0 {
		t.Fatalf("nil-attr hydrator miss: want 0 entries, got %d", len(got))
	}
}

// TestFilterByAccess_NilAttrHydratorHits verifies that a hydrator that
// returns FileAttr restores the normal ACL/POSIX decision path. Without
// the hydrator the entry would have been hidden; with it the caller sees
// the file iff they can read it.
func TestFilterByAccess_NilAttrHydratorHits(t *testing.T) {
	ownerOnly := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: fullReadMask, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		{Name: "lazy.txt", Attr: nil},
	}

	hydrate := func(_ *metadata.DirEntry) *metadata.FileAttr {
		return &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o600,
			UID:  1000,
			GID:  1000,
			ACL:  ownerOnly,
		}
	}

	// Owner with hydration: visible.
	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), authForUID(1000, 1000), hydrate)
	if len(got) != 1 {
		t.Fatalf("owner via hydrate: want 1 entry, got %d (%v)", len(got), got)
	}

	// Non-owner with hydration: hidden by ACL.
	got = filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), authForUID(2000, 2000), hydrate)
	if len(got) != 0 {
		t.Fatalf("non-owner via hydrate: want 0 entries, got %d (%v)", len(got), got)
	}
}

// TestFilterByAccess_NilAttrRootBypass verifies that the UID 0 fast-path
// bypasses the hydrator entirely — root sees orphaned entries as well.
func TestFilterByAccess_NilAttrRootBypass(t *testing.T) {
	entries := []metadata.DirEntry{
		{Name: "noattr.txt", Attr: nil},
	}

	hydratorCalled := false
	hydrate := func(_ *metadata.DirEntry) *metadata.FileAttr {
		hydratorCalled = true
		return nil
	}

	got := filterByAccess(abeChecker(), append([]metadata.DirEntry(nil), entries...), authForUID(0, 0), hydrate)
	if len(got) != 1 {
		t.Fatalf("root: want 1 entry (bypass), got %d", len(got))
	}
	if hydratorCalled {
		t.Fatalf("root bypass should skip hydrator")
	}
}
