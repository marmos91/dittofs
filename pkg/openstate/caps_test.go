package openstate

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// lower sets tab's cap of kind k to n.
func (tab *Table) lower(k capped, n int) {
	tab.idx.Lock()
	defer tab.idx.Unlock()
	tab.caps[k] = n
}

// A client's opens, lock ranges, lock owners and waiters are each capped
// here: a request past a cap is ErrDelay (DelayResources) and changes
// nothing, and what a close or an unlock frees is room again. An unlock
// splitting a range adds one, so at the cap it is refused as a lock is.
func TestPerClientCaps(t *testing.T) {
	t.Run("opens", func(t *testing.T) {
		f := newFixture(t)
		f.lower(cappedOpens, 2)
		a := f.caller(t, nfs("a", 1, entity.NewUser()))
		first := f.mustOpen(t, a, in(1), OpenRequest{Access: rd})
		f.mustOpen(t, a, in(1), OpenRequest{Access: rd})
		third := in(1)
		if _, err := f.open(a, third, OpenRequest{Access: rd}); delayedFor(err) != DelayResources {
			t.Fatalf("an open past the cap: %v, want ErrDelay (DelayResources)", err)
		}
		if f.entryOf(third.ID) != nil {
			t.Error("a refused open left state")
		}
		f.close(t, a, first)
		up := f.mustOpen(t, a, third, OpenRequest{Access: rd, Owner: owner(a, "oo")})
		if _, err := f.open(a, third, OpenRequest{Access: rd | wr, Owner: owner(a, "oo")}); err != nil {
			t.Errorf("an upgrade at the cap: %v, want it granted", err)
		}
		f.close(t, a, up)
	})
	t.Run("ranges", func(t *testing.T) {
		f := newFixture(t)
		f.lower(cappedRanges, 2)
		a := f.caller(t, nfs("a", 1, entity.NewUser()))
		file, lo := in(1), owner(a, "lo")
		f.mustLock(t, a, file, LockReq{Owner: lo, Ranges: []LockRange{x(0, 10), x(20, 10)}})
		if _, err := f.lock(a, file, LockReq{Owner: lo, Ranges: []LockRange{x(40, 10)}}); delayedFor(err) != DelayResources {
			t.Fatalf("a lock past the cap: %v, want ErrDelay (DelayResources)", err)
		}
		if h := f.holder(t, file, br(40, 10)); h != nil {
			t.Errorf("a refused lock is held: %+v", h)
		}
		if _, err := f.Locks().Unlock(ctx, a, file, none, lo, []ByteRange{br(4, 2)}, Seqs{}); delayedFor(err) != DelayResources {
			t.Fatalf("an unlock splitting a range at the cap: %v, want ErrDelay (DelayResources)", err)
		}
		if h := f.holder(t, file, br(4, 2)); h == nil {
			t.Error("a refused unlock released its range")
		}
		f.unlock(t, a, file, lo, br(20, 10))
		f.mustLock(t, a, file, LockReq{Owner: lo, Ranges: []LockRange{x(40, 10)}})
	})
	t.Run("owners", func(t *testing.T) {
		f := newFixture(t)
		f.lower(cappedOwners, 1)
		a := f.caller(t, nfs("a", 1, entity.NewUser()))
		one, two := in(1), in(1)
		f.mustLock(t, a, one, LockReq{Owner: owner(a, "first"), Ranges: []LockRange{x(0, 10)}})
		if _, err := f.lock(a, two, LockReq{Owner: owner(a, "second"), Ranges: []LockRange{x(0, 10)}}); delayedFor(err) != DelayResources {
			t.Fatalf("a lock by an owner past the cap: %v, want ErrDelay (DelayResources)", err)
		}
		if f.entryOf(two.ID) != nil {
			t.Error("a refused owner left a lock state")
		}
		f.mustLock(t, a, two, LockReq{Owner: owner(a, "first"), Ranges: []LockRange{x(0, 10)}})
	})
	// What a restart installs was granted before it: it is counted, and
	// never refused for a cap.
	t.Run("installed", func(t *testing.T) {
		f := newFixture(t)
		a := f.caller(t, smbMachine("pc-a"))
		for _, name := range []string{"one", "two"} {
			file, r := f.fileIn(t, name)
			r.Access = rd | wr
			o := f.mustOpen(t, a, file, keep(r, entity.DurabilityPersistent, s1, time.Hour, 1))
			f.mustLock(t, a, file, LockReq{Owner: smbOwner(a, o), Open: byID(o), Ranges: []LockRange{x(0, 10)}})
		}
		f.Stop()
		g := unstarted(t, f.fs, f.clock)
		for _, k := range []capped{cappedOpens, cappedOwners, cappedRanges} {
			g.lower(k, 1)
		}
		g.start(t, testShards)
		for _, k := range []capped{cappedOpens, cappedOwners, cappedRanges} {
			if n := g.held[heldKey{a.Client, k}]; n != 2 {
				t.Errorf("installed %d of kind %d, want both", n, k)
			}
		}
	})
	t.Run("waiters", func(t *testing.T) {
		f := newFixture(t)
		f.lower(cappedWaiters, 1)
		a, h := f.caller(t, nfs("a", 1, entity.NewUser())), f.caller(t, nfs("h", 1, entity.NewUser()))
		one, two := in(1), in(1)
		for _, file := range []FileKey{one, two} {
			f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}})
		}
		if _, err := f.lock(a, one, LockReq{Owner: owner(a, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitPoll}); lockedBy(err) != "h" {
			t.Fatalf("the first waiter: %v", err)
		}
		if _, err := f.lock(a, two, LockReq{Owner: owner(a, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitPoll}); delayedFor(err) != DelayResources {
			t.Fatalf("a waiter past the cap: %v, want ErrDelay (DelayResources)", err)
		}
		if err := f.step(two.ID, func(e *entry) error {
			if len(e.waiters) != 0 || len(e.lstates) != 1 {
				t.Errorf("a refused waiter left %d waiters and %d lock states", len(e.waiters), len(e.lstates))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Locks().CancelLock(ctx, a, one, owner(a, "w"), br(0, 10)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.lock(a, two, LockReq{Owner: owner(a, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitPoll}); lockedBy(err) != "h" {
			t.Errorf("a waiter once the first is cancelled: %v", err)
		}
	})
}

// A request refused for a cap changes nothing before the refusal: an open
// or a lock starts no recall of another client's lease.
func TestCappedRequestChangesNothing(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	f.mustOpen(t, b, in(1), OpenRequest{Access: rd | wr})
	leased := anyFile()
	f.grantOpen(t, a, leased, OpenRequest{Access: rd, Lease: LeaseKey{1}, Want: rh})

	f.lower(cappedOpens, 1)
	if _, err := f.open(b, leased, OpenRequest{Access: rd | wr}); delayedFor(err) != DelayResources {
		t.Fatalf("an open past the cap: %v", err)
	}
	if n := len(f.sink.takeRecalls()); n != 0 {
		t.Errorf("an open its cap refuses sent %d recalls", n)
	}

	for k, what := range map[capped]string{cappedRanges: "ranges", cappedOwners: "owners", cappedWaiters: "waiters"} {
		f.lower(k, 0)
		lock := LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}
		if _, err := f.lock(b, leased, lock); delayedFor(err) != DelayResources {
			t.Errorf("a lock past the %s cap: %v, want ErrDelay (DelayResources)", what, err)
		}
		f.lower(k, 10)
	}
	if n := len(f.sink.takeRecalls()); n != 0 {
		t.Errorf("a lock its cap refuses sent %d recalls", n)
	}
}

// A new open holds its place under its client's cap from the check on: an
// open of the client racing it past the check takes no place it counted on,
// so an open the cap refuses has closed no disconnected durable open its
// conflict broke.
func TestOpenCapHoldsItsPlaceAcrossABreak(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
	file, r := f.fileIn(t, "disk")
	r.Access, r.Deny, r.Lease, r.Want = rd, wr, LeaseKey{1}, rh
	o := f.mustOpen(t, a, file, keep(r, entity.DurabilityDurable, s1, time.Hour, 1))
	f.disconnect(t, a, s1)
	f.mustOpen(t, b, in(1), OpenRequest{Access: rd}) // b's shard listed, so its next opens write nothing
	f.lower(cappedOpens, 2)
	rnd := &blockingRand{entered: make(chan struct{}), released: make(chan struct{}), r: newSeeded(t.Name())}
	f.d.Rand = rnd
	rnd.armed.Store(true)
	opened := make(chan error)
	go func() {
		_, err := f.open(b, file, OpenRequest{Access: wr})
		opened <- err
	}()
	<-rnd.entered // past the cap check, minting its reference
	_, other := f.open(b, in(1), OpenRequest{Access: rd})
	close(rnd.released)
	err := <-opened
	if err != nil {
		if _, rerr := f.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}, Lease: LeaseKey{1}}, s2); rerr != nil {
			t.Errorf("an open the cap refused (%v) closed a's durable open first: reconnect %v", err, rerr)
		}
	}
	if (err == nil) == (other == nil) {
		t.Errorf("the open %v and the open racing it %v: want one refused for the cap", err, other)
	}
}

// A lock holds its places under its client's caps from the check on: a lock
// of the client racing it past the check takes no range, owner or waiter
// place it counted on, so a lock a cap refuses has closed no disconnected
// durable open its recall broke.
func TestLockCapHoldsItsPlaceAcrossARecall(t *testing.T) {
	for name, kind := range map[string]capped{"ranges": cappedRanges, "owners": cappedOwners, "waiters": cappedWaiters} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a, b := f.caller(t, smbMachine("pc-a")), f.caller(t, smbMachine("pc-b"))
			c := f.caller(t, nfs("c", 1, entity.NewUser()))
			file, r := f.fileIn(t, "disk")
			other := in(1)
			lock := LockReq{Owner: owner(b, "new"), Ranges: []LockRange{x(0, 10)}}
			racing := LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(20, 10)}}
			switch kind {
			case cappedOwners:
				racing.Owner = owner(b, "racing")
			case cappedWaiters:
				// the recall of d's lease leaves the lock waiting, and c's
				// lock the racing one
				d := f.caller(t, smbMachine("pc-d"))
				dr := r
				dr.Access, dr.Lease, dr.Want = rd, LeaseKey{2}, rh
				f.mustOpen(t, d, file, dr)
				f.mustLock(t, c, other, LockReq{Owner: owner(c, "c"), Ranges: []LockRange{x(20, 10)}})
				lock.Wait, racing.Wait, racing.Owner = WaitCallback, WaitCallback, owner(b, "racing")
			}
			r.Access, r.Lease, r.Want = rd, LeaseKey{1}, rh
			o := f.mustOpen(t, a, file, keep(r, entity.DurabilityDurable, s1, time.Hour, 1))
			f.disconnect(t, a, s1)
			f.mustLock(t, b, other, LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}})
			f.lower(kind, map[capped]int{cappedRanges: 2, cappedOwners: 2, cappedWaiters: 1}[kind])
			rnd := &blockingRand{entered: make(chan struct{}), released: make(chan struct{}), r: newSeeded(t.Name())}
			f.d.Rand = rnd
			rnd.armed.Store(true)
			locked := make(chan error)
			go func() {
				_, err := f.lock(b, file, lock)
				locked <- err
			}()
			<-rnd.entered // past the cap check and the recall, minting its lock state
			_, rerr := f.lock(b, other, racing)
			close(rnd.released)
			err := <-locked
			if err != nil {
				if _, cerr := f.reconnect(a, o, ReconnectMatch{Share: file.Share, Create: [16]byte{1}, Lease: LeaseKey{1}}, s2); cerr != nil {
					t.Errorf("a lock the cap refused (%v) closed a's durable open first: reconnect %v", err, cerr)
				}
			}
			if delayedFor(rerr) != DelayResources {
				t.Errorf("the lock racing past the check: %v, want ErrDelay (DelayResources)", rerr)
			}
		})
	}
}

// A lock's own ranges are counted once by the time it serves the file's
// waiters: a waiter of the same client that fits the range cap with the
// lock's ranges added is granted, not refused as if they counted twice.
func TestLockServesItsOwnClientsWaiterWithinTheCap(t *testing.T) {
	f := newFixture(t)
	h := f.host(t, "h", 1)
	file := anyFile()
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "a"), Ranges: []LockRange{x(0, 30)}})
	if res := f.mustLock(t, h, file, LockReq{Owner: owner(h, "b"), Ranges: []LockRange{sh(10, 10)}, Wait: WaitCallback}); !res.Waiting {
		t.Fatalf("b's lock: want waiting")
	}
	f.lower(cappedRanges, 4)
	// a's shared middle splits x(0,30) into three ranges, and b's grant is the fourth
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "a"), Ranges: []LockRange{sh(10, 10)}})
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "b" {
		t.Errorf("granted %v, want b", got)
	}
}

// A lock's owner keeps its place under the owner cap from the check on: the
// owner's last lock state elsewhere dropping before the lock mints its own
// frees no place another owner of the client can take, so the client never
// holds more owners than its cap.
func TestLockOwnerKeepsItsPlaceWhileItsLastStateDrops(t *testing.T) {
	f := newFixture(t)
	b := f.host(t, "b", 1)
	file, other, keeper := anyFile(), anyFile(), anyFile()
	f.mustLock(t, b, keeper, LockReq{Owner: owner(b, "k"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, b, other, LockReq{Owner: owner(b, "o"), Ranges: []LockRange{x(0, 10)}})
	f.lower(cappedOwners, 2)
	rnd := &blockingRand{entered: make(chan struct{}), released: make(chan struct{}), r: newSeeded(t.Name())}
	f.d.Rand = rnd
	rnd.armed.Store(true)
	locked := make(chan error)
	go func() {
		_, err := f.lock(b, file, LockReq{Owner: owner(b, "o"), Ranges: []LockRange{x(0, 10)}})
		locked <- err
	}()
	<-rnd.entered // past the cap check, minting o's lock state on file
	f.unlock(t, b, other, owner(b, "o"), br(0, 10))
	_, perr := f.lock(b, other, LockReq{Owner: owner(b, "p"), Ranges: []LockRange{x(0, 10)}})
	close(rnd.released)
	err := <-locked
	f.idx.Lock()
	n := f.held[heldKey{b.Client, cappedOwners}]
	f.idx.Unlock()
	if n > 2 {
		t.Errorf("o's lock: %v, p's lock: %v; the client holds %d owners past its cap of 2", err, perr, n)
	}
}

// A callback waiter whose grant would take its client past the range cap is
// answered not granted when its range frees: it holds nothing, and the
// waiter behind it is considered and granted.
func TestCallbackWaiterPastTheRangeCapIsAnsweredNotGranted(t *testing.T) {
	f := newFixture(t)
	a, h, n := f.host(t, "a", 1), f.host(t, "h", 1), f.host(t, "n", 1)
	file, other := anyFile(), anyFile()
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, other, LockReq{Owner: owner(h, "held"), Ranges: []LockRange{x(0, 10)}})
	for _, w := range []struct {
		who  Caller
		name string
	}{{h, "w"}, {n, "n"}} {
		if res := f.mustLock(t, w.who, file, LockReq{Owner: owner(w.who, w.name), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); !res.Waiting {
			t.Fatalf("%s's lock: want waiting", w.name)
		}
	}
	f.lower(cappedRanges, 1) // h holds its one range on other
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	ws := f.sink.takeWaits()
	if len(ws) != 2 || string(ws[0].w.Owner.Name) != "w" || ws[0].granted || string(ws[1].w.Owner.Name) != "n" || !ws[1].granted {
		t.Fatalf("answers %+v, want w not granted, then n granted", ws)
	}
	if l := f.holder(t, file, br(0, 10)); l == nil || l.Owner.Client != n.Client {
		t.Errorf("after w was refused: holder %+v, want n", l)
	}
	if err := f.step(file.ID, func(e *entry) error {
		for _, ls := range e.lstates {
			if ls.owner.Client == h.Client {
				t.Errorf("w was answered not granted, yet holds %v", ls.locks)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
