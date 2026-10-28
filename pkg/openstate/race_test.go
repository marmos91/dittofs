package openstate

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// admission is a per-file mutex, the service's admission lock as Order
// takes it.
type admission struct{ m sync.Map }

func (a *admission) lock(f entity.FileID) func() {
	mu, _ := a.m.LoadOrStore(f, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// watchdog fails t with every goroutine's stack if fn has not returned
// within d.
func watchdog(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		t.Fatalf("still running after %v:\n%s", d, buf[:runtime.Stack(buf, true)])
	}
}

// A check and the I/O it admits are never separated by a grant, on a file
// with state or none, and a file never has two entries. 10,000 rounds race
// a deny-write open against anonymous writes on a file whose entry the round
// before freed, each holding the file's admission lock as the service does,
// the write from its check to its staging, while reads that take none free
// and make the entry again: a write is refused, or staged before the open
// is granted. Then a
// timer step that frees a range a callback waiter waits on runs while a
// write admitted on that range holds the file's admission lock: the waiter
// is granted only once the write is staged.
func TestCheckAndIONeverSeparated(t *testing.T) {
	fs := newFixture(t).fs
	f := unstarted(t, fs, &fakeClock{})
	var adm admission
	if err := f.Start(ctx, f.sink, adm.lock); err != nil {
		t.Fatal(err)
	}
	for _, s := range testShards {
		if err := f.Recovery().ServeShard(ctx, fs.Share.ID, s, instances.Add(1)); err != nil {
			t.Fatal(err)
		}
	}
	a, b := f.caller(t, smb("pc-a")), f.caller(t, nfs("b", 1, entity.NewUser()))

	scratch := in(1)
	for round := range 10_000 {
		var opened atomic.Bool
		var oerr error
		var o Open
		var wg sync.WaitGroup
		anon := OpenRef{Kind: RefAnonymous, Handle: handle(scratch.ID)}
		wg.Go(func() {
			unlock := adm.lock(scratch.ID)
			defer unlock()
			o, oerr = f.open(b, scratch, OpenRequest{Access: rd, Deny: wr})
			opened.Store(true)
		})
		wg.Go(func() {
			for done := false; !done; {
				unlock := adm.lock(scratch.ID)
				after := opened.Load()
				if f.Checks().CheckIO(ctx, Caller{}, scratch, anon, br(0, 1), true) == nil {
					runtime.Gosched() // the write, staged under the same lock
					if !after && opened.Load() {
						t.Errorf("round %d: an open was granted between a write's check and its staging", round)
					}
					if after {
						t.Errorf("round %d: a write begun after a deny-write open was granted was admitted", round)
					}
				}
				unlock()
				done = after
			}
		})
		wg.Go(func() {
			for range 2 {
				_ = f.Checks().CheckIO(ctx, Caller{}, scratch, anon, br(0, 1), false)
			}
		})
		wg.Wait()
		if oerr != nil {
			t.Fatalf("round %d: the deny-write open: %v", round, oerr)
		}
		f.close(t, b, o)
	}

	// A callback waiter behind a poll waiter's reservation, which the timer
	// lets lapse.
	file := in(1)
	var opens [3]Open
	for i := range opens {
		opens[i] = f.mustOpen(t, a, file, OpenRequest{Access: rd | wr})
	}
	lockReq := func(i int, w WaitKind) LockReq {
		return LockReq{Owner: smbOwner(a, opens[i]), Open: byID(opens[i]), Ranges: []LockRange{x(0, 10)}, Wait: w}
	}
	f.mustLock(t, a, file, lockReq(0, WaitNone))
	if _, err := f.lock(a, file, lockReq(1, WaitPoll)); !errors.Is(err, ErrLocked) {
		t.Fatalf("poll waiter: %v", err)
	}
	if res, err := f.lock(a, file, lockReq(2, WaitCallback)); err != nil || !res.Waiting {
		t.Fatalf("callback waiter: %+v, %v", res, err)
	}
	if _, err := f.Locks().Unlock(ctx, a, file, byID(opens[0]), smbOwner(a, opens[0]), []ByteRange{br(0, 10)}, Seqs{}, nil); err != nil {
		t.Fatal(err)
	}
	f.clock.mu.Lock()
	f.clock.now += leasePeriod
	f.clock.mu.Unlock()
	granted := make(chan struct{})
	f.sink.onWait = func(LockWaiter) { close(granted) }
	unlock := adm.lock(file.ID)
	if err := f.Checks().CheckIO(ctx, Caller{}, file, OpenRef{Kind: RefAnonymous, Handle: handle(file.ID)}, br(0, 10), true); err != nil {
		t.Fatalf("the write before the timer: %v", err)
	}
	go f.serveFiles(ctx, f.sink, []entity.FileID{file.ID})
	select {
	case <-granted:
		t.Error("the waiter was granted between a write's check and its staging")
	case <-time.After(100 * time.Millisecond): // the write's I/O, staged under the admission lock
	}
	unlock()
	<-granted
}

// A release across files never holds an index while it waits for an entry:
// an expiry of a client holding 10,000 deny-write opens completes while 64
// openers open the same files, half of them for write against the deny
// mode, and the indexes are still what the entries derive.
func TestExpiryRacesOpeners(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	files := make([]FileKey, 10_000)
	for i := range files {
		files[i] = in(1)
		f.mustOpen(t, a, files[i], OpenRequest{Access: rd, Deny: wr})
	}
	var others [64]Caller
	for i := range others {
		others[i] = f.caller(t, nfs(fmt.Sprint("opener", i), 1, entity.NewUser()))
	}
	watchdog(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		for k, who := range others {
			wg.Go(func() {
				// Every other opener meets the deny mode until the expiry
				// releases it.
				access := rd
				if k%2 == 1 {
					access |= wr
				}
				for i := k; i < len(files); i += len(others) {
					if _, err := f.open(who, files[i], OpenRequest{Access: access}); err != nil && !errors.Is(err, ErrShareViolation) {
						t.Errorf("open: %v", err)
					}
				}
			})
		}
		wg.Go(func() {
			if err := f.Expire(ctx, a.Client); err != nil {
				t.Errorf("Expire: %v", err)
			}
		})
		wg.Wait()
	})
	if err := f.check(); err != nil {
		t.Error(err)
	}
	if f.holdsAny(a.Client) {
		t.Error("the expired client still holds state")
	}
}
