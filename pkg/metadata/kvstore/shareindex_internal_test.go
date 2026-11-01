package kvstore

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// The index finds a share by its current name, folded, and path only; a
// removed share leaves the name index and the listing, and is still read by
// ID in its state.
func TestShareIndexFollowsRenameAndRemoval(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp, x := s.ControlPlane(), s.ShareIndex()
	sh := entity.Share{ID: entity.NewShareID(), Name: "Data", Path: "/data", Root: entity.NewFileID()}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(cp.PutShare(ctx, sh))
	if id, err := x.ByName(ctx, "DATA"); err != nil || id != sh.ID {
		t.Errorf("ByName under the fold: %v, %v", id, err)
	}
	v0, _, err := x.List(ctx)
	must(err)

	sh.Name, sh.Path = "media", "/media"
	must(cp.PutShare(ctx, sh))
	if _, err := x.ByName(ctx, "Data"); !merrs.IsNotFoundError(err) {
		t.Errorf("the old name still finds the renamed share: %v", err)
	}
	if _, err := x.ByPath(ctx, "/data"); !merrs.IsNotFoundError(err) {
		t.Errorf("the old path still finds the moved share: %v", err)
	}
	if id, err := x.ByPath(ctx, "/media"); err != nil || id != sh.ID {
		t.Errorf("ByPath of the new path: %v, %v", id, err)
	}
	v1, list, err := x.List(ctx)
	must(err)
	if v1.Version <= v0.Version || len(list) != 1 || list[0].Path != "/media" {
		t.Errorf("after the rename List is version %d (was %d), %+v", v1.Version, v0.Version, list)
	}
	if v, err := x.Version(ctx); err != nil || v != v1 {
		t.Errorf("Version %d, %v; List read %d", v.Version, err, v1.Version)
	}

	if _, p, err := x.Share(ctx, sh.ID); err != nil || p.Squash != entity.SquashRoot || p.Anonymous != entity.AnonymousPrincipal {
		t.Errorf("a share with no stored policy: squash %v, anonymous %v, %v", p.Squash, p.Anonymous, err)
	}
	must(cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: sh.ID, Squash: entity.SquashAll}))
	must(cp.DeleteShare(ctx, sh.ID, entity.SnapshotsRefuse))
	if _, err := x.ByName(ctx, "media"); !merrs.IsNotFoundError(err) {
		t.Errorf("a removed share's name still finds it: %v", err)
	}
	if _, list, _ := x.List(ctx); len(list) != 0 {
		t.Errorf("a removed share is still listed: %+v", list)
	}
	got, p, err := x.Share(ctx, sh.ID)
	if err != nil || got.State != entity.ShareRemoving || p.Squash != entity.SquashAll || p.Anonymous != entity.AnonymousPrincipal {
		t.Errorf("a removed share by ID: state %v, squash %v, anonymous %v, %v", got.State, p.Squash, p.Anonymous, err)
	}
	if _, _, err := x.Share(ctx, entity.NewShareID()); !merrs.IsNotFoundError(err) {
		t.Errorf("a share never created: %v", err)
	}
}

// Granted leaves out every right the share grant withholds, a read-only
// narrowing included, even for a privileged caller. Authorize refuses a write
// the narrowing withholds as a read-only file system, and one the grant
// withholds as access denied.
func TestGrantedLeavesOutWhatTheGrantWithholds(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	share := newShare(t, s)
	u := entity.User{Principal: entity.NewUser(), Name: "u"}
	if err := s.ControlPlane().PutUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	set := func(a entity.ShareAccess) {
		if err := s.ControlPlane().SetGrant(ctx, entity.ShareGrant{Share: share, Principal: u.Principal, Access: a}); err != nil {
			t.Fatal(err)
		}
	}
	sh, _, err := s.ShareIndex().Share(ctx, share)
	if err != nil {
		t.Fatal(err)
	}
	h := entity.NewHandle(share, sh.Root)
	if _, _, err := s.Files().Root(ctx, entity.Identity{Principal: u.Principal, Privileged: true}, share); err == nil {
		t.Fatal("a principal with no grant reached the root")
	}
	const want = entity.ReadData | entity.WriteData | entity.ReadAttributes
	for _, c := range []struct {
		name   string
		access entity.ShareAccess
		ro     entity.ReadOnlyCause
		got    entity.AccessMask
		write  merrs.ErrorCode // Authorize's refusal of WriteData; zero: none
	}{
		{"no grant", entity.AccessNone, entity.NotNarrowed, 0, merrs.ErrAccessDenied},
		{"read", entity.AccessRead, entity.NotNarrowed, entity.ReadData | entity.ReadAttributes, merrs.ErrAccessDenied},
		{"read-write", entity.AccessReadWrite, entity.NotNarrowed, want, 0},
		{"read-write narrowed by a client rule", entity.AccessReadWrite, entity.NarrowedByClient, entity.ReadData | entity.ReadAttributes, merrs.ErrReadOnly},
		{"read narrowed by the share", entity.AccessRead, entity.NarrowedByShare, entity.ReadData | entity.ReadAttributes, merrs.ErrReadOnly},
	} {
		set(c.access)
		id := entity.Identity{Principal: u.Principal, Privileged: true, ReadOnly: c.ro}
		got, err := s.Files().Granted(ctx, id, h, nil, want)
		if err != nil || got != c.got {
			t.Errorf("%s: Granted %#x, %v, want %#x", c.name, got, err, c.got)
		}
		err = s.Files().Authorize(ctx, id, h, nil, entity.WriteData)
		var se *merrs.StoreError
		if c.write == 0 && err != nil || c.write != 0 && (!errors.As(err, &se) || se.Code != c.write) {
			t.Errorf("%s: Authorize of a write: %v, want code %v", c.name, err, c.write)
		}
	}
}

// User reads a user's disabled flag, and finds no deleted user.
func TestUserReadsTheRecord(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u := entity.User{Principal: entity.NewUser(), Name: "u", Disabled: true}
	if err := s.ControlPlane().PutUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Principals().User(ctx, u.Principal); err != nil || !got.Disabled || got.Name != "u" {
		t.Errorf("User: %+v, %v", got, err)
	}
	if err := s.ControlPlane().DeleteUser(ctx, u.Principal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Principals().User(ctx, u.Principal); !merrs.IsNotFoundError(err) {
		t.Errorf("a deleted user: %v", err)
	}
}

// Installation is the record Init wrote.
func TestInstallationIsInits(t *testing.T) {
	ctx := context.Background()
	db := memKV(t)
	id := uuid.New()
	if err := Init(ctx, db, id); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if in, err := s.Installation(ctx); err != nil || in.ID != id {
		t.Errorf("Installation: %x, %v, want %x", in.ID, err, id)
	}
}

// Netgroup finds a stored netgroup, and answers not found, with no error,
// for a name none is stored under.
func TestNetgroupFindsTheStoredOne(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if err := s.ControlPlane().PutNetgroup(ctx, entity.Netgroup{Name: "lab", Members: []string{"10.1.0.0/16"}}); err != nil {
		t.Fatal(err)
	}
	if g, found, err := s.ShareIndex().Netgroup(ctx, "lab"); err != nil || !found || len(g.Members) != 1 {
		t.Errorf("Netgroup of a stored netgroup: %+v, %v, %v", g, found, err)
	}
	if _, found, err := s.ShareIndex().Netgroup(ctx, "none"); err != nil || found {
		t.Errorf("Netgroup of a name none is stored under: %v, %v", found, err)
	}
}
