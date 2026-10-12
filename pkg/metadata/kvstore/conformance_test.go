package kvstore_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// RFC 7 §12.1's cookie row: deleting the entry a cookie came from, and the
// entries before it, skips and repeats nothing untouched.
func TestCookieSurvivesDeletes(t *testing.T) {
	f := newFS(t, entity.FoldIdentity)
	ns := f.s.Namespace()
	for i := range 700 {
		f.create(f.admin, f.root, fmt.Sprintf("e%03d", i), regular(0o644))
	}
	seen := map[string]int{}
	after := uint64(0)
	for page := 0; ; page++ {
		var last string
		count := 0
		for l, err := range ns.Entries(ctx, f.admin, f.root, after) {
			must(t, err)
			seen[string(l.Entry.Name)]++
			last, after = string(l.Entry.Name), l.Cookie
			if count++; count == 100 {
				break
			}
		}
		if count == 0 {
			break
		}
		// Remove the entry the cookie came from, before resuming from it.
		_, err := ns.Unlink(ctx, f.admin, f.root, []byte(last))
		must(t, err)
	}
	for i := range 700 {
		name := fmt.Sprintf("e%03d", i)
		if seen[name] != 1 {
			t.Fatalf("%s returned %d times", name, seen[name])
		}
	}
}

// RFC 7 §12.2's cost rows that a key count can answer.
func TestReadCosts(t *testing.T) {
	f := newFS(t, entity.FoldIdentity)
	files := f.s.Files()
	h, _ := f.create(f.admin, f.root, "x", regular(0o644))

	// GETATTR of a regular file: the grant and the file record.
	f.c.Take()
	must(t, errOf(files.Get(ctx, f.admin, h)))
	if n := f.c.Take(); n.Gets != 2 || n.Scanned != 0 {
		t.Errorf("GETATTR read %+v", n)
	}

	// Handle resolution reads the file only, at any depth.
	dir := f.root
	for i := range 16 {
		dir, _ = f.create(f.admin, dir, fmt.Sprintf("d%d", i), directory(0o755))
	}
	deep, _ := f.create(f.admin, dir, "leaf", regular(0o644))
	for _, hh := range []entity.Handle{h, deep} {
		f.c.Take()
		_, err := files.Resolve(ctx, f.share.ID, hh)
		must(t, err)
		if n := f.c.Take(); n.Gets != 1 || n.Scanned != 0 {
			t.Errorf("Resolve read %+v", n)
		}
	}

	// A listing page reads at most a page of entries, whatever the
	// directory's size.
	big, _ := f.create(f.admin, f.root, "big", directory(0o755))
	for i := range 1000 {
		f.create(f.admin, big, fmt.Sprintf("n%04d", i), regular(0o644))
	}
	f.c.Take()
	for range f.s.Namespace().Entries(ctx, f.admin, big, 0) {
		break
	}
	if n := f.c.Take(); n.Scanned > 257 {
		t.Errorf("one page scanned %d entries", n.Scanned)
	}
}

// RFC 7 §3.6: creates in one directory do not conflict with each other, so
// 64 at once all succeed and the directory holds all of them.
func TestParallelCreatesInOneDirectory(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			f, _ := newHeldFS(t, b, entity.FoldIdentity)
			ns := f.s.Namespace()
			dir, _ := f.create(f.admin, f.root, "busy", directory(0o777))
			var wg sync.WaitGroup
			errs := make(chan error, 64)
			for i := range 64 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _, err := ns.Create(ctx, f.admin, dir, []byte(fmt.Sprintf("c%02d", i)), regular(0o644))
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				must(t, err)
			}
			got, _ := names(t, ns.Entries(ctx, f.admin, dir, 0))
			if len(got) != 64 {
				t.Fatalf("%d entries after 64 creates", len(got))
			}
		})
	}
}
