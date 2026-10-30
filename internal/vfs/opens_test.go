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

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/sim"
	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest"
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

// stored gives f content in the fake engine.
func (f *fixture) stored(t *testing.T, id entity.FileID) {
	t.Helper()
	if _, err := f.engine.Write(t.Context(), id, 0, []byte("content"), WriteOpts{}); err != nil {
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
	id, err := s.Connect(t.Context(), c, info, &vfstest.RecordingCallbacks{})
	s.must(t, err)
	c.Protocol, c.Client, c.Session = entity.ProtocolNFSv4, id, vfs.SessionRef{ID: sess}
	return c
}

// An open creates, opens or refuses by its disposition, and a close ends it:
// the closed open is gone, refused as bad state.
func TestOpenThenClose(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := n.fs.Root
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	r, err := n.Open(ctx, c, byName(root, "f", vfs.OpenOrCreate, entity.ReadData|entity.WriteData))
	if err != nil || !r.Created || r.Dir == nil || r.Ref.Kind != openstate.RefOpened || r.File.ID.IsZero() {
		t.Fatalf("an open creating f: %+v, %v", r, err)
	}
	if _, err := n.Open(ctx, c, byName(root, "f", vfs.CreateNew, entity.ReadData)); !errors.Is(err, vfs.ErrExist) {
		t.Errorf("a create-new open of f: %v, want ErrExist", err)
	}
	if _, err := n.Open(ctx, c, byName(root, "g", vfs.OpenExisting, entity.ReadData)); !errors.Is(err, vfs.ErrNotFound) {
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
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	r, err := n.Open(ctx, c, byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData))
	n.must(t, err)
	n.stored(t, r.File.ID)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	if !n.held(r.File.ID) {
		t.Fatal("an unlinked file's content is released while an open holds it")
	}
	if _, err := n.Close(ctx, c, r.Ref, vfs.Seqs{}); err != nil {
		t.Fatal(err)
	}
	if n.held(r.File.ID) {
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
	alice := s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	c := s.nfs41(t, "host", 1000, 1)
	h, file := f.fs.Create(t, f.fs.Admin, f.fs.Root, "f", kvstore.Regular(0o666))
	_, err := f.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData})
	f.must(t, err)
	f.stored(t, file.ID)
	if _, err := f.Unlink(ctx, proven(f.fs.Admin.Principal, inLab), vfs.Target{H: f.fs.Root}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	lh, locked := f.fs.Create(t, f.fs.Admin, f.fs.Root, "l", kvstore.Regular(0o666))
	o, err := f.Open(ctx, c, vfs.OpenRequest{H: lh, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData})
	f.must(t, err)
	span := []openstate.LockRange{{ByteRange: openstate.ByteRange{Offset: 0, Length: 10}, Exclusive: true}}
	holder := openstate.Caller{Client: c.Client, Principal: alice.Principal, Protocol: entity.ProtocolNFSv4}
	_, err = f.tab.Locks().Lock(ctx, holder, keyOf(locked), openstate.LockReq{Owner: openstate.LockOwner{Client: c.Client, Name: []byte("lo")}, Open: o.Ref, Ranges: span})
	f.must(t, err)

	g := f.restart(t)
	g.must(t, g.Start(t.Context()))
	other := (&shares{fixture: g, cp: s.cp, share: s.share}).nfs41(t, "other", 1000, 2)
	who := openstate.Caller{Client: other.Client, Principal: alice.Principal, Protocol: entity.ProtocolNFSv4}
	conflicting := openstate.LockReq{Owner: openstate.LockOwner{Client: other.Client, Name: []byte("lo")}, Open: openstate.OpenRef{Kind: openstate.RefAnonymous}, Ranges: span}
	if _, err := g.tab.Locks().Lock(ctx, who, keyOf(locked), conflicting); !errors.Is(err, openstate.ErrGrace) {
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
	who.Client, conflicting.Owner.Client = late.Client, late.Client
	if _, err := g.tab.Locks().Lock(ctx, who, keyOf(locked), conflicting); err != nil {
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
	id, err := n.Connect(ctx, asAlice, smb("alice-pc", 1), alicePC)
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
	_, err = files.SetAttrs(ctx, n.fs.Admin, d, entity.Attrs{}.WithOwner(bob.Principal))
	n.must(t, err)
	r, err = n.Open(ctx, asAlice, maximal)
	if err != nil || r.MaximalAccess&entity.Delete != 0 {
		t.Errorf("a maximal open once the directory is bob's: %v, %v; want no delete", r.MaximalAccess, err)
	}
	doc := maximal
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
	req := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData)
	req.DeleteOnClose = true
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

// A backup-intent open with delete on close of a file whose DACL excludes
// everyone, by a share admin, and an EA set through it, succeed; once the
// admin grant is gone, so the DACL excludes its caller, its delete still runs
// at the close, under the privilege the open was granted. With the directory
// made immutable before the close, or with no backup intent asked, the
// deferred unlink is refused, counted, and the file keeps its name.
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
		req := byName(dir, "f", vfs.OpenExisting, entity.ReadData|entity.WriteNamedAttrs)
		req.DeleteOnClose, req.Backup = true, tc.backup
		r, err := n.Open(ctx, c, req)
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
		req := byName(dir.Handle, "a", vfs.OpenExisting, entity.ReadData)
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
	g.reg.register(pc.Client, "", entity.SessionRef{}, alice.Principal, cb)
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
	g.reg.register(pc.Client, "", entity.SessionRef{}, alice.Principal, &vfstest.RecordingCallbacks{})
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
// under way does, while hold is set.
type delaying struct {
	openstate.Checks
	hold atomic.Bool
}

func (d *delaying) CheckChange(ctx context.Context, who openstate.Caller, f openstate.FileKey, via *openstate.OpenRef, k openstate.ChangeMask) error {
	if k&openstate.ChangeRemove != 0 && d.hold.Load() {
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
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData)
	doc.DeleteOnClose = true
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
	doc := byName(n.fs.Root, "f", vfs.OpenOrCreate, entity.ReadData)
	doc.DeleteOnClose = true
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
	c := n.smbClient(t, "pc", n.fs.Admin.Principal)
	if _, err := n.Open(ctx, c, vfs.OpenRequest{H: h, Disposition: vfs.Overwrite, Access: entity.ReadData}); err == nil {
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
	for _, step := range []string{"List", "Removing", "RaiseIncarnations", "ServeShard", "DropShare"} {
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
			case "RaiseIncarnations":
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
	if _, err := n.Open(ctx, pc, byName(bRoot, "disk", vfs.OpenExisting, entity.ReadData)); err == nil {
		t.Error("an open in the share whose serve fails was granted")
	}
	rec.set(nil, nil)
	if _, err := n.Open(ctx, pc, byName(bRoot, "disk", vfs.OpenExisting, entity.ReadData)); err != nil {
		t.Errorf("an open in the share once its serve succeeds: %v", err)
	}
}
