package vfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/vfs/authz"
	"github.com/marmos91/dittofs/internal/vfs/lockorder"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// attrs is a serving filesystem whose share's one shard is served, with two
// unprivileged users, alice and bob, who may write it.
type attrs struct {
	*shares
	alice, bob entity.User
}

func newAttrs(t *testing.T) *attrs {
	t.Helper()
	s := newShares(t, nil, nil)
	s.must(t, s.tab.Recovery().ServeShard(t.Context(), s.share, 0, 1))
	return &attrs{
		shares: s,
		alice:  s.user(t, "alice", "", s.share, entity.AccessReadWrite),
		bob:    s.user(t, "bob", "", s.share, entity.AccessReadWrite),
	}
}

// file makes name in the share's root, owned by owner with mode, as the
// administrator sets them.
func (s *attrs) file(t *testing.T, name string, owner entity.Principal, mode uint32) vfs.Handle {
	t.Helper()
	h, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, name, kvstore.Regular(0o644))
	files := s.fs.Store.Files()
	_, err := files.SetAttrs(t.Context(), s.fs.Admin, h, entity.Attrs{}.WithOwner(owner))
	s.must(t, err)
	_, err = files.SetAttrs(t.Context(), s.fs.Admin, h, entity.Attrs{}.WithMode(mode))
	s.must(t, err)
	return h
}

// record is h's record as the store holds it, no overlay joined.
func (s *attrs) record(t *testing.T, h vfs.Handle) vfs.File {
	t.Helper()
	f, err := s.fs.Store.Files().Get(t.Context(), s.fs.Admin, h)
	s.must(t, err)
	return f
}

func size(n int64) vfs.AttrChange { return vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(n)} }

// V22: an unprivileged caller's truncate clears setuid and setgid, the
// owner's included, in a change committed before the engine stages the
// truncate; a privileged caller's, or one naming the mode, keeps the mode it
// leaves.
func TestATruncateClearsSetIDBeforeItIsStaged(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	for _, c := range []struct {
		name string
		who  entity.Principal
		ch   vfs.AttrChange
		want uint32
	}{
		{"bob", s.bob.Principal, size(3), 0o0777},
		{"alice", s.alice.Principal, size(3), 0o0777},
		{"admin", s.fs.Admin.Principal, size(3), 0o6777},
		{"alice naming the mode", s.alice.Principal, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(3).WithMode(0o4755)}, 0o4755},
	} {
		h := s.file(t, c.name, s.alice.Principal, 0o6777)
		staged := -1
		s.engine.truncating = func(entity.FileID) { staged = int(s.record(t, h).Mode) }
		w, err := s.SetAttr(ctx, proven(c.who, inLab), vfs.Target{H: h}, c.ch)
		s.must(t, err)
		if staged != int(c.want) {
			t.Errorf("%s: the record's mode as the truncate was staged: %o, want %o", c.name, staged, c.want)
		}
		got, err := s.GetAttr(ctx, proven(c.who, inLab), h)
		s.must(t, err)
		if got.Mode != c.want || got.Size != 3 || w.After.Mode != c.want || w.After.Size != 3 {
			t.Errorf("%s: GetAttr mode %o size %d, After mode %o size %d; want %o, 3", c.name, got.Mode, got.Size, w.After.Mode, w.After.Size, c.want)
		}
	}
}

// V22: a truncate of an append-only file is refused ErrPerm before the
// engine stages anything, and clears nothing.
func TestATruncateOfAnAppendOnlyFileClearsNothing(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.engine.truncating = func(entity.FileID) { t.Error("an append-only file's truncate was staged") }
	for _, mode := range []uint32{0o4777, 0o0666} {
		h := s.file(t, fmt.Sprintf("log%o", mode), s.alice.Principal, mode)
		_, err := s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithFlags(entity.FlagAppendOnly))
		s.must(t, err)
		if _, err := s.SetAttr(ctx, proven(s.bob.Principal, inLab), vfs.Target{H: h}, size(0)); !errors.Is(err, vfs.ErrPerm) {
			t.Errorf("truncate of an append-only %o file: %v, want ErrPerm", mode, err)
		}
		if m := s.record(t, h).Mode; m != mode {
			t.Errorf("a refused truncate left mode %o, want %o", m, mode)
		}
	}
}

// GetAttr answers the overlay's size when a truncate is pending, and the
// larger of it and the record's when not; its times and version never move
// back from the record's.
func TestGetAttrJoinsTheOverlay(t *testing.T) {
	s := newAttrs(t)
	f := entity.NewFileID()
	rec := vfs.File{ID: f, Type: entity.Regular, Size: 1 << 20, Version: entity.VersionOf(100), Change: time.Unix(200, 0)}
	for _, c := range []struct {
		name string
		o    Overlay
		want int64
	}{
		{"writes past the record", Overlay{Size: 2 << 20}, 2 << 20},
		{"writes within the record", Overlay{Size: 512 << 10}, 1 << 20},
		{"a truncate", Overlay{Size: 512 << 10, SizeSet: true}, 512 << 10},
	} {
		c.o.Version, c.o.Ctime = entity.VersionOf(5), time.Unix(100, 0)
		s.engine.overlays[f] = c.o
		got, err := s.joined(t.Context(), rec)
		s.must(t, err)
		if got.Size != c.want || got.Version != rec.Version || !got.Change.Equal(rec.Change) {
			t.Errorf("%s: size %d version %v ctime %v; want %d, the record's", c.name, got.Size, got.Version, got.Change, c.want)
		}
	}
	delete(s.engine.overlays, f)
	if got, _ := s.joined(t.Context(), rec); got.Size != rec.Size {
		t.Errorf("with nothing pending: size %d, want the record's", got.Size)
	}
}

// V31: every mutation's WCC is read in the one hold of the file's accept
// lock that makes the change: a write staged while a change waits for the
// lock is in its Before, and one staged after the change answers is in
// neither side.
func TestEveryChangesWCCIsReadInOneHold(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	x := entity.Xattr{Name: []byte("user.k"), Value: []byte("v")}
	for name, change := range map[string]func(vfs.Target) (vfs.WCC, error){
		"SetAttr": func(t vfs.Target) (vfs.WCC, error) {
			return s.SetAttr(ctx, admin, t, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o600)})
		},
		"SetACL": func(t vfs.Target) (vfs.WCC, error) {
			return s.SetACL(ctx, admin, t, entity.ACLFromMode(0o640))
		},
		"SetXattr":    func(t vfs.Target) (vfs.WCC, error) { return s.SetXattr(ctx, admin, t, x, vfs.XattrEither) },
		"RemoveXattr": func(t vfs.Target) (vfs.WCC, error) { return s.RemoveXattr(ctx, admin, t, x.Name) },
	} {
		h := s.file(t, name, s.alice.Principal, 0o644)
		id := fileOf(h)
		var w vfs.WCC
		var err error
		if name == "RemoveXattr" {
			_, err := s.SetXattr(ctx, admin, vfs.Target{H: h}, x, vfs.XattrEither)
			s.must(t, err)
		}
		unlock, lerr := s.locks.Take(ctx, lockorder.Set{Accept: []lockorder.Key{lockorder.Key(id)}})
		s.must(t, lerr)
		answered := make(chan struct{})
		go func() {
			defer close(answered)
			w, err = change(vfs.Target{H: h})
		}()
		time.Sleep(20 * time.Millisecond)
		_, _ = s.engine.Write(ctx, id, 0, []byte("staged while held"), WriteOpts{})
		unlock()
		<-answered
		s.must(t, err)
		if w.Before == nil || w.Before.Size != 17 || w.After.Size != 17 {
			t.Errorf("%s: WCC %+v, want a Before and an After both after the held write", name, w)
		}
		_, _ = s.engine.Write(ctx, id, 17, []byte("later"), WriteOpts{})
		if got, _ := s.GetAttr(ctx, admin, h); got.Size == w.After.Size {
			t.Errorf("%s: a later write did not move GetAttr past the WCC", name)
		}
	}
}

// V31: a directory's Before is nil over NFSv3, and returned over any other
// protocol.
func TestADirectorysBeforeIsNilOverNFSv3(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	dir, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "d", kvstore.Dir(0o755))
	for _, p := range []entity.Protocol{entity.ProtocolNFSv3, entity.ProtocolNFSv4, entity.ProtocolSMB} {
		c := proven(s.fs.Admin.Principal, inLab)
		c.Protocol = p
		w, err := s.SetAttr(ctx, c, vfs.Target{H: dir}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o750)})
		s.must(t, err)
		if (w.Before == nil) != (p == entity.ProtocolNFSv3) {
			t.Errorf("%v: Before %v", p, w.Before)
		}
	}
}

// A change another client's lease conflicts with is answered ErrDelay while
// open state breaks the lease, and changes nothing.
func TestAChangeWaitsOutAConflictingGrant(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o644)
	f := s.record(t, h)
	c := openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolSMB, Name: []byte("c"), Boot: [8]byte{1}}}
	id, err := s.tab.Clients().Connect(ctx, c, nil)
	s.must(t, err)
	who := openstate.Caller{Client: id, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	res, err := s.tab.Opens().Open(ctx, who, keyOf(f), openstate.OpenRequest{Access: entity.ReadData | entity.WriteData, Want: openstate.GrantRead | openstate.GrantWrite, Lease: openstate.LeaseKey{1}})
	s.must(t, err)
	if res.Grant == nil {
		t.Fatal("open state offered no lease to recall")
	}
	_, err = s.SetAttr(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o600)})
	var delay *vfs.Delay
	if !errors.As(err, &delay) || delay.Cause != vfs.DelayRecall {
		t.Errorf("a chmod under another client's grant: %v, want ErrDelay DelayRecall", err)
	}
	if m := s.record(t, h).Mode; m != 0o644 {
		t.Errorf("the refused chmod left mode %o", m)
	}
}

// A ctime guard that no longer holds refuses the change, which then changes
// nothing; one that holds lets it through.
func TestAStaleGuardChangesNothing(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	h := s.file(t, "f", s.alice.Principal, 0o644)
	stale := s.record(t, h).Change.Add(-time.Second)
	ch := vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o600), Guard: &stale}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, ch); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a stale guard: %v, want ErrInvalid", err)
	}
	if m := s.record(t, h).Mode; m != 0o644 {
		t.Errorf("a stale guard's change left mode %o", m)
	}
	now := s.record(t, h).Change
	ch.Guard = &now
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, ch); err != nil {
		t.Errorf("a guard that holds: %v", err)
	}
}

// DeleteOnClose, Suspend and Resume go to an open, ErrInvalid with none, and
// open state refuses an open it does not hold.
func TestDeleteOnCloseGoesThroughAnOpen(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	h := s.file(t, "f", s.alice.Principal, 0o644)
	yes := true
	ch := vfs.AttrChange{DeleteOnClose: &yes}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, ch); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("delete on close with no open: %v, want ErrInvalid", err)
	}
	ref := &openstate.OpenRef{Kind: openstate.RefOpened, Open: 42, Handle: h}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h, Open: ref}, ch); !errors.Is(err, vfs.ErrState) {
		t.Errorf("delete on close through an open nobody holds: %v, want ErrState", err)
	}
}

// XattrCreate refuses a name that exists and XattrReplace one that does not;
// removing one not there is ErrNotFound.
func TestXattrModes(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	alice := proven(s.alice.Principal, inLab)
	h := s.file(t, "f", s.alice.Principal, 0o644)
	x := entity.Xattr{Name: []byte("user.k"), Value: []byte("v")}
	if _, err := s.SetXattr(ctx, alice, vfs.Target{H: h}, x, vfs.XattrReplace); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("replace of a missing xattr: %v", err)
	}
	if _, err := s.SetXattr(ctx, alice, vfs.Target{H: h}, x, vfs.XattrCreate); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetXattr(ctx, alice, vfs.Target{H: h}, x, vfs.XattrCreate); !errors.Is(err, vfs.ErrExist) {
		t.Errorf("create of an existing xattr: %v", err)
	}
	if xs, err := s.Xattrs(ctx, alice, h); err != nil || len(xs) != 1 || string(xs[0].Value) != "v" {
		t.Errorf("Xattrs: %v, %v", xs, err)
	}
	if _, err := s.RemoveXattr(ctx, alice, vfs.Target{H: h}, []byte("user.none")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("remove of a missing xattr: %v", err)
	}
}

// Access is what the caller is granted of what it asks, nothing more; in a
// pseudo directory, reading and traversing it. A pseudo directory refuses
// every change ErrReadOnly.
func TestAccessAndThePseudoFilesystem(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	bob := proven(s.bob.Principal, inLab)
	h := s.file(t, "f", s.alice.Principal, 0o644)
	if got, err := s.Access(ctx, bob, h, nil, entity.ReadData|entity.WriteData); err != nil || got != entity.ReadData {
		t.Errorf("bob on alice's 0644 file: %v, %v; want read only", got, err)
	}
	root, _, err := s.Root(ctx, vfs.Call{}, vfs.ShareRef{})
	s.must(t, err)
	if got, err := s.Access(ctx, bob, root, nil, entity.ListDirectory|entity.AddFile); err != nil || got != entity.ListDirectory {
		t.Errorf("Access in the pseudo-root: %v, %v", got, err)
	}
	if _, err := s.SetAttr(ctx, bob, vfs.Target{H: root}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o777)}); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("SetAttr in the pseudo-root: %v, want ErrReadOnly", err)
	}
	if acl, err := s.GetACL(ctx, bob, root); err != nil || acl.Mode() != 0o555 {
		t.Errorf("GetACL in the pseudo-root: %v, %v", acl, err)
	}
}

// Every Attributes call is admitted to the handle's share before it reaches
// the store: a caller the share's client rules refuse is refused, whatever
// the call.
func TestEveryAttributesCallIsAdmitted(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o644)
	s.must(t, s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Clients: []entity.ClientRule{
		{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("192.168.0.0/16")}, Access: entity.AccessNone}}}))
	c := proven(s.alice.Principal, outside)
	tg := vfs.Target{H: h}
	calls := map[string]func() error{
		"GetAttr":     func() error { _, err := s.GetAttr(ctx, c, h); return err },
		"SetAttr":     func() error { _, err := s.SetAttr(ctx, c, tg, size(0)); return err },
		"GetACL":      func() error { _, err := s.GetACL(ctx, c, h); return err },
		"SetACL":      func() error { _, err := s.SetACL(ctx, c, tg, entity.ACLFromMode(0o600)); return err },
		"Xattrs":      func() error { _, err := s.Xattrs(ctx, c, h); return err },
		"SetXattr":    func() error { _, err := s.SetXattr(ctx, c, tg, entity.Xattr{Name: []byte("a")}, 0); return err },
		"RemoveXattr": func() error { _, err := s.RemoveXattr(ctx, c, tg, []byte("a")); return err },
		"Streams":     func() error { _, err := s.Streams(ctx, c, h); return err },
		"Access":      func() error { _, err := s.Access(ctx, c, h, nil, entity.ReadData); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("%s from a refused address: %v, want ErrAccess", name, err)
		}
	}
}

// auditedAttrs is newAttrs over a share whose audit policy records every
// refusal, and the sink recording them.
func auditedAttrs(t *testing.T) (*attrs, *recorder) {
	t.Helper()
	rec := &recorder{}
	s := newShares(t, rec, func(vfs.ShareID) authz.Policy { return authz.AuditDenials })
	s.must(t, s.tab.Recovery().ServeShard(t.Context(), s.share, 0, 1))
	return &attrs{shares: s,
		alice: s.user(t, "alice", "", s.share, entity.AccessReadWrite),
		bob:   s.user(t, "bob", "", s.share, entity.AccessReadWrite)}, rec
}

// refusals is the access events rec saw refused for op.
func refusals(rec *recorder, op vfs.Op) []vfs.AccessEvent {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []vfs.AccessEvent
	for _, e := range rec.got {
		if e.Op == op && !e.Allowed {
			out = append(out, e)
		}
	}
	return out
}

// A chmod, an xattr change and an ACL change refused for want of their
// right are each decided through the chokepoint and audited, naming the
// right asked: write the ACL for a chmod.
func TestARefusedChangeIsAudited(t *testing.T) {
	s, rec := auditedAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o644)
	bob := proven(s.bob.Principal, inLab)
	if _, err := s.SetAttr(ctx, bob, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o777)}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's chmod of alice's file: %v, want ErrAccess", err)
	}
	if got := refusals(rec, vfs.OpSetAttr); len(got) != 1 || got[0].Want != entity.WriteACL {
		t.Errorf("the refused chmod's events: %+v, want one asking write the ACL", got)
	}
	if _, err := s.SetXattr(ctx, bob, vfs.Target{H: h}, entity.Xattr{Name: []byte("k"), Value: []byte("v")}, vfs.XattrEither); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's xattr on alice's file: %v, want ErrAccess", err)
	}
	if got := refusals(rec, vfs.OpSetXattr); len(got) != 1 {
		t.Errorf("the refused xattr change's events: %+v, want one", got)
	}
}

// Each attribute asks its own right: a caller an entry grants write the ACL
// changes the mode without owning the file or holding write attributes, and
// not the group, which asks change the owner of a non-owner; a group writer
// sets a time it gives through an open holding write attributes, and not on
// no open.
func TestEachAttributeAsksItsOwnRight(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin, bob := proven(s.fs.Admin.Principal, inLab), proven(s.bob.Principal, inLab)
	h := s.file(t, "f", s.alice.Principal, 0o644)
	acl := entity.ACL{Entries: []entity.ACE{
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.ReadAttributes | entity.ReadACL | entity.WriteACL, Who: s.bob.Principal},
		{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteData | entity.ReadAttributes | entity.WriteAttributes, Who: entity.Owner},
	}}
	_, err := s.SetACL(ctx, admin, vfs.Target{H: h}, acl)
	s.must(t, err)
	if _, err := s.SetAttr(ctx, bob, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o600)}); err != nil {
		t.Errorf("a chmod by a caller granted write the ACL: %v", err)
	}
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	s.must(t, s.cp.PutGroup(ctx, staff))
	s.must(t, s.cp.AddMember(ctx, entity.Membership{Group: staff.Principal, Member: s.bob.Principal}))
	if _, err := s.SetAttr(ctx, bob, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithGroup(staff.Principal)}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a chgrp by a non-owner granted write the ACL, not change the owner: %v, want ErrAccess", err)
	}

	g := s.file(t, "g", s.alice.Principal, 0o664)
	_, err = s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, g, entity.Attrs{}.WithGroup(staff.Principal))
	s.must(t, err)
	at := time.Unix(1_900_000_000, 0)
	touch := vfs.AttrChange{Attrs: entity.Attrs{}.WithModify(at)}
	if _, err := s.SetAttr(ctx, bob, vfs.Target{H: g}, touch); err == nil {
		t.Error("a group writer set a time it gave on no open")
	}
	id, err := s.tab.Clients().Connect(ctx, openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolSMB, Name: []byte("pc"), Boot: [8]byte{1}}}, nil)
	s.must(t, err)
	who := openstate.Caller{Client: id, Principal: s.bob.Principal, Protocol: entity.ProtocolSMB}
	res, err := s.tab.Opens().Open(ctx, who, keyOf(s.record(t, g)), openstate.OpenRequest{Access: entity.ReadData | entity.WriteData | entity.WriteAttributes})
	s.must(t, err)
	viaOpen := bob
	viaOpen.Protocol, viaOpen.Client = entity.ProtocolSMB, id
	ref := &openstate.OpenRef{Kind: openstate.RefOpened, State: res.Open.Ref, Open: res.Open.ID, Handle: g}
	if _, err := s.SetAttr(ctx, viaOpen, vfs.Target{H: g, Open: ref}, touch); err != nil {
		t.Errorf("a group writer's time set through its open: %v", err)
	}
	if got := s.record(t, g).Modify; !got.Equal(at) {
		t.Errorf("the time set through the open left Modify %v, want %v", got, at)
	}
	// The open's grant, not the caller's rights on the file, decides: the
	// same group writer's read-only open of g sets no time it gives, and the
	// owner's read-only open sets one on the owner's own rights.
	ro, err := s.tab.Opens().Open(ctx, who, keyOf(s.record(t, g)), openstate.OpenRequest{Access: entity.ReadData})
	s.must(t, err)
	roRef := &openstate.OpenRef{Kind: openstate.RefOpened, State: ro.Open.Ref, Open: ro.Open.ID, Handle: g}
	if _, err := s.SetAttr(ctx, viaOpen, vfs.Target{H: g, Open: roRef}, vfs.AttrChange{Attrs: entity.Attrs{}.WithModify(at.Add(time.Hour))}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a group writer's time set through a read-only open: %v, want ErrAccess", err)
	}
	if got := s.record(t, g).Modify; !got.Equal(at) {
		t.Errorf("a refused time set through a read-only open left Modify %v, want %v", got, at)
	}
	aliceWho := openstate.Caller{Client: id, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	mine, err := s.tab.Opens().Open(ctx, aliceWho, keyOf(s.record(t, g)), openstate.OpenRequest{Access: entity.ReadData})
	s.must(t, err)
	alice := proven(s.alice.Principal, inLab)
	alice.Protocol, alice.Client = entity.ProtocolSMB, id
	mineRef := &openstate.OpenRef{Kind: openstate.RefOpened, State: mine.Open.Ref, Open: mine.Open.ID, Handle: g}
	if _, err := s.SetAttr(ctx, alice, vfs.Target{H: g, Open: mineRef}, touch); err != nil {
		t.Errorf("the owner's time set through her read-only open: %v, want it set on her own rights", err)
	}
	res, err = s.tab.Opens().Open(ctx, who, keyOf(s.record(t, h)), openstate.OpenRequest{Access: entity.ReadData})
	s.must(t, err)
	ref = &openstate.OpenRef{Kind: openstate.RefOpened, State: res.Open.Ref, Open: res.Open.ID, Handle: h}
	if _, err := s.SetAttr(ctx, viaOpen, vfs.Target{H: h, Open: ref}, touch); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a time set through an open by a caller the file grants no write attributes: %v, want ErrAccess", err)
	}
	if _, err := s.SetAttr(ctx, bob, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal)}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a chown by a caller granted no change the owner: %v, want ErrAccess", err)
	}
}

// Only an admin reads or writes the audit list: the owner reads none back,
// her set carrying audit entries is ErrAccess, and one carrying none, what
// she read back included, keeps the file's.
func TestOnlyAnAdminReadsOrWritesTheAuditList(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin, alice := proven(s.fs.Admin.Principal, inLab), proven(s.alice.Principal, inLab)
	h := s.file(t, "f", s.alice.Principal, 0o644)
	audited := entity.ACL{
		Entries: []entity.ACE{{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteData | entity.ReadAttributes | entity.WriteAttributes, Who: entity.Owner}},
		Audit:   []entity.ACE{{Type: entity.ACEAudit, Flags: entity.ACESuccessfulAccess | entity.ACEFailedAccess, Mask: entity.ReadData, Who: entity.Everyone}},
	}
	_, err := s.SetACL(ctx, admin, vfs.Target{H: h}, audited)
	s.must(t, err)
	if _, err := s.SetACL(ctx, alice, vfs.Target{H: h}, audited); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("the owner's set carrying audit entries: %v, want ErrAccess", err)
	}
	plain, err := s.GetACL(ctx, alice, h)
	s.must(t, err)
	if len(plain.Audit) != 0 {
		t.Errorf("the owner read the audit list: %+v", plain.Audit)
	}
	if got, err := s.GetACL(ctx, admin, h); err != nil || len(got.Audit) != 1 {
		t.Errorf("the admin's read of the audit list: %+v, %v", got.Audit, err)
	}
	if _, err := s.SetACL(ctx, alice, vfs.Target{H: h}, plain); err != nil {
		t.Errorf("the owner's set of what she read back: %v", err)
	}
	if got, _ := s.fs.Store.Files().ACL(ctx, s.fs.Admin, h); len(got.Audit) != 1 {
		t.Errorf("the owner's set erased the audit list: %+v", got.Audit)
	}
	_, err = s.SetACL(ctx, admin, vfs.Target{H: h}, plain)
	s.must(t, err)
	if got, _ := s.fs.Store.Files().ACL(ctx, s.fs.Admin, h); len(got.Audit) != 0 {
		t.Errorf("the admin's set with no audit entries kept %+v", got.Audit)
	}
}

// A truncate's clear of setuid and setgid changes the mode alone: a named
// entry of the stored ACL, which a chmod would merge, is kept as it was.
func TestATruncatesSetIDClearKeepsTheACL(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o6754)
	acl := entity.ACLFromMode(0o754)
	acl.Entries = append(acl.Entries, entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.WriteData | entity.WriteAttributes, Who: s.bob.Principal})
	_, err := s.SetACL(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, acl)
	s.must(t, err)
	before, err := s.fs.Store.Files().ACL(ctx, s.fs.Admin, h)
	s.must(t, err)
	if _, err := s.SetAttr(ctx, proven(s.bob.Principal, inLab), vfs.Target{H: h}, size(1)); err != nil {
		t.Fatal(err)
	}
	after, err := s.fs.Store.Files().ACL(ctx, s.fs.Admin, h)
	s.must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("bob's truncate rewrote the ACL:\n before %+v\n after  %+v", before, after)
	}
	if m := s.record(t, h).Mode; m != 0o754 {
		t.Errorf("bob's truncate left mode %o, want 754", m)
	}
}

// A size change is checked against the locks from the file's current size
// on: growing a file across another owner's mandatory lock is ErrLocked.
func TestAGrowingSizeChangeMeetsTheLocksPastTheOldSize(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	f := s.record(t, h)
	s.engine.overlays[f.ID] = Overlay{Size: 100}
	id, err := s.tab.Clients().Connect(ctx, openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolSMB, Name: []byte("pc"), Boot: [8]byte{1}}}, nil)
	s.must(t, err)
	who := openstate.Caller{Client: id, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	res, err := s.tab.Opens().Open(ctx, who, keyOf(f), openstate.OpenRequest{Access: entity.ReadData | entity.WriteData})
	s.must(t, err)
	owner := openstate.LockOwner{Client: id, Name: binary.BigEndian.AppendUint64(nil, uint64(res.Open.ID)), Stack: true}
	_, err = s.tab.Locks().Lock(ctx, who, keyOf(f), openstate.LockReq{Owner: owner,
		Open:   openstate.OpenRef{Kind: openstate.RefOpened, Open: res.Open.ID, State: openstate.StateRef{Shard: res.Open.Ref.Shard}},
		Ranges: []openstate.LockRange{{ByteRange: openstate.ByteRange{Offset: 150, Length: 10}, Exclusive: true}}})
	s.must(t, err)
	if _, err := s.SetAttr(ctx, proven(s.bob.Principal, inLab), vfs.Target{H: h}, size(300)); !errors.Is(err, vfs.ErrLocked) {
		t.Errorf("growing a file from 100 to 300 across a lock at 150: %v, want ErrLocked", err)
	}
}

// changeOf names to open state each kind of change an attribute change
// makes: the attributes always, its size, its times, a security change for
// an owner, group or mode, and setuid or setgid set.
func TestChangeOfNamesWhatOpenStateJudges(t *testing.T) {
	const attrs = openstate.ChangeAttrs
	for _, c := range []struct {
		name string
		a    vfs.Attrs
		want openstate.ChangeMask
	}{
		{"nothing", entity.Attrs{}, 0},
		{"size", entity.Attrs{}.WithSize(1), attrs | openstate.ChangeSize},
		{"times", entity.Attrs{}.WithModify(time.Unix(1, 0)), attrs | openstate.ChangeTimes},
		{"chmod", entity.Attrs{}.WithMode(0o644), attrs | openstate.ChangeSecurity},
		{"chmod setuid", entity.Attrs{}.WithMode(0o4755), attrs | openstate.ChangeSecurity | openstate.ChangeSetID},
		{"chown", entity.Attrs{}.WithOwner(entity.NewUser()), attrs | openstate.ChangeSecurity},
	} {
		if got := changeOf(c.a); got != c.want {
			t.Errorf("%s: %b, want %b", c.name, got, c.want)
		}
	}
}
