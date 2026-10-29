package openstate

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// lower sets the cap of kind k to n for one case.
func lower(t *testing.T, k capped, n int) {
	was := perClient[k]
	perClient[k] = n
	t.Cleanup(func() { perClient[k] = was })
}

// A client's opens, lock ranges, lock owners and waiters are each capped
// here: a request past a cap is ErrDelay (DelayResources) and changes
// nothing, and what a close or an unlock frees is room again. An unlock
// splitting a range adds one, so at the cap it is refused as a lock is.
func TestPerClientCaps(t *testing.T) {
	t.Run("opens", func(t *testing.T) {
		lower(t, cappedOpens, 2)
		f := newFixture(t)
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
		lower(t, cappedRanges, 2)
		f := newFixture(t)
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
		lower(t, cappedOwners, 1)
		f := newFixture(t)
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
		lower(t, cappedOpens, 1)
		lower(t, cappedOwners, 1)
		lower(t, cappedRanges, 1)
		g := f.restart(t)
		for _, k := range []capped{cappedOpens, cappedOwners, cappedRanges} {
			if n := g.held[heldKey{a.Client, k}]; n != 2 {
				t.Errorf("installed %d of kind %d, want both", n, k)
			}
		}
	})
	t.Run("waiters", func(t *testing.T) {
		lower(t, cappedWaiters, 1)
		f := newFixture(t)
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
		f.step(two.ID, func(e *entry) error {
			if len(e.waiters) != 0 || len(e.lstates) != 1 {
				t.Errorf("a refused waiter left %d waiters and %d lock states", len(e.waiters), len(e.lstates))
			}
			return nil
		})
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

	lower(t, cappedOpens, 1)
	if _, err := f.open(b, leased, OpenRequest{Access: rd | wr}); delayedFor(err) != DelayResources {
		t.Fatalf("an open past the cap: %v", err)
	}
	if n := len(f.sink.takeRecalls()); n != 0 {
		t.Errorf("an open its cap refuses sent %d recalls", n)
	}

	for k, what := range map[capped]string{cappedRanges: "ranges", cappedOwners: "owners", cappedWaiters: "waiters"} {
		lower(t, k, 0)
		lock := LockReq{Owner: owner(b, "lo"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}
		if _, err := f.lock(b, leased, lock); delayedFor(err) != DelayResources {
			t.Errorf("a lock past the %s cap: %v, want ErrDelay (DelayResources)", what, err)
		}
		lower(t, k, 10)
	}
	if n := len(f.sink.takeRecalls()); n != 0 {
		t.Errorf("a lock its cap refuses sent %d recalls", n)
	}
}
