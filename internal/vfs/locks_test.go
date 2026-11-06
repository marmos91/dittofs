package vfs

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// span is one range of n bytes from off, exclusive or shared.
func span(off, n uint64, exclusive bool) []vfs.LockRange {
	return []vfs.LockRange{{ByteRange: vfs.ByteRange{Offset: off, Length: n}, Exclusive: exclusive}}
}

// connected connects client info as c, with a recording path, and answers
// the call from it and the path.
func (s *shares) connected(t *testing.T, c vfs.Call, info vfs.ClientInfo) (vfs.Call, *vfstest.RecordingCallbacks) {
	t.Helper()
	cb := &vfstest.RecordingCallbacks{}
	id, _, err := s.Connect(t.Context(), c, info, cb)
	s.must(t, err)
	c.Protocol, c.Client = info.Protocol, id
	return c, cb
}

// waitOvers is each LockWaitOver cb saw: its waiter's owner name and
// whether it was granted.
func waitOvers(cb *vfstest.RecordingCallbacks) map[string]bool {
	got := map[string]bool{}
	for _, c := range cb.Calls() {
		if c.Method == "LockWaitOver" {
			got[string(c.Args[0].(vfs.LockWaiter).Owner.Name)] = c.Args[1].(bool)
		}
	}
	return got
}

// V23: an NLM and an SMB lock that wait are answered ErrBlocked, hold no
// goroutine while they wait, and are granted through LockWaitOver once the
// range frees; an NFSv4 one that polls is ErrLocked, and granted at its
// next poll after the unlock; a list of ranges one of which conflicts
// leaves none held.
func TestABlockedLockHoldsNoWorker(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	owner := func(c vfs.Call, name string, stack bool) vfs.LockOwner {
		return vfs.LockOwner{Client: c.Client, Name: []byte(name), Stack: stack}
	}

	holder, _ := s.connected(t, signedIn(s.alice.Principal, 1), smb("holder-pc", 1))
	held := s.opener(t, holder, "f", entity.ReadData|entity.WriteData)
	holds := owner(holder, "holder", true)
	if _, err := s.Lock(ctx, holder, held, vfs.LockReq{Owner: holds, Ranges: span(0, 10, true)}); err != nil {
		t.Fatal(err)
	}
	smbWaiter, smbCB := s.connected(t, signedIn(s.bob.Principal, 2), smb("waiter-pc", 1))
	smbRef := s.opener(t, smbWaiter, "f", entity.ReadData|entity.WriteData)
	nlmWaiter, nlmCB := s.connected(t, proven(s.bob.Principal, inLab), nlmHost("build01", 1))
	ro := s.file(t, "ro", s.alice.Principal, 0o644)
	if _, err := s.Lock(ctx, nlmWaiter, vfs.Anonymous(ro), vfs.LockReq{Owner: owner(nlmWaiter, "ro", false), Ranges: span(0, 1, true)}); err != nil {
		t.Errorf("bob's exclusive NLM lock of a file he may only read: %v", err)
	}
	if _, err := s.Lock(ctx, nlmWaiter, vfs.Anonymous(ro), vfs.LockReq{Owner: owner(nlmWaiter, "ro", false), Ranges: span(0, 1, false)}); err != nil {
		t.Errorf("bob's shared NLM lock of a file he may read: %v", err)
	}
	idle := filesystemGoroutines()
	if _, err := s.Lock(ctx, smbWaiter, smbRef, vfs.LockReq{Owner: owner(smbWaiter, "smb", true), Ranges: span(0, 5, true), Wait: openstate.WaitCallback}); !errors.Is(err, vfs.ErrBlocked) {
		t.Fatalf("a waiting SMB lock: %v, want ErrBlocked", err)
	}
	if _, err := s.Lock(ctx, nlmWaiter, vfs.Anonymous(h), vfs.LockReq{Owner: owner(nlmWaiter, "nlm", false), Ranges: span(5, 5, true), Wait: openstate.WaitCallback}); !errors.Is(err, vfs.ErrBlocked) {
		t.Fatalf("a waiting NLM lock: %v, want ErrBlocked", err)
	}
	if n := filesystemGoroutines(); n != idle {
		t.Errorf("two waiting locks hold %d goroutines, want none", n-idle)
	}
	if len(smbCB.Calls())+len(nlmCB.Calls()) != 0 {
		t.Fatal("a waiter was answered before the range freed")
	}
	if _, err := s.Unlock(ctx, holder, held, holds, []vfs.ByteRange{{Offset: 0, Length: 10}}, vfs.Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	calls(t, smbCB, 1)
	calls(t, nlmCB, 1)
	if got := waitOvers(smbCB); !got["smb"] {
		t.Errorf("the SMB waiter saw %v, want its grant", smbCB.Calls())
	}
	if got := waitOvers(nlmCB); !got["nlm"] {
		t.Errorf("the NLM waiter saw %v, want its grant", nlmCB.Calls())
	}
	tester := owner(holder, "tester", true)
	if l, err := s.TestLock(ctx, holder, held, tester, vfs.ByteRange{Offset: 6, Length: 1}, false); err != nil || l == nil || string(l.Owner.Name) != "nlm" {
		t.Errorf("a test inside the NLM grant: %+v, %v; want the NLM owner holding it", l, err)
	}

	s.user(t, "carol", "1000", s.share, entity.AccessReadWrite)
	nfs := s.nfs41(t, "poller", 1000, 7)
	nfsRef, err := s.Open(ctx, nfs, byName(s.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.WriteData))
	s.must(t, err)
	polls := owner(nfs, "nfs", false)
	poll := vfs.LockReq{Owner: polls, Ranges: span(20, 1, true), Wait: openstate.WaitPoll}
	if _, err := s.Lock(ctx, holder, held, vfs.LockReq{Owner: holds, Ranges: span(20, 1, true)}); err != nil {
		t.Fatal(err)
	}
	var locked *vfs.LockedError
	if _, err := s.Lock(ctx, nfs, nfsRef.Ref, poll); !errors.Is(err, vfs.ErrLocked) || !errors.As(err, &locked) || string(locked.Holder.Owner.Name) != "holder" {
		t.Fatalf("a polling NFSv4 lock: %v, want ErrLocked naming its holder", err)
	}
	if _, err := s.Unlock(ctx, holder, held, holds, []vfs.ByteRange{{Offset: 20, Length: 1}}, vfs.Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, nfs, nfsRef.Ref, poll); err != nil {
		t.Errorf("the NFSv4 lock's next poll after the unlock: %v", err)
	}

	list := vfs.LockReq{Owner: owner(smbWaiter, "list", true), Ranges: append(append(span(30, 1, true), span(31, 1, true)...), span(20, 1, true)...)}
	if _, err := s.Lock(ctx, smbWaiter, smbRef, list); !errors.Is(err, vfs.ErrLocked) {
		t.Fatalf("a list whose third range conflicts: %v, want ErrLocked", err)
	}
	if l, err := s.TestLock(ctx, holder, held, tester, vfs.ByteRange{Offset: 30, Length: 2}, true); err != nil || l != nil {
		t.Errorf("after the refused list, its first ranges are held by %+v, %v; want none", l, err)
	}
}

// V8: a lock or unlock through another client's open, and an Unwatch of
// another client's watch, are NotYours, though the other client is signed
// in as the same user; and the watch keeps reporting to its own.
func TestAnotherClientsLockAndWatchAreNotYours(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.file(t, "f", s.alice.Principal, 0o666)
	mine, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	theirs, _ := s.connected(t, signedIn(s.alice.Principal, 2), smb("build01", 1))
	ref := s.opener(t, mine, "f", entity.ReadData|entity.WriteData)
	o := vfs.LockOwner{Client: theirs.Client, Name: []byte("x"), Stack: true}
	if _, err := s.Lock(ctx, theirs, ref, vfs.LockReq{Owner: o, Ranges: span(0, 1, true)}); !notYours(err) {
		t.Errorf("a lock through another client's open: %v, want NotYours", err)
	}
	if _, err := s.Unlock(ctx, theirs, ref, o, []vfs.ByteRange{{Length: 1}}, vfs.Seqs{}, nil); !notYours(err) {
		t.Errorf("an unlock through another client's open: %v, want NotYours", err)
	}
	w, err := s.Watch(ctx, mine, vfs.WatchSpec{Dir: s.fs.Root, Filter: openstate.ChangeAddEntry})
	s.must(t, err)
	if err := s.Unwatch(ctx, theirs, w); !notYours(err) {
		t.Errorf("another client's Unwatch: %v, want NotYours", err)
	}
	if _, err := s.Create(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root, []byte("g"), vfs.CreateArgs{Kind: vfs.CreateRegular}); err != nil {
		t.Fatal(err)
	}
	if got := notes(t, s.shares, cb, 1); !reflect.DeepEqual(got, []string{"g"}) {
		t.Errorf("the watch after another client's Unwatch heard %v, want g", got)
	}
}

// V24: I/O under a write delegation is served to the client holding it,
// authorised for the caller's own principal, and to no other client or call
// with no session.
func TestDelegatedIOIsTheHoldersOwn(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	alice := s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	s.user(t, "bob", "1001", s.share, entity.AccessReadWrite)
	s.user(t, "carol", "1002", s.share, entity.AccessReadWrite)
	h, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o666))
	holder := s.nfs41(t, "host", 1002, 1)
	r, err := s.Open(ctx, holder, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData | entity.WriteData, Want: openstate.GrantRead | openstate.GrantWrite})
	s.must(t, err)
	if r.Grant == nil || r.Grant.Kind&openstate.GrantWrite == 0 {
		t.Fatalf("the open was offered %+v, want a write delegation", r.Grant)
	}
	files := s.fs.Store.Files()
	_, err = files.SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithOwner(alice.Principal))
	s.must(t, err)
	_, err = files.SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithMode(0o644))
	s.must(t, err)
	ref := vfs.Delegated(r.Grant.ID, h)
	as := func(uid uint32) vfs.Call {
		c := claimed(uid, inLab)
		c.Protocol, c.Client, c.Session = holder.Protocol, holder.Client, holder.Session
		return c
	}
	if _, err := s.Write(ctx, as(1000), ref, 0, []byte("x"), vfs.Unstable); err != nil {
		t.Errorf("the holder writing as alice, who may write: %v", err)
	}
	if _, err := s.Write(ctx, as(1001), ref, 0, []byte("x"), vfs.Unstable); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("the holder writing as bob, who may not: %v, want ErrAccess", err)
	}
	other := s.nfs41(t, "other", 1000, 2)
	if _, err := s.Write(ctx, other, ref, 0, []byte("x"), vfs.Unstable); !notYours(err) {
		t.Errorf("another client presenting the delegation: %v, want NotYours", err)
	}
	noSession := as(1000)
	noSession.Session = vfs.SessionRef{}
	if _, err := s.Write(ctx, noSession, ref, 0, []byte("x"), vfs.Unstable); !notYours(err) {
		t.Errorf("the delegation presented on no session: %v, want NotYours", err)
	}
}

// pass advances the clock by d a sweep at a time, renewing keep's clients,
// and lets the table's timer run each sweep.
func (s *shares) pass(t *testing.T, d time.Duration, keep ...vfs.Call) {
	t.Helper()
	for ; d > 0; d -= 10 * time.Second {
		s.clock.Advance(min(d, 10*time.Second))
		for _, c := range keep {
			_, err := s.Renew(t.Context(), c, c.Client)
			s.must(t, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// V6: a write conflicting with a delegation is ErrDelay at once, with no
// worker waiting on the recall; the recall missing its deadline revokes the
// delegation, which the holder is told of by name and its renewal reports,
// a write under it is Revoked, not NotYours, and the client's next open is
// offered no grant while the revoked one is unfreed.
func TestAMissedRecallRevokesTheGrant(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	s.user(t, "alice", "1000", s.share, entity.AccessReadWrite)
	h, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o666))
	s.fs.Create(t, s.fs.Admin, s.fs.Root, "g", kvstore.Regular(0o666))
	holder, cb := s.connected(t, claimed(1000, inLab), vfs.ClientInfo{Protocol: entity.ProtocolNFSv4, Name: []byte("host"), Boot: [8]byte{1}, Recovery: entity.RecoverStatus})
	holder.Session = vfs.SessionRef{ID: 1}
	r, err := s.Open(ctx, holder, vfs.OpenRequest{H: h, Disposition: vfs.OpenExisting, Access: entity.ReadData, Want: openstate.GrantRead})
	s.must(t, err)
	if r.Grant == nil {
		t.Fatal("the open was offered no delegation")
	}
	writer := claimed(1000, inLab)
	start := time.Now()
	var delay *vfs.Delay
	if _, err := s.Write(ctx, writer, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable); !errors.As(err, &delay) || delay.Cause != vfs.DelayRecall {
		t.Fatalf("a write under another client's delegation: %v, want ErrDelay(DelayRecall)", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the conflicting write answered after %v, waiting on the recall", d)
	}
	s.pass(t, 100*time.Second, holder)
	revoked := false
	for range 100 {
		for _, c := range cb.Calls() {
			if c.Method == "Revoked" {
				rv := c.Args[0].(vfs.Revocation)
				revoked = revoked || rv.Reason == openstate.RevokedGrant && rv.Grant == r.Grant.ID
			}
		}
		if revoked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !revoked {
		t.Errorf("the holder saw %v, want RevokedGrant naming the delegation", cb.Calls())
	}
	if st, err := s.Renew(ctx, holder, holder.Client); err != nil || !st.RecallableRevoked {
		t.Errorf("the holder's renewal: %+v, %v; want RecallableRevoked", st, err)
	}
	if _, err := s.Write(ctx, holder, vfs.Delegated(r.Grant.ID, h), 0, []byte("x"), vfs.Unstable); !stateKind(err, vfs.StateRevoked) {
		t.Errorf("a write under the revoked delegation: %v, want StateError Revoked", err)
	}
	next := byName(s.fs.Root, "g", vfs.OpenExisting, entity.ReadData)
	next.Want = openstate.GrantRead
	again, err := s.Open(ctx, holder, next)
	if err != nil || again.Grant != nil {
		t.Errorf("the holder's next open asking a delegation: %+v, %v; want it offered none", again.Grant, err)
	}
}

// V34: a lease break pending when Drain begins is neither forced nor
// revoked by it: the lease keeps its kind and its holder hears nothing more.
func TestDrainLeavesAPendingBreak(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	pc, cb := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	req := byName(s.fs.Root, "f", vfs.OpenExisting, entity.ReadData)
	req.Lease, req.Want = vfs.LeaseKey{1}, openstate.GrantRead|openstate.GrantWrite|openstate.GrantHandle
	r, err := s.Open(ctx, pc, req)
	if err != nil || r.Grant == nil {
		t.Fatalf("the leased open: %+v, %v", r.Grant, err)
	}
	if _, err := s.Write(ctx, proven(s.bob.Principal, inLab), vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable); !errors.Is(err, vfs.ErrDelay) {
		t.Fatalf("a write against the lease: %v, want ErrDelay", err)
	}
	to := calls(t, cb, 1)[0].Args[3].(vfs.GrantKind)
	s.must(t, s.Drain(ctx))
	time.Sleep(50 * time.Millisecond)
	if got := cb.Calls(); len(got) != 1 {
		t.Errorf("the holder saw %v after Drain, want the break alone", got)
	}
	opens, err := s.grants.Openers(ctx, r.Grant.ID)
	if err != nil || len(opens) != 1 {
		t.Errorf("the lease after Drain covers %v, %v; want its open", opens, err)
	}
	who := openstate.Caller{Client: pc.Client, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	if err := s.tab.Grants().AckRecall(ctx, who, r.Grant.ID, to); err != nil {
		t.Errorf("the break's answer after Drain: %v, want it still in flight", err)
	}
}

// V12: lock, unlock and test-lock of a file, and a watch of the share's
// root, meet admission whatever path their handle came by: a quiesced
// share is ErrDelay, a flavour it refuses ErrWrongSecurity, a client
// outside its rules ErrAccess, except for the unlock, which only releases
// state; a lock of a directory is ErrIsDir; and a user disabled after it was
// admitted is refused its next lock.
func TestLockingIsAdmittedOnEveryEntryPath(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	bob := s.user(t, "bob", "1000", s.share, entity.AccessReadWrite)
	s.must(t, s.cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab", Members: []string{"10.0.0.0/24"}}))
	host := s.nfs41(t, "host", 1000, 1)
	s.fs.Create(t, s.fs.Admin, s.fs.Root, "f", kvstore.Regular(0o644))
	at := func(addr bool) vfs.Call {
		c := host
		if addr {
			c.Identity.Client = outside
		}
		return c
	}
	paths := map[string]func(c vfs.Call) (vfs.Handle, error){
		"held handle": func(vfs.Call) (vfs.Handle, error) { return s.fs.Root, nil },
		"Root by path": func(c vfs.Call) (vfs.Handle, error) {
			h, _, err := s.Root(ctx, c, vfs.ShareRef{Path: "/data"})
			return h, err
		},
		"Root by name": func(c vfs.Call) (vfs.Handle, error) {
			h, _, err := s.Root(ctx, c, vfs.ShareRef{Name: "data"})
			return h, err
		},
		"walk": func(c vfs.Call) (vfs.Handle, error) {
			root, _, err := s.Root(ctx, c, vfs.ShareRef{})
			if err != nil {
				return nil, err
			}
			h, _, err := s.Lookup(ctx, c, root, []byte("data"))
			return h, err
		},
	}
	o := vfs.LockOwner{Client: host.Client, Name: []byte("lo")}
	r := vfs.ByteRange{Length: 1}
	ops := map[string]func(c vfs.Call, h vfs.Handle) error{
		"lock": func(c vfs.Call, h vfs.Handle) error {
			_, err := s.Lock(ctx, c, vfs.Anonymous(h), vfs.LockReq{Owner: o, Ranges: span(0, 1, false)})
			return err
		},
		"unlock": func(c vfs.Call, h vfs.Handle) error {
			_, err := s.Unlock(ctx, c, vfs.Anonymous(h), o, []vfs.ByteRange{r}, vfs.Seqs{}, nil)
			return err
		},
		"test-lock": func(c vfs.Call, h vfs.Handle) error {
			_, err := s.TestLock(ctx, c, vfs.Anonymous(h), o, r, false)
			return err
		},
		"watch": func(c vfs.Call, h vfs.Handle) error {
			_, err := s.Watch(ctx, c, vfs.WatchSpec{Dir: h, Filter: openstate.ChangeAddEntry})
			return err
		},
	}
	for _, c := range []struct {
		name    string
		policy  entity.ExportPolicy
		state   entity.ShareState
		outside bool
		want    error
		unlock  error
	}{
		{"admitted", entity.ExportPolicy{}, entity.ShareEnabled, false, nil, nil},
		{"quiesced", entity.ExportPolicy{}, entity.ShareQuiesced, false, vfs.ErrDelay, vfs.ErrDelay},
		{"proof", entity.ExportPolicy{MinProof: entity.ProofSecret}, entity.ShareEnabled, false, vfs.ErrWrongSecurity, vfs.ErrWrongSecurity},
		{"client rules", entity.ExportPolicy{Clients: []entity.ClientRule{{Match: entity.ClientMatch{Netgroup: "lab"}, Access: entity.AccessReadWrite}}},
			entity.ShareEnabled, true, vfs.ErrAccess, nil},
	} {
		// Each path's handles, the root's and its file's, are taken while
		// the share admits them, as a client holds one before the share
		// changes.
		handles := map[string][2]vfs.Handle{}
		for name, path := range paths {
			h, err := path(host)
			s.must(t, err)
			f, _, err := s.Lookup(ctx, host, h, []byte("f"))
			s.must(t, err)
			handles[name] = [2]vfs.Handle{h, f}
		}
		c.policy.Share = s.share
		s.must(t, s.cp.PutExportPolicy(ctx, c.policy))
		s.must(t, s.cp.SetShareState(ctx, s.share, entity.ShareEnabled, c.state))
		for name, hs := range handles {
			for op, call := range ops {
				h, want := hs[1], c.want
				switch op {
				case "watch":
					h = hs[0]
				case "unlock":
					want = c.unlock
				}
				if err := call(at(c.outside), h); want == nil && err != nil || want != nil && !errors.Is(err, want) {
					t.Errorf("%s, %s by %s: %v, want %v", c.name, op, name, err, want)
				}
			}
		}
		s.must(t, s.cp.SetShareState(ctx, s.share, c.state, entity.ShareEnabled))
		s.must(t, s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share}))
	}

	for _, op := range []string{"lock", "test-lock"} {
		if err := ops[op](host, s.fs.Root); !errors.Is(err, vfs.ErrIsDir) {
			t.Errorf("a %s of the share's root: %v, want ErrIsDir", op, err)
		}
	}

	bob.Disabled = true
	s.must(t, s.cp.PutUser(ctx, bob))
	f, _, err := s.Lookup(ctx, proven(s.fs.Admin.Principal, inLab), s.fs.Root, []byte("f"))
	s.must(t, err)
	if err := ops["lock"](host, f); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a lock by a user disabled since its last call: %v, want ErrAccess", err)
	}
}

// An NLM host whose user's share grant is removed still gives up its locks:
// an unlock and a cancel of its waiter only release state (RFC 19 §3.1), so
// another client then locks the range, and the cancelled waiter is never
// granted.
func TestARevokedCallerStillReleasesItsLocks(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	host, hostCB := s.connected(t, proven(s.bob.Principal, inLab), nlmHost("build01", 1))
	other, _ := s.connected(t, proven(s.alice.Principal, inLab), nlmHost("build02", 1))
	nlm := func(c vfs.Call, name string) vfs.LockOwner {
		return vfs.LockOwner{Client: c.Client, Name: []byte(name)}
	}
	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(host, "held"), Ranges: span(0, 10, true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, other, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(other, "o"), Ranges: span(20, 10, true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(host, "waits"), Ranges: span(20, 10, true), Wait: openstate.WaitCallback}); !errors.Is(err, vfs.ErrBlocked) {
		t.Fatalf("the host's waiting lock: %v, want ErrBlocked", err)
	}
	s.must(t, s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: s.bob.Principal, Access: entity.AccessNone}))

	if granted, err := s.CancelLock(ctx, host, vfs.Anonymous(h), nlm(host, "waits"), vfs.ByteRange{Offset: 20, Length: 10}); err != nil || granted {
		t.Errorf("the revoked host's cancel: %v, %v; want cancelled", granted, err)
	}
	if _, err := s.Unlock(ctx, other, vfs.Anonymous(h), nlm(other, "o"), []vfs.ByteRange{{Offset: 20, Length: 10}}, vfs.Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unlock(ctx, host, vfs.Anonymous(h), nlm(host, "held"), []vfs.ByteRange{{Offset: 0, Length: 10}}, vfs.Seqs{}, nil); err != nil {
		t.Errorf("the revoked host's unlock: %v", err)
	}
	third, _ := s.connected(t, proven(s.alice.Principal, inLab), nlmHost("build03", 1))
	if _, err := s.Lock(ctx, third, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(third, "t"), Ranges: span(0, 30, true)}); err != nil {
		t.Errorf("another host's lock after the revoked host released: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := waitOvers(hostCB); got["waits"] {
		t.Errorf("the cancelled waiter was granted: %v", hostCB.Calls())
	}
}

// V12: a client the share's rules stop admitting is refused a lock, a
// test-lock and a watch, while its unlock of a lock it holds, its cancel of
// a waiting lock and its close of an open taken before the rules changed all
// succeed, as they only release state; another client then takes the range
// it unlocked.
func TestAReleaseIsNotRefusedByClientRules(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	host, _ := s.connected(t, proven(s.bob.Principal, inLab), nlmHost("build01", 1))
	other, _ := s.connected(t, proven(s.alice.Principal, inLab), nlmHost("build02", 1))
	pc, _ := s.connected(t, signedIn(s.bob.Principal, 1), smb("bob-pc", 1))
	nlm := func(c vfs.Call, name string) vfs.LockOwner {
		return vfs.LockOwner{Client: c.Client, Name: []byte(name)}
	}
	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(host, "held"), Ranges: span(0, 10, true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, other, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(other, "o"), Ranges: span(20, 10, true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(host, "waits"), Ranges: span(20, 10, true), Wait: openstate.WaitCallback}); !errors.Is(err, vfs.ErrBlocked) {
		t.Fatalf("the host's waiting lock: %v, want ErrBlocked", err)
	}
	ref := s.opener(t, pc, "f", entity.ReadData)
	s.must(t, s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Clients: []entity.ClientRule{
		{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("192.168.0.0/16")}, Access: entity.AccessReadWrite},
	}}))

	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(host, "new"), Ranges: span(40, 1, false)}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a lock by a client outside the rules: %v, want ErrAccess", err)
	}
	if _, err := s.TestLock(ctx, host, vfs.Anonymous(h), nlm(host, "new"), vfs.ByteRange{Offset: 40, Length: 1}, false); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a test-lock by a client outside the rules: %v, want ErrAccess", err)
	}
	if _, err := s.Watch(ctx, pc, vfs.WatchSpec{Dir: s.fs.Root, Filter: openstate.ChangeAddEntry}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a watch by a client outside the rules: %v, want ErrAccess", err)
	}
	if granted, err := s.CancelLock(ctx, host, vfs.Anonymous(h), nlm(host, "waits"), vfs.ByteRange{Offset: 20, Length: 10}); err != nil || granted {
		t.Errorf("the cancel of a waiting lock by a client outside the rules: %v, %v; want cancelled", granted, err)
	}
	if _, err := s.Unlock(ctx, host, vfs.Anonymous(h), nlm(host, "held"), []vfs.ByteRange{{Offset: 0, Length: 10}}, vfs.Seqs{}, nil); err != nil {
		t.Errorf("the unlock by a client outside the rules: %v", err)
	}
	if _, err := s.Close(ctx, pc, ref, vfs.Seqs{}); err != nil {
		t.Errorf("the close by a client outside the rules: %v", err)
	}

	s.must(t, s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share}))
	if _, err := s.Lock(ctx, other, vfs.Anonymous(h), vfs.LockReq{Owner: nlm(other, "o"), Ranges: span(0, 10, true)}); err != nil {
		t.Errorf("another host's lock of the range the outside client unlocked: %v", err)
	}
	if _, err := s.Close(ctx, pc, ref, vfs.Seqs{}); !stateKind(err, vfs.StateBadState) {
		t.Errorf("a second close of the closed open: %v, want BadState", err)
	}
}

// A release is exempt from the client rules' refusal only: a netgroup read
// failing with an error that wraps ErrAccess still refuses the unlock, which
// succeeds once the store recovers.
func TestAReleaseFailsClosedOnANetgroupReadError(t *testing.T) {
	db := &failKV{}
	s := startShares(t, newFixtureOver(t, Deps{}, kvstoretest.NewFS(t, func(tb testing.TB) kv.KV { db.KV = memKV(tb); return db })), nil)
	ctx := t.Context()
	a := &attrs{shares: s, bob: s.user(t, "bob", "", s.share, entity.AccessReadWrite)}
	h := a.file(t, "f", a.bob.Principal, 0o666)
	s.must(t, s.cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab", Members: []string{"10.0.0.0/24"}}))
	s.must(t, s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Clients: []entity.ClientRule{
		{Match: entity.ClientMatch{Netgroup: "lab"}, Access: entity.AccessReadWrite},
	}}))
	host, _ := s.connected(t, proven(a.bob.Principal, inLab), nlmHost("build01", 1))
	o := vfs.LockOwner{Client: host.Client, Name: []byte("held")}
	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: o, Ranges: span(0, 10, true)}); err != nil {
		t.Fatal(err)
	}
	db.failing(func(update bool) error {
		if !update && onStack("vfs.netgroups.Members") {
			return fmt.Errorf("netgroup read: %w", vfs.ErrAccess)
		}
		return nil
	})
	if _, err := s.Unlock(ctx, host, vfs.Anonymous(h), o, []vfs.ByteRange{{Offset: 0, Length: 10}}, vfs.Seqs{}, nil); err == nil {
		t.Errorf("an unlock whose netgroup read fails wrapping ErrAccess succeeded, want it refused")
	}
	db.failing(nil)
	if _, err := s.Unlock(ctx, host, vfs.Anonymous(h), o, []vfs.ByteRange{{Offset: 0, Length: 10}}, vfs.Seqs{}, nil); err != nil {
		t.Errorf("the unlock once the store recovered: %v", err)
	}
}

// Ownership under no open is judged on the squashed principal: root,
// squashed to nobody on a root-squash share, is refused an NLM lock of a
// root-owned file nobody cannot read.
func TestASquashedOwnerIsDecidedAsReadData(t *testing.T) {
	s := newShares(t, nil, nil)
	ctx := t.Context()
	root := s.user(t, "root", "0", s.share, entity.AccessReadWrite)
	nobody := s.user(t, "nobody", "", s.share, entity.AccessReadWrite)
	a := &attrs{shares: s}
	h := a.file(t, "f", root.Principal, 0o600)
	s.must(t, s.cp.PutExportPolicy(ctx, entity.ExportPolicy{Share: s.share, Squash: entity.SquashRoot, Anonymous: nobody.Principal}))
	host, _ := s.connected(t, claimed(0, inLab), nlmHost("root-host", 1))
	o := vfs.LockOwner{Client: host.Client, Name: []byte("pid 1")}
	if _, err := s.Lock(ctx, host, vfs.Anonymous(h), vfs.LockReq{Owner: o, Ranges: span(0, 1, true)}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("squashed root's NLM lock of its own 0600 file: %v, want ErrAccess", err)
	}
}

// RFC 19 §3.8: a lock under no open needs read data or ownership of the
// file, an exclusive lock no more than a shared one, and a test is decided
// as a shared lock; ownership admits only within the owner's share grant.
func TestALockUnderNoOpenIsDecidedAsReadOrOwnership(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	private := s.file(t, "private", s.alice.Principal, 0o600)
	writeOnly := s.file(t, "write-only", s.alice.Principal, 0o200)
	bob, _ := s.connected(t, proven(s.bob.Principal, inLab), nlmHost("bob-host", 1))
	alice, _ := s.connected(t, proven(s.alice.Principal, inLab), nlmHost("alice-host", 1))
	o := func(c vfs.Call) vfs.LockOwner { return vfs.LockOwner{Client: c.Client, Name: []byte("pid 1")} }

	for _, exclusive := range []bool{false, true} {
		if _, err := s.Lock(ctx, bob, vfs.Anonymous(private), vfs.LockReq{Owner: o(bob), Ranges: span(0, 1, exclusive)}); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("bob's NLM lock, exclusive %v, of alice's 0600 file: %v, want ErrAccess", exclusive, err)
		}
	}
	if _, err := s.TestLock(ctx, bob, vfs.Anonymous(private), o(bob), vfs.ByteRange{Length: 1}, true); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's NLM test of alice's 0600 file: %v, want ErrAccess", err)
	}
	for _, exclusive := range []bool{true, false} {
		if _, err := s.Lock(ctx, alice, vfs.Anonymous(writeOnly), vfs.LockReq{Owner: o(alice), Ranges: span(0, 1, exclusive)}); err != nil {
			t.Errorf("alice's NLM lock, exclusive %v, of her own 0200 file: %v", exclusive, err)
		}
	}

	s.must(t, s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: s.alice.Principal, Access: entity.AccessNone}))
	if _, err := s.Lock(ctx, alice, vfs.Anonymous(writeOnly), vfs.LockReq{Owner: o(alice), Ranges: span(10, 1, true)}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("the owner's NLM lock with no share grant: %v, want ErrAccess", err)
	}
}

// An unlock resent under one lock sequence after a lost reply is answered
// as the first was, not refused for the range it already released; and a
// test under no open is decided as the caller's read of the file.
func TestAnUnlockReplayAndANoOpenTestPassThrough(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.file(t, "f", s.alice.Principal, 0o666)
	pc, _ := s.connected(t, signedIn(s.alice.Principal, 1), smb("alice-pc", 1))
	ref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	o := vfs.LockOwner{Client: pc.Client, Name: []byte("o"), Stack: true}
	if _, err := s.Lock(ctx, pc, ref, vfs.LockReq{Owner: o, Ranges: span(0, 10, true)}); err != nil {
		t.Fatal(err)
	}
	seq := &vfs.LockSequence{Index: 1, Seq: 1}
	for i := range 2 {
		if _, err := s.Unlock(ctx, pc, ref, o, []vfs.ByteRange{{Length: 10}}, vfs.Seqs{}, seq); err != nil {
			t.Errorf("unlock %d under one lock sequence: %v", i, err)
		}
	}

	shut := s.file(t, "shut", s.alice.Principal, 0o600)
	host, _ := s.connected(t, proven(s.bob.Principal, inLab), nlmHost("build01", 1))
	if _, err := s.TestLock(ctx, host, vfs.Anonymous(shut), vfs.LockOwner{Client: host.Client, Name: []byte("t")}, vfs.ByteRange{Length: 1}, false); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's NLM test of alice's 0600 file: %v, want ErrAccess", err)
	}
}
