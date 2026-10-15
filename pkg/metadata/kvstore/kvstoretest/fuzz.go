package kvstoretest

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// FuzzNamespace reads its input as a stream of namespace operations —
// create, mkdir, link, unlink, rename, chmod — over a few directories and a
// small pool of names, so they collide. It applies each to the store and to
// a model tree, and after each one compares whether it succeeded, every
// directory's entries and every link count; at the end the store's Check
// must pass. The seeds run in a plain go test.
func FuzzNamespace(f *testing.F, open Opener) {
	for _, seed := range [][]byte{
		{opMkdir, 0, 0, 0, opCreate, 1, 1, 0, opRename, 0, 0, 4},                  // a directory moved into itself
		{opCreate, 0, 0, 0, opCreate, 0, 1, 0, opRename, 0, 0, 1},                 // a file renamed over a file
		{opMkdir, 0, 0, 0, opMkdir, 0, 1, 0, opRename, 0, 0, 1},                   // a directory over an empty one
		{opCreate, 0, 0, 0, opLink, 0, 0, 1, opUnlink, 0, 0, 0},                   // a link, then one name goes
		{opMkdir, 0, 0, 0, opCreate, 1, 0, 0, opUnlink, 0, 0, 0},                  // a directory that is not empty
		{opMkdir, 0, 0, 0, opMkdir, 1, 1, 0, opRename, 0, 0, 8, opChmod, 1, 0, 0}, // moving a parent under its child
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) { runNamespaceOps(t, open, ops) })
}

const (
	opCreate byte = iota
	opMkdir
	opLink
	opUnlink
	opRename
	opChmod
	opKinds
)

// maxOps bounds one input, so a long one costs no more than a short one.
const maxOps = 48

var opNames = []string{"a", "b", "c"}

// mnode is a file or directory of the model tree.
type mnode struct {
	dir     bool
	links   int // entries naming it
	entries map[string]*mnode
	parent  *mnode
	id      entity.FileID
	h       entity.Handle
}

func (n *mnode) subdirs() int {
	c := 0
	for _, e := range n.entries {
		if e.dir {
			c++
		}
	}
	return c
}

// under reports whether n is d or lies beneath it.
func (n *mnode) under(d *mnode) bool {
	for x := n; x != nil; x = x.parent {
		if x == d {
			return true
		}
	}
	return false
}

func runNamespaceOps(t *testing.T, open Opener, ops []byte) {
	fx := newFS(t, open, entity.FoldIdentity)
	ns, files := fx.s.Namespace(), fx.s.Files()
	_, rootID, err := fx.root.Parse()
	must(t, err)
	root := &mnode{dir: true, entries: map[string]*mnode{}, id: rootID, h: fx.root}
	dirs, plain := []*mnode{root}, []*mnode{}

	drop := func(n *mnode) {
		n.links--
		if n.dir {
			dirs = slices.DeleteFunc(dirs, func(x *mnode) bool { return x == n })
		} else if n.links == 0 {
			plain = slices.DeleteFunc(plain, func(x *mnode) bool { return x == n })
		}
	}

	for step := 0; step+4 <= len(ops) && step/4 < maxOps; step += 4 {
		kind, x, y, z := ops[step]%opKinds, int(ops[step+1]), int(ops[step+2]), int(ops[step+3])
		var desc string
		var want bool
		var err error
		switch kind {
		case opCreate, opMkdir:
			d, name := dirs[x%len(dirs)], opNames[y%len(opNames)]
			desc = fmt.Sprintf("create %q dir=%v in %v", name, kind == opMkdir, d.id)
			want = d.entries[name] == nil
			nf := regular(0o644)
			if kind == opMkdir {
				nf = directory(0o755)
			}
			var h entity.Handle
			var made entity.File
			h, made, err = ns.Create(ctx, fx.admin, d.h, []byte(name), nf)
			if err == nil && want {
				n := &mnode{dir: kind == opMkdir, links: 1, id: made.ID, h: h}
				if n.dir {
					n.entries, n.parent = map[string]*mnode{}, d
					dirs = append(dirs, n)
				} else {
					plain = append(plain, n)
				}
				d.entries[name] = n
			}
		case opLink:
			if len(plain) == 0 {
				continue
			}
			target, d, name := plain[x%len(plain)], dirs[y%len(dirs)], opNames[z%len(opNames)]
			desc = fmt.Sprintf("link %v as %q in %v", target.id, name, d.id)
			want = d.entries[name] == nil
			err = ns.Link(ctx, fx.admin, d.h, []byte(name), target.h)
			if err == nil && want {
				target.links++
				d.entries[name] = target
			}
		case opUnlink:
			d, name := dirs[x%len(dirs)], opNames[y%len(opNames)]
			desc = fmt.Sprintf("unlink %q in %v", name, d.id)
			e := d.entries[name]
			want = e != nil && (!e.dir || len(e.entries) == 0)
			_, err = ns.Unlink(ctx, fx.admin, d.h, []byte(name))
			if err == nil && want {
				delete(d.entries, name)
				drop(e)
			}
		case opRename:
			sd, sn := dirs[x%len(dirs)], opNames[y%len(opNames)]
			dd, dn := dirs[(z>>2)%len(dirs)], opNames[z%len(opNames)]
			desc = fmt.Sprintf("rename %q in %v to %q in %v", sn, sd.id, dn, dd.id)
			s, d := sd.entries[sn], dd.entries[dn]
			switch {
			case s == nil, s.dir && dd.under(s):
				want = false
			case d == s:
				want = true // two names of one file, or one name to itself: nothing changes
			case d != nil && (d.dir != s.dir || d.dir && len(d.entries) > 0):
				want = false
			default:
				want = true
			}
			_, err = ns.Rename(ctx, fx.admin, sd.h, []byte(sn), dd.h, []byte(dn))
			if err == nil && want && d != s {
				if d != nil {
					delete(dd.entries, dn)
					drop(d)
				}
				delete(sd.entries, sn)
				dd.entries[dn] = s
				if s.dir {
					s.parent = dd
				}
			}
		case opChmod:
			all := append(slices.Clone(dirs), plain...)
			n := all[x%len(all)]
			desc = fmt.Sprintf("chmod %v", n.id)
			want = true
			_, err = files.SetAttrs(ctx, fx.admin, n.h, entity.Attrs{}.WithMode(uint32(0o700|y&0o77)))
		}
		if (err == nil) != want {
			t.Fatalf("step %d, %s: error %v, want success %v", step/4, desc, err, want)
		}
		compareTree(t, fx, dirs, plain, step/4, desc)
	}
	if err := fx.s.Check(ctx); err != nil {
		t.Fatal(err)
	}
}

// compareTree checks every model directory's entries against the store's
// listing, and every node's link count against its record.
func compareTree(t *testing.T, fx *fs, dirs, plain []*mnode, step int, desc string) {
	t.Helper()
	ns, files := fx.s.Namespace(), fx.s.Files()
	for _, d := range dirs {
		got := map[string]entity.FileID{}
		for l, err := range ns.Entries(ctx, fx.admin, d.h, 0) {
			if err != nil {
				t.Fatalf("step %d, %s: listing %v: %v", step, desc, d.id, err)
			}
			got[string(l.Entry.Name)] = l.Entry.Child
		}
		want := map[string]entity.FileID{}
		for name, e := range d.entries {
			want[name] = e.id
		}
		if !maps.Equal(got, want) {
			t.Fatalf("step %d, %s: %v lists %v, model has %v", step, desc, d.id, got, want)
		}
		f, err := files.Get(ctx, fx.admin, d.h)
		if err != nil {
			t.Fatalf("step %d, %s: get %v: %v", step, desc, d.id, err)
		}
		if lc := f.LinkCount(); lc != uint64(2+d.subdirs()) {
			t.Fatalf("step %d, %s: directory %v link count %d, model %d", step, desc, d.id, lc, 2+d.subdirs())
		}
	}
	for _, n := range plain {
		f, err := files.Get(ctx, fx.admin, n.h)
		if err != nil {
			t.Fatalf("step %d, %s: get %v: %v", step, desc, n.id, err)
		}
		if f.Nlink != uint32(n.links) {
			t.Fatalf("step %d, %s: %v Nlink %d, model %d", step, desc, n.id, f.Nlink, n.links)
		}
	}
}

// Writers racing over one small tree: each runs random namespace operations
// on shared directories and names, so they conflict and retry. Any of them
// may fail as an operation of its own would, never otherwise; afterwards
// every file's Nlink is its entries, every directory's link count its
// subdirectories, and Check holds. This is the path a single sequence never
// takes: the conflict and the retry.
func testConcurrentNamespace(t *testing.T, kvo Opener) {
	const writers, steps = 8, 150
	fx := newFS(t, kvo, entity.FoldIdentity)
	ns, files := fx.s.Namespace(), fx.s.Files()
	var mu sync.Mutex
	dirs, plain := []entity.Handle{fx.root}, []entity.Handle{}
	for i := range 3 {
		h, _ := fx.create(fx.admin, fx.root, fmt.Sprintf("d%d", i), directory(0o755))
		dirs = append(dirs, h)
	}
	pick := func(rng *rand.Rand, from *[]entity.Handle) entity.Handle {
		mu.Lock()
		defer mu.Unlock()
		if len(*from) == 0 {
			return nil
		}
		return (*from)[rng.IntN(len(*from))]
	}
	add := func(to *[]entity.Handle, h entity.Handle) { mu.Lock(); *to = append(*to, h); mu.Unlock() }

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			for range steps {
				d, name := pick(rng, &dirs), []byte(opNames[rng.IntN(len(opNames))])
				var err error
				switch rng.IntN(5) {
				case 0:
					var h entity.Handle
					if h, _, err = ns.Create(ctx, fx.admin, d, name, regular(0o644)); err == nil {
						add(&plain, h)
					}
				case 1:
					var h entity.Handle
					if h, _, err = ns.Create(ctx, fx.admin, d, name, directory(0o755)); err == nil {
						add(&dirs, h)
					}
				case 2:
					if target := pick(rng, &plain); target != nil {
						err = ns.Link(ctx, fx.admin, d, name, target)
					}
				case 3:
					_, err = ns.Unlink(ctx, fx.admin, d, name)
				case 4:
					to := []byte(opNames[rng.IntN(len(opNames))])
					_, err = ns.Rename(ctx, fx.admin, d, name, pick(rng, &dirs), to)
				}
				if err != nil && !expected(err) {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a writer failed unexpectedly: %v", err)
	}

	// Walk the tree: every entry reachable from the root, every link count.
	names := map[entity.FileID]uint32{}
	queue := []entity.Handle{fx.root}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		sub := uint64(0)
		for l, err := range ns.Entries(ctx, fx.admin, d, 0) {
			must(t, err)
			names[l.Entry.Child]++
			if l.Entry.IsDir() {
				sub++
				queue = append(queue, entity.NewHandle(fx.share.ID, l.Entry.Child))
			}
		}
		got, err := files.Get(ctx, fx.admin, d)
		must(t, err)
		if got.LinkCount() != 2+sub {
			t.Errorf("directory %v: link count %d with %d subdirectories", got.ID, got.LinkCount(), sub)
		}
	}
	for _, h := range plain {
		got, err := files.Get(ctx, fx.admin, h)
		if errors.Is(err, entity.ErrStale) {
			continue // released
		}
		must(t, err)
		if got.Nlink != names[got.ID] {
			t.Errorf("%v: Nlink %d, %d entries", got.ID, got.Nlink, names[got.ID])
		}
	}
	must(t, fx.s.Check(ctx))
}

// expected is an error an operation meets on its own merits: a name taken
// or missing, a directory not empty, a move under itself, a handle whose
// file another writer removed. Anything else is a bug.
func expected(err error) bool {
	var se *merrs.StoreError
	return errors.As(err, &se) || errors.Is(err, entity.ErrStale)
}
