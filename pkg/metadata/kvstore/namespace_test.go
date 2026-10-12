package kvstore_test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// fs is one share in a fresh store, with an administrator who may do
// anything and a handle on the share's root.
type fs struct {
	t     *testing.T
	s     *kvstore.Store
	c     *kv.Counting
	share entity.Share
	admin entity.Identity
	root  entity.Handle
}

func newFS(t *testing.T, fold entity.FoldRule) *fs {
	s, c := newStore(t)
	return setupFS(t, s, c, fold)
}

// newHeldFS is newFS over a store whose next Update can be held between its
// snapshot and its commit, on the given backend.
func newHeldFS(t *testing.T, b backend, fold entity.FoldRule) (*fs, *holding) {
	s, h := heldStore(t, b)
	return setupFS(t, s, nil, fold), h
}

func setupFS(t *testing.T, s *kvstore.Store, c *kv.Counting, fold entity.FoldRule) *fs {
	cp := s.ControlPlane()
	sh := share("data", "/data")
	sh.Fold = fold
	must(t, cp.PutShare(ctx, sh))
	admin := user("admin")
	must(t, cp.PutUser(ctx, admin))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: admin.Principal, Access: entity.AccessAdmin}))
	id := entity.Identity{Principal: admin.Principal, Privileged: true}
	root, _, err := s.Files().Root(ctx, id, sh.ID)
	must(t, err)
	return &fs{t: t, s: s, c: c, share: sh, admin: id, root: root}
}

// member makes a user with a grant on the share, in the given groups.
func (f *fs) member(name string, primary entity.Principal, groups ...entity.Principal) entity.Identity {
	f.t.Helper()
	cp := f.s.ControlPlane()
	u := user(name)
	u.Primary = primary
	must(f.t, cp.PutUser(ctx, u))
	must(f.t, cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: u.Principal, Access: entity.AccessReadWrite}))
	for _, g := range groups {
		must(f.t, cp.AddMember(ctx, entity.Membership{Group: g, Member: u.Principal}))
	}
	return entity.Identity{Principal: u.Principal, Groups: groups}
}

func (f *fs) group(name string) entity.Principal {
	f.t.Helper()
	g := group(name)
	must(f.t, f.s.ControlPlane().PutGroup(ctx, g))
	return g.Principal
}

func (f *fs) create(id entity.Identity, dir entity.Handle, name string, nf kvstore.NewFile) (entity.Handle, entity.File) {
	f.t.Helper()
	h, file, err := f.s.Namespace().Create(ctx, id, dir, []byte(name), nf)
	must(f.t, err)
	return h, file
}

func regular(mode uint32) kvstore.NewFile { return kvstore.NewFile{Type: entity.Regular, Mode: mode} }
func directory(mode uint32) kvstore.NewFile {
	return kvstore.NewFile{Type: entity.Directory, Mode: mode}
}

func TestCreateAndLookup(t *testing.T) {
	f := newFS(t, entity.FoldIdentity)
	ns := f.s.Namespace()
	h, made := f.create(f.admin, f.root, "README", regular(0o644))
	if made.Nlink != 1 || made.Number < 2 || made.Owner != f.admin.Principal || made.Validate() != nil {
		t.Fatalf("created %+v", made)
	}
	gh, got, err := ns.Lookup(ctx, f.admin, f.root, []byte("README"))
	if err != nil || got.ID != made.ID || string(gh) != string(h) {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	if _, _, err := ns.Lookup(ctx, f.admin, f.root, []byte("readme")); !merrs.IsNotFoundError(err) {
		t.Errorf("case-sensitive share found readme: %v", err)
	}
	if _, _, err := ns.Create(ctx, f.admin, f.root, []byte("README"), regular(0o644)); !isExist(err) {
		t.Errorf("second README: %v", err)
	}
	if _, _, err := ns.Create(ctx, f.admin, h, []byte("x"), regular(0o644)); err == nil {
		t.Error("created inside a regular file")
	}
	if _, _, err := ns.Create(ctx, f.admin, f.root, []byte("a/b"), regular(0o644)); !errors.Is(err, entity.ErrInvalidName) {
		t.Errorf("name with a slash: %v", err)
	}
	// The root's record is not rewritten by the create, but its version and
	// times move through the delta.
	before, _ := f.s.Files().Get(ctx, f.admin, f.root)
	f.create(f.admin, f.root, "other", regular(0o644))
	after, err := f.s.Files().Get(ctx, f.admin, f.root)
	must(t, err)
	if after.Version <= before.Version || after.Modify.Before(before.Modify) {
		t.Errorf("root version %d -> %d", before.Version, after.Version)
	}
}

// RFC 7 §12.1's case row.
func TestCaseInsensitiveShare(t *testing.T) {
	f := newFS(t, entity.FoldUpcase)
	ns := f.s.Namespace()
	_, made := f.create(f.admin, f.root, "README", regular(0o644))
	if _, _, err := ns.Create(ctx, f.admin, f.root, []byte("readme"), regular(0o644)); !isExist(err) {
		t.Errorf("readme beside README: %v", err)
	}
	f.c.Take()
	_, got, err := ns.Lookup(ctx, f.admin, f.root, []byte("ReadMe"))
	if err != nil || got.ID != made.ID {
		t.Fatalf("Lookup(ReadMe) = %v, %v", got.ID, err)
	}
	// The directory, the share grant, the entry and the file: one entry key.
	if n := f.c.Take(); n.Gets != 4 || n.Scanned != 0 {
		t.Errorf("Lookup read %+v", n)
	}
	// A windows share refuses names Windows clients cannot open.
	for _, name := range []string{"CON", "aux.txt", "a?b", "dir."} {
		if _, _, err := ns.Create(ctx, f.admin, f.root, []byte(name), regular(0o644)); !errors.Is(err, entity.ErrInvalidName) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// RFC 7 §12.1's owner and group at create, setgid for a non-member, and no
// group at all.
func TestOwnerAndGroupAtCreate(t *testing.T) {
	f := newFS(t, entity.FoldIdentity)
	primary, other, finance := f.group("users"), f.group("eng"), f.group("finance")
	alice := f.member("alice", primary, primary, other)
	carol := f.member("carol", entity.Principal{}) // no primary group, no groups
	accountant := f.member("acct", primary, finance)
	open := directory(0o777)
	pub, _ := f.create(f.admin, f.root, "pub", open)

	// NFS: the credential's group. SMB: none on the credential, so the
	// primary group.
	nfs := alice
	nfs.Group = other
	if _, file := f.create(nfs, pub, "nfs", regular(0o644)); file.Owner != alice.Principal || file.Group != other {
		t.Errorf("NFS create: owner %v group %v", file.Owner, file.Group)
	}
	if _, file := f.create(alice, pub, "smb", regular(0o644)); file.Group != primary {
		t.Errorf("SMB create: group %v, want the primary group", file.Group)
	}
	if _, file := f.create(carol, pub, "lone", regular(0o644)); file.Group != carol.Principal {
		t.Errorf("no group at all: group %v, want the creator's own principal", file.Group)
	}

	// A setgid directory of group finance, mode 3777.
	drop, _ := f.create(f.admin, f.root, "drop", kvstore.NewFile{Type: entity.Directory, Mode: 0o3777,
		Attrs: entity.Attrs{}.WithGroup(finance)})
	if _, file := f.create(alice, drop, "f", regular(0o2755)); file.Group != finance || file.Mode&entity.ModeSetgid != 0 {
		t.Errorf("non-member in a setgid dir: group %v mode %o", file.Group, file.Mode)
	}
	if _, file := f.create(accountant, drop, "g", regular(0o2755)); file.Group != finance || file.Mode&entity.ModeSetgid == 0 {
		t.Errorf("member in a setgid dir: group %v mode %o", file.Group, file.Mode)
	}
	if _, file := f.create(alice, drop, "sub", directory(0o755)); file.Group != finance || file.Mode&entity.ModeSetgid == 0 {
		t.Errorf("directory in a setgid dir: group %v mode %o", file.Group, file.Mode)
	}
	// A requested owner other than the caller needs privilege.
	if _, _, err := f.s.Namespace().Create(ctx, alice, pub, []byte("given"), kvstore.NewFile{Type: entity.Regular,
		Attrs: entity.Attrs{}.WithOwner(carol.Principal)}); err == nil {
		t.Error("an unprivileged create gave its file away")
	}
	// The root is owned by nobody: only a privileged caller creates there.
	if _, _, err := f.s.Namespace().Create(ctx, alice, f.root, []byte("top"), regular(0o644)); !isDenied(err) {
		t.Errorf("create in the unowned root: %v", err)
	}
}

// RFC 7 §12.1's exclusive create row.
func TestExclusiveCreate(t *testing.T) {
	f := newFS(t, entity.FoldIdentity)
	ns := f.s.Namespace()
	excl := func(v string) kvstore.NewFile {
		return kvstore.NewFile{Type: entity.Regular, Mode: 0o644, Verifier: []byte(v)}
	}
	_, first := f.create(f.admin, f.root, "lock", excl("v1"))
	_, again, err := ns.Create(ctx, f.admin, f.root, []byte("lock"), excl("v1"))
	if err != nil || again.ID != first.ID {
		t.Fatalf("retry with the same verifier: %v, %v", again.ID, err)
	}
	if _, _, err := ns.Create(ctx, f.admin, f.root, []byte("lock"), excl("v2")); !isExist(err) {
		t.Errorf("another verifier: %v", err)
	}
	if first.Modify.IsZero() || string(first.CreateVerifier) != "v1" {
		t.Errorf("verifier stored as %q, times %v", first.CreateVerifier, first.Modify)
	}
}

// RFC 7 §12.2: records written per create are constant in the directory's
// size, and none of them is the directory's record.
func TestCreateCost(t *testing.T) {
	f := newFS(t, entity.FoldUpcase)
	f.create(f.admin, f.root, "warm", regular(0o644))
	for i := range 300 {
		f.c.Take()
		f.create(f.admin, f.root, "f"+string(rune('a'+i%26))+string(rune('a'+i/26)), regular(0o644))
		// The file, its entry and the directory's delta.
		if n := f.c.Take(); n.Sets != 3 || n.Deletes != 0 || n.Scanned != 0 {
			t.Fatalf("create %d wrote %+v", i, n)
		}
	}
}

func TestGetAndGrant(t *testing.T) {
	f := newFS(t, entity.FoldIdentity)
	h, made := f.create(f.admin, f.root, "x", regular(0o600))
	got, err := f.s.Files().Get(ctx, f.admin, h)
	if err != nil || got.ID != made.ID {
		t.Fatalf("Get = %v, %v", got.ID, err)
	}
	stranger := entity.Identity{Principal: entity.NewUser()}
	if _, err := f.s.Files().Get(ctx, stranger, h); !isDenied(err) {
		t.Errorf("Get with no grant: %v", err)
	}
	reader := f.member("reader", entity.Principal{})
	must(t, f.s.ControlPlane().SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: reader.Principal, Access: entity.AccessRead}))
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	if _, _, err := f.s.Namespace().Create(ctx, reader, pub, []byte("y"), regular(0o644)); !isDenied(err) {
		t.Errorf("create under a read-only grant: %v", err)
	}
}
