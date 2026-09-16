package identity

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// fakeProvider serves a single canned ShareInfo (or an error) for the share
// named in want. Any other share name yields a not-found error so the
// "share missing" branch can be exercised.
type fakeProvider struct {
	want string
	info *ShareInfo
	err  error
}

func (f fakeProvider) GetShareIdentityInfo(shareName string) (*ShareInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	if shareName != f.want {
		return nil, errors.New("no such share")
	}
	return f.info, nil
}

func u32(v uint32) *uint32 { return &v }

const (
	anonUID = uint32(65534)
	anonGID = uint32(65533)
)

func anonInfo(mode models.SquashMode) *ShareInfo {
	return &ShareInfo{Squash: mode, AnonymousUID: anonUID, AnonymousGID: anonGID}
}

func TestApplyIdentityMapping_ShareNotFound(t *testing.T) {
	svc := New()
	prov := fakeProvider{want: "exists", info: anonInfo(models.SquashNone)}
	_, err := svc.ApplyIdentityMapping("missing", &metadata.Identity{UID: u32(1000)}, prov)
	if err == nil {
		t.Fatal("expected error for missing share")
	}
}

// AUTH_NULL (nil UID) is always squashed to anonymous regardless of mode.
func TestApplyIdentityMapping_NilUIDAlwaysAnonymous(t *testing.T) {
	svc := New()
	for _, mode := range []models.SquashMode{
		models.SquashNone, models.SquashRootToAdmin, models.SquashRootToGuest,
		models.SquashAllToAdmin, models.SquashAllToGuest, "",
	} {
		in := &metadata.Identity{UID: nil, GID: nil, Username: "orig"}
		out, err := svc.ApplyIdentityMapping("s", in, fakeProvider{want: "s", info: anonInfo(mode)})
		if err != nil {
			t.Fatalf("mode %q: unexpected error: %v", mode, err)
		}
		if out.UID == nil || *out.UID != anonUID || out.GID == nil || *out.GID != anonGID {
			t.Errorf("mode %q: nil UID not squashed to anonymous: %+v", mode, out)
		}
		if out.Username != "anonymous(65534)" {
			t.Errorf("mode %q: username = %q", mode, out.Username)
		}
	}
}

func TestApplyIdentityMapping_SquashModes(t *testing.T) {
	svc := New()

	cases := []struct {
		name     string
		mode     models.SquashMode
		inUID    uint32
		wantUID  uint32
		wantUser string
	}{
		// No-mapping modes leave non-root identities untouched.
		{"none/nonroot", models.SquashNone, 1000, 1000, "user"},
		{"empty/nonroot", "", 1000, 1000, "user"},
		{"root_to_admin/nonroot", models.SquashRootToAdmin, 1000, 1000, "user"},
		{"root_to_admin/root", models.SquashRootToAdmin, 0, 0, "user"},
		// root_to_guest only squashes UID 0.
		{"root_to_guest/root", models.SquashRootToGuest, 0, anonUID, "anonymous(65534)"},
		{"root_to_guest/nonroot", models.SquashRootToGuest, 1000, 1000, "user"},
		// Empty squash defaults to root_to_guest, so root (UID 0) is squashed
		// to anonymous (the new default).
		{"empty/root", "", 0, anonUID, "anonymous(65534)"},
		// all_to_admin maps every UID to root.
		{"all_to_admin/nonroot", models.SquashAllToAdmin, 1000, 0, "root"},
		{"all_to_admin/root", models.SquashAllToAdmin, 0, 0, "root"},
		// all_to_guest maps every UID to anonymous.
		{"all_to_guest/nonroot", models.SquashAllToGuest, 1000, anonUID, "anonymous(65534)"},
		{"all_to_guest/root", models.SquashAllToGuest, 0, anonUID, "anonymous(65534)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := &metadata.Identity{UID: u32(c.inUID), GID: u32(c.inUID), Username: "user"}
			out, err := svc.ApplyIdentityMapping("s", in, fakeProvider{want: "s", info: anonInfo(c.mode)})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.UID == nil || *out.UID != c.wantUID {
				t.Errorf("UID = %v, want %d", out.UID, c.wantUID)
			}
			if out.Username != c.wantUser {
				t.Errorf("Username = %q, want %q", out.Username, c.wantUser)
			}
		})
	}
}

// The returned identity must be a distinct copy; mutating it must not touch the
// caller's input (the mapping builds a fresh effective identity).
func TestApplyIdentityMapping_DoesNotMutateInput(t *testing.T) {
	svc := New()
	in := &metadata.Identity{UID: u32(0), GID: u32(0), Username: "root-user"}
	out, err := svc.ApplyIdentityMapping("s", in, fakeProvider{want: "s", info: anonInfo(models.SquashAllToGuest)})
	if err != nil {
		t.Fatal(err)
	}
	if in.UID == nil || *in.UID != 0 || in.Username != "root-user" {
		t.Errorf("input identity was mutated: %+v", in)
	}
	if out.UID == nil || *out.UID != anonUID {
		t.Errorf("output not squashed: %+v", out)
	}
}

func TestApplyAnonymousIdentity(t *testing.T) {
	id := &metadata.Identity{UID: u32(5), GID: u32(5), GIDs: []uint32{5, 6}, Username: "x"}
	ApplyAnonymousIdentity(id, 1001, 1002)
	if *id.UID != 1001 || *id.GID != 1002 {
		t.Errorf("uid/gid = %d/%d, want 1001/1002", *id.UID, *id.GID)
	}
	if len(id.GIDs) != 1 || id.GIDs[0] != 1002 {
		t.Errorf("GIDs = %v, want [1002]", id.GIDs)
	}
	if id.Username != "anonymous(1001)" {
		t.Errorf("Username = %q", id.Username)
	}
}

// TestApplyIdentityMapping_PreservesWindowsIdentity pins the Windows half of
// the identity across a no-op mapping. The runtime variant previously copied
// only UID/GID/GIDs/Username, silently dropping SID, GroupSIDs and Domain; the
// NFS resolver never set those so it stayed latent, but the SMB path does set
// them, and a dropped SID means Windows ACE matching sees an unnamed principal.
func TestApplyIdentityMapping_PreservesWindowsIdentity(t *testing.T) {
	svc := New()
	sid := "S-1-5-21-1-2-3-1013"
	in := &metadata.Identity{
		UID:       u32(1000),
		GID:       u32(1000),
		GIDs:      []uint32{1000, 1001},
		SID:       &sid,
		GroupSIDs: []string{"S-1-5-21-1-2-3-513", "S-1-5-32-544"},
		Username:  "alice",
		Domain:    "EXAMPLE",
	}

	out, err := svc.ApplyIdentityMapping("s", in, fakeProvider{want: "s", info: anonInfo(models.SquashNone)})
	if err != nil {
		t.Fatal(err)
	}

	if out.SID == nil || *out.SID != sid {
		t.Errorf("SID = %v, want %q", out.SID, sid)
	}
	if len(out.GroupSIDs) != 2 || out.GroupSIDs[0] != in.GroupSIDs[0] {
		t.Errorf("GroupSIDs = %v, want %v", out.GroupSIDs, in.GroupSIDs)
	}
	if out.Domain != "EXAMPLE" {
		t.Errorf("Domain = %q, want EXAMPLE", out.Domain)
	}
}

// TestApplyAnonymousIdentity_ClearsWindowsIdentity pins that squashing to
// anonymous also drops the SID/GroupSIDs/Domain: leaving a named principal's
// SID behind would let ACE matching survive the squash.
func TestApplyAnonymousIdentity_ClearsWindowsIdentity(t *testing.T) {
	sid := "S-1-5-21-1-2-3-1013"
	id := &metadata.Identity{
		UID:       u32(0),
		SID:       &sid,
		GroupSIDs: []string{"S-1-5-32-544"},
		Domain:    "EXAMPLE",
		Username:  "root",
	}
	ApplyAnonymousIdentity(id, anonUID, anonGID)
	if id.SID != nil {
		t.Errorf("SID = %v, want nil after squash to anonymous", *id.SID)
	}
	if id.GroupSIDs != nil {
		t.Errorf("GroupSIDs = %v, want nil after squash to anonymous", id.GroupSIDs)
	}
	if id.Domain != "" {
		t.Errorf("Domain = %q, want empty after squash to anonymous", id.Domain)
	}
}

func TestApplyRootIdentity(t *testing.T) {
	id := &metadata.Identity{UID: u32(5), GID: u32(5), GIDs: []uint32{5, 6}, Username: "x"}
	ApplyRootIdentity(id)
	if *id.UID != 0 || *id.GID != 0 {
		t.Errorf("uid/gid = %d/%d, want 0/0", *id.UID, *id.GID)
	}
	if len(id.GIDs) != 1 || id.GIDs[0] != 0 {
		t.Errorf("GIDs = %v, want [0]", id.GIDs)
	}
	if id.Username != "root" {
		t.Errorf("Username = %q, want root", id.Username)
	}
}

// TestApplyRootIdentity_ClearsWindowsIdentity pins the same invariant as the
// anonymous case: a root squash must not leave a named principal's SID behind,
// or Windows ACE matching would still see the original user after the squash.
func TestApplyRootIdentity_ClearsWindowsIdentity(t *testing.T) {
	sid := "S-1-5-21-1111111111-2222222222-3333333333-1001"
	id := &metadata.Identity{
		UID:       u32(1000),
		GID:       u32(1000),
		GIDs:      []uint32{1000},
		Username:  "alice",
		SID:       &sid,
		GroupSIDs: []string{"S-1-5-21-1111111111-2222222222-3333333333-513"},
		Domain:    "EXAMPLE.COM",
	}

	ApplyRootIdentity(id)

	if id.SID != nil {
		t.Errorf("SID = %q, want nil after root squash", *id.SID)
	}
	if len(id.GroupSIDs) != 0 {
		t.Errorf("GroupSIDs = %v, want empty after root squash", id.GroupSIDs)
	}
	if id.Domain != "" {
		t.Errorf("Domain = %q, want empty after root squash", id.Domain)
	}
}
