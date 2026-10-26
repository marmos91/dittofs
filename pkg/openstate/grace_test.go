package openstate

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// nfsAs is an NFSv4 client that recovers as k says: status for NFSv4.1,
// refusal for NFSv4.0.
func nfsAs(name string, k entity.RecoveryKind) Client {
	c := nfs(name, 1, entity.NewUser())
	c.Recovery = k
	return c
}

// restart stops f's table and starts another over its store, every test
// shard begun under a new grace instance.
func (f *fixture) restart(t *testing.T) *fixture {
	t.Helper()
	f.Stop()
	return reopen(t, f.fs, f.clock)
}

// renew renews each client, as each one's first request after a restart
// does, failing the test on a refusal.
func (f *fixture) renew(t *testing.T, cs ...Caller) {
	t.Helper()
	for _, c := range cs {
		if _, err := f.Renew(ctx, c.Client); err != nil {
			t.Fatalf("Renew %d: %v", c.Client, err)
		}
	}
}

func in(s ShardID) FileKey { return FileKey{Shard: s, ID: entity.NewFileID()} }

// After a restart only a reclaim is granted, by a client the shard's records
// name, and none that conflicts with one reclaimed before it; every other
// request for state is ErrGrace, a file nobody held included, until grace
// ends on its own a lease period after it began.
func TestGraceAdmitsOnlyReclaims(t *testing.T) {
	f := newFixture(t)
	c, d := f.caller(t, nfsAs("c", entity.RecoverStatus)), f.caller(t, nfsAs("d", entity.RecoverStatus))
	file, idle := in(5), in(5)
	oc := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo")})
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(oc), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, d, idle, OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, c, d)
	for what, err := range map[string]error{
		"a conflicting lock":           errOf(g.lock(d, file, LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}})),
		"a lock of a file nobody held": errOf(g.lock(d, in(5), LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}})),
		"an open":                      func() error { _, err := g.open(d, file, OpenRequest{Access: rd}); return err }(),
	} {
		if !errors.Is(err, ErrGrace) {
			t.Errorf("%s in grace: %v, want ErrGrace", what, err)
		}
	}

	res, err := g.Opens().Open(ctx, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true, Want: rw})
	if err != nil {
		t.Fatal(err)
	}
	if res.Grant != nil {
		t.Errorf("a reclaim in grace was offered a grant: %+v", *res.Grant)
	}
	again := res.Open
	g.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(again), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if _, err := g.lock(d, file, LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(5, 1)}, Wait: WaitCallback, Reclaim: true}); !errors.Is(err, ErrReclaimConflict) {
		t.Errorf("a second client's reclaim of the range: %v, want ErrReclaimConflict", err)
	}
	if h := g.holder(t, file, br(0, 10)); h == nil || h.Owner.Client != c.Client {
		t.Errorf("holder after both reclaims: %+v, want c's", h)
	}
	if _, err := g.open(d, file, OpenRequest{Access: rd, Deny: wr, Reclaim: true}); !errors.Is(err, ErrReclaimConflict) {
		t.Errorf("a reclaimed open denying the write c reclaimed: %v, want ErrReclaimConflict", err)
	}
	e := g.caller(t, nfsAs("e", entity.RecoverStatus))
	if _, err := g.open(e, file, OpenRequest{Access: rd, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim by a client the records do not name: %v, want ErrNoReclaim", err)
	}

	g.pass(t, leasePeriod-time.Second, c.Client, d.Client)
	if _, err := g.lock(d, in(5), LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a lock just before grace ends: %v, want ErrGrace", err)
	}
	g.pass(t, time.Second, c.Client, d.Client)
	g.mustLock(t, d, in(5), LockReq{Owner: owner(d, "lo"), Ranges: []LockRange{x(0, 10)}})
}

// In grace a test of a range another client held before the loss is
// ErrGrace, since that lock is not held again until it is reclaimed; after
// grace it is answered as usual.
func TestTestLockInGrace(t *testing.T) {
	f := newFixture(t)
	c, d := f.caller(t, nfsAs("c", entity.RecoverStatus)), f.caller(t, nfsAs("d", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, c, d)
	if h, err := g.Locks().TestLock(ctx, d, file, owner(d, "lo"), br(0, 1), true); !errors.Is(err, ErrGrace) {
		t.Errorf("TestLock in grace of a range c held: %+v, %v; want ErrGrace", h, err)
	}
	g.pass(t, leasePeriod, c.Client, d.Client)
	if h, err := g.Locks().TestLock(ctx, d, file, owner(d, "lo"), br(0, 1), true); err != nil || h != nil {
		t.Errorf("TestLock after grace: %+v, %v; want no conflict", h, err)
	}
}

// In grace, anonymous I/O waits for its end in a shard whose records name a
// client that can hold a deny mode or a mandatory lock, and not in one that
// names only NLM hosts, which hold neither. A shard whose records name only
// SMB clients ends its grace at once: they reconnect no persistent open, and
// reclaim nothing else.
func TestAnonymousIOWaitsForGrace(t *testing.T) {
	f := newFixture(t)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	n, s, h := f.caller(t, nfsAs("n", entity.RecoverStatus)), f.caller(t, sc), f.host(t, "h", 1)
	file, sfile, hfile := in(5), in(7), in(1)
	f.mustOpen(t, n, file, OpenRequest{Access: rd, Deny: wr})
	o := f.mustOpen(t, s, sfile, OpenRequest{Access: rd | wr})
	f.mustLock(t, s, sfile, LockReq{Owner: smbOwner(s, o), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, hfile, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	write := func(file FileKey) error {
		// The reference carries no shard: the file's key names it.
		return g.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 1), true)
	}
	if err := write(file); !errors.Is(err, ErrGrace) {
		t.Errorf("an anonymous write where an NFSv4 client held a deny mode: %v, want ErrGrace", err)
	}
	if err := write(hfile); err != nil {
		t.Errorf("an anonymous write where only an NLM host held a lock: %v", err)
	}
	if err := write(sfile); err != nil {
		t.Errorf("an anonymous write where only an SMB client held a lock: %v", err)
	}
	for _, what := range []ChangeMask{ChangeRemove, ChangeRename} {
		if err := g.Checks().CheckChange(ctx, Caller{}, file, nil, what); !errors.Is(err, ErrGrace) {
			t.Errorf("change %d where an NFSv4 client held a deny mode: %v, want ErrGrace", what, err)
		}
	}
	if err := g.Checks().CheckChange(ctx, Caller{}, file, nil, ChangeAttrs); err != nil {
		t.Errorf("a set-attribute in grace: %v", err)
	}
	if err := g.Checks().CheckChange(ctx, Caller{}, hfile, nil, ChangeRemove); err != nil {
		t.Errorf("a remove where only an NLM host held a lock: %v", err)
	}
	g.clock.advance(leasePeriod)
	if err := write(file); err != nil {
		t.Errorf("an anonymous write after grace: %v", err)
	}
}

// Anonymous I/O waits only while a client that can hold a deny mode is left
// to reclaim: once an NFSv4.1 client has completed, a write passes while
// grace still runs for an NLM host, which holds no deny mode.
func TestAnonymousIOFreedOnceDenyHoldersFinish(t *testing.T) {
	f := newFixture(t)
	c, h, z := f.caller(t, nfsAs("c", entity.RecoverStatus)), f.host(t, "h", 1), f.caller(t, nfsAs("z", entity.RecoverStatus))
	file := in(5)
	f.mustOpen(t, c, file, OpenRequest{Access: rd, Deny: wr})
	f.mustLock(t, h, in(5), LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, c, z)
	write := func() error {
		return g.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 1), true)
	}
	if err := write(); !errors.Is(err, ErrGrace) {
		t.Errorf("an anonymous write before c completes: %v, want ErrGrace", err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, []GraceRef{{5, g.shards[5].instance}}); err != nil {
		t.Fatal(err)
	}
	if err := write(); err != nil {
		t.Errorf("an anonymous write with only the host left to reclaim: %v", err)
	}
	if _, err := g.lock(z, in(5), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a new lock while the host may still reclaim: %v, want ErrGrace", err)
	}
}

// A client that recovers by status is told the grace instances it owes,
// and only those; its reclaim is accepted, and grace waits for it until it
// completes. A completion ends only the instances it names: one begun after
// the client was shown its owed ones stays owed, and keeps its grace.
func TestReclaimCompleteEndsOnlyWhatWasShown(t *testing.T) {
	f := newFixture(t)
	cl := nfsAs("c", entity.RecoverStatus)
	c, other := f.caller(t, cl), f.caller(t, nfsAs("other", entity.RecoverStatus))
	if err := f.Recovery().ReclaimComplete(ctx, c, nil); err != nil { // at mount
		t.Fatal(err)
	}
	a, b := in(5), in(7)
	f.mustOpen(t, c, a, OpenRequest{Access: rd, Owner: owner(c, "oo")})
	held := f.mustOpen(t, c, b, OpenRequest{Access: rd, Owner: owner(c, "oo")})
	if s, _ := f.Renew(ctx, c.Client); s.ReclaimNeeded {
		t.Errorf("before any loss: %+v", s)
	}

	g := f.restart(t)
	g.renew(t, other)
	st, err := g.Renew(ctx, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	shown := st.Reclaim
	want := []GraceRef{{5, g.shards[5].instance}, {7, g.shards[7].instance}}
	slices.SortFunc(shown, func(x, y GraceRef) int { return int(x.Shard) - int(y.Shard) })
	if !st.ReclaimNeeded || !reflect.DeepEqual(shown, want) {
		t.Fatalf("after the restart: %+v, want ReclaimNeeded for %v", st, want)
	}
	g.mustOpen(t, c, a, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true})
	reclaimed := g.mustOpen(t, c, b, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true})
	// None of these ends what c owes: confirming again, another principal's
	// completion, or one naming an instance not yet begun.
	g.connect(t, cl)
	stranger := c
	stranger.Principal = entity.NewUser()
	if err := g.Recovery().ReclaimComplete(ctx, stranger, shown); !errors.Is(err, ErrNotYours) {
		t.Errorf("another principal's completion: %v, want ErrNotYours", err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, []GraceRef{{5, shown[0].Instance + 1}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Renew(ctx, c.Client); len(st.Reclaim) != 2 {
		t.Errorf("owed after completions that end nothing: %+v, want both shards", st)
	}

	// Shard 7 is lost again after c was shown its instance: what it
	// reclaimed there is gone, and its reference stale.
	if err := g.Recovery().ServeShard(ctx, 7, g.shards[7].instance+100); err != nil {
		t.Fatal(err)
	}
	if err := g.Recovery().TestState(ctx, c, []StateRef{reclaimed.Ref, held.Ref}); !errors.Is(err[0], ErrStaleState) || !errors.Is(err[1], ErrStaleState) {
		t.Errorf("references of shard 7 after its second loss: %v, want ErrStaleState", err)
	}
	if _, err := g.lock(other, in(5), LockReq{Owner: owner(other, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("before c completes: %v, want ErrGrace", err)
	}
	if err := g.Recovery().ReclaimComplete(ctx, c, shown); err != nil {
		t.Fatal(err)
	}
	g.mustLock(t, other, in(5), LockReq{Owner: owner(other, "lo"), Ranges: []LockRange{x(0, 1)}})
	if _, err := g.lock(other, in(7), LockReq{Owner: owner(other, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("in the instance c was not shown: %v, want ErrGrace", err)
	}
	// A completion naming the instance c joined under, which it finished,
	// takes nothing back.
	if err := g.Recovery().ReclaimComplete(ctx, c, []GraceRef{{5, f.shards[5].instance}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Renew(ctx, c.Client); !reflect.DeepEqual(st.Reclaim, []GraceRef{{7, g.shards[7].instance}}) {
		t.Errorf("after the completion: %+v, want the later instance of shard 7 alone", st)
	}
}

// Only a client that recovers by status finishes by completing: an NLM
// host's or an NFSv4.0 client's completion ends no grace.
func TestReclaimCompleteOnlyByStatus(t *testing.T) {
	f := newFixture(t)
	h, c40, z := f.host(t, "h", 1), f.caller(t, nfsAs("c40", entity.RecoverRefusal)), f.caller(t, nfsAs("z", entity.RecoverStatus))
	f.mustLock(t, h, in(5), LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, c40, in(7), OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, z)
	for _, c := range []struct {
		who Caller
		s   ShardID
	}{{h, 5}, {c40, 7}} {
		s := c.s
		if err := g.Recovery().ReclaimComplete(ctx, c.who, []GraceRef{{s, g.shards[s].instance}}); err != nil {
			t.Fatal(err)
		}
		if _, err := g.lock(z, in(s), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
			t.Errorf("a new lock in shard %d after its only client completed: %v, want ErrGrace", s, err)
		}
	}
}

// A client that recovers by refusal is refused on every call naming it
// until it connects again under its name and Boot, which keeps its ID; a
// reference it held is stale, and its reclaim is then granted. It never
// counts as finished, so grace runs until it expires.
func TestRefusalClientConnectsAgain(t *testing.T) {
	f := newFixture(t)
	cl := nfsAs("c40", entity.RecoverRefusal)
	c, done := f.caller(t, cl), f.caller(t, nfsAs("done", entity.RecoverStatus))
	c2 := f.caller(t, nfsAs("c40-2", entity.RecoverRefusal))
	file := in(5)
	f.mustLock(t, c2, in(5), LockReq{Owner: owner(c2, "lo"), Ranges: []LockRange{x(0, 1)}})
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo")})
	lk := f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	f.mustOpen(t, done, in(5), OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, done)
	if err := g.Recovery().ReclaimComplete(ctx, done, []GraceRef{{5, g.shards[5].instance}}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the first adopts the client after the restart
		if _, err := g.Renew(ctx, c.Client); !errors.Is(err, ErrStaleClient) {
			t.Errorf("Renew: %v, want ErrStaleClient", err)
		}
	}
	if _, err := g.open(c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a reclaim before connecting again: %v, want ErrStaleClient", err)
	}
	// c2 has sent nothing since the restart.
	if _, err := g.Locks().TestLock(ctx, c2, file, owner(c2, "lo"), br(0, 1), true); !errors.Is(err, ErrStaleClient) {
		t.Errorf("TestLock: %v, want ErrStaleClient", err)
	}
	if err := g.Locks().ReleaseLockOwner(ctx, c2, owner(c2, "lo")); !errors.Is(err, ErrStaleClient) {
		t.Errorf("ReleaseLockOwner: %v, want ErrStaleClient", err)
	}
	if err := g.Recovery().TestState(ctx, c, []StateRef{lk.Ref})[0]; !errors.Is(err, ErrStaleState) {
		t.Errorf("the lock state held before: %v, want ErrStaleState", err)
	}

	if id := g.connect(t, cl); id != c.Client {
		t.Fatalf("connecting again: ID %d, want %d", id, c.Client)
	}
	g.renew(t, c)
	again := g.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true})
	g.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(again), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if _, err := g.lock(done, in(5), LockReq{Owner: owner(done, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("with the refusal client reclaimed: %v, want ErrGrace", err)
	}
	if err := g.Expire(ctx, c.Client); err != nil {
		t.Fatal(err)
	}
	if err := g.Expire(ctx, c2.Client); err != nil {
		t.Fatal(err)
	}
	g.mustLock(t, done, in(5), LockReq{Owner: owner(done, "lo"), Ranges: []LockRange{x(0, 1)}})
}

// A shard where NLM hosts held locks raises the restart-notice number before
// its grace begins and tells each host, with its adapter bytes, as it tells
// an SMB client; the hosts' reclaims are granted, and a host revoked before
// the restart, or whose record is ended, is told nothing and reclaims
// nothing.
func TestShardLossToldToHosts(t *testing.T) {
	f := newFixture(t)
	h1, h2, gone, ended := f.host(t, "h1", 1), f.host(t, "h2", 1), f.host(t, "gone", 1), f.host(t, "ended", 1)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	s := f.caller(t, sc)
	file := in(5)
	for i, h := range []Caller{h1, h2, gone, ended} {
		f.mustLock(t, h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(uint64(i)*10, 10)}})
	}
	f.mustOpen(t, s, file, OpenRequest{Access: rd})
	if _, err := f.AddSession(ctx, h1.Client, entity.NodeID{}, nil, []byte("h1-addr")); err != nil {
		t.Fatal(err)
	}
	if err := f.Revoke(ctx, gone.Client); err != nil {
		t.Fatal(err)
	}
	rec, err := f.fs.Store.Clients().Get(ctx, ended.Client)
	if err != nil {
		t.Fatal(err)
	}
	next := rec
	next.Ended = true
	if _, err := f.fs.Store.Clients().Put(ctx, rec, next); err != nil {
		t.Fatal(err)
	}
	before, err := f.fs.Store.NSMState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	f.Stop()
	g := unstarted(t, f.fs, f.clock)
	if err := g.Start(ctx, g.sink, g.order.order); err != nil {
		t.Fatal(err)
	}
	g.sink.takeRevoked()
	var toldAt []uint32 // the restart-notice number as each client is told
	g.sink.onRevoke = func(Revocation) {
		n, _ := f.fs.Store.NSMState(ctx)
		toldAt = append(toldAt, n)
	}
	if err := g.Recovery().ServeShard(ctx, 5, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	for _, n := range toldAt {
		if n <= before {
			t.Errorf("restart-notice number %d as a client was told, %d before", n, before)
		}
	}
	got := map[entity.ClientID]Revocation{}
	for _, r := range g.sink.takeRevoked() {
		if len(r.Adapter) == 0 {
			r.Adapter = nil
		}
		got[r.Client] = r
	}
	wantTold := map[entity.ClientID]Revocation{
		h1.Client: {Client: h1.Client, Shard: 5, Reason: RevokedShardState, Adapter: []byte("h1-addr")},
		h2.Client: {Client: h2.Client, Shard: 5, Reason: RevokedShardState},
		s.Client:  {Client: s.Client, Shard: 5, Reason: RevokedShardState},
	}
	if !reflect.DeepEqual(got, wantTold) {
		t.Errorf("Revoked %+v, want %+v", got, wantTold)
	}
	raised, _ := f.fs.Store.NSMState(ctx)
	for _, s := range testShards[:2] {
		if err := g.Recovery().ServeShard(ctx, s, instances.Add(1)); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := f.fs.Store.NSMState(ctx); n != raised {
		t.Errorf("restart-notice number %d after shards no host held state in began, %d before", n, raised)
	}
	g.renew(t, h1, h2)
	g.mustLock(t, h1, file, LockReq{Owner: owner(h1, "p"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if _, err := g.lock(h2, file, LockReq{Owner: owner(h2, "p"), Ranges: []LockRange{x(100, 10)}}); !errors.Is(err, ErrGrace) {
		t.Errorf("a host's new lock in grace: %v, want ErrGrace", err)
	}
	again := g.host(t, "gone", 1)
	if _, err := g.lock(again, file, LockReq{Owner: owner(again, "p"), Ranges: []LockRange{x(20, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("the revoked host's reclaim: %v, want ErrNoReclaim", err)
	}
}

// A revoked client's shard list is emptied, so after a restart its reclaim
// is refused rather than taking back the lock another client took since.
func TestRevokedClientReclaimsNothing(t *testing.T) {
	f := newFixture(t)
	x1, y := f.caller(t, nfsAs("x", entity.RecoverStatus)), f.caller(t, nfsAs("y", entity.RecoverStatus))
	file := in(5)
	f.mustLock(t, x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}})
	if err := f.Revoke(ctx, x1.Client); err != nil {
		t.Fatal(err)
	}
	f.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}})

	g := f.restart(t)
	g.renew(t, x1, y)
	if _, err := g.lock(x1, file, LockReq{Owner: owner(x1, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("the revoked client's reclaim: %v, want ErrNoReclaim", err)
	}
	g.mustLock(t, y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	// A revoke in grace ends the client's reclaim there too.
	if err := g.Revoke(ctx, y.Client); err != nil {
		t.Fatal(err)
	}
	if _, err := g.lock(y, file, LockReq{Owner: owner(y, "lo"), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim after a revoke in grace: %v, want ErrNoReclaim", err)
	}
}

// A reference whose record is gone is stale in grace and when minted under
// another grace instance than the shard's, and bad otherwise: across 40
// restarts, only the one whose instance aliases the first's low bits finds
// it bad.
func TestMissJudgedByGraceInstance(t *testing.T) {
	f := newFixture(t)
	const s ShardID = 9
	serve := func(g *fixture, inst uint64) {
		if err := g.Recovery().ServeShard(ctx, s, inst); err != nil {
			t.Fatal(err)
		}
	}
	serve(f, 100)
	ref := mintIn(t, f.Table, entity.NewFileID(), 1, s)
	for k := uint64(1); k <= 40; k++ {
		f = f.restart(t)
		serve(f, 100+k)
		want := ErrStaleState
		if k == 32 {
			want = ErrBadState
		}
		if err := f.Recovery().TestState(ctx, Caller{Client: 1}, []StateRef{ref})[0]; !errors.Is(err, want) {
			t.Errorf("restart %d: %v, want %v", k, err, want)
		}
	}
	// In grace a miss is stale whatever its instance.
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	f.mustOpen(t, c, in(s), OpenRequest{Access: rd})
	f = f.restart(t)
	serve(f, 100+32+32)
	if err := f.Recovery().TestState(ctx, Caller{Client: 1}, []StateRef{ref})[0]; !errors.Is(err, ErrStaleState) {
		t.Errorf("an aliasing reference in grace: %v, want ErrStaleState", err)
	}
}

// A reclaim in a shard that lost nothing is answered with what the client
// still holds, its delegation included, and refused for more than it holds;
// in grace a reclaimed open is offered no delegation, the shard having lost
// its grants, and its owner needs no confirm.
func TestReclaimWhereNothingWasLost(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(5)
	res, err := f.Opens().Open(ctx, c, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Want: GrantRead})
	if err != nil || res.Grant == nil {
		t.Fatalf("Open: %+v, %v; want a delegation", res, err)
	}
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(res.Open), Ranges: []LockRange{sh(0, 10)}})

	again, err := f.Opens().Open(ctx, c, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true})
	if err != nil || again.Open.Ref.Token != res.Open.Ref.Token || again.Grant == nil || again.Grant.ID != res.Grant.ID {
		t.Errorf("a reclaim of the held open: %+v, %v; want it with its delegation", again, err)
	}
	if _, err := f.open(c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim of more access than held: %v, want ErrNoReclaim", err)
	}
	stranger := c
	stranger.Principal = entity.NewUser()
	if _, err := f.open(stranger, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Reclaim: true}); !errors.Is(err, ErrNotYours) {
		t.Errorf("a reclaim of the open by another principal: %v, want ErrNotYours", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(res.Open), Ranges: []LockRange{sh(2, 3)}, Reclaim: true}); err != nil {
		t.Errorf("a reclaim of a held lock: %v", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(res.Open), Ranges: []LockRange{x(0, 10)}, Reclaim: true}); !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim of an exclusive lock over a shared one: %v, want ErrNoReclaim", err)
	}

	g := f.restart(t)
	g.renew(t, c)
	one := uint32(1)
	got, err := g.Opens().Open(ctx, c, file, OpenRequest{Access: rd, Owner: owner(c, "oo"), Want: GrantRead, Reclaim: true, Seqs: Seqs{Owner: &one}})
	if err != nil || got.Grant != nil || got.Confirm {
		t.Errorf("a reclaim in grace: %+v, %v; want granted, with no delegation and no confirm", got, err)
	}
}

// A call that mints state in a shard the table has not begun serving is
// refused and writes nothing; once the shard is begun it is granted.
func TestUnservedShardMintsNothing(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(9)
	if _, err := f.open(c, file, OpenRequest{Access: rd}); !errors.Is(err, ErrDelay) {
		t.Errorf("an open in a shard not begun: %v, want ErrDelay", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: owner(c, "lo"), Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrDelay) {
		t.Errorf("a lock in a shard not begun: %v, want ErrDelay", err)
	}
	if rec, _ := f.Record(ctx, c.Client); len(rec.Shards) != 0 {
		t.Errorf("shards listed by the refusals: %+v", rec.Shards)
	}
	if err := f.Recovery().ServeShard(ctx, 9, 1); err != nil {
		t.Fatal(err)
	}
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd})
	if o.Ref.Instance != 1 {
		t.Errorf("reference minted under instance bits %d, want 1", o.Ref.Instance)
	}
	// Begun again under the same instance, as a handover is, it lost
	// nothing: no grace.
	if err := f.Recovery().ServeShard(ctx, 9, 1); err != nil {
		t.Fatal(err)
	}
	f.mustOpen(t, c, in(9), OpenRequest{Access: rd})
}

// A rename of the entry an open was made through moves the open's name, so
// the deferred unlink removes the entry's current name, and the directory
// it was made through follows it.
func TestRenameMovesTheDeferredUnlink(t *testing.T) {
	f := newFixture(t)
	p := f.caller(t, smb("p-pc"))
	file, d1, d2 := anyFile(), entity.NewFileID(), entity.NewFileID()
	o := f.mustOpen(t, p, file, OpenRequest{Via: d1, Name: []byte("a"), Access: rd | del, DeleteOnClose: true})
	link := f.mustOpen(t, p, file, OpenRequest{Via: d1, Name: []byte("c"), Access: rd}) // through another link
	if err := f.Opens().Renamed(ctx, file, d1, []byte("a"), d2, []byte("b")); err != nil {
		t.Fatal(err)
	}
	f.idx.Lock()
	n1, n2 := f.via[d1], f.via[d2]
	f.idx.Unlock()
	if n1 != 1 || n2 != 1 {
		t.Errorf("opens made through each directory: %d and %d, want 1 and 1", n1, n2)
	}
	f.close(t, p, link)
	if r := f.close(t, p, o); r.Unlink == nil || r.Unlink.Parent != d2 || string(r.Unlink.Name) != "b" {
		t.Errorf("the deferred unlink: %+v, want b in the directory it was renamed to", r.Unlink)
	}
}

// A client that ends in grace, by a reboot under a new Boot or a destroy, is
// no longer waited for: grace ends early once the others have finished.
func TestEndedClientEndsGraceEarly(t *testing.T) {
	f := newFixture(t)
	xc, yc := nfsAs("x", entity.RecoverStatus), nfsAs("y", entity.RecoverRefusal)
	xcl, y, z := f.caller(t, xc), f.caller(t, yc), f.caller(t, nfsAs("z", entity.RecoverStatus))
	f.mustOpen(t, xcl, in(5), OpenRequest{Access: rd})
	f.mustOpen(t, y, in(5), OpenRequest{Access: rd})

	g := f.restart(t)
	g.renew(t, z)
	free := func() error {
		_, err := g.lock(z, in(5), LockReq{Owner: owner(z, "lo"), Ranges: []LockRange{x(0, 1)}})
		return err
	}
	xc.Boot = [8]byte{2}
	if _, err := g.Connect(ctx, xc, nil); err != nil {
		t.Fatal(err)
	}
	if err := free(); !errors.Is(err, ErrGrace) {
		t.Errorf("with y still recorded: %v, want ErrGrace", err)
	}
	if err := g.Destroy(ctx, y); err != nil {
		t.Fatal(err)
	}
	if err := free(); err != nil {
		t.Errorf("once x rebooted and y was destroyed: %v, want granted", err)
	}
}

// A lock refused ErrGrace answers its owner's number, so the owner's next
// request is judged as following it.
func TestGraceRefusedLockAdvancesItsOwner(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file, other := in(5), in(5)
	oo := owner(c, "oo")
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, c, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	f.mustOpen(t, c, other, OpenRequest{Access: rd, Owner: oo, Seqs: num(3)})
	g := f.restart(t)
	g.renew(t, c)
	again := g.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Reclaim: true, Seqs: num(1)})
	_, err := g.lock(c, file, LockReq{Owner: owner(c, "lo"), Open: refOf(again), Ranges: []LockRange{x(0, 1)}, Seqs: Seqs{Owner: ptr(uint32(2)), NewOwner: 1}})
	if !errors.Is(err, ErrGrace) {
		t.Fatalf("a lock in grace: %v, want ErrGrace", err)
	}
	if _, err := g.open(c, other, OpenRequest{Access: rd, Owner: oo, Reclaim: true, Seqs: num(3)}); err != nil {
		t.Errorf("the owner's next request after the refused lock: %v, want granted", err)
	}
}

// A reclaim of locks answered from what the owner holds answers the lock
// owner's number.
func TestHeldLockReclaimAdvancesItsOwner(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	file := in(5)
	oo, lo := owner(c, "oo"), owner(c, "lo")
	o := f.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, c, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	res := f.mustLock(t, c, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Seqs: Seqs{Owner: ptr(uint32(3)), NewOwner: 1}})
	ref := res.Ref
	ref.Seq = 0
	byLock := OpenRef{Kind: RefLocked, State: ref}
	if _, err := f.lock(c, file, LockReq{Owner: lo, Open: byLock, Ranges: []LockRange{x(0, 10)}, Reclaim: true, Seqs: num(2)}); err != nil {
		t.Fatalf("a reclaim of a held lock: %v", err)
	}
	if _, err := f.lock(c, file, LockReq{Owner: lo, Open: byLock, Ranges: []LockRange{x(20, 10)}, Seqs: num(3)}); err != nil {
		t.Errorf("the lock owner's next lock after the reclaim: %v, want granted", err)
	}
}

// A reclaim admitted in grace that reaches its file's step after grace has
// ended is judged as one sent after it: of state not held, ErrNoReclaim.
func TestReclaimLandingAfterGrace(t *testing.T) {
	f := newFixture(t)
	c := f.caller(t, nfsAs("c", entity.RecoverStatus))
	f.mustOpen(t, c, in(5), OpenRequest{Access: rd, Owner: owner(c, "oo")})
	g := f.restart(t)
	g.renew(t, c)
	g.clock.advance(leasePeriod / 2)
	g.renew(t, c)
	target := in(5) // c never held it
	hold, release := make(chan struct{}), make(chan struct{})
	go g.step(target.ID, func(*entry) error { close(hold); <-release; return nil })
	<-hold
	done := make(chan error, 1)
	go func() {
		_, err := g.open(c, target, OpenRequest{Access: rd | wr, Deny: wr, Owner: owner(c, "oo"), Reclaim: true})
		done <- err
	}()
	fs := &g.files[target.ID[0]]
	for waiting := false; !waiting; {
		fs.mu.Lock()
		waiting = fs.entries[target.ID].users == 2
		fs.mu.Unlock()
	}
	g.clock.advance(leasePeriod / 2) // grace ends while the reclaim waits for the step
	close(release)
	if err := <-done; !errors.Is(err, ErrNoReclaim) {
		t.Errorf("a reclaim admitted in grace landing after it: %v, want ErrNoReclaim", err)
	}
}

// A host reclaiming the range a callback grant has just given it, while that
// callback is still on its way, keeps the lock when it then refuses the
// callback: the reclaim answered it granted.
func TestHeldReclaimSettlesAnOfferedGrant(t *testing.T) {
	f := newFixture(t)
	a, h := f.host(t, "a", 1), f.host(t, "h", 1)
	file := anyFile()
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	ws := f.sink.takeWaits()
	if g := granted(t, ws); len(g) != 1 {
		t.Fatalf("after the unlock: granted %v, want h", g)
	}
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Reclaim: true})
	if err := f.Locks().WaitAnswered(ctx, h, ws[0].w, true); err != nil {
		t.Fatal(err)
	}
	if f.holder(t, file, br(0, 10)) == nil {
		t.Errorf("h reclaimed the lock, then lost it to its own refused callback")
	}
}
