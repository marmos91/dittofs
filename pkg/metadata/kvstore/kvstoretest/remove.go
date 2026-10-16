package kvstoretest

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

func isNotEmpty(err error) bool {
	var se *merrs.StoreError
	return errors.As(err, &se) && se.Code == merrs.ErrNotEmpty
}

func pending(t *testing.T, f *fs) []entity.FileID {
	t.Helper()
	var ids []entity.FileID
	root, err := f.s.Files().Get(ctx, f.admin, f.root)
	must(t, err)
	for id, err := range f.s.Namespace().PendingReleases(ctx, f.share.ID, root.Shard) {
		must(t, err)
		ids = append(ids, id)
	}
	return ids
}

func testUnlinkAndLink(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	h, file := f.create(f.admin, f.root, "a", regular(0o644))
	must(t, ns.Link(ctx, f.admin, f.root, []byte("b"), h))
	if got, _ := files.Get(ctx, f.admin, h); got.Nlink != 2 {
		t.Fatalf("Nlink after link = %d", got.Nlink)
	}
	if o, err := ns.Unlink(ctx, f.admin, f.root, []byte("a")); err != nil || o.File != (entity.FileID{}) {
		t.Fatalf("unlink of one of two names: %v, %v", o, err)
	}
	if len(pending(t, f)) != 0 {
		t.Error("a file with a name left has a pending release")
	}
	o, err := ns.Unlink(ctx, f.admin, f.root, []byte("b"))
	if err != nil || o.File != file.ID {
		t.Fatalf("unlink of the last name: %v, %v", o, err)
	}
	if got := pending(t, f); !slices.Equal(got, []entity.FileID{file.ID}) {
		t.Errorf("pending releases %v", got)
	}
	if _, _, err := ns.Lookup(ctx, f.admin, f.root, []byte("b")); !merrs.IsNotFoundError(err) {
		t.Errorf("lookup of an unlinked name: %v", err)
	}
	// A file with no entry left cannot be given one back.
	if err := ns.Link(ctx, f.admin, f.root, []byte("c"), h); !errors.Is(err, entity.ErrStale) {
		t.Errorf("link of an unlinked file: %v", err)
	}
	// The name is free again at once.
	f.create(f.admin, f.root, "b", regular(0o644))
	if _, err := ns.Unlink(ctx, f.admin, f.root, []byte("nope")); !merrs.IsNotFoundError(err) {
		t.Errorf("unlink of a missing name: %v", err)
	}
	d, _ := f.create(f.admin, f.root, "d", directory(0o755))
	if err := ns.Link(ctx, f.admin, f.root, []byte("d2"), d); err == nil {
		t.Error("hard link to a directory")
	}
}

func testRemoveDirectory(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns := f.s.Namespace()
	d, made := f.create(f.admin, f.root, "d", directory(0o755))
	f.create(f.admin, d, "x", regular(0o644))
	if _, err := ns.Unlink(ctx, f.admin, f.root, []byte("d")); !isNotEmpty(err) {
		t.Fatalf("removing a non-empty directory: %v", err)
	}
	_, err := ns.Unlink(ctx, f.admin, d, []byte("x"))
	must(t, err)
	o, err := ns.Unlink(ctx, f.admin, f.root, []byte("d"))
	if err != nil || o.File != made.ID {
		t.Fatalf("removing the empty directory: %v, %v", o, err)
	}
	if _, _, err := ns.Create(ctx, f.admin, d, []byte("y"), regular(0o644)); !errors.Is(err, entity.ErrStale) {
		t.Errorf("create in a removed directory: %v", err)
	}
}

// RFC 7 §12.1's rmdir-against-create row, on every backend and in both
// commit orders: exactly one commits, and nothing is left under a removed
// directory.
func testRemoveDirectoryRacesCreate(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for _, removalHeld := range []bool{true, false} {
			name := b.name + "/create commits first"
			if !removalHeld {
				name = b.name + "/removal commits first"
			}
			t.Run(name, func(t *testing.T) {
				f, h := newHeldFS(t, b, entity.FoldIdentity)
				ns := f.s.Namespace()
				d, _ := f.create(f.admin, f.root, "d", directory(0o755))
				remove := func() error { _, err := ns.Unlink(ctx, f.admin, f.root, []byte("d")); return err }
				create := func() error { _, _, err := ns.Create(ctx, f.admin, d, []byte("x"), regular(0o644)); return err }
				var removeErr, createErr error
				if removalHeld {
					removeErr, createErr = race(h, remove, create)
				} else {
					createErr, removeErr = race(h, create, remove)
				}
				if removalHeld {
					if createErr != nil || !isNotEmpty(removeErr) {
						t.Fatalf("create %v, removal %v; want the create, and the removal refused as not empty", createErr, removeErr)
					}
				} else if removeErr != nil || !errors.Is(createErr, entity.ErrStale) {
					t.Fatalf("removal %v, create %v; want the removal, and the create refused as stale", removeErr, createErr)
				}
			})
		}
	}
}

// RFC 7 §12.1's removal rights, as far as the mode decides them.
func testRemovalRights(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns := f.s.Namespace()
	alice, bob := f.member("alice", entity.Principal{}), f.member("bob", entity.Principal{})
	// A 0777 directory gives everyone delete-a-child, so bob removes
	// alice's 0600 file he has no right on.
	open, _ := f.create(f.admin, f.root, "open", directory(0o777))
	f.create(alice, open, "a", regular(0o600))
	if _, err := ns.Unlink(ctx, bob, open, []byte("a")); err != nil {
		t.Errorf("delete-a-child on the directory: %v", err)
	}
	// A 0755 directory of alice's: bob has no right to remove from it.
	mine, _ := f.create(alice, open, "mine", directory(0o755))
	f.create(alice, mine, "b", regular(0o666))
	if _, err := ns.Unlink(ctx, bob, mine, []byte("b")); !isDenied(err) {
		t.Errorf("neither right: %v", err)
	}
	// A sticky 1777 directory of admin's: bob may not remove alice's file.
	tmp, _ := f.create(f.admin, f.root, "tmp", directory(0o1777))
	f.create(alice, tmp, "c", regular(0o666))
	if _, err := ns.Unlink(ctx, bob, tmp, []byte("c")); !isDenied(err) {
		t.Errorf("another's file in another's sticky directory: %v", err)
	}
	if _, err := ns.Unlink(ctx, alice, tmp, []byte("c")); err != nil {
		t.Errorf("the owner in a sticky directory: %v", err)
	}
}

// RFC 7 §12.1's Nlink row: over random creates, links and unlinks, every
// file's Nlink equals the entries naming it after every transaction.
func testNlinkIsExactlyItsEntries(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	rng := rand.New(rand.NewPCG(1, 2))
	dirs := []entity.Handle{f.root}
	for i := range 3 {
		d, _ := f.create(f.admin, f.root, fmt.Sprintf("d%d", i), directory(0o755))
		dirs = append(dirs, d)
	}
	var handles []entity.Handle
	for step := range 400 {
		dir := dirs[rng.IntN(len(dirs))]
		name := []byte(fmt.Sprintf("n%d", rng.IntN(20)))
		switch rng.IntN(3) {
		case 0:
			if h, _, err := ns.Create(ctx, f.admin, dir, name, regular(0o644)); err == nil {
				handles = append(handles, h)
			}
		case 1:
			if len(handles) > 0 {
				_ = ns.Link(ctx, f.admin, dir, name, handles[rng.IntN(len(handles))])
			}
		case 2:
			_, _ = ns.Unlink(ctx, f.admin, dir, name)
		}
		names := map[entity.FileID]uint32{}
		for _, d := range dirs {
			for l, err := range ns.Entries(ctx, f.admin, d, 0) {
				must(t, err)
				names[l.Entry.Child]++
			}
		}
		for _, h := range handles {
			got, err := files.Get(ctx, f.admin, h)
			must(t, err)
			if got.Nlink != names[got.ID] {
				t.Fatalf("step %d: %v has Nlink %d and %d entries", step, got.ID, got.Nlink, names[got.ID])
			}
		}
	}
}

// POSIX asks write and search on a directory to change its entries: with w
// and no x a caller adds, removes, renames and links nothing, as it already
// looks up nothing. A rename needs search on its source directory too.
func testWriteWithoutSearch(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns := f.s.Namespace()
	alice := f.member("alice", entity.Principal{})
	d, _ := f.create(f.admin, f.root, "d", directory(0o777))
	f.create(alice, d, "victim", regular(0o644))
	f.create(alice, d, "mv", regular(0o644))
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	other, _ := f.create(alice, pub, "other", regular(0o644))
	must(t, errOf(f.s.Files().SetAttrs(ctx, f.admin, d, entity.Attrs{}.WithMode(0o772)))) // others: w, no x

	if _, _, err := ns.Lookup(ctx, alice, d, []byte("victim")); !isDenied(err) {
		t.Fatalf("lookup without x: %v; scenario invalid", err)
	}
	if _, _, err := ns.Create(ctx, alice, d, []byte("new"), regular(0o644)); !isDenied(err) {
		t.Errorf("create in a directory with w but no x: %v", err)
	}
	if _, err := ns.Unlink(ctx, alice, d, []byte("victim")); !isDenied(err) {
		t.Errorf("unlink in a directory with w but no x: %v", err)
	}
	if _, err := ns.Rename(ctx, alice, d, []byte("mv"), d, []byte("mv2")); !isDenied(err) {
		t.Errorf("rename in a directory with w but no x: %v", err)
	}
	if _, err := ns.Rename(ctx, alice, d, []byte("mv"), pub, []byte("mv2")); !isDenied(err) {
		t.Errorf("rename out of a directory with w but no x: %v", err)
	}
	if err := ns.Link(ctx, alice, d, []byte("lnk"), other); !isDenied(err) {
		t.Errorf("link into a directory with w but no x: %v", err)
	}
	if _, err := ns.Rename(ctx, alice, pub, []byte("other"), d, []byte("o2")); !isDenied(err) {
		t.Errorf("rename into a directory with w but no x: %v", err)
	}
	// With x back, each goes through.
	must(t, errOf(f.s.Files().SetAttrs(ctx, f.admin, d, entity.Attrs{}.WithMode(0o773))))
	if _, _, err := ns.Create(ctx, alice, d, []byte("new"), regular(0o644)); err != nil {
		t.Errorf("create with w and x: %v", err)
	}
	if _, err := ns.Unlink(ctx, alice, d, []byte("victim")); err != nil {
		t.Errorf("unlink with w and x: %v", err)
	}
}
