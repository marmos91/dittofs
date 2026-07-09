package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// fakeDirectorySIDBridge is a deterministic in-memory DirectorySIDBridge for
// exercising the #1617 emit/parse paths. It maps a fixed uid/gid to a fixed AD
// SID string and back; everything else misses (ok=false).
type fakeDirectorySIDBridge struct {
	uid    uint32
	uidSID string
	gid    uint32
	gidSID string
}

func (f fakeDirectorySIDBridge) SIDForUID(uid uint32) (string, bool) {
	if uid == f.uid {
		return f.uidSID, true
	}
	return "", false
}

func (f fakeDirectorySIDBridge) SIDForGID(gid uint32) (string, bool) {
	if gid == f.gid {
		return f.gidSID, true
	}
	return "", false
}

func (f fakeDirectorySIDBridge) UIDForSID(sidStr string) (uint32, bool) {
	if sidStr == f.uidSID {
		return f.uid, true
	}
	return 0, false
}

func (f fakeDirectorySIDBridge) GIDForSID(sidStr string) (uint32, bool) {
	if sidStr == f.gidSID {
		return f.gid, true
	}
	return 0, false
}

// withDirectoryBridge installs b for the duration of the test and restores the
// prior (usually nil) bridge afterward, so tests do not leak the hook.
func withDirectoryBridge(t *testing.T, b DirectorySIDBridge) {
	t.Helper()
	prev := directoryBridge()
	SetDirectorySIDBridge(b)
	t.Cleanup(func() { SetDirectorySIDBridge(prev) })
}

// A file owned by an AD account is emitted with that account's real directory
// SID for the Owner and Group SD sections (#1617), not the algorithmic
// machine-domain SID.
func TestBuildSD_DirectoryBridge_EmitsADOwnerGroupSID(t *testing.T) {
	const (
		adUID    = 500
		adGID    = 512
		ownerSID = "S-1-5-21-100-200-300-500"
		groupSID = "S-1-5-21-100-200-300-512"
	)
	withDirectoryBridge(t, fakeDirectorySIDBridge{
		uid: adUID, uidSID: ownerSID, gid: adGID, gidSID: groupSID,
	})

	file := &metadata.File{FileAttr: metadata.FileAttr{UID: adUID, GID: adGID, Mode: 0o644}}

	data, err := BuildSecurityDescriptor(file, allSecInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	gotOwner, gotGroup, hasOwner, hasGroup := securityDescriptorOwnerGroupSIDs(data)
	if !hasOwner || !hasGroup {
		t.Fatalf("owner/group sections missing: hasOwner=%v hasGroup=%v", hasOwner, hasGroup)
	}
	if got := sid.FormatSID(gotOwner); got != ownerSID {
		t.Errorf("owner SID = %q, want %q", got, ownerSID)
	}
	if got := sid.FormatSID(gotGroup); got != groupSID {
		t.Errorf("group SID = %q, want %q", got, groupSID)
	}
}

// When the bridge does not map the uid/gid (a local account, or no AD provider),
// the descriptor falls back to the machine-domain SIDs — byte-identical to the
// pre-#1617 output. This guards the transparent no-op invariant.
func TestBuildSD_DirectoryBridge_MissFallsBackToMachineSID(t *testing.T) {
	file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o644}}

	baseline, err := BuildSecurityDescriptor(file, allSecInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor(baseline): %v", err)
	}

	// Bridge that only knows uid/gid 500/512 — 1000 misses.
	withDirectoryBridge(t, fakeDirectorySIDBridge{
		uid: 500, uidSID: "S-1-5-21-1-2-3-500", gid: 512, gidSID: "S-1-5-21-1-2-3-512",
	})

	withBridge, err := BuildSecurityDescriptor(file, allSecInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor(withBridge): %v", err)
	}
	if !bytes.Equal(baseline, withBridge) {
		t.Fatal("bridge miss changed the descriptor; expected byte-identical machine-SID fallback")
	}
}

// An owner emitted as an AD SID round-trips: parsing the built descriptor
// recovers the same UID/GID via the bridge, even though the machine mapper does
// not recognize the AD SID. Without this the owner would look foreign and trip
// the SET_INFO unmappable-owner gate.
func TestParseSD_DirectoryBridge_ADOwnerRoundTrips(t *testing.T) {
	const (
		adUID    = 500
		adGID    = 512
		ownerSID = "S-1-5-21-100-200-300-500"
		groupSID = "S-1-5-21-100-200-300-512"
	)
	withDirectoryBridge(t, fakeDirectorySIDBridge{
		uid: adUID, uidSID: ownerSID, gid: adGID, gidSID: groupSID,
	})

	file := &metadata.File{FileAttr: metadata.FileAttr{UID: adUID, GID: adGID, Mode: 0o644}}
	data, err := BuildSecurityDescriptor(file, allSecInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	gotUID, gotGID, _, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if gotUID == nil || *gotUID != adUID {
		t.Errorf("parsed ownerUID = %v, want %d", gotUID, adUID)
	}
	if gotGID == nil || *gotGID != adGID {
		t.Errorf("parsed ownerGID = %v, want %d", gotGID, adGID)
	}
}

// The OWNER@/GROUP@ special ACEs in the DACL also carry the AD SID when the
// bridge maps the file's owner/group (#1617), matching the Owner/Group SD
// sections so Windows shows one coherent principal.
func TestBuildSD_DirectoryBridge_SpecialOwnerACEUsesADSID(t *testing.T) {
	const (
		adUID    = 500
		ownerSID = "S-1-5-21-100-200-300-500"
	)
	withDirectoryBridge(t, fakeDirectorySIDBridge{
		uid: adUID, uidSID: ownerSID, gid: 999, gidSID: "S-1-5-21-100-200-300-999",
	})

	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID: adUID, GID: adUID, Mode: 0o644,
			ACL: &acl.ACL{ACEs: []acl.ACE{{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: 0x001F01FF,
				Who:        acl.SpecialOwner,
			}}},
		},
	}

	got := principalToSID(acl.SpecialOwner, file.UID, file.GID)
	if s := sid.FormatSID(got); s != ownerSID {
		t.Errorf("OWNER@ SID = %q, want %q", s, ownerSID)
	}
}

// A SET_INFO that re-sends the file's CURRENT owner/group AD SID (Windows does
// this on any DACL edit) must be recognized as a no-op by isCurrentOwnerSID /
// isCurrentGroupSID — independent of whether the reverse directory lookup is up
// — so the #1228 foreign-domain gate does not reject it (#1617 finding #2).
func TestSetInfo_DirectoryBridge_CurrentOwnerGroupSIDIsNoOp(t *testing.T) {
	const (
		adUID    = 500
		adGID    = 512
		ownerSID = "S-1-5-21-100-200-300-500"
		groupSID = "S-1-5-21-100-200-300-512"
	)
	withDirectoryBridge(t, fakeDirectorySIDBridge{
		uid: adUID, uidSID: ownerSID, gid: adGID, gidSID: groupSID,
	})

	reqOwner, err := sid.ParseSIDString(ownerSID)
	if err != nil {
		t.Fatalf("ParseSIDString(owner): %v", err)
	}
	reqGroup, err := sid.ParseSIDString(groupSID)
	if err != nil {
		t.Fatalf("ParseSIDString(group): %v", err)
	}

	if !isCurrentOwnerSID(reqOwner, adUID) {
		t.Error("re-set of the file's current AD owner SID must be a no-op")
	}
	if !isCurrentGroupSID(reqGroup, adGID) {
		t.Error("re-set of the file's current AD group SID must be a no-op")
	}

	// A different SID is a genuine change, not a no-op.
	other, err := sid.ParseSIDString("S-1-5-21-100-200-300-999")
	if err != nil {
		t.Fatalf("ParseSIDString(other): %v", err)
	}
	if isCurrentOwnerSID(other, adUID) {
		t.Error("a different SID must not be treated as the current owner")
	}
}
