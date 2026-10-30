package vfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/sim"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// fuzzShare is a serving filesystem over one share, its shard served, an
// admin's call and the share's root, for a fuzz target to share across its
// inputs.
func fuzzShare(f *testing.F) (*Filesystem, vfs.Call, vfs.Handle) {
	ctx := context.Background()
	db := memKV(f)
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		f.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		f.Fatal(err)
	}
	cp := st.ControlPlane()
	admin := entity.User{Principal: entity.NewUser(), Name: "admin", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "100"}}}
	sh := entity.Share{ID: entity.NewShareID(), Name: "data", Path: "/data", Root: entity.NewFileID()}
	for _, err := range []error{
		cp.PutUser(ctx, admin), cp.PutShare(ctx, sh),
		cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: admin.Principal, Access: entity.AccessAdmin}),
	} {
		if err != nil {
			f.Fatal(err)
		}
	}
	tab, err := openstate.New(openstate.Deps{Records: st, Clock: sim.NewTestClock(time.Unix(0, 0)), Rand: rand.Reader})
	if err != nil {
		f.Fatal(err)
	}
	s, err := New(Deps{Meta: st, Open: tab, Engine: newFakeContent()})
	if err != nil {
		f.Fatal(err)
	}
	for _, err := range []error{tab.Start(ctx, s, s.Admission), s.Start(ctx), tab.Recovery().ServeShard(ctx, sh.ID, 0, 1)} {
		if err != nil {
			f.Fatal(err)
		}
	}
	f.Cleanup(func() { _ = s.Drain(ctx); _ = s.Stop(); tab.Stop() })
	return s, proven(admin.Principal, inLab), entity.NewHandle(sh.ID, sh.Root)
}

// FuzzNames: an arbitrary name given to Create, Link or Rename is refused
// ErrNameTooLong or ErrInvalid with nothing changed, or accepted and then
// found once by Lookup and once by ReadDir. Each input works in a directory
// of its own holding one file, "seed".
func FuzzNames(f *testing.F) {
	s, admin, root := fuzzShare(f)
	ctx := context.Background()
	for _, seed := range []string{"a", ".", "..", "", "a/b", "nul\x00", "seed", "Seed", "\xff\xfe", string(bytes.Repeat([]byte("x"), 255)), string(bytes.Repeat([]byte("x"), 256))} {
		f.Add([]byte(seed))
	}
	var dirs atomic.Uint64
	f.Fuzz(func(t *testing.T, name []byte) {
		for _, op := range []string{"create", "link", "rename"} {
			d, err := s.Create(ctx, admin, root, fmt.Appendf(nil, "dir%d", dirs.Add(1)), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o755)})
			if err != nil {
				t.Fatal(err)
			}
			seed, err := s.Create(ctx, admin, d.Handle, []byte("seed"), vfs.CreateArgs{Kind: vfs.CreateRegular})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"seed"}
			var opErr error
			switch op {
			case "create":
				_, opErr = s.Create(ctx, admin, d.Handle, name, vfs.CreateArgs{Kind: vfs.CreateRegular})
			case "link":
				_, opErr = s.Link(ctx, admin, d.Handle, name, seed.Handle)
			case "rename":
				_, opErr = s.Rename(ctx, admin, vfs.Target{H: d.Handle}, []byte("seed"), d.Handle, name, false)
			}
			switch {
			case string(name) == "seed" && op != "rename":
				if !errors.Is(opErr, vfs.ErrExist) {
					t.Fatalf("%s of the name taken: %v", op, opErr)
				}
			case opErr != nil:
				if !errors.Is(opErr, vfs.ErrNameTooLong) && !errors.Is(opErr, vfs.ErrInvalid) {
					t.Fatalf("%s of %q answered %v", op, name, opErr)
				}
			case op == "rename":
				want = []string{string(name)}
			default:
				want = append(want, string(name))
			}
			got, err := listAll(ctx, s, admin, d.Handle)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Sort(got); !slices.Equal(got, sorted(want)) {
				t.Fatalf("%s of %q (%v): listed %q, want %q", op, name, opErr, got, want)
			}
			if opErr == nil {
				if _, _, err := s.Lookup(ctx, admin, d.Handle, name); err != nil {
					t.Fatalf("%s of %q accepted, then Lookup: %v", op, name, err)
				}
			}
		}
	})
}

// listAll is every name ReadDir lists in dir, a page of two at a time.
func listAll(ctx context.Context, s *Filesystem, c vfs.Call, dir vfs.Handle) ([]string, error) {
	var out []string
	var from vfs.Cursor
	for {
		p, err := s.ReadDir(ctx, c, dir, from, 2, vfs.EntryHandle)
		if err != nil {
			return nil, err
		}
		for _, e := range p.Entries {
			out = append(out, string(e.Name))
		}
		if p.EOF {
			return out, nil
		}
		from = p.Next
	}
}

// FuzzCursors: a ReadDir cursor of any value, one taken from another
// directory included, resumes inside the directory listed: every entry it
// yields is that directory's, none twice, and the entries before it and
// after it together are the directory's, each once.
func FuzzCursors(f *testing.F) {
	s, admin, root := fuzzShare(f)
	ctx := context.Background()
	mk := func(dir vfs.Handle, name string, k vfs.CreateKind) vfs.Handle {
		r, err := s.Create(ctx, admin, dir, []byte(name), vfs.CreateArgs{Kind: k, Attrs: entity.Attrs{}.WithMode(0o755)})
		if err != nil {
			f.Fatal(err)
		}
		return r.Handle
	}
	a, b := mk(root, "a", vfs.CreateDir), mk(root, "b", vfs.CreateDir)
	var all []string
	for i := range 40 {
		name := fmt.Sprintf("entry%02d", i)
		mk(a, name, vfs.CreateRegular)
		mk(b, name+"-b", vfs.CreateRegular)
		all = append(all, name)
	}
	// Seeds: zero, the end, and every cursor either directory hands out.
	f.Add(uint64(0))
	f.Add(^uint64(0))
	for _, dir := range []vfs.Handle{a, b} {
		p, err := s.ReadDir(ctx, admin, dir, 0, 0, 0)
		if err != nil {
			f.Fatal(err)
		}
		for _, e := range p.Entries {
			f.Add(uint64(e.Cursor))
		}
	}
	whole, err := s.ReadDir(ctx, admin, a, 0, 0, 0)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, cur uint64) {
		var after []string
		from := vfs.Cursor(cur)
		for {
			p, err := s.ReadDir(ctx, admin, a, from, 3, 0)
			if err != nil {
				t.Fatalf("ReadDir from %#x: %v", cur, err)
			}
			for _, e := range p.Entries {
				after = append(after, string(e.Name))
			}
			if p.EOF {
				break
			}
			from = p.Next
		}
		var before []string
		for _, e := range whole.Entries {
			if uint64(e.Cursor) <= cur {
				before = append(before, string(e.Name))
			}
		}
		got := sorted(append(before, after...))
		if !slices.Equal(got, all) {
			t.Fatalf("from %#x: the entries before it and those listed after it are %q, want each of a's once", cur, got)
		}
	})
}
