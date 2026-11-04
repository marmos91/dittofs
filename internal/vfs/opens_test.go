package vfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// byName is an open of name in dir, asking access, as disposition d.
func byName(dir vfs.Handle, name string, d vfs.Disposition, access vfs.Access) vfs.OpenRequest {
	return vfs.OpenRequest{
		Dir: dir, Name: []byte(name), Disposition: d, Access: access,
		Create: vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)},
	}
}

// held reports whether the fake engine holds content for f.
func (f *fixture) held(id entity.FileID) bool {
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	_, ok := f.engine.files[id]
	return ok
}

// stored gives f content in the fake engine, in the fixture's share.
func (f *fixture) stored(t *testing.T, id entity.FileID) {
	t.Helper()
	f.storedIn(t, f.fs.Share.ID, id)
}

// storedIn gives f content in the fake engine, in share.
func (f *fixture) storedIn(t *testing.T, share entity.ShareID, id entity.FileID) {
	t.Helper()
	if _, err := f.engine.Write(t.Context(), share, id, 0, []byte("content"), WriteOpts{}); err != nil {
		t.Fatal(err)
	}
}

// released waits for the fake engine to drop f's content, advancing the
// clock a sweep at a time, and reports whether it did within a few seconds.
func (f *fixture) released(id entity.FileID, step time.Duration) bool {
	for range 200 {
		if !f.held(id) {
			return true
		}
		if step > 0 {
			f.clock.Advance(step)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// restart is a new open-state table and filesystem over f's store and
// engine, as a node's restart builds them, with f's stopped first. The table
// is started; the filesystem is not.
func (f *fixture) restart(t *testing.T) *fixture {
	t.Helper()
	_ = f.Drain(context.Background())
	_ = f.Stop()
	f.tab.Stop()
	clock := sim.NewTestClock(time.Unix(0, 0))
	tab, err := openstate.New(openstate.Deps{Records: f.fs.Store, Clock: clock, Rand: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Deps{Meta: f.fs.Store, Open: tab, Engine: f.engine})
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Start(t.Context(), s, s.Admission); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Drain(context.Background())
		_ = s.Stop()
		tab.Stop()
	})
	return &fixture{Filesystem: s, tab: tab, fs: f.fs, clock: clock, engine: f.engine}
}

// nfs41 connects an NFSv4 client named name, claimed as uid, on session
// sess, and answers a call from it.
func (s *shares) nfs41(t *testing.T, name string, uid uint32, sess uint64) vfs.Call {
	t.Helper()
	c := claimed(uid, inLab)
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte(name), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}
	p, err := s.Propose(t.Context(), c, info)
	s.must(t, err)
	info.Proposal = &p
	id, _, err := s.Connect(t.Context(), c, info, &vfstest.RecordingCallbacks{})
	s.must(t, err)
	c.Protocol, c.Client, c.Session = entity.ProtocolNFSv4, id, vfs.SessionRef{ID: sess}
	return c
}

// An open creates, opens or refuses by its disposition, and a close ends it:
// the closed open is gone, refused as bad state.
func TestOpenThenClose(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	r, err := n.Open(ctx, c, byName(home, "f", vfs.OpenOrCreate, entity.ReadData|entity.WriteData))
	if err != nil || !r.Created || r.Dir == nil || r.Ref.Kind != openstate.RefOpened || r.File.ID.IsZero() {
		t.Fatalf("an open creating f: %+v, %v", r, err)
	}
	if _, err := n.Open(ctx, c, byName(home, "f", vfs.CreateNew, entity.ReadData)); !errors.Is(err, vfs.ErrExist) {
		t.Errorf("a create-new open of f: %v, want ErrExist", err)
	}
	if _, err := n.Open(ctx, c, byName(home, "g", vfs.OpenExisting, entity.ReadData)); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("an open of no file: %v, want ErrNotFound", err)
	}
	again, err := n.Open(ctx, c, vfs.OpenRequest{H: r.Ref.Handle, Disposition: vfs.OpenExisting, Access: entity.ReadData})
	if err != nil || again.Created || again.File.ID != r.File.ID {
		t.Errorf("an open of f by handle: %+v, %v", again, err)
	}
	if w, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil || w != nil {
		t.Errorf("Close: %v, %v", w, err)
	}
	var se *vfs.StateError
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); !errors.As(err, &se) {
		t.Errorf("a second Close of one open: %v, want a state error", err)
	}
}

// An overwriting open truncates the file once the open is granted; a
// directory is never overwritten.
func TestAnOverwritingOpenTruncates(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	h, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	n.stored(t, f.ID)
	r, err := n.Open(ctx, c, byName(n.fs.Root, "f", vfs.Overwrite, entity.ReadData))
	if err != nil || r.File.Size != 0 || !bytes.Equal(r.Ref.Handle, h) {
		t.Errorf("an overwriting open: size %d, %v; want 0", r.File.Size, err)
	}
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o755))
	if _, err := n.Open(ctx, c, byName(n.fs.Root, "d", vfs.Overwrite, entity.ReadData)); !errors.Is(err, vfs.ErrIsDir) {
		t.Errorf("an overwriting open of a directory: %v, want ErrIsDir", err)
	}
}

// Start serves the share's shard itself: a filesystem started as the node
// starts it answers a change open state judges, which a shard not served
// refuses with ErrDelay.
func TestStartServesTheShards(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	if _, err := s.Create(ctx, admin, s.fs.Root, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular}); err != nil {
		t.Errorf("a create once started: %v", err)
	}
	if _, err := s.Unlink(ctx, admin, vfs.Target{H: s.fs.Root}, []byte("f")); err != nil {
		t.Errorf("an unlink once started: %v", err)
	}
	sh, err := s.fs.Store.Shard(ctx, 0)
	if err != nil || sh.Incarnation != 1 || sh.GraceInstance != 1 {
		t.Errorf("the shard after one start: %+v, %v; want incarnation and grace instance 1", sh, err)
	}
}

// V17: an unlinked file whose last open closes is released at that close,
// and not before.
func TestALastCloseReleasesAnUnlinkedFile(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	r, err := n.Open(ctx, c, byName(home, "f", vfs.OpenOrCreate, entity.ReadData))
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: home}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	if !n.held(r.File.ID) {
		t.Fatal("an unlinked file's content is released while an open holds it")
	}
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if !n.released(r.File.ID, 0) {
		t.Error("an unlinked file's content is held after its last close")
	}
}

// V17: a release pending in the store when the node stopped, its file
// unlinked and never released, runs when the restarted shard's grace ends.
func TestAPendingReleaseRunsAfterARestart(t *testing.T) {
	f := newFixture(t, nil)
	f.must(t, f.Start(t.Context()))
	_, file := f.fs.Create(t, f.fs.Admin, f.fs.Root, "f", kvstore.Regular(0o644))
	f.stored(t, file.ID)
	if _, err := f.fs.Store.Namespace().Unlink(t.Context(), f.fs.Admin, f.fs.Root, []byte("f")); err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	g.must(t, g.Start(t.Context()))
	g.sending.Wait() // every release the start began has run
	if !g.held(file.ID) {
		t.Fatal("a pending release ran as the restart began")
	}
	if !g.released(file.ID, 11*time.Second) {
		t.Error("a pending release written before the restart never ran")
	}
}

func (f *fixture) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// V35: after a restart, a client that held a lock in the shard may reclaim
// it, so a lock conflicting with it answers ErrGrace, and a release pending
// there runs only once the grace ends, when the conflicting lock is granted.
func TestPendingReleasesWaitOutGrace(t *testing.T) {
	f := newFixture(t, nil)
	s := &shares{fixture: f, cp: f.fs.Store.ControlPlane(), share: f.fs.Share.ID}
	f.must(t, f.Start(t.Context()))
	ctx := t.Context()
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	c := s.nfs41(t, "host", 1000, 1)
	h, file := f.fs.Create(t, f.fs.Admin, f.fs.Root, "f", kvstore.Regular(0o666))
	_, err := f.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData})
	f.must(t, err)
	f.stored(t, file.ID)
	if _, err := f.Unlink(ctx, proven(f.fs.Admin.Principal, inLab), vfs.Target{H: f.fs.Root}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	lh, _ := f.fs.Create(t, f.fs.Admin, f.fs.Root, "l", kvstore.Regular(0o666))
	o, err := f.Open(ctx, c, vfs.OpenRequest{H: lh, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData})
	f.must(t, err)
	span := []openstate.LockRange{{ByteRange: openstate.ByteRange{Offset: 0, Length: 10}, Exclusive: true}}
	_, err = f.Lock(ctx, c, o.Ref, vfs.LockReq{Owner: openstate.LockOwner{Client: c.Client, Name: []byte("lo")}, Ranges: span})
	f.must(t, err)

	g := f.restart(t)
	g.must(t, g.Start(t.Context()))
	other := (&shares{fixture: g, cp: s.cp, share: s.share}).nfs41(t, "other", 1000, 2)
	conflicting := vfs.LockReq{Owner: openstate.LockOwner{Client: other.Client, Name: []byte("lo")}, Ranges: span}
	if _, err := g.Lock(ctx, other, vfs.Anonymous(lh), conflicting); !errors.Is(err, vfs.ErrGrace) {
		t.Errorf("a lock conflicting with a reclaimable one in grace: %v, want ErrGrace", err)
	}
	for range 6 {
		g.clock.Advance(11 * time.Second)
		time.Sleep(10 * time.Millisecond)
	}
	if !g.held(file.ID) {
		t.Fatal("a pending release ran inside the grace")
	}
	if !g.released(file.ID, 11*time.Second) {
		t.Error("a pending release never ran once the grace ended")
	}
	// A client connected now, as other's lease ran out with the clock.
	late := (&shares{fixture: g, cp: s.cp, share: s.share}).nfs41(t, "late", 1000, 3)
	conflicting.Owner.Client = late.Client
	if _, err := g.Lock(ctx, late, vfs.Anonymous(lh), conflicting); err != nil {
		t.Errorf("the conflicting lock once the grace ended: %v", err)
	}
}

// V21: an open meeting a deny mode held only by an open a handle-caching
// lease keeps is answered ErrDelay while the lease is broken, and once the
// holder closes, its retry is granted; with no such lease, the deny mode
// refuses at once.
func TestADenyModeUnderAHandleLeaseIsBrokenFirst(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "1000", n.share, entity.AccessReadWrite)
	alicePC := &vfstest.RecordingCallbacks{}
	asAlice := signedIn(alice.Principal, 1)
	id, _, err := n.Connect(ctx, asAlice, smb("alice-pc", 1), alicePC)
	n.must(t, err)
	asAlice.Client = id
	bob := n.smbClient(t, "build01", alice.Principal)
	nfs := n.nfs41(t, "host", 1000, 9)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))

	req := byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData)
	req.Deny, req.Lease, req.Want = entity.WriteData, vfs.LeaseKey{1}, openstate.GrantRead|openstate.GrantHandle
	held, err := n.Open(ctx, asAlice, req)
	if err != nil || held.Grant == nil {
		t.Fatalf("alice's leased open: %+v, %v", held.Grant, err)
	}
	for name, c := range map[string]vfs.Call{"SMB": bob, "NFSv4": nfs} {
		if _, err := n.Open(ctx, c, byName(n.fs.Root, "f", vfs.OpenExisting, entity.WriteData)); !errors.Is(err, vfs.ErrDelay) {
			t.Errorf("an %s write open against the cached deny-write: %v, want ErrDelay", name, err)
		}
	}
	if calls(t, alicePC, 1); len(breaks(alicePC)) == 0 {
		t.Error("the lease keeping the deny-write open was not broken")
	}
	if _, err := n.Close(ctx, asAlice, held.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Open(ctx, bob, byName(n.fs.Root, "f", vfs.OpenExisting, entity.WriteData)); err != nil {
		t.Errorf("the write open retried once the holder closed: %v", err)
	}

	n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o666))
	plain := byName(n.fs.Root, "g", vfs.OpenExisting, entity.ReadData)
	plain.Deny = entity.WriteData
	_, err = n.Open(ctx, asAlice, plain)
	n.must(t, err)
	if _, err := n.Open(ctx, bob, byName(n.fs.Root, "g", vfs.OpenExisting, entity.WriteData)); !errors.Is(err, vfs.ErrShareViolation) {
		t.Errorf("a write open against an uncached deny-write: %v, want ErrShareViolation", err)
	}
}

// V8: a close by another client, signed in as the opener's own user, is
// NotYours, over SMB and over NFSv4.1, and leaves the open to its opener.
func TestAnotherClientsCloseIsNotYours(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "1000", n.share, entity.AccessReadWrite)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	for name, pair := range map[string][2]vfs.Call{
		"SMB":     {n.smbClient(t, "alice-pc", alice.Principal), n.smbClient(t, "build01", alice.Principal)},
		"NFSv4.1": {n.nfs41(t, "a-host", 1000, 1), n.nfs41(t, "b-host", 1000, 2)},
	} {
		a, b := pair[0], pair[1]
		r, err := n.Open(ctx, a, byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData))
		n.must(t, err)
		if _, err := n.Close(ctx, b, r.Ref, vfs.Seqs{}); !notYours(err) {
			t.Errorf("%s: another client's close of the open: %v, want NotYours", name, err)
		}
		if _, err := n.Close(ctx, a, r.Ref, vfs.Seqs{}); err != nil {
			t.Errorf("%s: the opener's close after the refused one: %v", name, err)
		}
	}
}

// V9: a maximal open is granted delete when its parent grants deleting a
// child, decided afresh on every open, so it is not once the parent's owner
// changes; and a caller whose share grant is removed is refused its next
// open of a handle it holds.
func TestAMaximalOpenGetsDeleteThroughItsParent(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	bob := n.user(t, "bob", "", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o755))
	files := n.fs.Store.Files()
	_, err := files.SetAttrs(ctx, n.fs.Admin, d, entity.Attrs{}.WithOwner(alice.Principal))
	n.must(t, err)
	h, _ := n.fs.Create(t, n.fs.Admin, d, "f", kvstore.Regular(0o644))
	asAlice := n.smbClient(t, "alice-pc", alice.Principal)
	maximal := byName(d, "f", vfs.OpenExisting, vfs.MaximumAllowed)
	r, err := n.Open(ctx, asAlice, maximal)
	if err != nil || r.MaximalAccess&entity.Delete == 0 {
		t.Errorf("a maximal open in alice's directory: %v, %v; want delete through it", r.MaximalAccess, err)
	}
	// The open itself holds delete, so an open denying delete is refused.
	denying := byName(d, "f", vfs.OpenExisting, entity.ReadData)
	denying.Deny = entity.Delete
	if _, err := n.Open(ctx, n.smbClient(t, "build01", n.fs.Admin.Principal), denying); !errors.Is(err, vfs.ErrShareViolation) {
		t.Errorf("an open denying delete beside the maximal open: %v, want ErrShareViolation", err)
	}
	_, err = files.SetAttrs(ctx, n.fs.Admin, d, entity.Attrs{}.WithOwner(bob.Principal))
	n.must(t, err)
	r, err = n.Open(ctx, asAlice, maximal)
	if err != nil || r.MaximalAccess&entity.Delete != 0 {
		t.Errorf("a maximal open once the directory is bob's: %v, %v; want no delete", r.MaximalAccess, err)
	}
	doc := byName(d, "f", vfs.OpenExisting, vfs.MaximumAllowed|entity.Delete)
	doc.DeleteOnClose = true
	if _, err := n.Open(ctx, asAlice, doc); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a delete-on-close open once the directory is bob's: %v, want ErrAccess", err)
	}
	n.must(t, n.cp.SetGrant(ctx, entity.ShareGrant{Share: n.share, Principal: alice.Principal, Access: entity.AccessNone}))
	if _, err := n.Open(ctx, asAlice, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("an open once alice's grant is gone: %v, want ErrAccess", err)
	}
}

// On a share narrowed to read-only, a maximal open of a file whose parent
// grants deleting a child is granted the read row alone, and one asking
// delete by name is ErrReadOnly.
func TestAReadOnlyShareStripsAMaximalOpen(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o755))
	_, err := n.fs.Store.Files().SetAttrs(ctx, n.fs.Admin, d, entity.Attrs{}.WithOwner(alice.Principal))
	n.must(t, err)
	n.fs.Create(t, n.fs.Admin, d, "f", kvstore.Regular(0o666))
	n.must(t, n.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: n.share, ReadOnly: true}))
	asAlice := n.smbClient(t, "alice-pc", alice.Principal)
	const readRow = entity.ReadData | entity.ReadNamedAttrs | entity.Execute | entity.ReadAttributes | entity.ReadACL | entity.Synchronize
	r, err := n.Open(ctx, asAlice, byName(d, "f", vfs.OpenExisting, vfs.MaximumAllowed))
	if err != nil || r.MaximalAccess&^readRow != 0 || r.MaximalAccess&entity.ReadData == 0 {
		t.Errorf("a maximal open on a read-only share: %v, %v; want the read row", r.MaximalAccess, err)
	}
	if _, err := n.Open(ctx, asAlice, byName(d, "f", vfs.OpenExisting, vfs.MaximumAllowed|entity.Delete)); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("an open asking delete on a read-only share: %v, want ErrReadOnly", err)
	}
}

// V28: a creating open into a directory marked delete on close is refused,
// over SMB and over NFS, and makes nothing.
func TestACreatingOpenIntoADeletePendingDirectoryIsRefused(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o777))
	n.markDeletePending(t, "d", d)
	n.user(t, "u", "1000", n.share, entity.AccessReadWrite)
	for name, c := range map[string]vfs.Call{"SMB": n.smbClient(t, "pc", n.fs.Admin.Principal), "NFSv4": n.nfs41(t, "host", 1000, 1)} {
		if _, err := n.Open(ctx, c, byName(d, "new", vfs.OpenOrCreate, entity.ReadData)); !errors.Is(err, vfs.ErrDeletePending) {
			t.Errorf("%s: a creating open into a delete-pending directory: %v, want ErrDeletePending", name, err)
		}
	}
	if _, _, err := n.Lookup(ctx, n.admin, d, []byte("new")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the refused create's name: %v, want ErrNotFound", err)
	}
}

// Delete on close asked at open removes the name at the last close, and the
// close answers the directory's WCC.
func TestDeleteOnCloseRunsAtTheLastClose(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	req := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	req.DeleteOnClose, req.Backup = true, true
	r, err := n.Open(ctx, c, req)
	n.must(t, err)
	n.stored(t, r.File.ID)
	other, err := n.Open(ctx, c, byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData))
	n.must(t, err)
	if w, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil || w != nil {
		t.Errorf("the marking open's close, another open left: %v, %v", w, err)
	}
	w, err := n.Close(ctx, c, other.Ref, vfs.Seqs{})
	if err != nil || w == nil {
		t.Errorf("the last close: %v, %v; want the directory's WCC", w, err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("f after its last close: %v, want gone", err)
	}
	if n.held(r.File.ID) {
		t.Error("f's content is held once its delete on close removed its last name")
	}
}

// A logoff closes its session's delete-on-close open as a Close would: the
// file's name goes and its content is released, though no Close came.
func TestALogoffRunsADeleteOnClose(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	c.Session = vfs.SessionRef{ID: 1}
	req := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	req.DeleteOnClose, req.Backup = true, true
	r, err := n.Open(ctx, c, req)
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Disconnect(ctx, c, c.Client, c.Session, vfs.SessionEnd{Logoff: true}); err != nil {
		t.Fatal(err)
	}
	if !n.released(r.File.ID, 0) {
		t.Error("f's content is held after its delete-on-close open's session logged off")
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("f after its delete-on-close open's session logged off: %v, want gone", err)
	}
}

// A session's end closes its volatile open of an unlinked file as a last
// Close would: the orphan's content is released.
func TestADisconnectReleasesAnOrphan(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	c.Session = vfs.SessionRef{ID: 1}
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	r, err := n.Open(ctx, c, byName(home, "f", vfs.OpenOrCreate, entity.ReadData))
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: home}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Disconnect(ctx, c, c.Client, c.Session, vfs.SessionEnd{}); err != nil {
		t.Fatal(err)
	}
	if !n.released(r.File.ID, 0) {
		t.Error("an unlinked file's content is held after its last open's session ended")
	}
}

// A backup-intent open with delete on close of a file whose DACL excludes
// everyone, by a share admin, and an EA set through it, succeed; once the
// admin grant is gone, so the DACL excludes its caller, its delete still runs
// at the close, under the privilege the open was granted. With the directory
// made immutable before the close the deferred unlink is refused, counted,
// and the file keeps its name; with no backup intent asked, the admin's open
// is decided without privilege, and refused.
func TestABackupDeleteOnCloseAndItsRefusal(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessAdmin)
	c := n.smbClient(t, "pc", alice.Principal)
	locked := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEDeny, Mask: entity.EntryRights, Who: entity.Everyone}}}
	for i, tc := range []struct{ backup, immutable bool }{{true, false}, {true, true}, {false, false}} {
		n.must(t, n.cp.SetGrant(ctx, entity.ShareGrant{Share: n.share, Principal: alice.Principal, Access: entity.AccessAdmin}))
		dir := n.create(t, n.admin, n.fs.Root, string(rune('a'+i)), vfs.CreateDir).Handle
		h, _ := n.fs.Create(t, n.fs.Admin, dir, "f", kvstore.Regular(0o644))
		_, err := n.fs.Store.Files().SetACL(ctx, n.fs.Admin, h, locked)
		n.must(t, err)
		req := byName(dir, "f", vfs.OpenExisting, entity.ReadData|entity.WriteNamedAttrs|entity.Delete)
		req.DeleteOnClose, req.Backup = true, tc.backup
		r, err := n.Open(ctx, c, req)
		if !tc.backup {
			if !errors.Is(err, vfs.ErrAccess) {
				t.Errorf("an admin's open with delete on close of a file denying everyone, without backup intent: %v, want ErrAccess", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("an admin's open with delete on close of a file denying everyone: %v", err)
		}
		ref := r.Ref
		if _, err := n.SetXattr(ctx, c, vfs.Target{H: h, Open: &ref}, vfs.Xattr{Name: []byte("user.k"), Value: []byte("v")}, vfs.XattrEither); err != nil {
			t.Errorf("an EA set through the open: %v", err)
		}
		n.must(t, n.cp.SetGrant(ctx, entity.ShareGrant{Share: n.share, Principal: alice.Principal, Access: entity.AccessReadWrite}))
		if tc.immutable {
			_, err := n.fs.Store.Files().SetAttrs(ctx, n.fs.Admin, dir, entity.Attrs{}.WithFlags(entity.FlagImmutable))
			n.must(t, err)
		}
		before := testutil.ToFloat64(n.refusals)
		if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
			t.Errorf("the close: %v", err)
		}
		_, _, err = n.Lookup(ctx, n.admin, dir, []byte("f"))
		refused := testutil.ToFloat64(n.refusals) - before
		kept := !tc.backup || tc.immutable
		switch {
		case !kept && (!errors.Is(err, vfs.ErrNotFound) || refused != 0):
			t.Errorf("%+v: after the close: %v, %v refusals; want f gone", tc, err, refused)
		case kept && (err != nil || refused != 1):
			t.Errorf("%+v: after the close: %v, %v refusals; want f kept and one refusal", tc, err, refused)
		}
	}
}

// V38: a last close running a delete on close and an unlink of another name
// of the same file, the directory's ID below the file's, never wait for each
// other's locks, and leave exactly the third name.
func TestADeleteOnCloseTakesTheDirectoryAfterTheFile(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	// The row's 10⁴ runs take about 20s; under -race, about 30 times that,
	// so the race detector sees 300, and -short 50.
	runs := 10_000
	switch {
	case testing.Short():
		runs = 50
	case raceEnabled:
		runs = 300
	}
	var dir vfs.CreateResult
	for i := range runs {
		// A directory's read applies each time delta not yet folded, and
		// nothing folds them here, so each directory serves 100 runs alone,
		// its ID in the lower half so a file's falls above it as often.
		for i%100 == 0 {
			dir = n.create(t, n.admin, n.fs.Root, fmt.Sprint("d", i), vfs.CreateDir)
			if dir.File.ID[0] < 0x80 {
				break
			}
			_, err := n.Rmdir(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte(fmt.Sprint("d", i)))
			n.must(t, err)
		}
		var f vfs.CreateResult
		for {
			f = n.create(t, n.admin, dir.Handle, "a", vfs.CreateRegular)
			if bytes.Compare(dir.File.ID[:], f.File.ID[:]) < 0 {
				break
			}
			_, err := n.Unlink(ctx, n.admin, vfs.Target{H: dir.Handle}, []byte("a"))
			n.must(t, err)
		}
		for _, name := range []string{"b", "c"} {
			_, err := n.Link(ctx, n.admin, dir.Handle, []byte(name), f.Handle)
			n.must(t, err)
		}
		req := byName(dir.Handle, "a", vfs.OpenExisting, entity.ReadData|entity.Delete)
		req.DeleteOnClose = true
		r, err := n.Open(ctx, c, req)
		n.must(t, err)
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		var wg sync.WaitGroup
		var closeErr, unlinkErr error
		wg.Go(func() { _, closeErr = n.Close(deadline, c, r.Ref, vfs.Seqs{}) })
		wg.Go(func() { _, unlinkErr = n.Unlink(deadline, n.admin, vfs.Target{H: dir.Handle}, []byte("b")) })
		wg.Wait()
		cancel()
		if closeErr != nil || unlinkErr != nil {
			t.Fatalf("run %d: the close %v, the unlink %v; want neither to wait out its deadline", i, closeErr, unlinkErr)
		}
		left := n.list(t, n.admin, dir.Handle, 0, 0)
		if len(left) != 1 || left[0] != "c" {
			t.Fatalf("run %d: names left %q, want only c", i, left)
		}
		_, err = n.Unlink(ctx, n.admin, vfs.Target{H: dir.Handle}, []byte("c"))
		n.must(t, err)
	}
}

// A share marked for removal before a restart has its open state dropped by
// the start: once Start returns, its persistent open is gone and its holder
// has been told RevokedShare.
func TestAShareMarkedBeforeARestartIsDroppedAtStart(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	pc := n.smbClient(t, "alice-pc", alice.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "disk", kvstore.Regular(0o666))
	req := byName(n.fs.Root, "disk", vfs.OpenExisting, entity.ReadData)
	req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, [16]byte{1}
	r, err := n.Open(ctx, pc, req)
	n.must(t, err)
	n.must(t, n.cp.DeleteShare(ctx, n.share, entity.SnapshotsRefuse))

	g := n.restart(t)
	cb := &vfstest.RecordingCallbacks{}
	g.reg.register(pc.Client, "", cb)
	g.must(t, g.Start(ctx))
	who := openstate.Caller{Client: pc.Client, Principal: alice.Principal, Protocol: entity.ProtocolSMB}
	m := openstate.ReconnectMatch{Share: n.share, Create: [16]byte{1}}
	if _, err := g.tab.Opens().Reconnect(ctx, who, r.Ref.Open, m, vfs.SessionRef{ID: 2}); err == nil {
		t.Error("the removed share's persistent open reconnected after the start")
	}
	got := calls(t, cb, 1)
	if len(got) != 1 || got[0].Method != "Revoked" || got[0].Args[0].(vfs.Revocation).Reason != openstate.RevokedShare {
		t.Errorf("the holder was told %v, want RevokedShare", got)
	}
}

// second adds a share at /b that the admin and alice may use, its root and a
// file in it, answering the share, its root and the file's handle.
func (n *names) second(t *testing.T, alice entity.User) (entity.ShareID, vfs.Handle, vfs.Handle) {
	t.Helper()
	ctx := t.Context()
	b := n.addShare(t, "b", "/b")
	n.must(t, n.cp.SetGrant(ctx, entity.ShareGrant{Share: b, Principal: n.fs.Admin.Principal, Access: entity.AccessAdmin}))
	n.must(t, n.cp.SetGrant(ctx, entity.ShareGrant{Share: b, Principal: alice.Principal, Access: entity.AccessReadWrite}))
	root, _, err := n.fs.Store.Files().Root(ctx, n.fs.Admin, b)
	n.must(t, err)
	h, _ := n.fs.Create(t, n.fs.Admin, root, "disk", kvstore.Regular(0o666))
	return b, root, h
}

// Start serves every share's own shard: a persistent open in each of two
// shares is installed by the restart, so each reconnects.
func TestStartServesEveryShare(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	b, bRoot, _ := n.second(t, alice)
	pc := n.smbClient(t, "alice-pc", alice.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "disk", kvstore.Regular(0o666))
	opens := []struct {
		share  entity.ShareID
		root   vfs.Handle
		create [16]byte
		ref    vfs.OpenRef
	}{{share: n.share, root: n.fs.Root, create: [16]byte{1}}, {share: b, root: bRoot, create: [16]byte{2}}}
	for i, o := range opens {
		req := byName(o.root, "disk", vfs.OpenExisting, entity.ReadData)
		req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, o.create
		r, err := n.Open(ctx, pc, req)
		n.must(t, err)
		opens[i].ref = r.Ref
	}

	g := n.restart(t)
	g.reg.register(pc.Client, "", &vfstest.RecordingCallbacks{})
	g.must(t, g.Start(ctx))
	who := openstate.Caller{Client: pc.Client, Principal: alice.Principal, Protocol: entity.ProtocolSMB}
	for _, o := range opens {
		m := openstate.ReconnectMatch{Share: o.share, Create: o.create}
		if _, err := g.tab.Opens().Reconnect(ctx, who, o.ref.Open, m, vfs.SessionRef{ID: 2}); err != nil {
			t.Errorf("the persistent open in share %v after the restart: %v", o.share, err)
		}
	}
}

// A share added while serving has its shard served by the first call that
// sees it: an open there is granted with no restart.
func TestAShareAddedWhileServingIsServed(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	pc := n.smbClient(t, "alice-pc", alice.Principal)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "disk", kvstore.Regular(0o666))
	if _, err := n.Open(ctx, pc, byName(n.fs.Root, "disk", vfs.OpenExisting, entity.ReadData)); err != nil {
		t.Fatal(err)
	}
	_, bRoot, _ := n.second(t, alice)
	if _, err := n.Open(ctx, pc, byName(bRoot, "disk", vfs.OpenExisting, entity.ReadData)); err != nil {
		t.Errorf("an open in a share added after Start: %v", err)
	}
}

// failMark is a releaser that finds every file held, or fails with err, or
// records the outcome each release reports.
type failMark struct {
	held bool
	err  error
	mu   sync.Mutex
	got  []bool
}

func (m *failMark) Releasing(context.Context, openstate.FileKey) (func(bool), bool, error) {
	return func(ok bool) { m.mu.Lock(); m.got = append(m.got, ok); m.mu.Unlock() }, m.held, m.err
}

// A release is marked first: a file the mark finds held keeps its content,
// and an unmarked one is deleted and its mark ended as released.
func TestAReleaseIsMarkedFirst(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	for _, held := range []bool{true, false} {
		m := &failMark{held: held}
		n.marks = m
		_, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
		n.stored(t, f.ID)
		_, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("f"))
		n.must(t, err)
		switch {
		case held && (!n.held(f.ID) || len(m.got) != 0):
			t.Errorf("a release the mark found held: content held %v, marks ended %v", n.held(f.ID), m.got)
		case !held && (n.held(f.ID) || len(m.got) != 1 || !m.got[0]):
			t.Errorf("a release marked: content held %v, marks ended %v; want deleted and one ended as released", n.held(f.ID), m.got)
		}
	}
}

// The release mark is open state's: a release run while the table holds an
// open of the file finds it held and leaves its content, as the engine's own
// re-check of the holders is not relied on.
func TestAReleaseOfAnOpenFileFindsItHeld(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	_, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	n.stored(t, f.ID)
	_, err := n.Open(ctx, c, byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData))
	n.must(t, err)
	n.must(t, n.releaseAlone(ctx, f.Share, f.ID))
	if !n.held(f.ID) {
		t.Error("a release of a file open in the table deleted its content")
	}
}

// A failed start leaves the filesystem built, with nothing of its own
// running, and a start that then succeeds serves; L29: each start raises the
// shard's incarnation, so each grace is a new instance.
func TestAFailedStartLeavesItBuilt(t *testing.T) {
	f := newFixture(t, nil)
	f.tab.Stop()
	if err := f.Start(t.Context()); err == nil {
		t.Fatal("Start with open state stopped succeeded")
	}
	if f.inState() != built || filesystemGoroutines() != 0 {
		t.Errorf("after a failed start: %v with %d goroutines, want built with none", f.inState(), filesystemGoroutines())
	}
	g := f.restart(t)
	g.must(t, g.Start(t.Context()))
	h := g.restart(t)
	h.must(t, h.Start(t.Context()))
	sh, err := h.fs.Store.Shard(t.Context(), 0)
	if err != nil || sh.Incarnation != 3 || sh.GraceInstance != 3 {
		t.Errorf("the shard after three starts: %+v, %v; want incarnation and grace instance 3", sh, err)
	}
}

// A delegation's stateid presented on no session is NotYours, its holder's
// own included, since NFSv4.0 is offered no delegation: a truncate, a close,
// a rename and an unlink through it.
func TestADelegationOnNoSessionIsNotYours(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	n.user(t, "alice", "1000", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o777))
	n.fs.Create(t, n.fs.Admin, d, "f", kvstore.Regular(0o666))
	c := n.nfs41(t, "a-host", 1000, 1)
	q := byName(d, "f", vfs.OpenExisting, entity.ReadData|entity.WriteData)
	q.Want = vfs.GrantRead | vfs.GrantWrite
	r, err := n.Open(ctx, c, q)
	if err != nil || r.Grant == nil {
		t.Fatalf("the delegated open answered %+v, %v", r.Grant, err)
	}
	ref := vfs.Delegated(r.Grant.ID, r.Ref.Handle)
	off := c
	off.Session = vfs.SessionRef{}
	if _, err := n.SetAttr(ctx, off, vfs.Target{H: r.Ref.Handle, Open: &ref}, size(0)); !notYours(err) {
		t.Errorf("a truncate through the delegation on no session: %v, want NotYours", err)
	}
	if _, err := n.Close(ctx, off, ref, vfs.Seqs{}); !notYours(err) {
		t.Errorf("a close of the delegation on no session: %v, want NotYours", err)
	}
	if _, err := n.Rename(ctx, off, vfs.Target{H: d, Open: &ref}, []byte("f"), d, []byte("g"), false); !notYours(err) {
		t.Errorf("a rename through the delegation on no session: %v, want NotYours", err)
	}
	if _, err := n.Unlink(ctx, off, vfs.Target{H: d, Open: &ref}, []byte("f")); !notYours(err) {
		t.Errorf("an unlink through the delegation on no session: %v, want NotYours", err)
	}
}

// delaying is open state's checks, answering a removal ErrDelay, as a recall
// under way does, or refuse when set, while hold is set.
type delaying struct {
	openstate.Checks
	hold   atomic.Bool
	refuse error
}

func (d *delaying) CheckChange(ctx context.Context, who openstate.Caller, f openstate.FileKey, via *openstate.OpenRef, k openstate.ChangeMask) error {
	if k&openstate.ChangeRemove != 0 && d.hold.Load() {
		if d.refuse != nil {
			return d.refuse
		}
		return &openstate.DelayError{Cause: openstate.DelayRecall}
	}
	return d.Checks.CheckChange(ctx, who, f, via, k)
}

// A deferred unlink open state delays at the last close, as it recalls a
// grant, runs once the recall ends: the name goes, and nothing is counted
// refused.
func TestADelayedDeleteOnCloseRunsOnceTheRecallEnds(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	d := &delaying{Checks: n.checks}
	d.hold.Store(true)
	n.checks = d
	before := testutil.ToFloat64(n.refusals)
	if w, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil || w != nil {
		t.Errorf("the last close while a recall runs: %v, %v; want no WCC", w, err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil {
		t.Fatalf("f while the recall runs: %v, want kept", err)
	}
	d.hold.Store(false)
	for range 200 {
		if _, _, err = n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refused := testutil.ToFloat64(n.refusals) - before; !errors.Is(err, vfs.ErrNotFound) || refused != 0 {
		t.Errorf("f once the recall ends: %v, %v refusals; want gone and none", err, refused)
	}
}

// A deferred unlink whose release then fails has run all the same: the last
// close answers the directory's WCC, the name is gone, and nothing is
// counted refused.
func TestAFailedReleaseAfterADeleteOnCloseIsNoRefusal(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	n.marks = &failMark{err: errors.New("mark lost")}
	before := testutil.ToFloat64(n.refusals)
	w, err := n.Close(ctx, c, r.Ref, vfs.Seqs{})
	if refused := testutil.ToFloat64(n.refusals) - before; err != nil || w == nil || refused != 0 {
		t.Errorf("the last close, its release failing: %v, %v, %v refusals; want the WCC and none", w, err, refused)
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("f after its delete on close: %v, want gone", err)
	}
}

// An overwriting open of an unlinked file nothing holds, whose truncate
// fails, is undone, and the file, held by no open once it is, is released.
func TestAFailedOverwriteOfAnUnlinkedFileReleasesIt(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, file := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	n.stored(t, file.ID)
	_, err := n.fs.Store.Namespace().Unlink(ctx, n.fs.Admin, n.fs.Root, []byte("f"))
	n.must(t, err)
	n.engine.truncErr = errors.New("disk gone")
	// The client drops as the undo's release begins: the release lands all
	// the same.
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	n.marks = &cancelling{releaser: n.marks, cancel: cancel}
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	if _, err := n.Open(call, c, vfs.OpenRequest{H: h, Disposition: vfs.Overwrite, Access: entity.ReadData}); err == nil {
		t.Fatal("an overwrite whose truncate fails was granted")
	}
	if n.held(file.ID) {
		t.Error("the unlinked file's content is held once its failed overwrite was undone")
	}
}

// failRecovery is open state's recovery, failing every serve while serve is
// set and every drop while drop is, and counting each share's serves that
// succeed.
type failRecovery struct {
	openstate.Recovery
	mu          sync.Mutex
	serve, drop error
	served      map[entity.ShareID]int
}

func (r *failRecovery) ServeShard(ctx context.Context, share entity.ShareID, s openstate.ShardID, instance uint64) error {
	r.mu.Lock()
	err := r.serve
	r.mu.Unlock()
	if err == nil {
		err = r.Recovery.ServeShard(ctx, share, s, instance)
	}
	if err == nil {
		r.mu.Lock()
		r.served[share]++
		r.mu.Unlock()
	}
	return err
}

func (r *failRecovery) DropShare(ctx context.Context, share entity.ShareID, moved netip.Addr) error {
	r.mu.Lock()
	err := r.drop
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.Recovery.DropShare(ctx, share, moved)
}

// set sets the serve and drop failures.
func (r *failRecovery) set(serve, drop error) {
	r.mu.Lock()
	r.serve, r.drop = serve, drop
	r.mu.Unlock()
}

// V33: Start failing at each of its steps, reading the share list, reading
// the shares being removed, raising the incarnations, serving a shard and
// dropping a removed share, answers that step's error and leaves the
// filesystem built with no goroutine of its own; Start retried then serves,
// each shard served once.
func TestEachFailedStartStepLeavesItBuilt(t *testing.T) {
	boom := errors.New("boom")
	for _, step := range []string{"List", "Removing", "RaiseAtStart", "ServeShard", "DropShare"} {
		t.Run(step, func(t *testing.T) {
			ctx := t.Context()
			db := &failKV{}
			fx := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV { db.KV = memKV(tb); return db })
			cp := fx.Store.ControlPlane()
			live := entity.Share{ID: entity.NewShareID(), Name: "b", Path: "/b", Root: entity.NewFileID()}
			tab, err := openstate.New(openstate.Deps{Records: fx.Store, Clock: sim.NewTestClock(time.Unix(0, 0)), Rand: rand.Reader})
			s, err2 := New(Deps{Meta: fx.Store, Open: tab, Engine: newFakeContent()})
			if err := errors.Join(err, err2, cp.PutShare(ctx, live), cp.DeleteShare(ctx, fx.Share.ID, entity.SnapshotsRefuse)); err != nil {
				t.Fatal(err)
			}
			if err := tab.Start(ctx, s, s.Admission); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Drain(context.Background()); _ = s.Stop(); tab.Stop() })
			rec := &failRecovery{Recovery: s.recovery, served: map[entity.ShareID]int{}}
			s.recovery = rec
			views := 0
			switch step {
			case "List", "Removing":
				nth := 1
				if step == "Removing" {
					nth = 2
				}
				db.failing(func(update bool) error {
					if !update {
						if views++; views == nth {
							return boom
						}
					}
					return nil
				})
			case "RaiseAtStart":
				db.failing(func(update bool) error {
					if update {
						return boom
					}
					return nil
				})
			case "ServeShard":
				rec.set(boom, nil)
			case "DropShare":
				rec.set(nil, boom)
			}
			if err := s.Start(ctx); !errors.Is(err, boom) {
				t.Fatalf("Start failing at %s: %v, want its error", step, err)
			}
			if s.inState() != built || filesystemGoroutines() != 0 {
				t.Errorf("after a failed start: %v with %d goroutines, want built with none", s.inState(), filesystemGoroutines())
			}
			db.failing(nil)
			rec.set(nil, nil)
			if err := s.Start(ctx); err != nil {
				t.Fatalf("Start retried: %v", err)
			}
			for _, id := range []entity.ShareID{live.ID, fx.Share.ID} {
				if n := rec.served[id]; n != 1 {
					t.Errorf("share %v served %d times, want once", id, n)
				}
			}
		})
	}
}

// A share added while serving whose shard fails to serve fails its own
// calls alone: another share's call is answered, and once a serve
// succeeds, so is the share's.
func TestAShareWhoseServeFailsFailsAlone(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	pc := n.smbClient(t, "alice-pc", alice.Principal)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "disk", kvstore.Regular(0o666))
	rec := &failRecovery{Recovery: n.recovery, served: map[entity.ShareID]int{}}
	n.recovery = rec
	rec.set(errors.New("boom"), nil)
	_, bRoot, _ := n.second(t, alice)
	if _, err := n.Open(ctx, pc, byName(n.fs.Root, "disk", vfs.OpenExisting, entity.ReadData)); err != nil {
		t.Errorf("an open in a share served: %v", err)
	}
	if _, err := n.Open(ctx, pc, byName(bRoot, "disk", vfs.OpenExisting, entity.ReadData)); !errors.Is(err, vfs.ErrUnavailable) {
		t.Errorf("an open in the share whose serve fails: %v, want ErrUnavailable", err)
	}
	rec.set(nil, nil)
	if _, err := n.Open(ctx, pc, byName(bRoot, "disk", vfs.OpenExisting, entity.ReadData)); err != nil {
		t.Errorf("an open in the share once its serve succeeds: %v", err)
	}
}

// V29: a creating open and an unlink of the name it makes, racing, never
// wait for each other's locks, whichever of the new file's and the
// directory's IDs sorts first.
func TestACreatingOpenTakesItsLocksInOrder(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	for i := range 300 {
		name := fmt.Sprint("f", i)
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		var wg sync.WaitGroup
		var openErr, unlinkErr error
		wg.Go(func() {
			r, err := n.Open(deadline, c, byName(home, name, vfs.OpenOrCreate, entity.ReadData))
			if openErr = err; err == nil {
				_, _ = n.Close(ctx, c, r.Ref, vfs.Seqs{})
			}
		})
		wg.Go(func() {
			for deadline.Err() == nil {
				_, err := n.Unlink(deadline, n.admin, vfs.Target{H: home}, []byte(name))
				if !errors.Is(err, vfs.ErrNotFound) {
					unlinkErr = err
					return
				}
			}
		})
		wg.Wait()
		cancel()
		if openErr != nil || unlinkErr != nil {
			t.Fatalf("run %d: the open %v, the unlink %v; want neither to wait out its deadline", i, openErr, unlinkErr)
		}
	}
}

// probing is open state's opens, trying each open's file lock as open state
// records the open: free reports, per open, whether another call could take
// it then.
type probing struct {
	openstate.Opens
	locks *lockorder.Locks
	free  []bool
}

func (p *probing) Open(ctx context.Context, who openstate.Caller, f openstate.FileKey, r openstate.OpenRequest) (openstate.OpenResult, error) {
	try, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	unlock, err := p.locks.Take(try, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(f.ID)}})
	cancel()
	if err == nil {
		unlock()
	}
	p.free = append(p.free, err == nil)
	return p.Opens.Open(ctx, who, f, r)
}

// RFC 17 §5.0: a creating open holds the new file's admission lock when open
// state records the open, as an open of a file that exists does.
func TestACreatingOpenHoldsTheNewFilesLock(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	p := &probing{Opens: n.opens, locks: &n.locks}
	n.opens = p
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	for _, d := range []vfs.Disposition{vfs.CreateNew, vfs.OpenExisting} {
		r, err := n.Open(ctx, c, byName(home, "f", d, entity.ReadData))
		n.must(t, err)
		if p.free[len(p.free)-1] {
			t.Errorf("disposition %d: the file's admission lock was free as open state recorded the open (created %v)", d, r.Created)
		}
	}
}

// An open whose answer fails once open state recorded it is closed again:
// its deny modes refuse no later open.
func TestAnOpenWhoseAnswerFailsIsClosed(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	n.engine.overlayErr = errors.New("overlay lost")
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData, Deny: entity.ReadData | entity.WriteData | entity.Delete}
	if _, err := n.Open(ctx, n.smbClient(t, "pc", n.fs.Admin.Principal), req); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	other := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
	if _, err := n.Open(ctx, n.smbClient(t, "build01", n.fs.Admin.Principal), other); err != nil {
		t.Errorf("another client's open after the failed one: %v, want granted", err)
	}
}

// An NFSv4.0 open whose answer fails once open state recorded it is closed
// again, though its owner numbers its requests: the close quotes no number,
// so the open's deny mode refuses no later open, and the owner's next
// request takes the next number.
func TestAFailedOpenOfANumberingOwnerIsClosed(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	g, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "g", kvstore.Regular(0o666))
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "h", kvstore.Regular(0o666))
	n.user(t, "carol", "4321", n.share, entity.AccessReadWrite)
	c := n.nfs41(t, "v40", 4321, 0) // on no session, as NFSv4.0
	owner := openstate.LockOwner{Client: c.Client, Name: []byte("open-owner")}
	seq := func(n uint32) vfs.Seqs { return vfs.Seqs{Owner: &n} }
	r, err := n.Open(ctx, c, vfs.OpenRequest{H: g, Disposition: vfs.OpenExisting, Access: entity.ReadData, Owner: owner, Seqs: seq(1)})
	n.must(t, err)
	_, err = n.ConfirmOpen(ctx, c, r.Ref, seq(2))
	n.must(t, err)
	n.engine.overlayErr = errors.New("overlay lost")
	deny := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData, Deny: entity.ReadData | entity.WriteData, Owner: owner, Seqs: seq(3)}
	if _, err := n.Open(ctx, c, deny); err == nil {
		t.Fatal("an open whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	if _, err := n.Open(ctx, n.smbClient(t, "build01", n.fs.Admin.Principal), vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}); err != nil {
		t.Errorf("another client's open after the failed one: %v, want granted", err)
	}
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData, Owner: owner, Seqs: seq(4)}); err != nil {
		t.Errorf("the owner's next open, numbered 4: %v, want granted", err)
	}
}

// A reconnect whose answer fails once open state attached the open closes it
// again: the client's own fresh open and another client's are granted.
func TestAFailedReconnectIsClosed(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData, Deny: entity.ReadData | entity.WriteData | entity.Delete}
	req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, [16]byte{1}
	r, err := n.Open(ctx, pc, req)
	n.must(t, err)
	_, err = n.Disconnect(ctx, pc, pc.Client, pc.Session, vfs.SessionEnd{})
	n.must(t, err)
	pc.Session = vfs.SessionRef{ID: 2}
	n.engine.overlayErr = errors.New("journal read failed")
	if _, err := n.Open(ctx, pc, vfs.OpenRequest{H: h, Reconnect: &r.Ref, CreateID: [16]byte{1}}); err == nil {
		t.Fatal("a reconnect whose file cannot be read was answered")
	}
	n.engine.overlayErr = nil
	other := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
	if _, err := n.Open(ctx, pc, other); err != nil {
		t.Errorf("the client's fresh open after its failed reconnect: %v, want granted", err)
	}
	if _, err := n.Open(ctx, n.smbClient(t, "build01", n.fs.Admin.Principal), other); err != nil {
		t.Errorf("another client's open after the failed reconnect: %v, want granted", err)
	}
}

// bounded is the engine failing every overlay read on a done context, as a
// store bounded by its call's context does.
type bounded struct{ Content }

func (b bounded) Overlay(ctx context.Context, s entity.ShareID, f entity.FileID) (Overlay, bool, error) {
	if err := ctx.Err(); err != nil {
		return Overlay{}, false, err
	}
	return b.Content.Overlay(ctx, s, f)
}

// ending is open state's opens, ending the call's context once a reconnect
// has attached, as a client dropping then does.
type ending struct {
	openstate.Opens
	cancel context.CancelFunc
}

func (o *ending) Reconnect(ctx context.Context, who openstate.Caller, id entity.OpenID, m openstate.ReconnectMatch, s entity.SessionRef) (openstate.OpenResult, error) {
	r, err := o.Opens.Reconnect(ctx, who, id, m, s)
	o.cancel()
	return r, err
}

// A reconnect whose client drops once open state attached it still answers
// the open it attached.
func TestAReconnectOutlivesItsCall(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
	req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, [16]byte{1}
	r, err := n.Open(ctx, pc, req)
	n.must(t, err)
	_, err = n.Disconnect(ctx, pc, pc.Client, pc.Session, vfs.SessionEnd{})
	n.must(t, err)
	pc.Session = vfs.SessionRef{ID: 2}
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	n.opens = &ending{Opens: n.opens, cancel: cancel}
	n.Filesystem.engine = bounded{n.engine}
	ref := r.Ref
	if _, err := n.Open(call, pc, vfs.OpenRequest{H: h, Reconnect: &ref, CreateID: [16]byte{1}}); err != nil {
		t.Errorf("a reconnect whose client dropped once it attached: %v, want its open answered", err)
	}
}

// An open asking delete on close without delete is refused ErrInvalid, and
// a creating one makes nothing.
func TestADeleteOnCloseWithoutDeleteIsInvalid(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData)
	doc.DeleteOnClose = true
	if _, err := n.Open(ctx, n.smbClient(t, "pc", n.fs.Admin.Principal), doc); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("an open asking delete on close without delete: %v, want ErrInvalid", err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the refused open's name: %v, want ErrNotFound", err)
	}
}

// A creating open asking system security is refused a caller other than an
// admin, and makes nothing; an admin's is granted.
func TestACreatingOpenAsksSystemSecurityOfAnAdmin(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o777))
	req := byName(d, "f", vfs.CreateNew, entity.ReadData|entity.SystemSecurity)
	if _, err := n.Open(ctx, n.smbClient(t, "pc", alice.Principal), req); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("alice's creating open asking system security: %v, want ErrAccess", err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, d, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the refused open's name: %v, want ErrNotFound", err)
	}
	if _, err := n.Open(ctx, n.smbClient(t, "build01", n.fs.Admin.Principal), req); err != nil {
		t.Errorf("an admin's creating open asking system security: %v", err)
	}
}

// A creating open asking a right no entry names, as on an existing file,
// is refused.
func TestACreatingOpenAsksOnlyNamedRights(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	bob := n.user(t, "bob", "", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "pub", kvstore.Dir(0o777))
	if _, err := n.Open(ctx, n.smbClient(t, "pc", bob.Principal), byName(d, "f", vfs.CreateNew, entity.ReadData|0x10000000)); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a creating open asking 0x10000000: %v, want ErrAccess", err)
	}
	if _, _, err := n.Lookup(ctx, n.admin, d, []byte("f")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the refused open's name: %v, want ErrNotFound", err)
	}
}

// An SMB admin's open with backup intent of a file whose DACL denies
// everyone answers the maximal access it holds without privilege, and so
// does its reconnect.
func TestABackupOpensMaximalAccessIsUnprivileged(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	locked := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEDeny, Mask: entity.EntryRights, Who: entity.Everyone}}}
	_, err := n.fs.Store.Files().SetACL(ctx, n.fs.Admin, h, locked)
	n.must(t, err)
	pc := n.smbClient(t, "pc", n.fs.Admin.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData, Backup: true}
	req.Durability, req.Timeout, req.CreateID = entity.DurabilityPersistent, time.Hour, [16]byte{1}
	r, err := n.Open(ctx, pc, req)
	if err != nil || r.MaximalAccess&entity.ReadData != 0 {
		t.Fatalf("a backup-intent open of a file denying everyone: %v, %v; want granted, its maximal access without read data", r.MaximalAccess, err)
	}
	if _, err := n.Disconnect(ctx, pc, pc.Client, pc.Session, vfs.SessionEnd{}); err != nil {
		t.Fatal(err)
	}
	pc.Session = vfs.SessionRef{ID: 2}
	ref := r.Ref
	back, err := n.Open(ctx, pc, vfs.OpenRequest{H: h, Reconnect: &ref, CreateID: [16]byte{1}})
	if err != nil || back.MaximalAccess&entity.ReadData != 0 {
		t.Errorf("its reconnect: %v, %v; want its maximal access without read data", back.MaximalAccess, err)
	}
}

// An SMB admin's delete on close asked without backup intent unlinks
// without privilege: once the directory denies everyone, it is refused at
// the close.
func TestADeleteOnCloseWithoutIntentIsUnprivileged(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	d := n.create(t, n.admin, n.fs.Root, "d", vfs.CreateDir).Handle
	n.fs.Create(t, n.fs.Admin, d, "f", kvstore.Regular(0o644))
	doc := byName(d, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	doc.DeleteOnClose = true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	locked := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEDeny, Mask: entity.EntryRights, Who: entity.Everyone}}}
	_, err = n.fs.Store.Files().SetACL(ctx, n.fs.Admin, d, locked)
	n.must(t, err)
	before := testutil.ToFloat64(n.refusals)
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = n.Lookup(ctx, n.admin, d, []byte("f"))
	if refused := testutil.ToFloat64(n.refusals) - before; err != nil || refused != 1 {
		t.Errorf("after the close: %v, %v refusals; want f kept and one refusal", err, refused)
	}
}

// An SMB admin's open without backup intent is decided as anyone's is in
// all of it, its lookup, create and truncate included: in a directory
// denying everyone it neither finds nor adds a name, it names no other
// owner, and its overwrite clears setuid; with the intent, it adds the name.
func TestAnAdminsOpenWithoutIntentIsUnprivilegedThroughout(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessAdmin)
	bob := n.user(t, "bob", "", n.share, entity.AccessReadWrite)
	c := n.smbClient(t, "alice-pc", alice.Principal)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "locked", kvstore.Dir(0o777))
	n.fs.Create(t, n.fs.Admin, d, "old", kvstore.Regular(0o644))
	deny := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEDeny, Mask: entity.EntryRights, Who: entity.Everyone}}}
	_, err := n.fs.Store.Files().SetACL(ctx, n.fs.Admin, d, deny)
	n.must(t, err)
	if _, err := n.Open(ctx, c, byName(d, "new", vfs.CreateNew, entity.ReadData)); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a creating open in a directory denying everyone: %v, want ErrAccess", err)
	}
	if _, err := n.Open(ctx, c, byName(d, "old", vfs.OpenExisting, entity.ReadData)); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("an open by name through a directory denying everyone: %v, want ErrAccess", err)
	}
	pub, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "pub", kvstore.Dir(0o777))
	req := byName(pub, "bobs", vfs.CreateNew, entity.ReadData)
	req.Create.Attrs = req.Create.Attrs.WithOwner(bob.Principal)
	if _, err := n.Open(ctx, c, req); !errors.Is(err, vfs.ErrPerm) {
		t.Errorf("a creating open naming bob its owner: %v, want ErrPerm", err)
	}
	h, _ := n.fs.Create(t, n.fs.Admin, pub, "prog", kvstore.Regular(0o777))
	_, err = n.fs.Store.Files().SetAttrs(ctx, n.fs.Admin, h, entity.Attrs{}.WithMode(0o4777))
	n.must(t, err)
	if _, err := n.Open(ctx, c, byName(pub, "prog", vfs.Overwrite, entity.WriteData)); err != nil {
		t.Fatal(err)
	}
	if f, err := n.fs.Store.Files().Get(ctx, n.fs.Admin, h); err != nil || f.Mode&entity.ModeSetuid != 0 {
		t.Errorf("an overwrite of a setuid file: %o, %v; want setuid cleared", f.Mode, err)
	}
	in := byName(d, "new", vfs.CreateNew, entity.ReadData)
	in.Backup = true
	if _, err := n.Open(ctx, c, in); err != nil {
		t.Errorf("a backup-intent creating open in a directory denying everyone: %v, want granted", err)
	}
	locked, _ := n.fs.Create(t, n.fs.Admin, pub, "locked-file", kvstore.Regular(0o644))
	_, err = n.fs.Store.Files().SetACL(ctx, n.fs.Admin, locked, deny)
	n.must(t, err)
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: locked, Disposition: vfs.OpenExisting, Access: entity.ReadData}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("an open by handle of a file denying everyone: %v, want ErrAccess", err)
	}
	const genericAll = entity.AccessMask(1 << 28)
	if _, err := n.Open(ctx, c, byName(pub, "generic", vfs.CreateNew, entity.ReadData|genericAll)); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a creating open asking a generic right: %v, want ErrAccess", err)
	}
}

// Over NFS, which has no backup intent, an open is decided with the caller's
// privilege: a privileged caller's open of a file whose DACL denies everyone
// is granted.
func TestAnNFSOpenIsDecidedWithThePrivilege(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	h, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	locked := entity.ACL{Entries: []entity.ACE{{Type: entity.ACEDeny, Mask: entity.EntryRights, Who: entity.Everyone}}}
	_, err := n.fs.Store.Files().SetACL(ctx, n.fs.Admin, h, locked)
	n.must(t, err)
	c := proven(n.fs.Admin.Principal, inLab)
	info := vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("host"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus}
	pr, err := n.Propose(ctx, c, info)
	n.must(t, err)
	info.Proposal = &pr
	id, _, err := n.Connect(ctx, c, info, &vfstest.RecordingCallbacks{})
	n.must(t, err)
	c.Protocol, c.Client, c.Session = entity.ProtocolNFSv4, id, vfs.SessionRef{ID: 1}
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}); err != nil {
		t.Errorf("a privileged NFS open of a file denying everyone: %v, want granted", err)
	}
}

// cancelling is a releaser that ends the call's context as the release
// begins, as a client dropping does, and fails a release on an ended
// context, as a store bounded by its context does.
type cancelling struct {
	releaser
	cancel context.CancelFunc
}

func (m *cancelling) Releasing(ctx context.Context, f openstate.FileKey) (func(bool), bool, error) {
	m.cancel()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return m.releaser.Releasing(ctx, f)
}

// A last close's release outlives its call: the client dropping as the
// release begins leaves the unlinked file released all the same.
func TestALastClosesReleaseOutlivesItsCall(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	r, err := n.Open(ctx, c, byName(home, "f", vfs.OpenOrCreate, entity.ReadData))
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: home}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	n.marks = &cancelling{releaser: n.marks, cancel: cancel}
	if _, err := n.Close(call, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if !n.released(r.File.ID, 0) {
		t.Error("an unlinked file whose last close's client dropped mid-release is still held")
	}
}

// slowMark delays every release, as a release over a remote store or a
// large file's Delete takes time.
type slowMark struct {
	releaser
	wait time.Duration
}

func (m *slowMark) Releasing(ctx context.Context, f openstate.FileKey) (func(bool), bool, error) {
	select {
	case <-time.After(m.wait):
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	return m.releaser.Releasing(ctx, f)
}

// A last close answers once its release has run.
func TestALastCloseAnswersOnceItsReleaseRan(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	r, err := n.Open(ctx, c, byName(home, "f", vfs.OpenOrCreate, entity.ReadData))
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: home}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	n.marks = &slowMark{releaser: n.marks, wait: 100 * time.Millisecond}
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if n.held(r.File.ID) {
		t.Error("a last close answered before its release ran")
	}
}

// A delete on close's release outlives its call: the client dropping as the
// release begins leaves the file released all the same.
func TestADeleteOnClosesReleaseOutlivesItsCall(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	_, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o666))
	n.stored(t, f.ID)
	doc := byName(n.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	n.marks = &cancelling{releaser: n.marks, cancel: cancel}
	if _, err := n.Close(call, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if n.held(f.ID) {
		t.Error("a file deleted on close whose client dropped mid-release is still held")
	}
}

// An unlink's release outlives its call, as a last close's does.
func TestAnUnlinksReleaseOutlivesItsCall(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	_, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	n.stored(t, f.ID)
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	n.marks = &cancelling{releaser: n.marks, cancel: cancel}
	if _, err := n.Unlink(call, n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
		t.Errorf("an unlink whose client dropped as its release began: %v, want nil", err)
	}
	if n.held(f.ID) {
		t.Error("an unlinked file whose client dropped mid-release is still held")
	}
}

// A deferred unlink met by grace is tried again until the grace ends: the
// name goes then, and nothing is counted refused.
func TestADeleteOnCloseMetByGraceRunsOnceItEnds(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	d := &delaying{Checks: n.checks, refuse: openstate.ErrGrace}
	d.hold.Store(true)
	n.checks = d
	before := testutil.ToFloat64(n.refusals)
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	d.hold.Store(false)
	for range 300 {
		if _, _, err = n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refused := testutil.ToFloat64(n.refusals) - before; !errors.Is(err, vfs.ErrNotFound) || refused != 0 {
		t.Errorf("f once the grace ends: %v, %v refusals; want gone and none", err, refused)
	}
}

// graceThenRecall is open state's checks answering a removal ErrGrace for its
// first graces tries, then a recall's delay once, as a lease broken at
// grace's end does.
type graceThenRecall struct {
	openstate.Checks
	graces int32
	tries  atomic.Int32
}

func (g *graceThenRecall) CheckChange(ctx context.Context, who openstate.Caller, f openstate.FileKey, via *openstate.OpenRef, k openstate.ChangeMask) error {
	if k&openstate.ChangeRemove != 0 {
		switch n := g.tries.Add(1); {
		case n <= g.graces:
			return openstate.ErrGrace
		case n == g.graces+1:
			return &openstate.DelayError{Cause: openstate.DelayRecall}
		}
	}
	return g.Checks.CheckChange(ctx, who, f, via, k)
}

// A deferred unlink's tries met by grace spend none of a delay's retries: a
// recall met once a grace outlasting them ends is tried again, and the name
// goes.
func TestAGraceSpendsNoUnlinkRetry(t *testing.T) {
	first, longest := unlinkRetry, unlinkWait
	t.Cleanup(func() { unlinkRetry, unlinkWait = first, longest })
	unlinkRetry, unlinkWait = time.Millisecond, 20*time.Millisecond
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	n.checks = &graceThenRecall{Checks: n.checks, graces: unlinkRetries + 1}
	before := testutil.ToFloat64(n.refusals)
	start := time.Now()
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	for range 300 {
		if _, _, err = n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil || testutil.ToFloat64(n.refusals) != before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refused := testutil.ToFloat64(n.refusals) - before; !errors.Is(err, vfs.ErrNotFound) || refused != 0 {
		t.Errorf("f after %d grace answers and a recall: %v, %v refusals; want gone and none", unlinkRetries+1, err, refused)
	}
	// Each grace answer waits unlinkWait, not a delay's shorter first wait.
	if took, least := time.Since(start), time.Duration(unlinkRetries+1)*unlinkWait; took < least {
		t.Errorf("f gone %v after its close, want at least %v: a grace answer waited less than unlinkWait", took, least)
	}
}

// A close open state makes itself, a durable open timing out after its
// session ended, releases the unlinked file it held, found in its own share
// though another share is listed first. The shard's grace has ended first,
// so pending release recovery does not run it instead. The open is
// persistent: a durable one holding no handle caching is closed at the
// disconnect, not kept, and a handle lease is broken by the unlink.
func TestATimedOutDurableOpenReleasesItsUnlinkedFile(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	n.second(t, alice) // "b", listed before "data"
	h, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "disk", kvstore.Regular(0o666))
	n.stored(t, f.ID)
	pc := n.smbClient(t, "alice-pc", alice.Principal)
	pc.Session = vfs.SessionRef{ID: 1}
	req := vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData}
	req.Durability, req.Timeout = entity.DurabilityPersistent, time.Minute
	_, err := n.Open(ctx, pc, req)
	n.must(t, err)
	_, x := n.fs.Create(t, n.fs.Admin, n.fs.Root, "x", kvstore.Regular(0o644))
	n.stored(t, x.ID)
	_, err = n.fs.Store.Namespace().Unlink(ctx, n.fs.Admin, n.fs.Root, []byte("x"))
	n.must(t, err)
	if !n.released(x.ID, 11*time.Second) {
		t.Fatal("the shard's grace never ended")
	}
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("disk")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Disconnect(ctx, pc, pc.Client, pc.Session, vfs.SessionEnd{}); err != nil {
		t.Fatal(err)
	}
	n.sending.Wait() // a close the disconnect made has run its release
	if !n.held(f.ID) {
		t.Fatal("released while its durable open waits for a reconnect")
	}
	if !n.released(f.ID, 11*time.Second) {
		t.Error("an unlinked file whose durable open timed out is still held")
	}
}

// L29: each start serves the shard under the grace instance it raised, so
// a client that held state there owes a completion in each new grace.
func TestEachStartsGraceWaitsForANewCompletion(t *testing.T) {
	f := newFixture(t, nil)
	s := &shares{fixture: f, cp: f.fs.Store.ControlPlane(), share: f.fs.Share.ID}
	f.must(t, f.Start(t.Context()))
	ctx := t.Context()
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	c := s.nfs41(t, "host", 1000, 1)
	h, _ := f.fs.Create(t, f.fs.Admin, f.fs.Root, "f", kvstore.Regular(0o666))
	_, err := f.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData})
	f.must(t, err)
	g := f
	for want := uint64(2); want <= 3; want++ {
		g = g.restart(t)
		g.must(t, g.Start(ctx))
		st, err := g.Renew(ctx, c, c.Client)
		if err != nil || !st.ReclaimNeeded || len(st.Reclaim) != 1 || st.Reclaim[0].Instance != want {
			t.Fatalf("after start %d: %+v, %v; want grace instance %d owed", want, st, err, want)
		}
		g.must(t, g.ReclaimComplete(ctx, c, c.Client, st.Reclaim))
	}
}

// An overwriting open asks write data: a caller who may only read the file
// is refused, and nothing is truncated.
func TestAnOverwritingOpenAsksWriteData(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	truncated := false
	n.engine.truncating = func(entity.FileID) { truncated = true }
	c := n.smbClient(t, "pc", alice.Principal)
	if _, err := n.Open(ctx, c, byName(n.fs.Root, "f", vfs.Overwrite, entity.ReadData)); !errors.Is(err, vfs.ErrAccess) || truncated {
		t.Errorf("an overwriting open by a caller who may only read: %v, truncated %v; want ErrAccess and nothing truncated", err, truncated)
	}
}

// A backup intent is a privilege only for a caller holding it: a share
// user's backup-intent open with delete on close, who loses write on the
// directory before the close, has its deferred unlink refused.
func TestABackupIntentWithoutThePrivilegeGrantsNone(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o777))
	n.fs.Create(t, n.fs.Admin, d, "f", kvstore.Regular(0o666))
	c := n.smbClient(t, "pc", alice.Principal)
	req := byName(d, "f", vfs.OpenExisting, entity.ReadData|entity.Delete)
	req.DeleteOnClose, req.Backup = true, true
	r, err := n.Open(ctx, c, req)
	n.must(t, err)
	_, err = n.fs.Store.Files().SetAttrs(ctx, n.fs.Admin, d, entity.Attrs{}.WithMode(0o755))
	n.must(t, err)
	before := testutil.ToFloat64(n.refusals)
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = n.Lookup(ctx, n.admin, d, []byte("f"))
	if refused := testutil.ToFloat64(n.refusals) - before; err != nil || refused != 1 {
		t.Errorf("after the close: %v, %v refusals; want f kept and one refusal", err, refused)
	}
}

// An open that creates its file is granted what it asks: a create asking
// write with a mode granting none is granted, its maximal access the new
// file's own.
func TestACreatingOpenIsGrantedWhatItAsks(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	d, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "d", kvstore.Dir(0o777))
	c := n.smbClient(t, "pc", alice.Principal)
	req := byName(d, "ro", vfs.CreateNew, entity.ReadData|entity.WriteData)
	req.Create.Attrs = entity.Attrs{}.WithMode(0o444)
	r, err := n.Open(ctx, c, req)
	if err != nil || !r.Created || r.MaximalAccess&entity.WriteData != 0 {
		t.Errorf("a create asking write with mode 444: %v, %v; want granted, its maximal access without write data", r.MaximalAccess, err)
	}
}

// Each disposition opens, creates or truncates as its name says, CreateNew
// of an existing file refused by name and by handle.
func TestEachDispositionOpensCreatesOrTruncates(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	home, _ := n.fs.Create(t, n.fs.Admin, n.fs.Root, "home", kvstore.Dir(0o755))
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	for _, tc := range []struct {
		d                  vfs.Disposition
		truncates, creates bool
	}{
		{vfs.OpenExisting, false, false}, {vfs.CreateNew, false, true}, {vfs.OpenOrCreate, false, true},
		{vfs.Overwrite, true, false}, {vfs.OverwriteOrCreate, true, true}, {vfs.Supersede, true, true},
	} {
		name := fmt.Sprint("f", tc.d)
		h, f := n.fs.Create(t, n.fs.Admin, home, name, kvstore.Regular(0o644))
		n.stored(t, f.ID)
		r, err := n.Open(ctx, c, byName(home, name, tc.d, entity.ReadData))
		switch {
		case tc.d == vfs.CreateNew:
			if !errors.Is(err, vfs.ErrExist) {
				t.Errorf("disposition %d of an existing name: %v, want ErrExist", tc.d, err)
			}
			if _, err := n.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: tc.d, Access: entity.ReadData}); !errors.Is(err, vfs.ErrExist) {
				t.Errorf("disposition %d by handle: %v, want ErrExist", tc.d, err)
			}
		case err != nil || r.Created || (r.File.Size == 0) != tc.truncates:
			t.Errorf("disposition %d of an existing file: size %d, created %v, %v; want truncated %v", tc.d, r.File.Size, r.Created, err, tc.truncates)
		}
		missing := fmt.Sprint("new", tc.d)
		r, err = n.Open(ctx, c, byName(home, missing, tc.d, entity.ReadData))
		if tc.creates != (err == nil && r.Created) || !tc.creates && !errors.Is(err, vfs.ErrNotFound) {
			t.Errorf("disposition %d of a missing name: created %v, %v; want created %v", tc.d, r.Created, err, tc.creates)
		}
	}
}

// An open in the pseudo-filesystem is refused: ErrReadOnly for one that may
// create a name there, ErrIsDir otherwise.
func TestAnOpenInThePseudoFilesystemIsRefused(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root, _, err := n.Root(ctx, n.admin, vfs.ShareRef{})
	n.must(t, err)
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	if _, err := n.Open(ctx, c, byName(root, "x", vfs.OpenOrCreate, entity.ReadData)); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("a creating open in the pseudo-root: %v, want ErrReadOnly", err)
	}
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: root, Disposition: vfs.OpenExisting, Access: entity.ReadData}); !errors.Is(err, vfs.ErrIsDir) {
		t.Errorf("an open of the pseudo-root: %v, want ErrIsDir", err)
	}
}

// V17: a rename over a file no open holds releases the file it replaced.
func TestARenameOverAFileReleasesIt(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "a", kvstore.Regular(0o644))
	_, dst := n.fs.Create(t, n.fs.Admin, n.fs.Root, "b", kvstore.Regular(0o644))
	n.stored(t, dst.ID)
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("a"), n.fs.Root, []byte("b"), false); err != nil {
		t.Fatal(err)
	}
	if n.held(dst.ID) {
		t.Error("the file a rename replaced is held, with no name and no open")
	}
}

// A delete on close whose name came to name another file removes nothing:
// the file now under the name keeps it.
func TestADeleteOnCloseLeavesANameThatMoved(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	_, other := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if _, got, err := n.Lookup(ctx, n.admin, n.fs.Root, []byte("f")); err != nil || got.ID != other.ID {
		t.Errorf("the name after the close: %v, %v; want the new file %v kept", got.ID, err, other.ID)
	}
}

// A delete on close on a share narrowed to read-only after the open is
// refused at the close: the name stays and the refusal is counted.
func TestADeleteOnCloseOnAShareMadeReadOnlyIsRefused(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	n.must(t, n.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: n.share, ReadOnly: true}))
	before := testutil.ToFloat64(n.refusals)
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = n.Lookup(ctx, n.admin, n.fs.Root, []byte("f"))
	if refused := testutil.ToFloat64(n.refusals) - before; err != nil || refused != 1 {
		t.Errorf("after the close on a read-only share: %v, %v refusals; want f kept and one refusal", err, refused)
	}
}

// A deferred unlink still delayed when the filesystem stops is refused and
// counted.
func TestADelayedDeleteOnCloseIsRefusedAtStop(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	d := &delaying{Checks: n.checks}
	d.hold.Store(true)
	n.checks = d
	before := testutil.ToFloat64(n.refusals)
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	n.must(t, n.Drain(ctx))
	n.must(t, n.Stop())
	if refused := testutil.ToFloat64(n.refusals) - before; refused != 1 {
		t.Errorf("a deferred unlink delayed when the filesystem stopped: %v refusals, want 1", refused)
	}
}

// V17: a pending release in a second share's shard runs when that shard's
// grace ends after a restart.
func TestAPendingReleaseInASecondShareRunsAfterARestart(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	_, bRoot, h := n.second(t, alice)
	f, err := n.fs.Store.Files().Get(ctx, n.fs.Admin, h)
	n.must(t, err)
	n.storedIn(t, h.Share(), f.ID)
	if _, err := n.fs.Store.Namespace().Unlink(ctx, n.fs.Admin, bRoot, []byte("disk")); err != nil {
		t.Fatal(err)
	}
	g := n.restart(t)
	g.must(t, g.Start(ctx))
	if !g.released(f.ID, 11*time.Second) {
		t.Error("a pending release in the second share's shard never ran")
	}
}

// A file with setuid or setgid set is offered no write caching, as a write
// must still clear the bits; one without is.
func TestASetIDFileIsOfferedNoWriteCaching(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	n.user(t, "alice", "1000", n.share, entity.AccessReadWrite)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "plain", kvstore.Regular(0o666))
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "prog", kvstore.Regular(0o4777))
	c := n.nfs41(t, "a-host", 1000, 1)
	for name, want := range map[string]bool{"plain": true, "prog": false} {
		q := byName(n.fs.Root, name, vfs.OpenExisting, entity.ReadData|entity.WriteData)
		q.Want = vfs.GrantRead | vfs.GrantWrite
		r, err := n.Open(ctx, c, q)
		if got := r.Grant != nil && r.Grant.Kind&vfs.GrantWrite != 0; err != nil || got != want {
			t.Errorf("%s: write caching offered %v, %v; want %v", name, got, err, want)
		}
	}
}

// A share added while serving has its shard served by the first call to see
// the share list move, though that call is into another share.
func TestAnAddedShareIsServedByTheFirstCallThatSeesIt(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	alice := n.user(t, "alice", "", n.share, entity.AccessReadWrite)
	pc := n.smbClient(t, "alice-pc", alice.Principal)
	n.fs.Create(t, n.fs.Admin, n.fs.Root, "disk", kvstore.Regular(0o666))
	rec := &failRecovery{Recovery: n.recovery, served: map[entity.ShareID]int{}}
	n.recovery = rec
	b, _, _ := n.second(t, alice)
	if _, err := n.Open(ctx, pc, byName(n.fs.Root, "disk", vfs.OpenExisting, entity.ReadData)); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	got := rec.served[b]
	rec.mu.Unlock()
	if got != 1 {
		t.Errorf("share b served %d times after a call into another share saw it, want 1", got)
	}
}

// A retry under way when the filesystem stops is counted refused once its
// next try cannot be scheduled.
func TestARetryUnderWayAtStopIsRefused(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData|entity.Delete)
	doc.DeleteOnClose, doc.Backup = true, true
	r, err := n.Open(ctx, c, doc)
	n.must(t, err)
	d := &delaying{Checks: n.checks}
	d.hold.Store(true)
	n.checks = d
	before := testutil.ToFloat64(n.refusals)
	_, err = n.Close(ctx, c, r.Ref, vfs.Seqs{})
	n.must(t, err)
	// The root's admission lock held, the first retry, at 50ms, waits in
	// Take across Drain and Stop.
	unlock := n.Admission(fileOf(n.fs.Root))
	time.Sleep(200 * time.Millisecond)
	n.must(t, n.Drain(ctx))
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop() }()
	time.Sleep(100 * time.Millisecond)
	unlock()
	n.must(t, <-stopped)
	if got := testutil.ToFloat64(n.refusals) - before; got != 1 {
		t.Errorf("refusals after a stop during a retry: +%v, want +1", got)
	}
}

// phaseOne is the fake engine whose Delete also runs the store's release
// phase 1, as the engine's does, counting each file's Deletes; the first
// waits for open to close.
type phaseOne struct {
	*fakeContent
	st      *kvstore.Store
	share   entity.ShareID
	mu      sync.Mutex
	n       map[entity.FileID]int
	open    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (d *phaseOne) Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (<-chan error, error) {
	d.once.Do(func() { close(d.entered); <-d.open })
	d.mu.Lock()
	d.n[f]++
	d.mu.Unlock()
	if _, err := d.fakeContent.Delete(ctx, s, f); err != nil {
		return nil, err
	}
	return nil, d.st.Existence().Remove(ctx, d.share, f, 0, kvstore.RemoveOp{Version: entity.VersionOf(1 << 40), Kind: entity.RemoveRelease}, nil)
}

// Two releases of one unlinked file at once delete it once: the second
// reads the file under its admission lock, after the first removed it.
func TestTwoReleasesOfOneFileDeleteItOnce(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	_, f := n.fs.Create(t, n.fs.Admin, n.fs.Root, "f", kvstore.Regular(0o644))
	n.stored(t, f.ID)
	d := &phaseOne{fakeContent: n.engine, st: n.fs.Store, share: n.fs.Share.ID, n: map[entity.FileID]int{},
		open: make(chan struct{}), entered: make(chan struct{})}
	n.Filesystem.engine = d
	_, err := n.fs.Store.Namespace().Unlink(ctx, n.fs.Admin, n.fs.Root, []byte("f"))
	n.must(t, err)
	var wg sync.WaitGroup
	wg.Go(func() { _ = n.releaseAlone(ctx, n.fs.Share.ID, f.ID) })
	<-d.entered
	wg.Go(func() { _ = n.releaseAlone(ctx, n.fs.Share.ID, f.ID) })
	time.Sleep(50 * time.Millisecond) // the second is waiting
	close(d.open)
	wg.Wait()
	if got := d.n[f.ID]; got != 1 {
		t.Errorf("Deletes of one file released twice at once: %d, want 1", got)
	}
}
