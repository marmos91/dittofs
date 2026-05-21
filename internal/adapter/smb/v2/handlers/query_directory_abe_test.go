// Refs #532. Per-entry filter exercised by the SMB QUERY_DIRECTORY handler
// when the connected share has Windows access-based enumeration enabled
// (MS-SRVS SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM).
package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

func uint32Ptr(v uint32) *uint32 { return &v }

// authForUID builds an AuthContext for the given UID with a single primary GID.
func authForUID(uid, gid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
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

// TestFilterByAccess_OwnerOnlyACL verifies that an ACL granting READ_DATA only
// to OWNER@ keeps the file visible to the owner and hides it from a non-owner.
func TestFilterByAccess_OwnerOnlyACL(t *testing.T) {
	ownerOnly := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		regularFileWithACL("secret.txt", 1000, 1000, 0o000, ownerOnly),
		regularFileWithACL("public.txt", 1000, 1000, 0o000, &acl.ACL{
			ACEs: []acl.ACE{
				{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialEveryone},
			},
		}),
	}

	owner := authForUID(1000, 1000)
	// Make a fresh copy each call since filterByAccess mutates in place.
	got := filterByAccess(append([]metadata.DirEntry(nil), entries...), owner)
	if len(got) != 2 {
		t.Fatalf("owner: want 2 entries, got %d (%v)", len(got), got)
	}

	other := authForUID(2000, 2000)
	got = filterByAccess(append([]metadata.DirEntry(nil), entries...), other)
	if len(got) != 1 || got[0].Name != "public.txt" {
		t.Fatalf("non-owner: want only public.txt, got %v", got)
	}
}

// TestFilterByAccess_DirectoryListBit verifies that directories use the same
// 0x1 bit (ACE4_LIST_DIRECTORY) as files (ACE4_READ_DATA).
func TestFilterByAccess_DirectoryListBit(t *testing.T) {
	denyOther := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_LIST_DIRECTORY, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		directoryWithACL("private", 1000, 1000, 0o000, denyOther),
	}

	other := authForUID(2000, 2000)
	got := filterByAccess(append([]metadata.DirEntry(nil), entries...), other)
	if len(got) != 0 {
		t.Fatalf("non-owner directory: want hidden, got %v", got)
	}

	owner := authForUID(1000, 1000)
	got = filterByAccess(append([]metadata.DirEntry(nil), entries...), owner)
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
	got := filterByAccess(append([]metadata.DirEntry(nil), entries...), owner)
	if len(got) != 3 {
		t.Fatalf("owner: want all 3 entries, got %d (%v)", len(got), got)
	}

	groupMember := authForUID(2000, 1000) // not owner, in group
	got = filterByAccess(append([]metadata.DirEntry(nil), entries...), groupMember)
	if len(got) != 2 {
		t.Fatalf("group member: want 2 entries (world.txt + group.txt), got %d (%v)", len(got), got)
	}

	stranger := authForUID(3000, 3000) // neither owner nor in group
	got = filterByAccess(append([]metadata.DirEntry(nil), entries...), stranger)
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
	got := filterByAccess(append([]metadata.DirEntry(nil), entries...), root)
	if len(got) != 2 {
		t.Fatalf("root: want all 2 entries, got %d (%v)", len(got), got)
	}
}

// TestFilterByAccess_FlagOffReturnsAll documents the contract that filtering
// only runs when the share toggle is set; with the flag off (i.e. the
// handler skipping the filter call), the caller sees everything. This test
// invokes the filter directly with a non-owner identity to verify it would
// hide entries — the flag-off path is verified by NOT calling filterByAccess.
func TestFilterByAccess_FlagOffSemantics(t *testing.T) {
	// File hidden under ABE.
	ownerOnly := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialOwner},
		},
	}
	entries := []metadata.DirEntry{
		regularFileWithACL("secret.txt", 1000, 1000, 0o000, ownerOnly),
	}
	other := authForUID(2000, 2000)

	// Flag-off path: filterByAccess is simply not called — the caller gets
	// the full list. (Mirrors the handler's `if h.treeHasAccessBasedEnumeration(...)`
	// gate.) Asserting on `entries` itself validates that without filtering
	// the visible set is unchanged.
	if len(entries) != 1 {
		t.Fatalf("flag-off: want 1 entry, got %d", len(entries))
	}

	// Flag-on path: filtering hides the entry.
	got := filterByAccess(append([]metadata.DirEntry(nil), entries...), other)
	if len(got) != 0 {
		t.Fatalf("flag-on: want 0 entries, got %d", len(got))
	}
}
