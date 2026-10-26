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

	again := g.mustOpen(t, c, file, OpenRequest{Access: rd | wr, Owner: owner(c, "oo"), Reclaim: true})
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
		return g.Checks().CheckIO(ctx, Caller{}, OpenRef{Kind: RefAnonymous, State: StateRef{Shard: file.Shard}, Handle: handle(file.ID)}, br(0, 1), true)
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
	g.clock.advance(leasePeriod)
	if err := write(file); err != nil {
		t.Errorf("an anonymous write after grace: %v", err)
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
	f.mustOpen(t, c, b, OpenRequest{Access: rd, Owner: owner(c, "oo")})
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

	// Shard 7 is lost again after c was shown its instance.
	if err := g.Recovery().ServeShard(ctx, 7, g.shards[7].instance+100); err != nil {
		t.Fatal(err)
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

// A client that recovers by refusal is refused on every call naming it
// until it connects again under its name and Boot, which keeps its ID; a
// reference it held is stale, and its reclaim is then granted. It never
// counts as finished, so grace runs until it expires.
func TestRefusalClientConnectsAgain(t *testing.T) {
	f := newFixture(t)
	cl := nfsAs("c40", entity.RecoverRefusal)
	c, done := f.caller(t, cl), f.caller(t, nfsAs("done", entity.RecoverStatus))
	file := in(5)
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
	if _, err := g.Locks().TestLock(ctx, c, file, owner(c, "lo"), br(0, 1), true); !errors.Is(err, ErrStaleClient) {
		t.Errorf("TestLock: %v, want ErrStaleClient", err)
	}
	if err := g.Locks().ReleaseLockOwner(ctx, c, owner(c, "lo")); !errors.Is(err, ErrStaleClient) {
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
	g.mustLock(t, done, in(5), LockReq{Owner: owner(done, "lo"), Ranges: []LockRange{x(0, 1)}})
}

// A shard where NLM hosts held locks raises the restart-notice number before
// its grace begins and tells each host, with its adapter bytes, as it tells
// an SMB client; the hosts' reclaims are granted, and a host revoked before
// the restart is told nothing and reclaims nothing.
func TestShardLossToldToHosts(t *testing.T) {
	f := newFixture(t)
	h1, h2, gone := f.host(t, "h1", 1), f.host(t, "h2", 1), f.host(t, "gone", 1)
	sc := smb("pc")
	sc.Recovery = entity.RecoverReconnect
	s := f.caller(t, sc)
	file := in(5)
	for i, h := range []Caller{h1, h2, gone} {
		f.mustLock(t, h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(uint64(i)*10, 10)}})
	}
	f.mustOpen(t, s, file, OpenRequest{Access: rd})
	if _, err := f.AddSession(ctx, h1.Client, entity.NodeID{}, nil, []byte("h1-addr")); err != nil {
		t.Fatal(err)
	}
	if err := f.Revoke(ctx, gone.Client); err != nil {
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
	if err := g.Recovery().ServeShard(ctx, 5, instances.Add(1)); err != nil {
		t.Fatal(err)
	}
	if after, _ := f.fs.Store.NSMState(ctx); after <= before {
		t.Errorf("restart-notice number %d after the shard began, %d before", after, before)
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
	if err := f.Opens().Renamed(ctx, file, d1, []byte("a"), d2, []byte("b")); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[entity.FileID]error{d1: nil, d2: ErrShareViolation} {
		if err := f.Checks().CheckChange(ctx, p, FileKey{ID: dir}, nil, ChangeRemove); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("an SMB remove of the directory: %v, want %v", err, want)
		}
	}
	if r := f.close(t, p, o); r.Unlink == nil || r.Unlink.Parent != d2 || string(r.Unlink.Name) != "b" {
		t.Errorf("the deferred unlink: %+v, want b in the directory it was renamed to", r.Unlink)
	}
}
