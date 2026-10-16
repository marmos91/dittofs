package kvstoretest

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// What a change costs, in keys read and written: a rename touches its record
// and its name rows and nothing else.
func testCosts(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cp := s.ControlPlane()
	u := user("alice", uid("1000"))
	must(t, cp.PutUser(ctx, u))
	sh := share("photos", "/photos")
	must(t, cp.PutShare(ctx, sh))
	c.Take()

	u.Name = "alicia"
	must(t, cp.PutUser(ctx, u))
	// Read the record and the new name row; write both; delete the old row.
	if got, want := c.Take(), (kv.Counts{Gets: 2, Sets: 2, Deletes: 1}); got != want {
		t.Errorf("user rename: %+v, want %+v", got, want)
	}

	sh.Name = "pictures"
	must(t, cp.PutShare(ctx, sh))
	// Read the info and state records, the new name row and ShareList; write
	// the info, the name row and ShareList; delete the old name row. The path
	// did not change, so no path row is read.
	if got, want := c.Take(), (kv.Counts{Gets: 4, Sets: 3, Deletes: 1}); got != want {
		t.Errorf("share rename: %+v, want %+v", got, want)
	}

	must(t, cp.PutShare(ctx, sh))
	// The info and state records only.
	if got, want := c.Take(), (kv.Counts{Gets: 2}); got != want {
		t.Errorf("unchanged share put: %+v, want %+v", got, want)
	}
}

// The keys each namespace call reads and writes, pinned so a change to an
// operation's cost shows in review rather than in a profile. None scans but
// a listing, which scans its page, and none range-guards but a directory's
// removal, which proves it empty.
func testNamespaceCosts(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	ns, files := f.s.Namespace(), f.s.Files()
	d1, _ := f.create(f.admin, f.root, "d1", directory(0o755))
	d2, _ := f.create(f.admin, f.root, "d2", directory(0o755))
	h, _ := f.create(f.admin, d1, "a", regular(0o644))
	for i := range 20 {
		f.create(f.admin, d1, fmt.Sprintf("f%d", i), regular(0o644))
	}
	for _, c := range []struct {
		name string
		op   func() error
		want kv.Counts
	}{
		// The directory, its share's grant and bound settings, the entry, the file.
		{"lookup", func() error { _, _, err := ns.Lookup(ctx, f.admin, d1, []byte("a")); return err },
			kv.Counts{Gets: 4}},
		{"getattr", func() error { _, err := files.Get(ctx, f.admin, h); return err }, kv.Counts{Gets: 2}},
		// 21 entries scanned, the last one ending the page.
		{"list 21 entries", func() error {
			for _, err := range ns.Entries(ctx, f.admin, d1, 0) {
				if err != nil {
					return err
				}
			}
			return nil
		}, kv.Counts{Gets: 2, Scanned: 21}},
		// The file and its version-index entry moved.
		{"chmod", func() error { _, err := files.SetAttrs(ctx, f.admin, h, entity.Attrs{}.WithMode(0o600)); return err },
			kv.Counts{Gets: 3, Sets: 2, Deletes: 1}},
		// The entry, the file and its index entry, the directory's time delta and its index entry.
		{"link", func() error { return ns.Link(ctx, f.admin, d2, []byte("b"), h) }, kv.Counts{Gets: 4, Sets: 5, Deletes: 1}},
		{"rename in a directory", func() error { _, err := ns.Rename(ctx, f.admin, d1, []byte("a"), d1, []byte("c")); return err },
			kv.Counts{Gets: 7, Sets: 5, Deletes: 2}},
		// A time delta under each directory.
		{"rename across", func() error { _, err := ns.Rename(ctx, f.admin, d1, []byte("c"), d2, []byte("e")); return err },
			kv.Counts{Gets: 7, Sets: 7, Deletes: 2}},
		// The replaced file loses its entry: its record, index entry and pending release too.
		{"rename over", func() error { _, err := ns.Rename(ctx, f.admin, d1, []byte("f0"), d1, []byte("f1")); return err },
			kv.Counts{Gets: 10, Sets: 9, Deletes: 4}},
		{"unlink, a name left", func() error { _, err := ns.Unlink(ctx, f.admin, d2, []byte("b")); return err },
			kv.Counts{Gets: 5, Sets: 4, Deletes: 2}},
		// The last name: the pending release and its index entry as well.
		{"unlink, the last name", func() error { _, err := ns.Unlink(ctx, f.admin, d2, []byte("e")); return err },
			kv.Counts{Gets: 5, Sets: 6, Deletes: 2}},
		{"rmdir", func() error { _, err := ns.Unlink(ctx, f.admin, f.root, []byte("d2")); return err },
			kv.Counts{Gets: 5, RangeGuards: 1, Sets: 6, Deletes: 2}},
	} {
		f.c.Take()
		if err := c.op(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := f.c.Take(); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
