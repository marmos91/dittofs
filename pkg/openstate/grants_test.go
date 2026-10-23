package openstate

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

const (
	rwh = GrantRead | GrantWrite | GrantHandle
	rw  = GrantRead | GrantWrite
	rh  = GrantRead | GrantHandle
)

// grantOpen opens file for who, asking r, and answers the grant offered.
func (f *fixture) grantOpen(t *testing.T, who Caller, file FileKey, r OpenRequest) (Open, *CachingGrant) {
	t.Helper()
	res, err := f.Opens().Open(ctx, who, file, r)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return res.Open, res.Grant
}

// pass moves the clock d, firing no sweep, renewing each of keep often
// enough that its lease outlives it, then runs what the timer runs on each
// file holding a recall handed to the Sink.
func (f *fixture) pass(t *testing.T, d time.Duration, keep ...entity.ClientID) {
	t.Helper()
	for d > 0 {
		s := min(d, sweepEvery)
		f.clock.mu.Lock()
		f.clock.now += s
		f.clock.mu.Unlock()
		d -= s
		for _, c := range keep {
			if _, err := f.Renew(ctx, c); err != nil {
				t.Fatalf("Renew: %v", err)
			}
		}
	}
	f.idx.Lock()
	files := slices.Collect(maps.Keys(f.recalling))
	f.idx.Unlock()
	f.serveFiles(f.sink, files)
}

// ordered reports whether the file's admission lock was taken, since the last
// call, for file.
func (f *fixture) ordered(file FileKey) bool {
	f.order.mu.Lock()
	defer f.order.mu.Unlock()
	taken := slices.Contains(f.order.files, file.ID)
	f.order.files = nil
	return taken
}

func delayedFor(err error) DelayCause {
	var d *DelayError
	if errors.As(err, &d) {
		return d.Cause
	}
	return 0
}

func delegated(g *CachingGrant, file FileKey) OpenRef {
	return OpenRef{Kind: RefDelegated, State: g.ID.ref(), Handle: handle(file.ID)}
}

func (f *fixture) ack(who Caller, g *CachingGrant, to GrantKind, epoch uint16) error {
	return f.Grants().AckRecall(ctx, who, g.ID, to, epoch)
}

// One grant per client, lease key and file: a second open under the key is
// covered by it and breaks nothing, a third under another key breaks it, and
// each break carries a higher epoch. A delegation is never a handle grant,
// and its holder may return it unasked.
func TestOneGrantPerLeaseKey(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	k1, k2 := LeaseKey{1}, LeaseKey{2}
	o1, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: k1, Want: rwh})
	if g == nil || g.Kind != rwh || o1.Grant != g.ID {
		t.Fatalf("first open's grant %+v, open covered by %v", g, o1.Grant)
	}
	o2, g2 := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: k1, Want: rwh})
	if g2 == nil || g2.ID != g.ID || o2.Grant != g.ID || len(f.sink.takeRecalls()) != 0 {
		t.Fatalf("second open under the key: grant %+v, covered by %v; want the first's, no recall", g2, o2.Grant)
	}
	if _, err := f.open(a, file, OpenRequest{Access: rd, Lease: k2, Want: rwh}); delayedFor(err) != DelayRecall {
		t.Fatalf("an open under a second key: %v, want ErrDelay(DelayRecall)", err)
	}
	r := f.sink.takeRecalls()
	if len(r) != 1 || r[0].g.ID != g.ID || r[0].to != rh || r[0].g.Epoch <= g.Epoch {
		t.Fatalf("recalls %+v; want the lease to RH, past epoch %d", r, g.Epoch)
	}
	if _, err := f.open(a, anyFile(), OpenRequest{Access: rd, Lease: k1}); !errors.Is(err, ErrInvalid) {
		t.Errorf("the key on another file: %v, want ErrInvalid", err)
	}
	f.close(t, a, o1)
	f.close(t, a, o2)
	if _, err := f.open(a, anyFile(), OpenRequest{Access: rd, Lease: k1}); err != nil {
		t.Errorf("the key on another file once its lease closed: %v", err)
	}

	n := f.caller(t, nfs("host-n", 1, entity.NewUser()))
	nfile := anyFile()
	_, d := f.grantOpen(t, n, nfile, OpenRequest{Access: rd | wr, Want: rwh})
	if d == nil || d.Kind != rw {
		t.Fatalf("delegation %+v, want read and write without handle", d)
	}
	if _, again := f.grantOpen(t, n, nfile, OpenRequest{Access: rd, Want: rw}); again != nil {
		t.Errorf("a second delegation of one file to one client: %+v", again)
	}
	if err := f.ack(n, d, GrantNone, 0); err != nil {
		t.Fatalf("an unasked return: %v", err)
	}
	if err := f.ack(n, d, GrantNone, 0); !errors.Is(err, ErrBadState) {
		t.Errorf("a returned delegation: %v, want ErrBadState", err)
	}
}

// A later open under a lease's key widens it, raising its epoch, and the
// lease lasts until the last open it covers closes.
func TestLeaseUpgradesOnALaterOpen(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	o1, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: GrantRead})
	_, up := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh})
	if up == nil || up.ID != g.ID || up.Kind != rh || up.Epoch <= g.Epoch {
		t.Fatalf("upgrade: %+v, from %+v", up, g)
	}
	f.close(t, a, o1)
	if _, still := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh}); still == nil || still.ID != g.ID {
		t.Errorf("the lease after one of its opens closed: %+v, want it kept", still)
	}
}

// A conflicting request is answered ErrDelay at once, never waiting on the
// holder. A delegation unanswered for a lease period is revoked: the
// conflicting open goes ahead, the holder is told, its later writes are
// ErrRevoked, its renewals report it, and FreeState clears it. A lease
// unanswered for 35 s is cut to none, and its holder's open stays usable.
func TestRecallDeadline(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	b := f.caller(t, nfs("host-b", 1, entity.NewUser()))
	file := anyFile()
	_, d := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Want: rw})
	if err := f.Checks().CheckIO(ctx, a, delegated(d, file), br(0, 1), true); err != nil {
		t.Fatalf("the holder's write under its delegation: %v", err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("a conflicting open: %v, want ErrDelay(DelayRecall)", err)
	}
	if r := f.sink.takeRecalls(); len(r) != 1 || r[0].to != GrantNone {
		t.Fatalf("recalls %+v, want one to none", r)
	}
	f.pass(t, leasePeriod-time.Second, a.Client, b.Client)
	if _, err := f.open(b, file, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("before the deadline: %v, want ErrDelay", err)
	}
	f.pass(t, time.Second, a.Client, b.Client)
	f.mustOpen(t, b, file, OpenRequest{Access: rd})
	if r := f.sink.takeRevoked(); len(r) != 1 || r[0] != (Revocation{Client: a.Client, Grant: d.ID, Reason: RevokedGrant}) {
		t.Errorf("Revoked %+v", r)
	}
	var rev *RevokedError
	if err := f.Checks().CheckIO(ctx, a, delegated(d, file), br(0, 1), true); !errors.As(err, &rev) || rev.Cause != RevokedGrant {
		t.Errorf("a write under the revoked delegation: %v, want ErrRevoked(RevokedGrant)", err)
	}
	if s, _ := f.Renew(ctx, a.Client); !s.RecallableRevoked || s.AdminRevoked {
		t.Errorf("renewal %+v, want RecallableRevoked alone", s)
	}
	wrong := d.ID.ref()
	wrong.Type = StateOpen
	if errs := f.Recovery().TestState(ctx, a, []StateRef{wrong}); !errors.Is(errs[0], ErrBadState) {
		t.Errorf("the revoked delegation named as an open: %v, want ErrBadState", errs[0])
	}
	if err := f.Recovery().FreeState(ctx, b, d.ID.ref()); !errors.Is(err, ErrNotYours) {
		t.Errorf("another client freeing the revoked delegation: %v, want ErrNotYours", err)
	}
	if err := f.Recovery().FreeState(ctx, a, d.ID.ref()); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.Renew(ctx, a.Client); s.RecallableRevoked {
		t.Error("the flag outlived FreeState")
	}
	if errs := f.Recovery().TestState(ctx, a, []StateRef{d.ID.ref()}); !errors.Is(errs[0], ErrBadState) {
		t.Errorf("a freed reference: %v, want ErrBadState", errs[0])
	}

	s := f.caller(t, smb("pc-s"))
	other := f.caller(t, smb("pc-o"))
	sfile := anyFile()
	so, _ := f.grantOpen(t, s, sfile, OpenRequest{Access: rd | wr, Lease: LeaseKey{1}, Want: rwh})
	if _, err := f.open(other, sfile, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("a conflicting SMB open: %v", err)
	}
	f.pass(t, coversDeadline-time.Second)
	if _, err := f.open(other, sfile, OpenRequest{Access: rd}); delayedFor(err) != DelayRecall {
		t.Fatalf("before 35 s: %v, want ErrDelay", err)
	}
	f.pass(t, time.Second)
	f.mustOpen(t, other, sfile, OpenRequest{Access: rd})
	if err := f.Checks().CheckIO(ctx, s, refOf(so), br(0, 1), true); err != nil {
		t.Errorf("a write through the open of a lease cut to none: %v", err)
	}
	if r := f.sink.takeRevoked(); len(r) != 0 {
		t.Errorf("a lease cut to none was reported revoked: %+v", r)
	}
	f.idx.Lock()
	counted := f.granted[s.Client]
	f.idx.Unlock()
	if counted != 0 {
		t.Errorf("a lease cut to none still counts %d against its client's budget", counted)
	}
}

// A client that let a recall time out is offered no grant at that primary
// for a lease period after it, and one after.
func TestFenceAfterAMissedRecall(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rwh})
	if _, err := f.open(b, file, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	f.pass(t, coversDeadline)
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: rwh}); g != nil {
		t.Fatalf("a fenced client was offered %+v", g)
	}
	f.pass(t, leasePeriod)
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Lease: LeaseKey{3}, Want: rwh}); g == nil {
		t.Fatal("no grant once the fence ended")
	}
}

// A recall's deadline starts when the Sink is handed it: one held in the
// table past its nominal deadline is not cut and fences nobody, and its
// holder answering within a deadline of its sending keeps its grants.
func TestDeadlineStartsWhenSent(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rwh})
	release, handed := make(chan struct{}), make(chan struct{})
	f.sink.onRecall = func(CachingGrant, GrantKind) {
		close(handed)
		<-release
	}
	done := make(chan error)
	go func() { _, err := f.open(b, file, OpenRequest{Access: wr}); done <- err }()
	<-handed
	f.pass(t, 2*coversDeadline)
	f.sink.onRecall = nil
	close(release)
	if err := <-done; delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	f.pass(t, coversDeadline-time.Second)
	r := f.sink.takeRecalls()
	if len(r) != 1 || r[0].g.Recall == nil {
		t.Fatalf("recalls %+v", r)
	}
	if err := f.ack(a, &r[0].g, GrantNone, r[0].g.Epoch); err != nil {
		t.Fatalf("an answer within a deadline of the sending: %v", err)
	}
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: rwh}); g == nil {
		t.Fatal("a holder that answered in time was fenced")
	}
}

// A request that would start a recall past 4,096 not yet handed to the Sink
// is ErrDelay with DelayResources, and starts none.
func TestUnsentRecallsAreBounded(t *testing.T) {
	f := newFixture(t)
	holders := []Caller{f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))}
	b := f.caller(t, smb("pc-c"))
	files := make([]FileKey, maxUnsent+1)
	for i := range files {
		files[i] = anyFile()
		f.grantOpen(t, holders[i%2], files[i], OpenRequest{Access: rd, Lease: LeaseKey{byte(i), byte(i >> 8)}, Want: rw})
	}
	release := make(chan struct{})
	f.sink.onRecall = func(CachingGrant, GrantKind) { <-release }
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(release)
	for _, file := range files[:maxUnsent] {
		wg.Go(func() { _, _ = f.open(b, file, OpenRequest{Access: wr}) })
	}
	for {
		f.idx.Lock()
		n := f.unsent
		f.idx.Unlock()
		if n == maxUnsent {
			break
		}
		time.Sleep(time.Millisecond)
	}
	past := make(chan error, 1)
	wg.Go(func() { _, err := f.open(b, files[maxUnsent], OpenRequest{Access: wr}); past <- err })
	select {
	case err := <-past:
		if delayedFor(err) != DelayResources {
			t.Errorf("a recall past the bound: %v, want ErrDelay(DelayResources)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request past the bound started a recall: it is held in the Sink")
	}
	if err := f.step(files[maxUnsent].ID, func(e *entry) error {
		for _, g := range e.grants {
			if g.fsm.Recall != nil {
				t.Error("the refused request started a recall")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A primary holds at most 4,096 grants per client; past it the client is
// offered none and keeps those it holds, and is offered one again once one
// of them caches nothing.
func TestGrantBudget(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	var first *CachingGrant
	firstFile := anyFile()
	for i := range maxGrants {
		file := anyFile()
		if i == 0 {
			file = firstFile
		}
		_, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{byte(i), byte(i >> 8), 1}, Want: rh})
		if g == nil {
			t.Fatalf("grant %d not offered", i)
		}
		if i == 0 {
			first = g
		}
	}
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Lease: LeaseKey{0xff, 0xff}, Want: rh}); g != nil {
		t.Fatal("a grant past the budget")
	}
	if err := f.ack(a, first, GrantNone, 0); err != nil {
		t.Fatal(err)
	}
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Lease: LeaseKey{0xfe, 0xff}, Want: rh}); g == nil {
		t.Fatal("no grant once a lease went to none")
	}
	if _, g := f.grantOpen(t, a, firstFile, OpenRequest{Access: rd, Lease: first.Key, Want: rh}); g == nil || g.Kind != GrantNone {
		t.Errorf("a lease at none widened past the budget: %+v", g)
	}
}

// A lock request recalls every other client's caching grant on the file to
// none before it is decided: a callback request waits and is granted once
// the holder returns the grant; any other is ErrDelay, and granted on its
// retry after.
func TestLockRecallsGrants(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	h := f.host(t, "nlm-b", 1)
	file := anyFile()
	file.Shard = 1
	_, d := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Want: rw})
	res, err := f.lock(h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	if err != nil || !res.Waiting {
		t.Fatalf("a blocking lock against a delegation: %+v, %v; want Waiting", res, err)
	}
	if r := f.sink.takeRecalls(); len(r) != 1 || r[0].to != GrantNone {
		t.Fatalf("recalls %+v", r)
	}
	if _, err := f.lock(h, file, LockReq{Owner: owner(h, "q"), Ranges: []LockRange{x(20, 10)}}); delayedFor(err) != DelayRecall {
		t.Fatalf("a non-blocking lock while the recall runs: %v, want ErrDelay", err)
	}
	// The holder flushes its locks: one elsewhere, released at once, and one
	// over the waiter's range.
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(50, 10)}})
	f.unlock(t, a, file, owner(a, "a"), br(50, 10))
	if w := f.sink.takeWaits(); len(w) != 0 {
		t.Fatalf("the waiter was served while the delegation was held: %+v", w)
	}
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	h2 := f.host(t, "nlm-c", 1)
	f.mustLock(t, h2, file, LockReq{Owner: owner(h2, "r"), Ranges: []LockRange{x(30, 5)}, Wait: WaitCallback})
	f.ordered(file)
	if err := f.ack(a, d, GrantNone, 0); err != nil {
		t.Fatal(err)
	}
	if w := f.sink.takeWaits(); len(w) != 1 || w[0].w.Owner.Client != h2.Client {
		t.Fatalf("waiters granted on the return: %+v; want the one no lock blocks, not the one under the holder's flushed lock", w)
	}
	if !f.ordered(file) {
		t.Error("the return granted a waiter without the file's admission lock")
	}
	f.pass(t, 2*leasePeriod, a.Client)
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Want: GrantRead}); g != nil {
		t.Fatalf("a grant on a file with a lock waiter: %+v", g)
	}
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	if g := granted(t, f.sink.takeWaits()); len(g) != 1 {
		t.Fatalf("waiters granted at the holder's unlock: %v", g)
	}
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "q"), Ranges: []LockRange{x(20, 10)}})
}

// An administrator's revoke releases the client's state for others at once,
// keeps its record with its Shards emptied, and answers each reference
// ErrRevoked, flagged AdminRevoked, until freed. An ownerless client's revoke
// deletes its record.
func TestRevoke(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	b := f.caller(t, nfs("host-b", 1, entity.NewUser()))
	file := anyFile()
	file.Shard = 1
	o, d := f.grantOpen(t, a, file, OpenRequest{Access: rd, Deny: wr, Want: GrantRead})
	f.mustOpen(t, b, file, OpenRequest{Access: rd})
	h := f.host(t, "nlm-h", 1)
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.ordered(file)
	if err := f.Revoke(ctx, a.Client); err != nil {
		t.Fatal(err)
	}
	if w := granted(t, f.sink.takeWaits()); len(w) != 1 || !f.ordered(file) {
		t.Errorf("waiters granted by the revoke: %v; want one, under the file's admission lock", w)
	}
	f.mustOpen(t, b, file, OpenRequest{Access: wr})
	for _, ref := range []StateRef{o.Ref, d.ID.ref()} {
		var rev *RevokedError
		if errs := f.Recovery().TestState(ctx, a, []StateRef{ref}); !errors.As(errs[0], &rev) || rev.Cause != RevokedByAdmin {
			t.Errorf("%v after the revoke: %v, want ErrRevoked(RevokedByAdmin)", ref.Type, errs[0])
		}
	}
	if s, _ := f.Renew(ctx, a.Client); !s.AdminRevoked || s.RecallableRevoked {
		t.Errorf("renewal %+v, want AdminRevoked alone", s)
	}
	if err := f.Destroy(ctx, a); !errors.Is(err, ErrClientBusy) {
		t.Errorf("Destroy holding revoked references: %v, want ErrClientBusy", err)
	}
	if rec, err := f.Record(ctx, a.Client); err != nil || len(rec.Shards) != 0 {
		t.Errorf("record after the revoke: %+v, %v; want kept, its Shards empty", rec.Shards, err)
	}
	if r := f.sink.takeRevoked(); len(r) != 1 || r[0].Reason != RevokedByAdmin {
		t.Errorf("Revoked %+v", r)
	}
	if err := f.Recovery().FreeState(ctx, a, o.Ref); err != nil {
		t.Fatal(err)
	}
	f.mustOpen(t, a, file, OpenRequest{Access: rd})
	if rec, _ := f.Record(ctx, a.Client); len(rec.Shards) != 1 {
		t.Errorf("Shards after an open following the revoke: %+v, want its shard listed again", rec.Shards)
	}
	if err := f.Expire(ctx, a.Client); err != nil {
		t.Fatal(err)
	}
	if errs := f.Recovery().TestState(ctx, a, []StateRef{d.ID.ref()}); !errors.Is(errs[0], ErrBadState) {
		t.Errorf("a revoked reference of an expired client: %v, want ErrBadState", errs[0])
	}

	s := f.caller(t, smb("pc-s"))
	if err := f.Revoke(ctx, s.Client); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Record(ctx, s.Client); !errors.Is(err, ErrStaleClient) {
		t.Errorf("an ownerless client's record after its revoke: %v, want deleted", err)
	}
}

// No write caching is offered on a setuid or setgid file, and a mode change
// setting either bit recalls the write caching of every grant on the file.
func TestSetIDRecallsWriteCaching(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	if _, d := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd | wr, Want: rw, SetID: true}); d == nil || d.Kind != GrantRead {
		t.Fatalf("on a setuid file: %+v, want a read delegation", d)
	}
	file := anyFile()
	f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Want: rw})
	if err := f.Checks().CheckChange(ctx, a, file, ChangeSetID); delayedFor(err) != DelayRecall {
		t.Fatalf("setting setuid under the holder's own write delegation: %v, want ErrDelay", err)
	}
	if r := f.sink.takeRecalls(); len(r) != 1 || r[0].to != GrantNone {
		t.Fatalf("recalls %+v, want the write delegation recalled", r)
	}
}

// A file recalled is offered no grant for a recall deadline after the
// holder's answer, so a holder that reopens at once does not overtake the
// request the recall delayed.
func TestRecallNotRacedByANewGrant(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	b := f.caller(t, nfs("host-b", 1, entity.NewUser()))
	file := anyFile()
	ao, d := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Want: rw})
	if _, err := f.open(b, file, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	if err := f.ack(a, d, GrantNone, 0); err != nil {
		t.Fatal(err)
	}
	f.close(t, a, ao)
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Want: rw}); g != nil {
		t.Fatalf("the holder was granted again at once: %+v", g)
	}
	f.mustOpen(t, b, file, OpenRequest{Access: wr})
}

// A delegation's reference is its client's alone: another client naming it
// is ErrNotYours and leaves it intact, and naming another kind of state by a
// grant's ID is ErrBadState.
func TestDelegationIsItsClients(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	b := f.caller(t, nfs("host-b", 1, entity.NewUser()))
	file := anyFile()
	o, d := f.grantOpen(t, a, file, OpenRequest{Access: rd, Want: GrantRead})
	if err := f.Checks().CheckIO(ctx, b, delegated(d, file), br(0, 1), false); !errors.Is(err, ErrNotYours) {
		t.Errorf("another client's I/O under the delegation: %v, want ErrNotYours", err)
	}
	if err := f.ack(b, d, GrantNone, 0); !errors.Is(err, ErrNotYours) {
		t.Errorf("another client's return: %v, want ErrNotYours", err)
	}
	if err := f.Checks().CheckIO(ctx, a, delegated(d, file), br(0, 1), true); !errors.Is(err, ErrAccess) {
		t.Errorf("a write under a read delegation: %v, want ErrAccess", err)
	}
	if err := f.Grants().AckRecall(ctx, a, GrantID{o.Ref.Shard, o.Ref.Token}, GrantNone, 0); !errors.Is(err, ErrBadState) {
		t.Errorf("an open's token named as a grant: %v, want ErrBadState", err)
	}
	if err := f.Checks().CheckIO(ctx, a, delegated(d, file), br(0, 1), false); err != nil {
		t.Errorf("the holder's read after: %v", err)
	}
}

// Handle caching is broken before a deny-mode conflict is refused: the
// conflicting request is ErrDelay until the break ends, and refused only by
// a conflict that remains.
func TestHandleBreaksBeforeSharingViolation(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	_, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Deny: wr, Lease: LeaseKey{1}, Want: rh})
	if _, err := f.open(b, file, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Fatalf("a conflict with a handle-cached open: %v, want ErrDelay", err)
	}
	r := f.sink.takeRecalls()
	if len(r) != 1 || r[0].to != GrantRead {
		t.Fatalf("recalls %+v, want handle caching broken", r)
	}
	if err := f.ack(a, g, GrantRead, r[0].g.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: wr}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("the conflict that remains: %v, want ErrShareViolation", err)
	}
}

// A client whose back channel is lost is offered no grant until it rebinds,
// which only its owner may do.
func TestRebind(t *testing.T) {
	f := newFixture(t)
	c := nfs("host-a", 1, entity.NewUser())
	c.Reach.Down = true
	a := f.caller(t, c)
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Want: GrantRead}); g != nil {
		t.Fatalf("a grant with the back channel down: %+v", g)
	}
	if err := f.Rebind(ctx, Caller{Client: a.Client, Principal: entity.NewUser()}, entity.NodeID{1}); !errors.Is(err, ErrNotYours) {
		t.Errorf("a rebind by another principal: %v, want ErrNotYours", err)
	}
	if err := f.Rebind(ctx, a, entity.NodeID{1}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := f.Record(ctx, a.Client); rec.Reach.Down || rec.Reach.Callback != (entity.NodeID{1}) {
		t.Errorf("record after the rebind: %+v", rec.Reach)
	}
	if _, g := f.grantOpen(t, a, anyFile(), OpenRequest{Access: rd, Want: GrantRead}); g == nil {
		t.Fatal("no grant after the rebind")
	}
}

// The holder's own I/O and changes recall nothing; another client's change
// recalls every caching, its read write caching alone, and its write every
// caching.
func TestIORecallsGrants(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	o, _ := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Lease: LeaseKey{1}, Want: rwh})
	if err := f.Checks().CheckIO(ctx, a, refOf(o), br(0, 1), true); err != nil {
		t.Fatalf("a write through the lease's own open: %v", err)
	}
	if err := f.Checks().CheckChange(ctx, a, file, ChangeAttrs); err != nil {
		t.Fatalf("the holder's own set-attribute: %v", err)
	}
	if err := f.Checks().CheckChange(ctx, f.caller(t, smb("pc-b")), file, ChangeAttrs); delayedFor(err) != DelayRecall {
		t.Fatalf("another client's set-attribute: %v, want ErrDelay", err)
	}
	if r := f.sink.takeRecalls(); len(r) != 1 || r[0].to != GrantNone {
		t.Fatalf("another client's set-attribute: recalls %+v, want to none", r)
	}
	file = anyFile()
	f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: rwh})
	anon := OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}
	if err := f.Checks().CheckIO(ctx, Caller{}, anon, br(0, 1), false); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	r := f.sink.takeRecalls()
	if len(r) != 1 || r[0].to != rh {
		t.Fatalf("an NFSv3 read: recalls %+v, want to RH", r)
	}
	if err := f.ack(a, &r[0].g, rh, r[0].g.Epoch); err != nil {
		t.Fatal(err)
	}
	if err := f.Checks().CheckIO(ctx, Caller{}, anon, br(0, 1), true); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	if r := f.sink.takeRecalls(); len(r) != 1 || r[0].to != GrantNone {
		t.Fatalf("an NFSv3 write: recalls %+v, want to none", r)
	}
}

// A lock state holding no locks is freed; one holding some is ErrLocksHeld.
func TestFreeStateOfALockState(t *testing.T) {
	f := newFixture(t)
	h := f.host(t, "nlm-a", 1)
	file := anyFile()
	file.Shard = 1
	res := f.mustLock(t, h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}})
	if err := f.Recovery().FreeState(ctx, h, res.Ref); !errors.Is(err, ErrLocksHeld) {
		t.Fatalf("a lock state holding a lock: %v, want ErrLocksHeld", err)
	}
	waiter := f.host(t, "nlm-b", 1)
	w := f.mustLock(t, waiter, file, LockReq{Owner: owner(waiter, "q"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	if err := f.Recovery().FreeState(ctx, waiter, w.Ref); err != nil {
		t.Fatalf("a lock state holding none: %v", err)
	}
	if errs := f.Recovery().TestState(ctx, waiter, []StateRef{w.Ref}); !errors.Is(errs[0], ErrBadState) {
		t.Errorf("a freed lock state: %v, want ErrBadState", errs[0])
	}
}

// A Sink that answers a recall from inside RecallGrant deadlocks nothing: no
// table lock is held while it runs.
func TestRecallSinkMayCallBack(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rwh})
	f.sink.onRecall = func(g CachingGrant, to GrantKind) { _ = f.ack(a, &g, to, g.Epoch) }
	done := make(chan error)
	go func() {
		done <- f.Checks().CheckIO(ctx, b, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 1), true)
	}()
	select {
	case err := <-done:
		if delayedFor(err) != DelayRecall {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Sink answering a recall from RecallGrant deadlocked")
	}
}

// No grant is offered beyond what another holder's open or grant allows:
// none beside another's writer, no write caching beside another's reader or
// read delegation, none past 16 grants caching on a file, and none while a
// recall runs there.
func TestOfferedBesideOthers(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	f.mustOpen(t, f.caller(t, smb("pc-w")), file, OpenRequest{Access: wr})
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rwh}); g != nil {
		t.Errorf("beside another's writer: %+v", g)
	}
	file = anyFile()
	f.mustOpen(t, f.caller(t, smb("pc-r")), file, OpenRequest{Access: rd})
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: rwh}); g == nil || g.Kind != rh {
		t.Errorf("beside another's reader: %+v, want RH", g)
	}
	file = anyFile()
	n := f.caller(t, nfs("host-n", 1, entity.NewUser()))
	no, _ := f.grantOpen(t, n, file, OpenRequest{Access: rd, Want: GrantRead})
	f.close(t, n, no)
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{3}, Want: rwh}); g == nil || g.Kind != rh {
		t.Errorf("beside a read delegation: %+v, want RH", g)
	}
	file = anyFile()
	var c0 Caller
	var g0 *CachingGrant
	for i := range maxShared + 1 {
		c := f.caller(t, smb(fmt.Sprint("pc-", i)))
		_, g := f.grantOpen(t, c, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh})
		if (g != nil) != (i < maxShared) {
			t.Fatalf("grant %d on one file: %+v", i, g)
		}
		if i == 0 {
			c0, g0 = c, g
		}
	}
	if err := f.ack(c0, g0, GrantNone, 0); err != nil {
		t.Fatal(err)
	}
	if _, g := f.grantOpen(t, f.caller(t, smb("pc-late")), file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh}); g == nil {
		t.Error("no grant once one of 16 on the file went to none")
	}
	// A recall in flight on the file holds back every new grant, even one
	// it would not conflict with.
	file = anyFile()
	f.grantOpen(t, c0, file, OpenRequest{Access: rd, Lease: LeaseKey{2}, Want: GrantRead})
	if _, err := f.open(f.caller(t, smb("pc-x")), file, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{4}, Want: GrantRead}); g != nil {
		t.Errorf("a grant while a recall is in flight on the file: %+v", g)
	}
}

// A callback lock waiter blocked only by another client's lease is served
// when the holder answers the recall by closing the lease's last open,
// whether the recall was handed to the Sink before the close or not.
func TestLeaseClosedServesWaiter(t *testing.T) {
	for _, early := range []bool{true, false} {
		f := newFixture(t)
		a := f.caller(t, smb("pc-a"))
		h := f.host(t, "nlm-b", 1)
		file := anyFile()
		file.Shard = 1
		ao, _ := f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh})
		if early {
			f.sink.onRecall = func(CachingGrant, GrantKind) { f.sink.onRecall = nil; f.close(t, a, ao) }
		}
		if res, err := f.lock(h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); err != nil || !res.Waiting {
			t.Fatalf("%+v, %v; want Waiting", res, err)
		}
		if !early {
			f.close(t, a, ao)
		}
		if w := f.sink.takeWaits(); len(w) != 1 {
			t.Errorf("closing before the recall was sent %v: waiters granted at the close %+v, want 1", early, w)
		}
	}
}

// A holder that answers a recall by closing and opening again is not granted
// again ahead of the request the recall delayed.
func TestRecallNotRacedByCloseAndReopen(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	ao, _ := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Lease: LeaseKey{1}, Want: rwh})
	if _, err := f.open(b, file, OpenRequest{Access: wr}); delayedFor(err) != DelayRecall {
		t.Fatal(err)
	}
	f.close(t, a, ao)
	if _, g := f.grantOpen(t, a, file, OpenRequest{Access: rd | wr, Lease: LeaseKey{1}, Want: rwh}); g != nil {
		t.Fatalf("the holder was granted again at once after closing: %+v", g)
	}
}

// A lock through one of a client's lease keys recalls its lease under
// another key, as another client's would be, and waits on it.
func TestLockRecallsOwnClientsOtherLease(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	file := anyFile()
	file.Shard = 1
	o2 := f.mustOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{2}})
	f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh})
	under2 := OpenRef{Kind: RefOpened, Open: o2.ID, State: StateRef{Shard: file.Shard}, Handle: handle(file.ID)}
	_, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o2), Open: under2, Ranges: []LockRange{x(0, 10)}})
	if r := f.sink.takeRecalls(); delayedFor(err) != DelayRecall || len(r) != 1 || r[0].to != GrantNone {
		t.Fatalf("a lock under key 2: %v, recalls %+v; want key 1's lease recalled to none", err, r)
	}
	res, err := f.lock(a, file, LockReq{Owner: smbOwner(a, o2), Open: under2, Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	if err != nil || !res.Waiting {
		t.Fatalf("a waiting lock under key 2: %+v, %v", res, err)
	}
	h := f.host(t, "nlm-h", 1)
	_, _ = f.lock(h, file, LockReq{Owner: owner(h, "p"), Ranges: []LockRange{x(50, 10)}}) // serves the file's waiters first
	if w := f.sink.takeWaits(); len(w) != 0 {
		t.Errorf("a waiter under key 2 granted while key 1's lease still caches: %+v", w)
	}
}

// A recall not yet handed to the Sink is never revoked, while another on the
// same file that was handed over is.
func TestOnlyASentRecallLapses(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("pc-a")), f.caller(t, smb("pc-b"))
	file := anyFile()
	f.grantOpen(t, a, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: GrantRead})
	f.grantOpen(t, b, file, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: GrantRead})
	release, held := make(chan struct{}), make(chan entity.ClientID, 1)
	calls := 0
	f.sink.onRecall = func(g CachingGrant, _ GrantKind) {
		if calls++; calls == 2 {
			held <- g.Client
			<-release
		}
	}
	done := make(chan error)
	go func() { _, err := f.open(f.caller(t, smb("pc-c")), file, OpenRequest{Access: wr}); done <- err }()
	slow := <-held
	f.pass(t, 2*coversDeadline)
	close(release)
	<-done
	f.sink.onRecall = nil
	for _, c := range []entity.ClientID{a.Client, b.Client} {
		f.cmu.Lock()
		fenced := f.fenced[c] != 0
		f.cmu.Unlock()
		if fenced != (c != slow) {
			t.Errorf("client %d fenced %v; want only the one whose recall was sent", c, fenced)
		}
	}
}
