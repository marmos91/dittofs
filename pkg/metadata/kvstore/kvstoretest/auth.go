package kvstoretest

import (
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

func isPerm(err error) bool { return isCode(err, merrs.ErrPermissionDenied) }

// RFC 7 §12.1's flags row, as far as the namespace answers it.
func testImmutableAndAppendOnly(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	alice := f.member("alice", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "x", regular(0o644))
	must(t, errOf(files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithFlags(entity.FlagImmutable))))

	for name, e := range map[string]error{
		"chmod":  errOf(files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithMode(0o600))),
		"write":  files.Authorize(ctx, alice, h, nil, entity.WriteData),
		"append": files.Authorize(ctx, alice, h, nil, entity.AppendData),
		"xattr":  files.SetXattr(ctx, alice, h, entity.Xattr{Name: []byte("k"), Value: []byte("v")}),
		"link":   ns.Link(ctx, alice, pub, []byte("y"), h),
	} {
		if !isPerm(e) {
			t.Errorf("%s of an immutable file: %v, want a permission error", name, e)
		}
	}
	for name, who := range map[string]entity.Identity{"owner": alice, "privileged": f.admin} {
		if _, e := ns.Unlink(ctx, who, pub, []byte("x")); !isPerm(e) {
			t.Errorf("unlink by the %s: %v", name, e)
		}
		if _, e := ns.Rename(ctx, who, pub, []byte("x"), pub, []byte("z")); !isPerm(e) {
			t.Errorf("rename by the %s: %v", name, e)
		}
	}
	if _, e := files.SetAttrs(ctx, alice, h, entity.Attrs{}.WithFlags(0)); !isCode(e, merrs.ErrPrivilegeRequired) {
		t.Errorf("the owner cleared immutable: %v", e)
	}
	// Clearing it is the one change accepted, and only that change.
	if _, e := files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithFlags(entity.FlagHidden)); !isPerm(e) {
		t.Errorf("another flag changed with immutable set: %v", e)
	}
	must(t, errOf(files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithFlags(entity.FlagAppendOnly))))

	// Append-only: appends, no overwrite, no removal.
	if e := files.Authorize(ctx, alice, h, nil, entity.AppendData); e != nil {
		t.Errorf("append to an append-only file: %v", e)
	}
	if e := files.Authorize(ctx, alice, h, nil, entity.WriteData); !isPerm(e) {
		t.Errorf("overwrite of an append-only file: %v", e)
	}
	if _, e := ns.Unlink(ctx, alice, pub, []byte("x")); !isPerm(e) {
		t.Errorf("unlink of an append-only file: %v", e)
	}

	// Directories: nothing enters an immutable one; an append-only one takes
	// entries and loses none.
	locked, _ := f.create(f.admin, f.root, "locked", directory(0o777))
	must(t, errOf(files.SetAttrs(ctx, f.admin, locked, entity.Attrs{}.WithFlags(entity.FlagImmutable))))
	if _, _, e := ns.Create(ctx, alice, locked, []byte("n"), regular(0o644)); !isPerm(e) {
		t.Errorf("create in an immutable directory: %v", e)
	}
	logs, _ := f.create(f.admin, f.root, "logs", directory(0o777))
	f.create(alice, logs, "l1", regular(0o644))
	must(t, errOf(files.SetAttrs(ctx, f.admin, logs, entity.Attrs{}.WithFlags(entity.FlagAppendOnly))))
	f.create(alice, logs, "l2", regular(0o644))
	if _, e := ns.Unlink(ctx, alice, logs, []byte("l1")); !isPerm(e) {
		t.Errorf("unlink out of an append-only directory: %v", e)
	}
}

func testStoredACLDecides(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	staff := f.group("staff")
	alice := f.member("alice", entity.Principal{})
	bob := f.member("bob", entity.Principal{}, staff)
	carol := f.member("carol", entity.Principal{}, staff)
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o755))
	h, _ := f.create(f.admin, pub, "doc", regular(0o600))

	// Deny bob ahead of an allow for his group: carol reads, bob does not.
	_, e := files.SetACL(ctx, f.admin, h, entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEDeny, Mask: entity.ReadData, Who: bob.Principal},
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.Delete, Who: staff},
	}})
	must(t, e)
	if e := files.Authorize(ctx, carol, h, nil, entity.ReadData); e != nil {
		t.Errorf("group allow: %v", e)
	}
	if e := files.Authorize(ctx, bob, h, nil, entity.ReadData); !isDenied(e) {
		t.Errorf("deny before the group allow: %v", e)
	}
	if e := files.Authorize(ctx, alice, h, nil, entity.ReadData); !isDenied(e) {
		t.Errorf("no entry for alice: %v", e)
	}
	if e := files.Authorize(ctx, carol, h, nil, entity.WriteData); !isDenied(e) {
		t.Errorf("a right no entry grants: %v", e)
	}
	// RFC 7 §12.1's removal rights: delete on the file, no right on the
	// directory, is enough.
	if _, e := ns.Unlink(ctx, carol, pub, []byte("doc")); e != nil {
		t.Errorf("unlink with delete on the file only: %v", e)
	}
}

// RFC 7 §12.1's share grant row: a removed grant refuses the next call on
// a handle the caller already holds.
func testRevokedGrantRefusesNextCall(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	alice := f.member("alice", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "x", regular(0o644))
	must(t, errOf(f.s.Files().Get(ctx, alice, h)))
	must(t, f.s.ControlPlane().RemoveGrant(ctx, f.share.ID, alice.Principal))
	if _, e := f.s.Files().Get(ctx, alice, h); !isDenied(e) {
		t.Errorf("call after the grant was removed: %v", e)
	}
}

// A share's grants decide in order: a principal's own grant, then the
// widest of its groups', then a grant naming authenticated users for an
// authenticated caller, then one naming everyone.
func testDefaultGrants(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	cp, files := f.s.ControlPlane(), f.s.Files()
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	dave := user("dave")
	must(t, cp.PutUser(ctx, dave))
	claimed := entity.Identity{Principal: dave.Principal}
	proven := entity.Identity{Principal: dave.Principal, Authenticated: true}
	if e := files.Authorize(ctx, proven, pub, nil, entity.ReadData); !isDenied(e) {
		t.Errorf("no grant at all: %v", e)
	}
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: entity.Everyone, Access: entity.AccessRead}))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: entity.AuthenticatedUsers, Access: entity.AccessReadWrite}))
	if e := files.Authorize(ctx, claimed, pub, nil, entity.ReadData); e != nil {
		t.Errorf("everyone's read grant, read: %v", e)
	}
	if e := files.Authorize(ctx, claimed, pub, nil, entity.WriteData); !isDenied(e) {
		t.Errorf("an unauthenticated caller took authenticated users' read-write: %v", e)
	}
	if e := files.Authorize(ctx, proven, pub, nil, entity.WriteData); e != nil {
		t.Errorf("authenticated users' read-write, write: %v", e)
	}
	if a, err := f.s.Principals().Grant(ctx, f.share.ID, dave.Principal); err != nil || a != entity.AccessReadWrite {
		t.Errorf("Grant of a signed-in user with no grant of its own: %v, %v", a, err)
	}
	staff := f.group("staff")
	must(t, cp.AddMember(ctx, entity.Membership{Group: staff, Member: dave.Principal}))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: staff, Access: entity.AccessRead}))
	inStaff := entity.Identity{Principal: dave.Principal, Groups: []entity.Principal{staff}, Authenticated: true}
	if e := files.Authorize(ctx, inStaff, pub, nil, entity.WriteData); !isDenied(e) {
		t.Errorf("a group's read grant gave way to authenticated users': %v", e)
	}
	if e := cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: entity.Owner, Access: entity.AccessRead}); !isCode(e, merrs.ErrInvalidArgument) {
		t.Errorf("a grant naming OWNER@: %v", e)
	}
	for _, d := range []entity.Principal{entity.Everyone, entity.AuthenticatedUsers} {
		if e := cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: d, Access: entity.AccessAdmin}); !isCode(e, merrs.ErrInvalidArgument) {
			t.Errorf("a default grant of admin to %v: %v", d, e)
		}
	}

	// A grant of none is stored and blocks, a user's and a group's alike,
	// where the defaults would admit.
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: staff, Access: entity.AccessNone}))
	if e := files.Authorize(ctx, inStaff, pub, nil, entity.ReadData); !isDenied(e) {
		t.Errorf("a group's grant of none gave way to the defaults: %v", e)
	}
	erin := user("erin")
	must(t, cp.PutUser(ctx, erin))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: erin.Principal, Access: entity.AccessNone}))
	for _, id := range []entity.Identity{{Principal: erin.Principal}, {Principal: erin.Principal, Authenticated: true}} {
		if e := files.Authorize(ctx, id, pub, nil, entity.ReadData); !isDenied(e) {
			t.Errorf("a user's grant of none gave way to the defaults (%+v): %v", id, e)
		}
	}
	must(t, cp.RemoveGrant(ctx, f.share.ID, erin.Principal))
	if e := files.Authorize(ctx, entity.Identity{Principal: erin.Principal}, pub, nil, entity.ReadData); e != nil {
		t.Errorf("everyone's read grant after removing the user's none: %v", e)
	}
}

// RFC 7 §2.6 and RFC 16 §2.2's well-known principals: each is matched by a
// field of the identity, never by privilege; a deny naming an unknown
// special principal is refused, an allow naming one grants nothing; an
// inheritable CREATOR OWNER entry is kept inherit-only; and an owner-rights
// entry withholds what ownership implies.
func testWellKnownPrincipals(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	alice := f.member("alice", entity.Principal{})
	bob := f.member("bob", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	h, _ := f.create(alice, pub, "doc", regular(0o600))
	set := func(es ...entity.ACE) error {
		_, err := files.SetACL(ctx, alice, h, entity.ACL{Entries: es})
		return err
	}
	read := entity.ReadData
	authed, admin := bob, bob
	authed.Authenticated, admin.Admin = true, true
	anonymous := entity.Identity{Principal: entity.AnonymousPrincipal, Access: entity.AccessRead}
	for _, c := range []struct {
		who          entity.Principal
		yes, no      entity.Identity
		name, reason string
	}{
		{entity.AuthenticatedUsers, authed, bob, "Authenticated Users", "an asserted identity"},
		{entity.BuiltinUsers, authed, bob, "BUILTIN\\Users", "an asserted identity"},
		{entity.Administrators, admin, bob, "Administrators", "a caller not marked admin"},
		{entity.AnonymousPrincipal, anonymous, bob, "anonymous", "a caller acting as itself"},
	} {
		must(t, set(entity.ACE{Type: entity.ACEAllow, Mask: read, Who: c.who}))
		if err := files.Authorize(ctx, c.yes, h, nil, read); err != nil {
			t.Errorf("%s did not match its caller: %v", c.name, err)
		}
		if err := files.Authorize(ctx, c.no, h, nil, read); err == nil {
			t.Errorf("%s matched %s", c.name, c.reason)
		}
	}
	must(t, set(entity.ACE{Type: entity.ACEAllow, Mask: read, Who: entity.Network}))
	if err := files.Authorize(ctx, bob, h, nil, read); err != nil {
		t.Errorf("NETWORK did not match every caller: %v", err)
	}
	for _, who := range []entity.Principal{entity.System, entity.CreatorOwner} {
		must(t, set(entity.ACE{Type: entity.ACEAllow, Mask: read, Who: who}))
		if err := files.Authorize(ctx, admin, h, nil, read); err == nil {
			t.Errorf("%v matched a caller", who)
		}
	}

	unknown := entity.Principal{Kind: entity.PrincipalSpecial, ID: entity.PrincipalID{15: 0x42}}
	if err := set(entity.ACE{Type: entity.ACEDeny, Mask: read, Who: unknown}); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("a deny naming an unknown special principal was stored: %v", err)
	}
	must(t, set(entity.ACE{Type: entity.ACEAllow, Mask: read, Who: unknown}))
	if err := files.Authorize(ctx, bob, h, nil, read); err == nil {
		t.Error("an allow naming an unknown special principal granted")
	}

	must(t, set(entity.ACE{Type: entity.ACEDeny, Flags: entity.ACEFileInherit, Mask: read, Who: entity.CreatorOwner},
		entity.ACE{Type: entity.ACEAllow, Mask: read, Who: entity.Owner}))
	acl, err := files.ACL(ctx, alice, h)
	must(t, err)
	if len(acl.Entries) == 0 || acl.Entries[0].Effective() {
		t.Errorf("an inheritable CREATOR OWNER deny was stored effective: %+v", acl.Entries)
	}

	must(t, set(entity.ACE{Type: entity.ACEAllow, Mask: read, Who: entity.OwnerRights}))
	if err := files.Authorize(ctx, alice, h, nil, entity.WriteACL); err == nil {
		t.Error("an owner-rights entry left the owner its implicit write ACL")
	}
	must(t, errOf(files.SetACL(ctx, f.admin, h, entity.ACL{Entries: []entity.ACE{{Type: entity.ACEAllow, Mask: read, Who: entity.Owner}}})))
	if err := files.Authorize(ctx, alice, h, nil, entity.WriteACL); err != nil {
		t.Errorf("without owner rights the owner lost its implicit write ACL: %v", err)
	}
}

// RFC 7 §2.6's inheritance row: a subdirectory bob creates in alice's
// directory gets an effective "bob: full" and keeps "CREATOR OWNER: full"
// inherit-only, the file bob creates in it gets "bob: full" and nothing for
// alice, and Finance keeps modify on both; the umask is ignored there and
// applied in a parent with nothing inheritable.
func testInheritance(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	finance := f.group("finance")
	alice := f.member("alice", entity.Principal{})
	bob := f.member("bob", entity.Principal{}, finance)
	const full = entity.ReadData | entity.WriteData | entity.AppendData | entity.Execute | entity.Delete | entity.DeleteChild |
		entity.ReadNamedAttrs | entity.WriteNamedAttrs | entity.WriteAttributes | entity.WriteACL | entity.WriteOwner
	const modify = entity.ReadData | entity.WriteData | entity.AppendData | entity.Execute | entity.Delete
	inherits := entity.ACEFileInherit | entity.ACEDirInherit
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	top, _ := f.create(alice, pub, "projects", directory(0o755))
	must(t, errOf(files.SetACL(ctx, alice, top, entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEAllow, Mask: full, Who: entity.Owner}, // alice's own, not inherited
		{Type: entity.ACEAllow, Flags: inherits, Mask: full, Who: entity.CreatorOwner},
		{Type: entity.ACEAllow, Flags: inherits, Mask: modify, Who: finance},
	}})))

	sub, _ := f.create(bob, top, "bobs", kvstore.NewFile{Type: entity.Directory, Mode: 0o777, Umask: 0o022})
	doc, _ := f.create(bob, sub, "doc", kvstore.NewFile{Type: entity.Regular, Mode: 0o777, Umask: 0o022})
	has := func(acl entity.ACL, who entity.Principal, mask entity.AccessMask, effective bool) bool {
		return slices.ContainsFunc(acl.Entries, func(e entity.ACE) bool {
			return e.Type == entity.ACEAllow && e.Who == who && e.Mask&mask == mask && e.Effective() == effective
		})
	}
	subACL, err := files.ACL(ctx, f.admin, sub)
	must(t, err)
	if !has(subACL, bob.Principal, full, true) || !has(subACL, entity.CreatorOwner, full, false) {
		t.Errorf("the subdirectory should hold an effective bob: full and an inherit-only CREATOR OWNER: full: %+v", subACL.Entries)
	}
	docACL, err := files.ACL(ctx, f.admin, doc)
	must(t, err)
	if !has(docACL, bob.Principal, full, true) {
		t.Errorf("the file should hold bob: full: %+v", docACL.Entries)
	}
	for _, acl := range []entity.ACL{subACL, docACL} {
		if slices.ContainsFunc(acl.Entries, func(e entity.ACE) bool { return e.Who == alice.Principal }) {
			t.Errorf("alice, whose directory it was, got an entry: %+v", acl.Entries)
		}
		if !has(acl, finance, modify, true) {
			t.Errorf("finance lost modify: %+v", acl.Entries)
		}
	}
	if err := files.Authorize(ctx, bob, doc, nil, entity.WriteData|entity.Delete); err != nil {
		t.Errorf("bob cannot write or delete his own file: %v", err)
	}

	// alice's own subfolder keeps CREATOR OWNER for its children, so a file
	// bob creates there is bob's, not "alice: full".
	shared, _ := f.create(alice, top, "shared", directory(0o777))
	bobs, _ := f.create(bob, shared, "notes", regular(0o777))
	bobsACL, err := files.ACL(ctx, f.admin, bobs)
	must(t, err)
	if !has(bobsACL, bob.Principal, full, true) || slices.ContainsFunc(bobsACL.Entries, func(e entity.ACE) bool { return e.Who == alice.Principal }) {
		t.Errorf("bob's file in alice's subfolder should hold bob: full and nothing for alice: %+v", bobsACL.Entries)
	}

	_, plain := f.create(bob, pub, "plain", kvstore.NewFile{Type: entity.Regular, Mode: 0o777, Umask: 0o022})
	if plain.Mode&0o777 != 0o755 {
		t.Errorf("with nothing inherited the umask left mode %o, want 755", plain.Mode&0o777)
	}
}

// Delete is granted through the directory a file was resolved by: a group
// member of a group-writable directory may delete her own file through it,
// though the file's mode grants no delete. A directory that does not hold the
// file grants nothing, and the sticky rule holds through the parent too.
func testDeleteThroughParent(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	team := f.group("team")
	bob := f.member("bob", entity.Principal{}, team)
	carol := f.member("carol", entity.Principal{}, team)
	shared := kvstore.NewFile{Type: entity.Directory, Mode: 0o775, Attrs: entity.Attrs{}.WithGroup(team)}
	dir, _ := f.create(f.admin, f.root, "team", shared)
	other, _ := f.create(f.admin, f.root, "other", shared)
	doc, _ := f.create(bob, dir, "report.txt", regular(0o644))
	sub, _ := f.create(bob, dir, "drafts", directory(0o755))

	if err := files.Authorize(ctx, bob, doc, nil, entity.Delete); !isDenied(err) {
		t.Errorf("delete on a 0644 file with no parent: %v", err)
	}
	for _, h := range []entity.Handle{doc, sub} {
		if err := files.Authorize(ctx, bob, h, dir, entity.Delete); err != nil {
			t.Errorf("delete through the group-writable directory holding it: %v", err)
		}
		if got, err := files.Granted(ctx, bob, h, dir, entity.Delete|entity.ReadAttributes); err != nil || got != entity.Delete|entity.ReadAttributes {
			t.Errorf("Granted through the directory holding it: %#x, %v", got, err)
		}
		if err := files.Authorize(ctx, bob, h, other, entity.Delete); !isDenied(err) {
			t.Errorf("delete through a writable directory not holding it: %v", err)
		}
		if got, _ := files.Granted(ctx, bob, h, other, entity.Delete); got != 0 {
			t.Errorf("Granted through a directory not holding it: %#x", got)
		}
	}
	// A removed directory keeps its record, and its parent, until released;
	// the directory it was removed from no longer holds it.
	if _, err := f.s.Namespace().Unlink(ctx, bob, dir, []byte("drafts")); err != nil {
		t.Fatal(err)
	}
	if err := files.Authorize(ctx, bob, sub, dir, entity.Delete); !isDenied(err) {
		t.Errorf("delete of a removed directory through its old parent: %v", err)
	}

	narrowed := bob
	narrowed.ReadOnly = entity.NarrowedByClient
	if err := files.Authorize(ctx, narrowed, doc, dir, entity.Delete); !isCode(err, merrs.ErrReadOnly) {
		t.Errorf("a caller narrowed to read-only deletes through the directory holding it: %v", err)
	}
	if got, err := files.Granted(ctx, narrowed, doc, dir, entity.Delete); err != nil || got != 0 {
		t.Errorf("Granted to a narrowed caller through the directory holding it: %#x, %v", got, err)
	}

	must(t, errOf(files.SetAttrs(ctx, f.admin, dir, entity.Attrs{}.WithMode(0o1775))))
	if err := files.Authorize(ctx, carol, doc, dir, entity.Delete); !isDenied(err) {
		t.Errorf("carol deletes bob's file through a sticky directory: %v", err)
	}
	if err := files.Authorize(ctx, bob, doc, dir, entity.Delete); err != nil {
		t.Errorf("bob deletes his own file through a sticky directory: %v", err)
	}

	// A stored ACL granting the group delete-a-child does not lift the
	// sticky rule.
	must(t, errOf(files.SetACL(ctx, f.admin, dir, entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEAllow, Mask: entity.EntryRights, Who: entity.Owner},
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.Execute | entity.WriteData | entity.DeleteChild, Who: team},
	}})))
	if d, err := files.Get(ctx, f.admin, dir); err != nil || d.Mode&entity.ModeSticky == 0 {
		t.Fatalf("the directory after its ACL was set: mode %o, %v", d.Mode, err)
	}
	if err := files.Authorize(ctx, carol, doc, dir, entity.Delete); !isDenied(err) {
		t.Errorf("carol deletes bob's file through a sticky directory whose ACL grants her group delete-a-child: %v", err)
	}
	if err := files.Authorize(ctx, bob, doc, dir, entity.Delete); err != nil {
		t.Errorf("bob deletes his own file through the sticky directory with an ACL: %v", err)
	}
}

// Privilege grants every entry right and nothing beyond them: a generic,
// maximal-access or system-security bit is refused, and left out of what
// Granted answers. System security is an administrator's alone, and stays so
// under a read-only narrowing.
func testPrivilegeGrantsOnlyEntryRights(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	files := f.s.Files()
	h, _ := f.create(f.admin, f.root, "f", regular(0o755))
	if got, err := files.Granted(ctx, f.admin, h, nil, ^entity.AccessMask(0)); err != nil || got != entity.EntryRights {
		t.Errorf("a privileged caller is granted %#x, %v; want %#x", got, err, entity.EntryRights)
	}
	const genericAll, maximal = 0x10000000, 0x02000000
	for _, bit := range []entity.AccessMask{genericAll, maximal, entity.SystemSecurity} {
		if err := files.Authorize(ctx, f.admin, h, nil, bit); !isDenied(err) {
			t.Errorf("a privileged caller asking %#x: %v", bit, err)
		}
	}
	admin := entity.Identity{Principal: f.admin.Principal, Authenticated: true, Admin: true}
	narrowed := admin
	narrowed.ReadOnly = entity.NarrowedByShare
	for _, id := range []entity.Identity{admin, narrowed} {
		if err := files.Authorize(ctx, id, h, nil, entity.SystemSecurity); err != nil {
			t.Errorf("an administrator asking system security, narrowed %v: %v", id.ReadOnly, err)
		}
	}
}
