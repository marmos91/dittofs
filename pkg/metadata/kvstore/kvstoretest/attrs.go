package kvstoretest

import (
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
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

// A chmod asks write the ACL: a non-owner granted it changes the permission
// bits but never sets setuid or setgid on the owner's file, keeps a
// directory's setgid, neither sets nor clears the sticky bit, one granted write
// attributes alone changes nothing; and a write's clear of setuid and
// setgid changes the mode alone, never the stored ACL a chmod would merge
// into.
func testWriteACLAndClearSetID(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	alice := f.member("alice", entity.Principal{})
	bob := f.member("bob", entity.Principal{})
	carol := f.member("carol", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "prog", regular(0o644))
	acl := entity.ACLFromMode(0o644)
	acl.Entries = append(acl.Entries,
		entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteACL, Who: bob.Principal},
		entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteData | entity.WriteAttributes, Who: carol.Principal})
	must(t, errOf(files.SetACL(ctx, alice, h, acl)))
	if _, err := files.SetAttrs(ctx, carol, h, entity.Attrs{}.WithMode(0o666)); !isDenied(err) {
		t.Errorf("chmod by a non-owner holding write attributes: %v", err)
	}
	if got, err := files.SetAttrs(ctx, bob, h, entity.Attrs{}.WithMode(0o6660)); err != nil || got.Mode&0o7777 != 0o660 || got.Owner != alice.Principal {
		t.Errorf("chmod 6660 by a non-owner holding write the ACL: %o, %v, want 660 with setuid and setgid dropped", got.Mode, err)
	}
	// One in the file's group sets no setgid either.
	staff := f.group("staff")
	dave := f.member("dave", entity.Principal{}, staff)
	g, _ := f.create(alice, pub, "grp", regular(0o644))
	must(t, errOf(files.SetAttrs(ctx, f.admin, g, entity.Attrs{}.WithGroup(staff))))
	gacl := entity.ACLFromMode(0o644)
	gacl.Entries = append(gacl.Entries, entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteACL, Who: dave.Principal})
	must(t, errOf(files.SetACL(ctx, alice, g, gacl)))
	if got, err := files.SetAttrs(ctx, dave, g, entity.Attrs{}.WithMode(0o2660)); err != nil || got.Mode&0o7777 != 0o660 {
		t.Errorf("chmod 2660 by a member of the file's group holding write the ACL: %o, %v, want 660 with setgid dropped", got.Mode, err)
	}
	// On alice's directories bob, holding write the ACL, keeps a directory's
	// setuid and setgid, and neither clears nor sets the sticky bit.
	for _, tc := range []struct {
		name      string
		mode, set uint32
		want      uint32
	}{
		{"setgid", 0o2775, 0o2770, 0o2770},
		{"setuid", 0o4775, 0o4770, 0o4770},
		{"sticky", 0o1777, 0o777, 0o1777},
		{"plain", 0o777, 0o1777, 0o777},
	} {
		d, _ := f.create(alice, pub, tc.name, directory(0o777))
		must(t, errOf(files.SetAttrs(ctx, alice, d, entity.Attrs{}.WithMode(tc.mode))))
		dacl := entity.ACLFromMode(tc.mode & 0o777)
		dacl.Entries = append(dacl.Entries, entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteACL, Who: bob.Principal})
		must(t, errOf(files.SetACL(ctx, alice, d, dacl)))
		if got, err := files.SetAttrs(ctx, bob, d, entity.Attrs{}.WithMode(tc.set)); err != nil || got.Mode&0o7777 != tc.want {
			t.Errorf("chmod %o of a %o directory by a non-owner holding write the ACL: %o, %v, want %o", tc.set, tc.mode, got.Mode&0o7777, err, tc.want)
		}
	}
	must(t, errOf(files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithMode(0o4660))))
	before, err := files.ACL(ctx, f.admin, h)
	must(t, err)
	if got, err := files.ClearSetID(ctx, carol, h, false); err != nil || got.Mode&0o7777 != 0o660 {
		t.Errorf("ClearSetID by a writer: %o, %v", got.Mode, err)
	}
	after, err := files.ACL(ctx, f.admin, h)
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("ClearSetID changed the stored ACL:\n before %+v\n after  %+v", before, after)
	}
	if _, err := files.ClearSetID(ctx, bob, h, false); !isDenied(err) {
		t.Errorf("ClearSetID by a caller without write data: %v", err)
	}
	// An append-only file takes appends alone, so the clear asks append data
	// of it: its owner, whose write data the flag refuses, clears the bits.
	log, _ := f.create(alice, pub, "log", regular(0o644))
	must(t, errOf(files.SetAttrs(ctx, alice, log, entity.Attrs{}.WithMode(0o4644))))
	must(t, errOf(files.SetAttrs(ctx, f.admin, log, entity.Attrs{}.WithFlags(entity.FlagAppendOnly))))
	if got, err := files.ClearSetID(ctx, alice, log, true); err != nil || got.Mode&0o7777 != 0o644 {
		t.Errorf("ClearSetID of an append-only file by its owner: %o, %v", got.Mode, err)
	}
	// Append data alone admits an append to a plain file, so it clears the
	// bits for one, and for no other change.
	erin := f.member("erin", entity.Principal{})
	suid, _ := f.create(alice, pub, "suid", regular(0o600))
	must(t, errOf(files.SetAttrs(ctx, alice, suid, entity.Attrs{}.WithMode(0o4600))))
	sacl := entity.ACLFromMode(0o4600 & 0o777)
	sacl.Entries = append(sacl.Entries, entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.AppendData, Who: erin.Principal})
	must(t, errOf(files.SetACL(ctx, alice, suid, sacl)))
	if _, err := files.ClearSetID(ctx, erin, suid, false); !isDenied(err) {
		t.Errorf("ClearSetID for an overwrite by a caller holding append data alone: %v", err)
	}
	if got, err := files.ClearSetID(ctx, erin, suid, true); err != nil || got.Mode&0o7777 != 0o600 {
		t.Errorf("ClearSetID for an append by a caller holding append data alone: %o, %v", got.Mode, err)
	}
}

// RFC 7 §2.6's "what a mode bit grants" row and §7.4's privileged execute:
// a group member's GENERIC_WRITE open of a 0664 file is granted, but write
// attributes from a mode bit sets no time, while one from an ACL entry does;
// a privileged caller executes only what an x bit makes executable.
func testModeBitRights(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	staff := f.group("staff")
	alice := f.member("alice", entity.Principal{}, staff)
	bob := f.member("bob", entity.Principal{}, staff)
	carol := f.member("carol", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "doc", kvstore.NewFile{Type: entity.Regular, Mode: 0o664, Attrs: entity.Attrs{}.WithGroup(staff)})
	const genericWrite = entity.WriteData | entity.AppendData | entity.WriteNamedAttrs | entity.WriteAttributes | entity.Synchronize
	if err := files.Authorize(ctx, bob, h, nil, genericWrite|entity.ReadData); err != nil {
		t.Errorf("a group member's GENERIC_READ and GENERIC_WRITE open of a 0664 file: %v", err)
	}
	if err := files.Authorize(ctx, carol, h, nil, genericWrite); err == nil {
		t.Error("an outsider's GENERIC_WRITE open of a 0664 file was granted")
	}
	at := time.Unix(1_900_000_000, 0)
	if _, err := files.SetAttrs(ctx, bob, h, entity.Attrs{}.WithModify(at)); err == nil {
		t.Error("write attributes from a mode bit set a client's time")
	}
	acl := entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes | entity.ReadACL | entity.Synchronize, Who: entity.Everyone},
		{Type: entity.ACEAllow, Mask: entity.WriteAttributes, Who: bob.Principal},
	}}
	must(t, errOf(files.SetACL(ctx, alice, h, acl)))
	if got, err := files.SetAttrs(ctx, bob, h, entity.Attrs{}.WithModify(at)); err != nil || !got.Modify.Equal(at) {
		t.Errorf("write attributes from an ACL entry set a time: %v, %v", got.Modify, err)
	}
	if _, err := files.SetAttrs(ctx, bob, h, entity.Attrs{}.WithMode(0o666)); err == nil {
		t.Error("a non-owner with write attributes changed the mode")
	}

	plain, _ := f.create(f.admin, pub, "plain", regular(0o644))
	prog, _ := f.create(f.admin, pub, "prog", regular(0o744))
	if err := files.Authorize(ctx, f.admin, plain, nil, entity.Execute); err == nil {
		t.Error("a privileged caller executes a file with no x bit")
	}
	for name, x := range map[string]entity.Handle{"a file with an x bit": prog, "a directory": pub} {
		if err := files.Authorize(ctx, f.admin, x, nil, entity.Execute); err != nil {
			t.Errorf("a privileged caller executes %s: %v", name, err)
		}
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
			Extent: entity.Extent{Length: 3 << 20}, Mtime: time.Now(), Ctime: time.Now()}}}}, nil)
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
		more, _, err = f.s.Namespace().FoldDirectory(ctx, f.share.ID, id)
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

// A read-only share grant refuses every attribute change, a privileged
// caller's too: privilege skips the file's mode and ACL, never the grant.
// The flags of an immutable file, which only privilege may change, are
// refused the same way.
func testPrivilegedReadOnlyGrant(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(f.admin, pub, "x", regular(0o644))
	frozen, _ := f.create(f.admin, pub, "frozen", regular(0o644))
	must(t, errOf(f.s.Files().SetAttrs(ctx, f.admin, frozen, entity.Attrs{}.WithFlags(entity.FlagImmutable))))

	u := user("rootro")
	must(t, f.s.ControlPlane().PutUser(ctx, u))
	must(t, f.s.ControlPlane().SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: u.Principal, Access: entity.AccessRead}))
	ro := entity.Identity{Principal: u.Principal, Privileged: true}

	if _, _, err := f.s.Namespace().Create(ctx, ro, pub, []byte("y"), regular(0o644)); !isDenied(err) {
		t.Errorf("create under a read-only grant: %v", err)
	}
	for _, c := range []struct {
		name string
		h    entity.Handle
		a    entity.Attrs
	}{
		{"chmod", h, entity.Attrs{}.WithMode(0o600)},
		{"chown", h, entity.Attrs{}.WithOwner(u.Principal)},
		{"immutable", h, entity.Attrs{}.WithFlags(entity.FlagImmutable)},
		{"clear immutable", frozen, entity.Attrs{}.WithFlags(0)},
	} {
		if _, err := f.s.Files().SetAttrs(ctx, ro, c.h, c.a); !isDenied(err) {
			t.Errorf("%s by a privileged caller with a read-only grant: %v", c.name, err)
		}
	}
	if _, err := f.s.Files().Get(ctx, ro, h); err != nil {
		t.Errorf("a read under the read-only grant: %v", err)
	}
}
