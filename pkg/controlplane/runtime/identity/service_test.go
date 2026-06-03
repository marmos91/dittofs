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
