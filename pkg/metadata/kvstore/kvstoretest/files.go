package kvstoretest

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

func testRootAndResolve(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, files := s.ControlPlane(), s.Files()
	sh, other := share("photos", "/photos"), share("docs", "/docs")
	must(t, cp.PutShare(ctx, sh))
	must(t, cp.PutShare(ctx, other))
	u, g := user("alice"), group("staff")
	must(t, cp.PutUser(ctx, u))
	must(t, cp.PutGroup(ctx, g))

	alice := entity.Identity{Principal: u.Principal, Groups: []entity.Principal{g.Principal}}
	if _, _, err := files.Root(ctx, alice, sh.ID); !isDenied(err) {
		t.Fatalf("Root with no grant: %v", err)
	}
	// A grant to a group the identity carries admits it.
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: g.Principal, Access: entity.AccessRead}))
	h, root, err := files.Root(ctx, alice, sh.ID)
	must(t, err)
	if !root.IsRoot() || root.ID != sh.Root || root.Number != 1 || root.Nlink != 0 || root.Validate() != nil {
		t.Fatalf("root %+v", root)
	}
	if id, err := files.Resolve(ctx, sh.ID, h); err != nil || id != sh.Root {
		t.Errorf("Resolve(root) = %v, %v", id, err)
	}
	if _, err := files.Resolve(ctx, other.ID, h); !errors.Is(err, entity.ErrWrongShare) {
		t.Errorf("Resolve in another share: %v", err)
	}
	for name, bad := range map[string]entity.Handle{
		"unknown file": entity.NewHandle(sh.ID, entity.NewFileID()),
		"truncated":    h[:len(h)-1],
		"empty":        nil,
	} {
		if _, err := files.Resolve(ctx, sh.ID, bad); !errors.Is(err, entity.ErrStale) {
			t.Errorf("%s: %v, want stale", name, err)
		}
	}
	if _, _, err := files.Root(ctx, alice, entity.NewShareID()); !merrs.IsNotFoundError(err) {
		t.Errorf("Root of a missing share: %v", err)
	}
}

func isDenied(err error) bool {
	var se *merrs.StoreError
	return errors.As(err, &se) && se.Code == merrs.ErrAccessDenied
}
