package kvstoretest

import (
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
		"write":  files.Authorize(ctx, alice, h, entity.WriteData),
		"append": files.Authorize(ctx, alice, h, entity.AppendData),
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
	if e := files.Authorize(ctx, alice, h, entity.AppendData); e != nil {
		t.Errorf("append to an append-only file: %v", e)
	}
	if e := files.Authorize(ctx, alice, h, entity.WriteData); !isPerm(e) {
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
	if e := files.Authorize(ctx, carol, h, entity.ReadData); e != nil {
		t.Errorf("group allow: %v", e)
	}
	if e := files.Authorize(ctx, bob, h, entity.ReadData); !isDenied(e) {
		t.Errorf("deny before the group allow: %v", e)
	}
	if e := files.Authorize(ctx, alice, h, entity.ReadData); !isDenied(e) {
		t.Errorf("no entry for alice: %v", e)
	}
	if e := files.Authorize(ctx, carol, h, entity.WriteData); !isDenied(e) {
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
	must(t, f.s.ControlPlane().SetGrant(ctx, entity.ShareGrant{Share: f.share.ID, Principal: alice.Principal, Access: entity.AccessNone}))
	if _, e := f.s.Files().Get(ctx, alice, h); !isDenied(e) {
		t.Errorf("call after the grant was removed: %v", e)
	}
}
