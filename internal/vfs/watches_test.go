package vfs

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// changes is every change cb was notified of, in order.
func changes(cb *vfstest.RecordingCallbacks) []vfs.Change {
	var out []vfs.Change
	for _, c := range cb.Calls() {
		if c.Method == "Notify" {
			out = append(out, c.Args[1].([]vfs.Change)...)
		}
	}
	return out
}

// notes waits for cb to have been notified of n changes, advancing the
// clock a sweep at a time so a watch hands over what it holds, and answers
// each change's name, a rename's as old>new.
func notes(t *testing.T, s *shares, cb *vfstest.RecordingCallbacks, n int) []string {
	t.Helper()
	for range 50 {
		if got := changes(cb); len(got) >= n {
			var names []string
			for _, ch := range got {
				name := string(ch.Name)
				if ch.To != nil {
					name += ">" + string(ch.To)
				}
				names = append(names, name)
			}
			return names
		}
		s.clock.Advance(10 * time.Second)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("notified of %v, want %d changes", changes(cb), n)
	return nil
}

// A recursive watch hears every change committed in its directory and below
// it, from names, attributes and data, each named by its path from the
// watched directory; nothing below a directory its holder cannot traverse;
// and nothing once it is ended.
func TestAWatchHearsWhatItsHolderMayTraverse(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	w, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Recursive: true, Filter: ^openstate.ChangeMask(0)})
	s.must(t, err)
	mkdir := func(dir vfs.Handle, name string, mode uint32) vfs.Handle {
		r, err := s.Create(ctx, admin, dir, []byte(name), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(mode)})
		s.must(t, err)
		return r.Handle
	}
	touch := func(dir vfs.Handle, name string) {
		_, err := s.Create(ctx, admin, dir, []byte(name), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o666)})
		s.must(t, err)
	}
	touch(s.fs.Root, "f")
	touch(mkdir(s.fs.Root, "hidden", 0o700), "x")
	touch(mkdir(mkdir(s.fs.Root, "open", 0o755), "sub", 0o755), "y")
	_, err = s.Rename(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("f"), s.fs.Root, []byte("f2"), false)
	s.must(t, err)
	_, err = s.Unlink(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("f2"))
	s.must(t, err)
	touch(s.fs.Root, "mine")
	o, err := s.Open(ctx, alice, byName(s.fs.Root, "mine", vfs.OpenExisting, entity.ReadData|entity.WriteData))
	s.must(t, err)
	_, err = s.Write(ctx, alice, o.Ref, 0, []byte("x"), vfs.Unstable)
	s.must(t, err)
	_, err = s.SetAttr(ctx, alice, vfs.Target{H: o.Ref.Handle, Open: &o.Ref}, size(0))
	s.must(t, err)

	want := []string{"f", "hidden", "open", "open/sub", "open/sub/y", "f>f2", "f2", "mine", "mine", "mine"}
	notes(t, s.shares, cb, len(want))
	time.Sleep(50 * time.Millisecond)
	got := notes(t, s.shares, cb, len(want))
	if len(got) != len(want) {
		t.Fatalf("the watch heard %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the watch heard %v, want %v", got, want)
		}
	}
	all := changes(cb)
	for i, what := range map[int]openstate.ChangeMask{0: openstate.ChangeAddEntry, 4: openstate.ChangeAddEntry, 5: openstate.ChangeRename, 6: openstate.ChangeRemove, 8: openstate.ChangeSize, 9: openstate.ChangeAttrs} {
		if all[i].What&what == 0 {
			t.Errorf("change %d, %q, is %b, want %b", i, all[i].Name, all[i].What, what)
		}
	}
	if !all[4].Nested || all[0].Nested {
		t.Errorf("a change below the watched directory is not nested, or one in it is: %+v, %+v", all[4], all[0])
	}

	s.must(t, s.Unwatch(ctx, alice, w))
	touch(s.fs.Root, "after")
	s.clock.Advance(10 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := len(changes(cb)); n != len(want) {
		t.Errorf("an ended watch heard %d changes more", n-len(want))
	}
}

// A watch ended by its share's removal, or its client's revocation,
// delivers nothing open state raised for it beside its end; one whose
// client lost a single grant, or a share it is not on, keeps delivering.
func TestAnEndedWatchDeliversNothing(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	ch := []vfs.Change{{What: openstate.ChangeAddEntry, Name: []byte("f")}}
	for _, end := range []struct {
		name      string
		end       func()
		delivered bool
	}{
		{"a grant revoked", func() {
			s.Revoked(vfs.Revocation{Client: alice.Client, Reason: openstate.RevokedGrant})
		}, true},
		{"the share removed", func() {
			s.Revoked(vfs.Revocation{Client: alice.Client, Share: s.share, Reason: openstate.RevokedShare})
		}, false},
		{"another share removed", func() {
			s.Revoked(vfs.Revocation{Client: alice.Client, Share: entity.NewShareID(), Reason: openstate.RevokedShare})
		}, true},
		{"the client revoked", func() {
			s.Revoked(vfs.Revocation{Client: alice.Client, Reason: openstate.RevokedByAdmin})
		}, false},
	} {
		w, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Filter: openstate.ChangeAddEntry})
		s.must(t, err)
		end.end()
		before := len(changes(cb))
		s.Notify(alice.Client, w, ch)
		time.Sleep(50 * time.Millisecond)
		if got := len(changes(cb)) > before; got != end.delivered {
			t.Errorf("%s: the watch delivered %v, want %v", end.name, got, end.delivered)
		}
	}
}

// mkdir makes name in dir with mode, as the administrator.
func (s *attrs) mkdir(t *testing.T, dir vfs.Handle, name string, mode uint32) vfs.Handle {
	t.Helper()
	r, err := s.Create(t.Context(), proven(s.fs.Admin.Principal, inLab), dir, []byte(name), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(mode)})
	s.must(t, err)
	return r.Handle
}

// touch makes name in dir, a file anyone may write, as the administrator.
func (s *attrs) touch(t *testing.T, dir vfs.Handle, name string) vfs.Handle {
	t.Helper()
	r, err := s.Create(t.Context(), proven(s.fs.Admin.Principal, inLab), dir, []byte(name), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o666)})
	s.must(t, err)
	return r.Handle
}

// heard waits for cb to have been notified of len(want) changes, waits
// again for any more, and fails unless they are want, in any order: each
// delivery runs on its own, so two need not arrive in the order raised.
func heard(t *testing.T, s *attrs, cb *vfstest.RecordingCallbacks, want ...string) map[string]openstate.ChangeMask {
	t.Helper()
	notes(t, s.shares, cb, len(want))
	s.clock.Advance(10 * time.Second)
	time.Sleep(50 * time.Millisecond)
	got := notes(t, s.shares, cb, len(want))
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the watch heard %q, want %q", got, want)
	}
	what := map[string]openstate.ChangeMask{}
	for _, ch := range changes(cb) {
		what[string(ch.Name)] |= ch.What
	}
	return what
}

// A change below a directory is judged by the directory it was made in, not
// by whatever its name leads to at delivery: a file made in a shut
// directory is not reported once that directory is renamed and an open one
// made under its old name.
func TestAWatchJudgesTheDirectoryAChangeWasMadeIn(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	_, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Recursive: true, Filter: ^openstate.ChangeMask(0)})
	s.must(t, err)
	s.touch(t, s.mkdir(t, s.fs.Root, "p", 0o700), "secret")
	_, err = s.Rename(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("p"), s.fs.Root, []byte("q"), false)
	s.must(t, err)
	s.mkdir(t, s.fs.Root, "p", 0o755)
	heard(t, s, cb, "p", "p>q", "p")
}

// A watch is refused a directory its holder may not list, and once its
// holder may no longer traverse the watched directory it hears nothing more
// of it.
func TestAWatchHearsNothingOnceItsDirectoryIsShut(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	if _, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.mkdir(t, s.fs.Root, "x", 0o711), Filter: openstate.ChangeAddEntry}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a watch of a directory its holder may not list: %v, want ErrAccess", err)
	}
	d := s.mkdir(t, s.fs.Root, "d", 0o755)
	_, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: d, Filter: openstate.ChangeAddEntry})
	s.must(t, err)
	s.touch(t, d, "a")
	notes(t, s.shares, cb, 1)
	_, err = s.SetAttr(ctx, admin, vfs.Target{H: d}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o700)})
	s.must(t, err)
	s.touch(t, d, "b")
	heard(t, s, cb, "a")
}

// A change through an open is reported under the entry it was opened
// through, and under none once an unlink, or a rename over it, removed that
// entry, though the file keeps another name.
func TestAChangeThroughAnOpenNamesNoRemovedEntry(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	_, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Filter: ^openstate.ChangeMask(0)})
	s.must(t, err)
	rw := entity.ReadData | entity.WriteData
	a := s.touch(t, s.fs.Root, "a")
	oa := s.opener(t, alice, "a", rw)
	_, err = s.Link(ctx, admin, s.fs.Root, []byte("b"), a)
	s.must(t, err)
	_, err = s.Unlink(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("a"))
	s.must(t, err)
	s.touch(t, s.fs.Root, "c")
	oc := s.opener(t, alice, "c", rw)
	s.touch(t, s.fs.Root, "d")
	_, err = s.Rename(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("d"), s.fs.Root, []byte("c"), false)
	s.must(t, err)
	for _, o := range []vfs.OpenRef{oa, oc} {
		_, err = s.Write(ctx, alice, o, 0, []byte("x"), vfs.Unstable)
		s.must(t, err)
	}
	heard(t, s, cb, "a", "b", "a", "c", "d", "d>c")
}

// A directory changed by handle, through no open, is reported under its
// name in its parent, as an NFSv3 SETATTR changes it.
func TestADirectoryChangedByHandleIsReported(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	sub := s.mkdir(t, s.fs.Root, "sub", 0o755)
	_, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Filter: openstate.ChangeAttrs})
	s.must(t, err)
	_, err = s.SetAttr(ctx, admin, vfs.Target{H: sub}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o750)})
	s.must(t, err)
	heard(t, s, cb, "sub")
}

// Every change to a file's data or attributes through an open is reported:
// a write, a deallocate, an xattr set, and an open that overwrites it.
func TestEveryChangeThroughAnOpenIsReported(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	s.touch(t, s.fs.Root, "f")
	_, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Filter: ^openstate.ChangeMask(0)})
	s.must(t, err)
	o := s.opener(t, alice, "f", entity.ReadData|entity.WriteData|entity.WriteNamedAttrs)
	_, err = s.Write(ctx, alice, o, 0, []byte("xyz"), vfs.Unstable)
	s.must(t, err)
	_, err = s.Deallocate(ctx, alice, o, 0, 1)
	s.must(t, err)
	_, err = s.SetXattr(ctx, alice, vfs.Target{H: o.Handle, Open: &o}, entity.Xattr{Name: []byte("user.k"), Value: []byte("v")}, vfs.XattrEither)
	s.must(t, err)
	_, err = s.Open(ctx, alice, byName(s.fs.Root, "f", vfs.Overwrite, entity.ReadData|entity.WriteData))
	s.must(t, err)
	want := openstate.ChangeSize | openstate.ChangeTimes | openstate.ChangeAttrs
	if what := heard(t, s, cb, "f", "f", "f", "f")["f"]; what&want != want {
		t.Errorf("the changes were %b, want %b among them", what, want)
	}
}

// A rename across directories is reported as a removal from the one and an
// addition to the other.
func TestARenameAcrossDirectoriesIsReportedInBoth(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	sub := s.mkdir(t, s.fs.Root, "sub", 0o755)
	s.touch(t, s.fs.Root, "x")
	_, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Recursive: true, Filter: ^openstate.ChangeMask(0)})
	s.must(t, err)
	_, err = s.Rename(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("x"), sub, []byte("x"), false)
	s.must(t, err)
	if what := heard(t, s, cb, "x", "sub/x"); what["x"]&openstate.ChangeRemove == 0 || what["sub/x"]&openstate.ChangeAddEntry == 0 {
		t.Errorf("the rename was reported %v, want a removal and an addition", what)
	}
}

// A watch is held for its own client only, and only until Unwatch or the
// client's end.
func TestAWatchIsHeldByItsClientUntilItEnds(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	alice, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	bob, bobs := s.connected(t, signedIn(s.bob.Principal, 2), smb("bob-pc", 1))
	ch := []vfs.Change{{What: openstate.ChangeAddEntry, Name: []byte("f")}}
	w, err := s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Filter: openstate.ChangeAddEntry})
	s.must(t, err)
	s.Notify(bob.Client, w, ch)
	time.Sleep(50 * time.Millisecond)
	if n := len(changes(bobs)); n != 0 {
		t.Errorf("a Notify naming another client delivered it %d changes, want none", n)
	}
	s.must(t, s.Unwatch(ctx, alice, w))
	s.Notify(alice.Client, w, ch)
	time.Sleep(50 * time.Millisecond)
	if n := len(changes(cb)); n != 0 {
		t.Errorf("an ended watch delivered %d changes, want none", n)
	}
	w, err = s.Watch(ctx, alice, vfs.WatchSpec{Dir: s.fs.Root, Filter: openstate.ChangeAddEntry})
	s.must(t, err)
	s.ClientEnded(alice.Client, nil, openstate.RevokedLeaseExpired)
	if _, ok := s.watchers.held(alice.Client, w); ok {
		t.Error("a watch is still held once its client ended")
	}
}
