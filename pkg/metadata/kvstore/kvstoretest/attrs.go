package kvstoretest

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

func testSetAttrsRights(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	finance, eng := f.group("finance"), f.group("eng")
	alice := f.member("alice", entity.Principal{}, eng)
	bob := f.member("bob", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "prog", regular(0o755))

	if _, err := files.SetAttrs(ctx, bob, h, entity.Attrs{}.WithMode(0o777)); err == nil {
		t.Error("a non-owner changed the mode")
	}
	if _, err := files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithOwner(bob.Principal)); !isCode(err, merrs.ErrPrivilegeRequired) {
		t.Errorf("the owner gave the file away: %v", err)
	}
	if got, err := files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithGroup(eng)); err != nil || got.Group != eng {
		t.Errorf("chgrp to a group the owner is in: %v, %v", got.Group, err)
	}
	if _, err := files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithGroup(finance)); err == nil {
		t.Error("chgrp to a group the owner is not in")
	}
	if _, err := files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithFlags(entity.FlagImmutable)); !isCode(err, merrs.ErrPrivilegeRequired) {
		t.Errorf("the owner set immutable: %v", err)
	}
	if _, err := files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithSize(0)); err == nil {
		t.Error("a size change bypassed the write path")
	}

	// RFC 7 §12.1: chown of a setuid file clears setuid, whoever makes it.
	must(t, errOf(files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithMode(0o4755))))
	if got, err := files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithOwner(bob.Principal)); err != nil || got.Mode != 0o755 {
		t.Errorf("chown of a 4755 file: mode %o, %v", got.Mode, err)
	}
	// RFC 7 §12.1: chmod 2755 of a finance file by its non-member owner
	// leaves setgid clear.
	g, _ := f.create(f.admin, pub, "ledger", kvstore.NewFile{Type: entity.Regular, Mode: 0o644,
		Attrs: entity.Attrs{}.WithOwner(alice.Principal).WithGroup(finance)})
	if got, err := files.SetAttrs(ctx, alice, g, entity.Attrs{}.WithMode(0o2755)); err != nil || got.Mode != 0o755 {
		t.Errorf("chmod 2755 by a non-member: mode %o, %v", got.Mode, err)
	}
}

func errOf(_ entity.File, e error) error { return e }

// RFC 7 §12.1's client time row and §9.4's clamp: a time the client gives
// is stored as given, earlier or later, and Change still advances.
func testTimes(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	h, made := f.create(f.admin, f.root, "x", regular(0o644))
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	got, e := files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithModify(past).WithAccess(past))
	must(t, e)
	if !got.Modify.Equal(past) || !got.Access.Equal(past) {
		t.Errorf("times %v %v, want %v", got.Modify, got.Access, past)
	}
	if !got.Change.After(made.Change) || !made.Version.Less(got.Version) {
		t.Errorf("Change %v -> %v, Version %d -> %d", made.Change, got.Change, made.Version.Counter(), got.Version.Counter())
	}
	again, _ := files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithMode(0o600))
	if !again.Change.After(got.Change) || !got.Version.Less(again.Version) {
		t.Error("a second change did not advance Change and Version")
	}
}

// RFC 7 §12.1's exclusive create row: after a SETATTR, a retry with the
// first verifier is an error.
func testSetAttrsClearsVerifier(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	excl := kvstore.NewFile{Type: entity.Regular, Mode: 0o644, Verifier: []byte("v1")}
	h, _ := f.create(f.admin, f.root, "lock", excl)
	must(t, errOf(files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithMode(0o640))))
	if _, _, e := ns.Create(ctx, f.admin, f.root, []byte("lock"), excl); !isExist(e) {
		t.Errorf("retry after SETATTR: %v", e)
	}
}

func testACLs(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	alice := f.member("alice", entity.Principal{})
	bob := f.member("bob", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "doc", regular(0o640))

	acl, e := files.ACL(ctx, alice, h)
	if e != nil || acl.Mode() != 0o640 {
		t.Fatalf("synthesised ACL: mode %o, %v", acl.Mode(), e)
	}
	// Give bob read and write by name: the mode follows the ACL.
	acl.Entries = append([]entity.ACE{{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteData | entity.AppendData, Who: bob.Principal}}, acl.Entries...)
	got, e := files.SetACL(ctx, alice, h, acl)
	if e != nil || got.Mode != 0o640 {
		t.Fatalf("SetACL: mode %o, %v", got.Mode, e)
	}
	if _, e := files.SetACL(ctx, bob, h, acl); e == nil {
		t.Error("a non-owner set the ACL")
	}
	// chmod 600 merges: bob keeps nothing, and the ACL and mode agree.
	must(t, errOf(files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithMode(0o600))))
	merged, _ := files.ACL(ctx, alice, h)
	if merged.Mode() != 0o600 {
		t.Errorf("merged ACL implies %o", merged.Mode())
	}
	for _, e := range merged.Entries {
		if e.Who == bob.Principal && e.Mask&(entity.ReadData|entity.WriteData) != 0 {
			t.Errorf("bob kept %x after chmod 600", e.Mask)
		}
	}
}

func testXattrs(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	h, made := f.create(f.admin, f.root, "x", regular(0o644))
	must(t, files.SetXattr(ctx, f.admin, h, entity.Xattr{Name: []byte("user.b"), Value: []byte("2")}))
	must(t, files.SetXattr(ctx, f.admin, h, entity.Xattr{Name: []byte("user.a"), Value: []byte("1")}))
	must(t, files.SetXattr(ctx, f.admin, h, entity.Xattr{Name: []byte("user.a"), Value: []byte("one")}))
	var got []string
	for x, e := range files.Xattrs(ctx, f.admin, h) {
		must(t, e)
		got = append(got, string(x.Name)+"="+string(x.Value))
	}
	if len(got) != 2 || got[0] != "user.a=one" || got[1] != "user.b=2" {
		t.Errorf("xattrs %v", got)
	}
	if after, _ := files.Get(ctx, f.admin, h); !made.Version.Less(after.Version) {
		t.Error("an xattr change did not advance Version")
	}
	must(t, files.RemoveXattr(ctx, f.admin, h, []byte("user.a")))
	if e := files.RemoveXattr(ctx, f.admin, h, []byte("user.a")); !merrs.IsNotFoundError(e) {
		t.Errorf("removing a missing xattr: %v", e)
	}
	if e := files.SetXattr(ctx, f.admin, h, entity.Xattr{Name: []byte("big"), Value: make([]byte, 65<<10)}); e == nil {
		t.Error("an oversized xattr was accepted")
	}
}

// RFC 6 §8.3 and RFC 7 §9.4: the version floor is the highest Version any
// file of the share reports, a directory's deltas and a chmod included, so a
// journal opened above it never reissues a change attribute.
func testVersionFloor(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	dir, _ := f.create(f.admin, f.root, "d", directory(0o755))
	var hs []entity.Handle
	for _, name := range []string{"a", "b", "c", "e", "g"} {
		h, _ := f.create(f.admin, dir, name, regular(0o644))
		hs = append(hs, h)
	}
	must(t, errOf(files.SetAttrs(ctx, f.admin, hs[2], entity.Attrs{}.WithMode(0o600))))
	_, err := f.s.Namespace().Rename(ctx, f.admin, dir, []byte("a"), f.root, []byte("a"))
	must(t, err)
	var top entity.JournalVersion
	for _, h := range append(hs, dir, f.root) {
		got, err := files.Get(ctx, f.admin, h)
		must(t, err)
		top = entity.MaxVersion(top, got.Version)
	}
	floor, err := f.s.VersionFloor(ctx, []entity.ShareID{f.share.ID})
	must(t, err)
	if floor != top {
		t.Errorf("floor %d, highest Version reported %d", floor.Counter(), top.Counter())
	}
}

// RFC 7 §2.8 and RFC 16 §4.4: a file is charged to its owner, its group
// and the share by its logical bytes; chown moves exactly that, and the
// fold changes no answer.
func testUsageFollowsOwner(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	staff := f.group("staff")
	alice := f.member("alice", staff, staff)
	bob := f.member("bob", staff, staff)
	h, made := f.create(f.admin, f.root, "mail", kvstore.NewFile{Type: entity.Regular, Mode: 0o644,
		Attrs: entity.Attrs{}.WithOwner(alice.Principal).WithGroup(staff)})
	_, id, err := h.Parse()
	must(t, err)
	failed, err := f.s.Existence().Commit(ctx, []kvstore.PendingExistence{{Share: f.share.ID, File: id,
		Ops: []kvstore.PendingOp{{Version: entity.VersionOf(made.Version.Counter() + 1), Kind: kvstore.OpWrite,
			Extent: entity.Extent{Length: 3 << 20}, Time: time.Now()}}}})
	must(t, err)
	must(t, failed[id])
	c := f.s.Capacity()
	usage := func(p entity.Principal) entity.Usage {
		u, err := c.PrincipalUsage(ctx, f.share.ID, p)
		must(t, err)
		return u
	}
	want := entity.Usage{Bytes: 3 << 20, Files: 1}
	if got := usage(alice.Principal); got != want {
		t.Errorf("alice %+v, want %+v", got, want)
	}
	must(t, errOf(f.s.Files().SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithOwner(bob.Principal))))
	for _, fold := range []bool{false, true} {
		if fold {
			_, err := c.FoldUsage(ctx, f.share.ID, 0)
			must(t, err)
		}
		if a, b, g := usage(alice.Principal), usage(bob.Principal), usage(staff); a != (entity.Usage{}) || b != want || g != want {
			t.Errorf("after chown (folded %v): alice %+v, bob %+v, staff %+v", fold, a, b, g)
		}
	}
}

// RFC 16 §4.4: folding a directory's time deltas changes nothing a Get
// returns, the version floor included, and leaves its Get no deltas to
// scan.
func testFoldDirectory(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	dir, _ := f.create(f.admin, f.root, "d", directory(0o755))
	for i := range 12 {
		f.create(f.admin, dir, "f"+string(rune('a'+i)), regular(0o644))
	}
	before, err := files.Get(ctx, f.admin, dir)
	must(t, err)
	floor, err := f.s.VersionFloor(ctx, []entity.ShareID{f.share.ID})
	must(t, err)
	_, id, err := dir.Parse()
	must(t, err)
	for more := true; more; {
		more, err = f.s.Namespace().FoldDirectory(ctx, f.share.ID, id)
		must(t, err)
	}
	f.c.Take()
	after, err := files.Get(ctx, f.admin, dir)
	must(t, err)
	if n := f.c.Take(); n.Scanned != 0 {
		t.Errorf("a folded directory's Get scanned %d deltas", n.Scanned)
	}
	if !after.Modify.Equal(before.Modify) || !after.Change.Equal(before.Change) || after.Version != before.Version {
		t.Errorf("fold changed the directory: %v %v %d -> %v %v %d", before.Modify, before.Change, before.Version.Counter(),
			after.Modify, after.Change, after.Version.Counter())
	}
	if again, err := f.s.VersionFloor(ctx, []entity.ShareID{f.share.ID}); err != nil || again != floor {
		t.Errorf("floor %d -> %d %v", floor.Counter(), again.Counter(), err)
	}
}

// RFC 7 §9.5: a time set explicitly on a directory holds, though creates
// in it left deltas with later times.
func testDirectoryTimeSet(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	dir, _ := f.create(f.admin, f.root, "d", directory(0o755))
	f.create(f.admin, dir, "a", regular(0o644))
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, errOf(files.SetAttrs(ctx, f.admin, dir, entity.Attrs{}.WithModify(past))))
	if got, err := files.Get(ctx, f.admin, dir); err != nil || !got.Modify.Equal(past) {
		t.Errorf("directory Modify %v %v, want %v", got.Modify, err, past)
	}
}
