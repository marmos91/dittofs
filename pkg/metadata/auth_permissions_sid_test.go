package metadata

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

func TestCalculatePermissions_SIDDenyACE(t *testing.T) {
	// File owned by uid 1001 with mode 0o777 (anyone-writable in POSIX terms).
	// SD denies WriteData for the requester's SID, then allows everything for
	// EVERYONE. SID-form deny ACE must win — POSIX bits cannot express it.
	uid := uint32(1001)
	denyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        "sid:S-1-5-21-1-2-3-2001",
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	file := &File{
		FileAttr: FileAttr{UID: 1001, GID: 1001, Mode: 0o777, ACL: denyACL},
	}
	requesterSID := "S-1-5-21-1-2-3-2001"
	id := &Identity{
		UID: &uid,
		SID: &requesterSID,
	}

	got := calculatePermissions(file, id, nil, PermissionWrite)
	if got&PermissionWrite != 0 {
		t.Fatalf("expected write denied via SID-form deny ACE, got 0x%x", got)
	}
}

func TestCalculatePermissions_DOSReadonlyDeniesOwnerWriteCrossProtocol(t *testing.T) {
	// SMB SET_INFO set FILE_ATTRIBUTE_READONLY: persists as modeDOSReadonly
	// (0x100000); POSIX bits stay 0o644. modeDOSExplicit is masked off by
	// ApplyModeDefault, so enforcement must gate on modeDOSReadonly alone.
	// An NFS owner write must be denied even though (mode>>6)&0x7 has write.
	uid := uint32(1001)
	file := &File{
		FileAttr: FileAttr{
			UID:  1001,
			GID:  1001,
			Mode: 0o644 | 0x100000,
		},
	}
	id := &Identity{UID: &uid}

	got := calculatePermissions(file, id, nil, PermissionWrite|PermissionRead)
	if got&PermissionWrite != 0 {
		t.Fatalf("READONLY: owner write should be denied; got 0x%x", got)
	}
	if got&PermissionRead == 0 {
		t.Fatalf("READONLY: owner read must still be granted; got 0x%x", got)
	}

	// Sanity: clearing modeDOSReadonly restores write (even with modeDOSExplicit set).
	file.Mode = 0o644 | 0x10000
	got = calculatePermissions(file, id, nil, PermissionWrite)
	if got&PermissionWrite == 0 {
		t.Fatalf("non-READONLY: owner write should be granted; got 0x%x", got)
	}
}
