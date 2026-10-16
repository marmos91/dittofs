package kvstoretest

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

func isCode(err error, code merrs.ErrorCode) bool {
	var se *merrs.StoreError
	return errors.As(err, &se) && se.Code == code
}

func testRename(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldUpcase)
	ns, files := f.s.Namespace(), f.s.Files()
	a, _ := f.create(f.admin, f.root, "a", directory(0o755))
	b, bdir := f.create(f.admin, f.root, "b", directory(0o755))
	h, file := f.create(f.admin, a, "note", regular(0o644))

	// Across directories: the file is the same file, at its new name only.
	_, err := ns.Rename(ctx, f.admin, a, []byte("note"), b, []byte("Note.txt"))
	must(t, err)
	if _, got, err := ns.Lookup(ctx, f.admin, b, []byte("note.TXT")); err != nil || got.ID != file.ID {
		t.Fatalf("lookup at the new name: %v, %v", got.ID, err)
	}
	if _, _, err := ns.Lookup(ctx, f.admin, a, []byte("note")); !merrs.IsNotFoundError(err) {
		t.Errorf("old name still resolves: %v", err)
	}
	if id, err := files.Resolve(ctx, f.share.ID, h); err != nil || id != file.ID {
		t.Errorf("the handle changed: %v, %v", id, err)
	}
	// A change of case only, on a case-insensitive share: one entry, the new
	// spelling.
	_, err = ns.Rename(ctx, f.admin, b, []byte("note.txt"), b, []byte("NOTE.txt"))
	must(t, err)
	got, _ := names(t, ns.Entries(ctx, f.admin, b, 0))
	if len(got) != 1 || got[0] != "NOTE.txt" {
		t.Errorf("entries after a case change: %v", got)
	}
	// A directory moved to another parent: its parent follows it.
	_, err = ns.Rename(ctx, f.admin, f.root, []byte("a"), b, []byte("a"))
	must(t, err)
	if moved, _ := files.Get(ctx, f.admin, a); moved.Parent != bdir.ID {
		t.Errorf("moved directory's parent %v, want %v", moved.Parent, bdir.ID)
	}
}

func testRenameOver(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns := f.s.Namespace()
	_, x := f.create(f.admin, f.root, "x", regular(0o644))
	_, y := f.create(f.admin, f.root, "y", regular(0o644))
	o, err := ns.Rename(ctx, f.admin, f.root, []byte("x"), f.root, []byte("y"))
	if err != nil || o.File != y.ID {
		t.Fatalf("file over file: %v, %v; want %v orphaned", o, err, y.ID)
	}
	if _, got, _ := ns.Lookup(ctx, f.admin, f.root, []byte("y")); got.ID != x.ID {
		t.Errorf("y names %v", got.ID)
	}
	f.create(f.admin, f.root, "d1", directory(0o755))
	_, d2 := f.create(f.admin, f.root, "d2", directory(0o755))
	full, _ := f.create(f.admin, f.root, "full", directory(0o755))
	f.create(f.admin, full, "inside", regular(0o644))
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("d1"), f.root, []byte("full")); !isNotEmpty(err) {
		t.Errorf("directory over a non-empty one: %v", err)
	}
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("d1"), f.root, []byte("y")); !isCode(err, merrs.ErrNotDirectory) {
		t.Errorf("directory over a file: %v", err)
	}
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("y"), f.root, []byte("d2")); !isCode(err, merrs.ErrIsDirectory) {
		t.Errorf("file over a directory: %v", err)
	}
	if o, err := ns.Rename(ctx, f.admin, f.root, []byte("d1"), f.root, []byte("d2")); err != nil || o.File != d2.ID {
		t.Errorf("directory over an empty one: %v, %v", o, err)
	}
	// Onto another name of the same file: nothing changes.
	h, _ := f.create(f.admin, f.root, "p", regular(0o644))
	must(t, ns.Link(ctx, f.admin, f.root, []byte("q"), h))
	if _, err := ns.Rename(ctx, f.admin, f.root, []byte("p"), f.root, []byte("q")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"p", "q"} {
		if _, _, err := ns.Lookup(ctx, f.admin, f.root, []byte(n)); err != nil {
			t.Errorf("%s gone after a rename onto its own link: %v", n, err)
		}
	}
}

// RFC 7 §12.1's sequential loop row: the one check that fails a build with
// no loop check, whatever the backend.
func testRenameUnderItself(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns := f.s.Namespace()
	a, _ := f.create(f.admin, f.root, "a", directory(0o755))
	b, _ := f.create(f.admin, a, "b", directory(0o755))
	c, _ := f.create(f.admin, b, "c", directory(0o755))
	for name, dst := range map[string]entity.Handle{"its child": b, "its grandchild": c, "itself": a} {
		if _, err := ns.Rename(ctx, f.admin, f.root, []byte("a"), dst, []byte("a")); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("under %s: %v", name, err)
		}
	}
}

// RFC 7 §12.1's concurrent loop row: A under B and B under A, in both
// commit orders: one is refused and no unreachable cycle is left.
func testRenameLoopRace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for _, aHeld := range []bool{true, false} {
			name := b.name + "/A under B held"
			if !aHeld {
				name = b.name + "/B under A held"
			}
			t.Run(name, func(t *testing.T) {
				f, h := newHeldFS(t, b, entity.FoldIdentity)
				ns := f.s.Namespace()
				ah, _ := f.create(f.admin, f.root, "A", directory(0o755))
				bh, _ := f.create(f.admin, f.root, "B", directory(0o755))
				aUnderB := func() error { _, err := ns.Rename(ctx, f.admin, f.root, []byte("A"), bh, []byte("A")); return err }
				bUnderA := func() error { _, err := ns.Rename(ctx, f.admin, f.root, []byte("B"), ah, []byte("B")); return err }
				first, second := aUnderB, bUnderA
				if !aHeld {
					first, second = bUnderA, aUnderB
				}
				outer, inner := race(h, first, second)
				if inner != nil || outer == nil {
					t.Fatalf("committed first: %v; committed second: %v; want only the first to succeed", inner, outer)
				}
				// Both directories still reach the root.
				for _, d := range []entity.Handle{ah, bh} {
					cur, err := f.s.Files().Get(ctx, f.admin, d)
					must(t, err)
					for steps := 0; !cur.IsRoot(); steps++ {
						if steps > 3 {
							t.Fatal("a directory no longer reaches the root")
						}
						cur, err = f.s.Files().Get(ctx, f.admin, entity.NewHandle(f.share.ID, cur.Parent))
						must(t, err)
					}
				}
			})
		}
	}
}

// RFC 7 §12.1's rename-over row: a create racing into the directory a
// rename replaces, in both commit orders.
func testRenameOverRacesCreate(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for _, renameHeld := range []bool{true, false} {
			name := b.name + "/create commits first"
			if !renameHeld {
				name = b.name + "/rename commits first"
			}
			t.Run(name, func(t *testing.T) {
				f, h := newHeldFS(t, b, entity.FoldIdentity)
				ns := f.s.Namespace()
				f.create(f.admin, f.root, "src", directory(0o755))
				target, _ := f.create(f.admin, f.root, "target", directory(0o755))
				rename := func() error {
					_, err := ns.Rename(ctx, f.admin, f.root, []byte("src"), f.root, []byte("target"))
					return err
				}
				create := func() error { _, _, err := ns.Create(ctx, f.admin, target, []byte("x"), regular(0o644)); return err }
				var renameErr, createErr error
				if renameHeld {
					renameErr, createErr = race(h, rename, create)
					if createErr != nil || !isNotEmpty(renameErr) {
						t.Fatalf("create %v, rename %v", createErr, renameErr)
					}
				} else {
					createErr, renameErr = race(h, create, rename)
					if renameErr != nil || !errors.Is(createErr, entity.ErrStale) {
						t.Fatalf("rename %v, create %v", renameErr, createErr)
					}
				}
			})
		}
	}
}

// RFC 7 §4.1 and §12.1's link count row: a directory's link count is 2 plus
// its subdirectories, the root's included, through a create, a move, a
// rename over an empty directory and a remove, before and after a fold; and
// RFC 16 §6.1's invariant, Subdirs with its deltas equals the entries that
// name a directory.
func testSubdirs(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	links := func(h entity.Handle) uint64 {
		t.Helper()
		got, err := files.Get(ctx, f.admin, h)
		must(t, err)
		var dirs int64
		for l, err := range ns.Entries(ctx, f.admin, h, 0) {
			must(t, err)
			if l.Entry.Type == entity.Directory {
				dirs++
			}
		}
		if got.Subdirs != dirs {
			t.Errorf("Subdirs %d, entries naming a directory %d", got.Subdirs, dirs)
		}
		return got.LinkCount()
	}
	want := func(step string, h entity.Handle, n uint64) {
		t.Helper()
		if got := links(h); got != n {
			t.Errorf("%s: link count %d, want %d", step, got, n)
		}
	}
	a, _ := f.create(f.admin, f.root, "a", directory(0o755))
	b, _ := f.create(f.admin, f.root, "b", directory(0o755))
	f.create(f.admin, f.root, "x", regular(0o644))
	want("two subdirectories and a file", f.root, 4)
	f.create(f.admin, a, "c", directory(0o755))
	want("a subdirectory created", a, 3)
	_, err := ns.Rename(ctx, f.admin, a, []byte("c"), b, []byte("c"))
	must(t, err)
	want("moved out", a, 2)
	want("moved in", b, 3)
	f.create(f.admin, b, "e", directory(0o755))
	want("another created", b, 4)
	_, err = ns.Rename(ctx, f.admin, b, []byte("e"), b, []byte("d"))
	must(t, err)
	want("renamed in place", b, 4)
	_, err = ns.Rename(ctx, f.admin, b, []byte("c"), b, []byte("d"))
	must(t, err)
	want("renamed over an empty directory", b, 3)
	_, err = ns.Unlink(ctx, f.admin, b, []byte("d"))
	must(t, err)
	want("removed", b, 2)
	for _, h := range []entity.Handle{f.root, a, b} {
		_, id, err := h.Parse()
		must(t, err)
		for more := true; more; {
			var recount bool
			more, recount, err = ns.FoldDirectory(ctx, f.share.ID, id)
			must(t, err)
			if recount {
				t.Error("a consistent directory asked for a recount")
			}
		}
	}
	want("root, folded", f.root, 4)
	want("b, folded", b, 2)
}
