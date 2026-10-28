package openstate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// host connects an NLM host in one step, as its first call does, and answers
// a caller of it.
func (f *fixture) host(t *testing.T, name string, boot byte) Caller {
	t.Helper()
	id, err := f.Connect(ctx, Client{ClientRecord: entity.ClientRecord{
		Protocol: entity.ProtocolNLM, Name: []byte(name), Boot: [8]byte{boot}, Recovery: entity.RecoverNotice,
	}}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return Caller{Client: id, Principal: entity.NewUser()}
}

func br(off, n uint64) ByteRange { return ByteRange{Offset: off, Length: n} }
func x(off, n uint64) LockRange  { return LockRange{br(off, n), true} }
func sh(off, n uint64) LockRange { return LockRange{br(off, n), false} }

func owner(who Caller, name string) LockOwner {
	return LockOwner{Client: who.Client, Name: []byte(name)}
}

// smbOwner is the stacking owner an SMB open is.
func smbOwner(who Caller, o Open) LockOwner {
	return LockOwner{Client: who.Client, Name: binary.BigEndian.AppendUint64(nil, uint64(o.ID)), Stack: true}
}

// lock asks for r on file, under no open where r names none.
func (f *fixture) lock(who Caller, file FileKey, r LockReq) (LockResult, error) {
	if file.Shard == 0 {
		file.Shard = 1
	}
	if r.Open.Kind == 0 {
		r.Open = none
	}
	return f.Locks().Lock(ctx, who, file, r)
}

// none is a reference to no open, as a lock request under none carries.
var none = OpenRef{Kind: RefAnonymous}

func (f *fixture) mustLock(t *testing.T, who Caller, file FileKey, r LockReq) LockResult {
	t.Helper()
	res, err := f.lock(who, file, r)
	if err != nil {
		t.Fatalf("Lock %s %v: %v", r.Owner.Name, r.Ranges, err)
	}
	return res
}

func (f *fixture) unlock(t *testing.T, who Caller, file FileKey, o LockOwner, rs ...ByteRange) {
	t.Helper()
	if _, err := f.Locks().Unlock(ctx, who, file, none, o, rs, Seqs{}); err != nil {
		t.Fatalf("Unlock %s %v: %v", o.Name, rs, err)
	}
}

// holder is the lock that refuses an exclusive lock of r by an owner holding
// nothing, or nil.
func (f *fixture) holder(t *testing.T, file FileKey, r ByteRange) *Lock {
	t.Helper()
	h, err := f.Locks().TestLock(ctx, Caller{}, file, LockOwner{Name: []byte("tester")}, r, true)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// granted is the owner names of the waiters a LockWaitOver granted, in order,
// and fails the test on one not granted.
func granted(t *testing.T, ws []waitOverCall) []string {
	t.Helper()
	var names []string
	for _, w := range ws {
		if !w.granted {
			t.Errorf("LockWaitOver(granted=false) for %s", w.w.Owner.Name)
		}
		names = append(names, string(w.w.Owner.Name))
	}
	return names
}

func lockedBy(err error) string {
	var le *LockedError
	if !errors.As(err, &le) {
		return "not a conflict: " + errorString(err)
	}
	return string(le.Holder.Owner.Name)
}

func errorString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// An owner's own locks never conflict and its overlapping ranges merge, while
// two owners of one client conflict, the refusal naming the holder. An NLM
// lock is held with no open and refuses an NFSv4 lock; a merging owner's
// unlock of a range it does not hold succeeds; a host's restart releases its
// locks at once.
func TestLockOwners(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr})
	lo1, lo2 := owner(a, "lo1"), owner(a, "lo2")
	under := refOf(o)

	f.mustLock(t, a, file, LockReq{Owner: lo1, Open: under, Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, a, file, LockReq{Owner: lo1, Open: under, Ranges: []LockRange{x(5, 15)}})
	_, err := f.lock(a, file, LockReq{Owner: lo2, Open: under, Ranges: []LockRange{x(15, 1)}})
	var le *LockedError
	if !errors.As(err, &le) || string(le.Holder.Owner.Name) != "lo1" || le.Holder.Range != x(0, 20) || le.Holder.Open != o.Ref.Token {
		t.Fatalf("a second owner of the client: %v %+v, want refused by lo1's merged [0, 20) under the open", err, le)
	}

	h := f.host(t, "nlm-host", 1)
	name := []byte("svid")
	f.mustLock(t, h, file, LockReq{Owner: LockOwner{Client: h.Client, Name: name}, Ranges: []LockRange{x(100, 10)}})
	copy(name, "XXXX") // the caller's buffer is its own again once Lock returns
	if _, err := f.lock(a, file, LockReq{Owner: lo1, Open: under, Ranges: []LockRange{sh(105, 1)}}); lockedBy(err) != "svid" {
		t.Errorf("an NFSv4 lock over an NLM lock: %s, want refused by the host's owner", lockedBy(err))
	}
	if _, err := f.Locks().Unlock(ctx, a, file, under, lo2, []ByteRange{br(500, 10)}, Seqs{}); err != nil {
		t.Errorf("a merging owner's unlock of a range it does not hold: %v, want success", err)
	}

	f.host(t, "nlm-host", 2)
	f.mustLock(t, a, file, LockReq{Owner: lo1, Open: under, Ranges: []LockRange{x(100, 10)}})
}

// An SMB lock list is granted all or none, at most 64 ranges, and a list of
// several never waits; a stacking owner's exclusive lock conflicts with its
// own, and a shared one stacks on it; an unlock list stops at the first range
// not held, keeping what it released before it.
func TestSMBLockLists(t *testing.T) {
	f := newFixture(t)
	s, r := f.caller(t, smb("pc-s")), f.caller(t, smb("pc-r"))
	file := anyFile()
	os, or := f.mustOpen(t, s, file, OpenRequest{Access: rd | wr}), f.mustOpen(t, r, file, OpenRequest{Access: rd | wr})
	so, ro := smbOwner(s, os), smbOwner(r, or)
	byS, byR := refOf(os), refOf(or)
	if _, err := f.Locks().Unlock(ctx, s, file, byS, so, []ByteRange{br(0, 1)}, Seqs{}); !errors.Is(err, ErrRangeNotLocked) {
		t.Errorf("an unlock by an owner holding nothing: %v, want ErrRangeNotLocked", err)
	}
	f.mustLock(t, r, file, LockReq{Owner: ro, Open: byR, Ranges: []LockRange{x(20, 10)}})

	if _, err := f.lock(s, file, LockReq{Owner: so, Open: byS, Ranges: []LockRange{x(0, 5), x(10, 5), x(25, 1)}}); !errors.Is(err, ErrLocked) {
		t.Fatalf("a list whose third range conflicts: %v, want ErrLocked", err)
	}
	if h := f.holder(t, file, br(0, 15)); h != nil {
		t.Errorf("a refused list left %v held", h.Range)
	}
	if _, err := f.lock(s, file, LockReq{Owner: so, Open: byS, Ranges: make([]LockRange, 65)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a 65-range list: %v, want ErrInvalid", err)
	}
	if _, err := f.Locks().Unlock(ctx, s, file, byS, so, make([]ByteRange, 65), Seqs{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a 65-range unlock list: %v, want ErrInvalid", err)
	}
	long := LockOwner{Client: s.Client, Name: make([]byte, 1025)}
	if _, err := f.lock(s, file, LockReq{Owner: long, Ranges: []LockRange{x(0, 1)}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an owner past 1,024 bytes: %v, want ErrInvalid", err)
	}
	res, err := f.lock(s, file, LockReq{Owner: so, Open: byS, Ranges: []LockRange{x(0, 1), x(25, 1)}, Wait: WaitCallback})
	if !errors.Is(err, ErrLocked) || res.Waiting {
		t.Errorf("a two-range list asking to wait: %+v, %v; want refused, not waiting", res, err)
	}
	if _, err := f.Locks().Unlock(ctx, r, file, byR, ro, []ByteRange{br(20, 10)}, Seqs{}); err != nil {
		t.Fatal(err)
	}
	if ws := f.sink.takeWaits(); len(ws) != 0 {
		t.Errorf("a two-range list left a waiter, reported %v", ws)
	}

	f.mustLock(t, s, file, LockReq{Owner: so, Open: byS, Ranges: []LockRange{x(0, 10)}})
	if _, err := f.lock(s, file, LockReq{Owner: so, Open: byS, Ranges: []LockRange{x(0, 10)}}); !errors.Is(err, ErrLocked) {
		t.Errorf("a stacking owner's exclusive lock over its own: %v, want ErrLocked", err)
	}
	f.mustLock(t, s, file, LockReq{Owner: so, Open: byS, Ranges: []LockRange{sh(0, 10)}})

	f.mustLock(t, s, file, LockReq{Owner: so, Open: byS, Ranges: []LockRange{x(40, 1), x(60, 1)}})
	_, err = f.Locks().Unlock(ctx, s, file, byS, so, []ByteRange{br(40, 1), br(50, 1), br(60, 1)}, Seqs{})
	if !errors.Is(err, ErrRangeNotLocked) {
		t.Fatalf("an unlock list whose second range is not held: %v, want ErrRangeNotLocked", err)
	}
	if f.holder(t, file, br(40, 1)) != nil || f.holder(t, file, br(60, 1)) == nil {
		t.Error("after the refused unlock list: want the first range released and the third held")
	}
}

// A range's last byte is offset + length - 1, a zero-length one's included,
// so a zero-length lock at X conflicts only with a range holding both X-1 and
// X, and one at 0 with nothing; a range to the last byte never overflows.
func TestZeroLengthLocks(t *testing.T) {
	f := newFixture(t)
	s := f.caller(t, smb("pc"))
	try := func(file FileKey, r LockRange) error {
		o := f.mustOpen(t, s, file, OpenRequest{Access: rd | wr})
		_, err := f.lock(s, file, LockReq{Owner: smbOwner(s, o), Open: refOf(o), Ranges: []LockRange{r}})
		return err
	}
	file := anyFile()
	if err := try(file, x(10, 10)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at   uint64
		want error
	}{{15, ErrLocked}, {10, nil}, {20, nil}} {
		if err := try(file, x(c.at, 0)); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("a zero-length lock at %d beside [10, 20): %v, want %v", c.at, err, c.want)
		}
	}
	whole := anyFile()
	if err := try(whole, x(0, ^uint64(0))); err != nil {
		t.Fatal(err)
	}
	if err := try(whole, x(0, 0)); err != nil {
		t.Errorf("a zero-length lock at 0 beside the whole file: %v", err)
	}

	zero := anyFile()
	if err := try(zero, x(15, 0)); err != nil {
		t.Fatal(err)
	}
	if err := try(zero, x(14, 2)); !errors.Is(err, ErrLocked) {
		t.Errorf("[14, 16) beside a zero-length lock at 15: %v, want ErrLocked", err)
	}
	if err := try(zero, x(15, 1)); err != nil {
		t.Errorf("[15, 16) beside a zero-length lock at 15: %v", err)
	}

	h, n := f.host(t, "h", 1), f.caller(t, nfs("host-n", 1, entity.NewUser()))
	last := anyFile()
	f.mustLock(t, h, last, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(^uint64(0)-1, ^uint64(0))}})
	o := f.mustOpen(t, n, last, OpenRequest{Access: rd | wr})
	if _, err := f.lock(n, last, LockReq{Owner: owner(n, "lo"), Open: refOf(o), Ranges: []LockRange{x(^uint64(0), 1)}}); !errors.Is(err, ErrLocked) {
		t.Errorf("the last byte beside a lock reaching it: %v, want ErrLocked", err)
	}
}

// An SMB lock is mandatory: an exclusive one refuses every other open's and
// anonymous I/O across it and admits its own open's; a shared one refuses
// every write, its own open's too. An NFS lock is advisory and refuses no I/O.
func TestMandatoryLocksRefuseIO(t *testing.T) {
	f := newFixture(t)
	s, n := f.caller(t, smb("pc")), f.caller(t, nfs("host-n", 1, entity.NewUser()))
	file := anyFile()
	os := f.mustOpen(t, s, file, OpenRequest{Access: rd | wr})
	on := f.mustOpen(t, n, file, OpenRequest{Access: rd | wr})
	f.mustLock(t, s, file, LockReq{Owner: smbOwner(s, os), Open: refOf(os), Ranges: []LockRange{x(0, 10), sh(20, 10)}})
	f.mustLock(t, n, file, LockReq{Owner: owner(n, "lo"), Open: refOf(on), Ranges: []LockRange{x(100, 10)}})

	io := func(who Caller, o OpenRef, off uint64, write bool) error {
		o.Handle = handle(file.ID)
		return f.Checks().CheckIO(ctx, who, o, br(off, 1), write)
	}
	for _, c := range []struct {
		what  string
		who   Caller
		o     OpenRef
		off   uint64
		write bool
		want  error
	}{
		{"an anonymous read under the exclusive lock", Caller{}, none, 5, false, ErrLocked},
		{"an NFSv4 open's write under the exclusive lock", n, refOf(on), 5, true, ErrLocked},
		{"the holding open's write under its exclusive lock", s, refOf(os), 5, true, nil},
		{"the holding open's read under its exclusive lock", s, refOf(os), 5, false, nil},
		{"the holding open's write under its shared lock", s, refOf(os), 25, true, ErrLocked},
		{"an anonymous read under the shared lock", Caller{}, none, 25, false, nil},
		{"an SMB write under an NFS lock", s, refOf(os), 105, true, nil},
	} {
		if err := io(c.who, c.o, c.off, c.write); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("%s: %v, want %v", c.what, err, c.want)
		}
	}
}

// Waiters are served first in, first out, and a waiter bars a request that
// conflicts with it from the locks that block it, so shared locks cannot
// starve an exclusive waiter. No LockWaitOver runs under a table lock: a Sink
// that calls back into the file's table deadlocks nothing.
func TestWaitersFirstInFirstOut(t *testing.T) {
	f := newFixture(t)
	file := anyFile()
	f.sink.onWait = func(LockWaiter) { _, _ = f.Locks().TestLock(ctx, Caller{}, file, LockOwner{}, br(0, 1), true) }
	all := br(0, 100)
	hs := map[string]Caller{}
	for _, n := range []string{"h", "a", "b", "c", "d"} {
		hs[n] = f.host(t, n, 1)
	}
	req := func(n string, exclusive bool, wait WaitKind) LockReq {
		return LockReq{Owner: owner(hs[n], n), Ranges: []LockRange{{all, exclusive}}, Wait: wait}
	}
	f.mustLock(t, hs["h"], file, req("h", false, WaitNone))
	for _, n := range []string{"a", "b", "c"} {
		if res := f.mustLock(t, hs[n], file, req(n, n != "b", WaitCallback)); !res.Waiting {
			t.Fatalf("%s: answered %+v, want Waiting", n, res)
		}
	}
	shared := func(when string) {
		t.Helper()
		if _, err := f.lock(hs["d"], file, req("d", false, WaitNone)); !errors.Is(err, ErrLocked) {
			t.Errorf("a shared lock while an exclusive request waits, %s: %v, want ErrLocked", when, err)
		}
	}
	shared("behind a shared holder")
	for i, n := range []string{"h", "a", "b"} {
		f.unlock(t, hs[n], file, owner(hs[n], n), all)
		want := []string{"a", "b", "c"}[i]
		if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != want {
			t.Fatalf("after %s unlocked: granted %v, want %s alone", n, got, want)
		}
		if n == "a" {
			shared("behind a shared waiter's grant")
		}
	}
}

// A waiter is barred only where the locks blocking it are: an owner holding
// byte 0 blocks a whole-file exclusive waiter of another owner of its client,
// another client still locks a range neither touches, and the blocking owner
// still changes its own lock.
func TestWaiterBarsOnlyItsBlockers(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "host-b", 1)
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr})
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a1"), Open: refOf(o), Ranges: []LockRange{sh(0, 1)}})
	if _, err := f.lock(a, file, LockReq{Owner: owner(a, "a2"), Open: refOf(o), Ranges: []LockRange{x(0, ^uint64(0))}, Wait: WaitPoll}); !errors.Is(err, ErrLocked) {
		t.Fatalf("the whole-file waiter: %v, want ErrLocked", err)
	}
	f.mustLock(t, b, file, LockReq{Owner: owner(b, "b"), Ranges: []LockRange{x(4096, 4096)}})
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a1"), Open: refOf(o), Ranges: []LockRange{x(0, 1)}})
}

// A poll that repeats the request it waits with keeps its place and its
// wait, and a lock that turns an owner's exclusive lock shared grants the
// shared waiters it frees.
func TestWaiterPlace(t *testing.T) {
	f := newFixture(t)
	h, p1, p2 := f.host(t, "h", 1), f.host(t, "p1", 1), f.host(t, "p2", 1)
	file := anyFile()
	rg := []LockRange{x(0, 10)}
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: rg})
	for i, p := range []Caller{p1, p2, p2, p1} {
		if i == 2 {
			f.clock.advance(leasePeriod / 2)
		}
		if _, err := f.lock(p, file, LockReq{Owner: owner(p, "p"), Ranges: rg, Wait: WaitPoll}); !errors.Is(err, ErrLocked) {
			t.Fatalf("a poll: %v, want ErrLocked", err)
		}
	}
	f.clock.advance(leasePeriod/2 + 1)
	f.serveFiles(f.sink, slices.Collect(maps.Keys(f.polls)))
	f.unlock(t, h, file, owner(h, "h"), br(0, 10))
	if _, err := f.lock(p2, file, LockReq{Owner: owner(p2, "p"), Ranges: rg, Wait: WaitPoll}); !errors.Is(err, ErrLocked) {
		t.Errorf("the later poller after the range freed: %v, want refused", err)
	}
	f.mustLock(t, p1, file, LockReq{Owner: owner(p1, "p"), Ranges: rg, Wait: WaitPoll})

	other, g := anyFile(), f.host(t, "g", 1)
	f.mustLock(t, g, other, LockReq{Owner: owner(g, "g"), Ranges: rg})
	f.mustLock(t, p2, other, LockReq{Owner: owner(p2, "p"), Ranges: []LockRange{sh(0, 10)}, Wait: WaitCallback})
	f.mustLock(t, g, other, LockReq{Owner: owner(g, "g"), Ranges: []LockRange{sh(0, 10)}})
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "p" {
		t.Errorf("after the holder's lock turned shared: granted %v, want the shared waiter", got)
	}
}

// One owner holds one waiter per file: its next request replaces it, and a
// replaced callback waiter is reported not granted. A granted waiter the
// client refuses is released as an unlock would, and the next waiter is
// granted.
func TestWaiterReplacedAndRefused(t *testing.T) {
	f := newFixture(t)
	h, w, v := f.host(t, "h", 1), f.host(t, "w", 1), f.host(t, "v", 1)
	file := anyFile()
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 100)}})
	f.mustLock(t, w, file, LockReq{Owner: owner(w, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.mustLock(t, w, file, LockReq{Owner: owner(w, "w"), Ranges: []LockRange{x(20, 10)}, Wait: WaitCallback})
	if ws := f.sink.takeWaits(); len(ws) != 1 || ws[0].granted || ws[0].w.Range != x(0, 10) {
		t.Fatalf("the replaced waiter: reported %+v, want [0, 10) not granted", ws)
	}
	f.mustLock(t, v, file, LockReq{Owner: owner(v, "v"), Ranges: []LockRange{x(20, 10)}, Wait: WaitCallback})

	f.unlock(t, h, file, owner(h, "h"), br(0, 100))
	ws := f.sink.takeWaits()
	if got := granted(t, ws); len(got) != 1 || got[0] != "w" || ws[0].w.Range != x(20, 10) {
		t.Fatalf("after the holder unlocked: %+v, want w's [20, 30) alone granted", ws)
	}
	if err := f.Locks().WaitAnswered(ctx, w, ws[0].w, true); err != nil {
		t.Fatal(err)
	}
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "v" {
		t.Errorf("after w refused its grant: granted %v, want v", got)
	}
}

// A poll waiter's freed range is reserved for it, refusing everyone else,
// until it polls or a lease period passes; then the timer drops it and, taking
// the file's admission lock first, grants the callback waiter behind it.
func TestPollReservationLapses(t *testing.T) {
	f := newFixture(t)
	h, p, q, r := f.host(t, "h", 1), f.host(t, "p", 1), f.host(t, "q", 1), f.host(t, "r", 1)
	file := anyFile()
	rg := []LockRange{x(0, 10)}
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: rg})
	if _, err := f.lock(p, file, LockReq{Owner: owner(p, "p"), Ranges: rg, Wait: WaitPoll}); !errors.Is(err, ErrLocked) {
		t.Fatalf("the poll waiter: %v, want ErrLocked", err)
	}
	f.mustLock(t, q, file, LockReq{Owner: owner(q, "q"), Ranges: rg, Wait: WaitCallback})
	f.unlock(t, h, file, owner(h, "h"), br(0, 10))
	if ws := f.sink.takeWaits(); len(ws) != 0 {
		t.Fatalf("the range freed under a poll waiter granted %+v, want it reserved", ws)
	}
	if _, err := f.lock(r, file, LockReq{Owner: owner(r, "r"), Ranges: rg}); lockedBy(err) != "p" {
		t.Errorf("a lock of the reserved range: %s, want refused by the reservation", lockedBy(err))
	}

	f.clock.advance(leasePeriod)
	f.sweep(ctx)
	f.Stop() // waits for the steps the sweep started
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "q" {
		t.Errorf("a lease period after the reservation: granted %v, want q", got)
	}
	if len(f.polls) != 0 {
		t.Errorf("the timer still watches %d files with no poll waiter", len(f.polls))
	}
	f.order.mu.Lock()
	defer f.order.mu.Unlock()
	// The clock's advance fires the timer's own sweep beside the one called
	// here, so the file's lock may be taken twice; never another file's.
	if len(f.order.files) == 0 || slices.ContainsFunc(f.order.files, func(g entity.FileID) bool { return g != file.ID }) {
		t.Errorf("the timer took the admission lock of %v, want the file's alone", f.order.files)
	}
}

// A lease expiry on the timer that frees a range a callback waiter waits on
// grants the waiter only under the file's admission lock.
func TestExpiryGrantsWaitersUnderOrder(t *testing.T) {
	f := newFixture(t)
	a, h := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "h", 1)
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr})
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	if res := f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}); !res.Waiting {
		t.Fatal("the host's lock was not left waiting")
	}

	f.clock.advance(2 * leasePeriod)
	f.sweep(ctx)
	f.Stop() // waits for the steps the timer started
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "h" {
		t.Fatalf("after the expiry: granted %v, want h", got)
	}
	f.order.mu.Lock()
	defer f.order.mu.Unlock()
	if !slices.Contains(f.order.files, file.ID) {
		t.Errorf("the waiter was granted on the timer without the file's admission lock: taken for %v", f.order.files)
	}
}

// A range a lease expiry frees goes to the waiter on it, even when a request
// for it arrives after the expiry and before the timer serves the file.
func TestExpiryGrantIsNotJumped(t *testing.T) {
	f := newFixture(t)
	a, h, c := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "h", 1), f.host(t, "c", 1)
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr})
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	f.Table.order = func(entity.FileID) func() { entered <- struct{}{}; <-gate; return func() {} }
	f.clock.advance(2 * leasePeriod)
	f.sweep(ctx)
	<-entered // the timer's serve is parked on the admission lock
	_, err := f.lock(c, file, LockReq{Owner: owner(c, "c"), Ranges: []LockRange{x(0, 10)}})
	close(gate)
	f.Stop()
	if !errors.Is(err, ErrLocked) {
		t.Errorf("a lock arriving after h waited, in the expiry's window: %v, want ErrLocked", err)
	}
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "h" {
		t.Errorf("after the expiry: granted %v, want h", got)
	}
}

// A host resending its blocked request after a grant step granted it, while
// the callback is still on its way, keeps the lock when it then refuses that
// callback.
func TestResendAfterGrantKeepsTheLock(t *testing.T) {
	f := newFixture(t)
	a, h := f.host(t, "a", 1), f.host(t, "h", 1)
	file := anyFile()
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	wait := LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}
	f.mustLock(t, h, file, wait)
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	ws := f.sink.takeWaits() // h granted, its callback in flight
	if g := granted(t, ws); len(g) != 1 {
		t.Fatalf("after the unlock: granted %v, want h", g)
	}
	f.mustLock(t, h, file, wait) // the resend, answered granted
	if err := f.Locks().WaitAnswered(ctx, h, ws[0].w, true); err != nil {
		t.Fatal(err)
	}
	if f.holder(t, file, br(0, 10)) == nil {
		t.Errorf("h was answered granted, then lost the lock to its own refused callback")
	}
}

// A refusal of the first of two callback grants still outstanding releases
// the range it granted.
func TestRefusalOfTheFirstOfTwoGrantsReleasesIt(t *testing.T) {
	f := newFixture(t)
	a, b, h := f.host(t, "a", 1), f.host(t, "b", 1), f.host(t, "h", 1)
	file := anyFile()
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, b, file, LockReq{Owner: owner(b, "b"), Ranges: []LockRange{x(20, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	ws1 := f.sink.takeWaits()
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(20, 10)}, Wait: WaitCallback})
	f.unlock(t, b, file, owner(b, "b"), br(20, 10))
	_ = f.sink.takeWaits()
	if err := f.Locks().WaitAnswered(ctx, h, ws1[0].w, true); err != nil {
		t.Fatal(err)
	}
	if l := f.holder(t, file, br(0, 10)); l != nil {
		t.Errorf("refused grant of [0,10) is still held: %+v", *l)
	}
}

// A late callback the host refuses, after the owner unlocked the range it
// granted and locked it again, leaves the later lock whole.
func TestRefusedStaleCallbackSparesALaterLock(t *testing.T) {
	f := newFixture(t)
	a, h := f.host(t, "a", 1), f.host(t, "h", 1)
	file := anyFile()
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(100, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	ws := f.sink.takeWaits()
	f.unlock(t, h, file, owner(h, "h"), br(0, 10))
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 20)}})
	if err := f.Locks().WaitAnswered(ctx, h, ws[0].w, true); err != nil {
		t.Fatal(err)
	}
	if f.holder(t, file, br(0, 10)) == nil {
		t.Errorf("h's later lock of [0,20) lost [0,10) to a refusal of a callback it had already released")
	}
}

// A late refusal of a callback the owner released by unlocking leaves a
// later grant of the same range, which the host accepted, whole.
func TestRefusedStaleCallbackSparesALaterGrant(t *testing.T) {
	f := newFixture(t)
	a, c, h := f.host(t, "a", 1), f.host(t, "c", 1), f.host(t, "h", 1)
	file := anyFile()
	wait := LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback}
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(100, 10)}})
	f.mustLock(t, h, file, wait)
	f.unlock(t, a, file, owner(a, "a"), br(0, 10))
	stale := f.sink.takeWaits()
	f.unlock(t, h, file, owner(h, "h"), br(0, 10))
	f.mustLock(t, c, file, LockReq{Owner: owner(c, "c"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, h, file, wait)
	f.unlock(t, c, file, owner(c, "c"), br(0, 10))
	later := f.sink.takeWaits()
	if err := f.Locks().WaitAnswered(ctx, h, later[0].w, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Locks().WaitAnswered(ctx, h, stale[0].w, true); err != nil {
		t.Fatal(err)
	}
	if f.holder(t, file, br(0, 10)) == nil {
		t.Errorf("h lost the grant it accepted to a late refusal of one it had unlocked")
	}
}

// Closing an open, or its host's restart, ends the waiters under it, and the
// waiters behind the locks it held are granted.
func TestCloseAndRestartEndWaiters(t *testing.T) {
	f := newFixture(t)
	s, h, w, v, u := f.caller(t, smb("pc")), f.host(t, "h", 1), f.host(t, "w", 1), f.host(t, "v", 1), f.host(t, "u", 1)
	file := anyFile()
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}})
	o := f.mustOpen(t, s, file, OpenRequest{Access: rd | wr})
	so := smbOwner(s, o)
	f.mustLock(t, s, file, LockReq{Owner: so, Open: refOf(o), Ranges: []LockRange{x(20, 10)}})
	f.mustLock(t, s, file, LockReq{Owner: so, Open: refOf(o), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.mustLock(t, w, file, LockReq{Owner: owner(w, "w"), Ranges: []LockRange{x(40, 10)}})
	f.mustLock(t, w, file, LockReq{Owner: owner(w, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.mustLock(t, v, file, LockReq{Owner: owner(v, "v"), Ranges: []LockRange{x(20, 10)}, Wait: WaitCallback})
	f.mustLock(t, u, file, LockReq{Owner: owner(u, "u"), Ranges: []LockRange{x(40, 10)}, Wait: WaitCallback})

	f.close(t, s, o)
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "v" {
		t.Errorf("after the close: granted %v, want v", got)
	}
	f.host(t, "w", 2)
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "u" {
		t.Errorf("after the restart: granted %v, want u", got)
	}
	f.unlock(t, h, file, owner(h, "h"), br(0, 10))
	if ws := f.sink.takeWaits(); len(ws) != 0 {
		t.Errorf("an ended waiter was served: %+v", ws)
	}
}

// A cancel names the request it cancels, and cancelling a waiter lifts its
// bar: a waiter barred only by it is granted.
func TestCancelServesWaitersBehind(t *testing.T) {
	f := newFixture(t)
	h, a, b := f.host(t, "h", 1), f.host(t, "a", 1), f.host(t, "b", 1)
	file := anyFile()
	f.mustLock(t, h, file, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{sh(0, 10)}})
	f.mustLock(t, a, file, LockReq{Owner: owner(a, "a"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	f.mustLock(t, b, file, LockReq{Owner: owner(b, "b"), Ranges: []LockRange{sh(0, 10)}, Wait: WaitCallback})
	if _, err := f.Locks().CancelLock(ctx, a, file, owner(a, "a"), br(0, 5)); err != nil {
		t.Fatal(err)
	}
	if ws := f.sink.takeWaits(); len(ws) != 0 {
		t.Fatalf("a cancel naming a range other than the waiter's ended its wait: %+v", ws)
	}
	if _, err := f.Locks().CancelLock(ctx, a, file, owner(a, "a"), br(0, 10)); err != nil {
		t.Fatal(err)
	}
	if got := granted(t, f.sink.takeWaits()); len(got) != 1 || got[0] != "b" {
		t.Errorf("after the barring waiter cancelled: granted %v, want b", got)
	}
}

// A cancel racing the unlock that frees its range either drops the waiter, so
// nothing is held for it, or reports the lock granted first, and it is held.
func TestCancelRacesGrant(t *testing.T) {
	f := newFixture(t)
	h, w := f.caller(t, smb("pc-h")), f.caller(t, smb("pc-w"))
	for range 1000 {
		file := anyFile()
		oh, ow := f.mustOpen(t, h, file, OpenRequest{Access: rd | wr}), f.mustOpen(t, w, file, OpenRequest{Access: rd | wr})
		hOwner, wOwner := smbOwner(h, oh), smbOwner(w, ow)
		f.mustLock(t, h, file, LockReq{Owner: hOwner, Open: refOf(oh), Ranges: []LockRange{x(0, 10)}})
		f.mustLock(t, w, file, LockReq{Owner: wOwner, Open: refOf(ow), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
		var got bool
		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = f.Locks().Unlock(ctx, h, file, refOf(oh), hOwner, []ByteRange{br(0, 10)}, Seqs{})
		})
		wg.Go(func() { got, _ = f.Locks().CancelLock(ctx, w, file, wOwner, br(0, 10)) })
		wg.Wait()
		if held := f.holder(t, file, br(0, 10)); got != (held != nil) {
			t.Fatalf("cancel answered granted=%v while the range is held by %v", got, held)
		}
		f.close(t, h, oh)
		f.close(t, w, ow)
		f.sink.takeWaits()
	}
}

// Releasing an owner of locks that still holds a lock, on any file, is
// refused and changes nothing; once it holds none, its lock states and its
// waiter go, so the range it waited on is free to others.
func TestReleaseLockOwner(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "b", 1)
	file, other := anyFile(), anyFile()
	if bytes.Compare(other.ID[:], file.ID[:]) > 0 {
		file, other = other, file // the release visits other, the file it may drop, first
	}
	o, p := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr}), f.mustOpen(t, a, other, OpenRequest{Access: rd | wr})
	lo := owner(a, "lo")
	f.mustLock(t, a, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	kept := f.mustLock(t, a, other, LockReq{Owner: lo, Open: refOf(p), Ranges: []LockRange{x(0, 10)}}).Ref
	kept.Seq = 0 // the current sequence, whatever the unlocks below make it
	unlock := func(file FileKey, under Open) {
		t.Helper()
		if _, err := f.Locks().Unlock(ctx, a, file, refOf(under), lo, []ByteRange{br(0, 10)}, Seqs{}); err != nil {
			t.Fatal(err)
		}
	}
	unlock(other, p)
	if err := f.Locks().ReleaseLockOwner(ctx, a, lo); !errors.Is(err, ErrLocksHeld) {
		t.Fatalf("releasing an owner holding a lock: %v, want ErrLocksHeld", err)
	}
	if h := f.holder(t, file, br(0, 10)); h == nil || string(h.Owner.Name) != "lo" {
		t.Fatalf("the refused release left the range held by %v, want lo", h)
	}
	if err := f.Checks().CheckIO(ctx, a, OpenRef{Kind: RefLocked, State: kept, Handle: handle(other.ID)}, br(0, 1), false); err != nil {
		t.Errorf("the refused release dropped lo's lock state on the other file: %v", err)
	}

	unlock(file, o)
	f.mustLock(t, b, other, LockReq{Owner: owner(b, "b"), Ranges: []LockRange{x(50, 10)}})
	if _, err := f.lock(a, other, LockReq{Owner: lo, Open: refOf(p), Ranges: []LockRange{x(50, 10)}, Wait: WaitPoll}); !errors.Is(err, ErrLocked) {
		t.Fatalf("lo's poll on b's range: %v, want ErrLocked", err)
	}
	if err := f.Locks().ReleaseLockOwner(ctx, a, lo); err != nil {
		t.Fatal(err)
	}
	f.unlock(t, b, other, owner(b, "b"), br(50, 10))
	if h := f.holder(t, other, br(50, 10)); h != nil {
		t.Errorf("the range lo waited on, after its release and b's unlock: held by %v, want free", h)
	}
}

// An NLM host's record goes with its last lock or its last waiter, and a
// record whose losses are not reported by a restart notice stays.
func TestNLMRecordGoesWithLastLock(t *testing.T) {
	f := newFixture(t)
	f1, f2 := anyFile(), anyFile()
	for _, n := range []string{"h1", "h2", "h3"} {
		h := f.host(t, n, 1)
		f.mustLock(t, h, f1, LockReq{Owner: owner(h, n), Ranges: []LockRange{x(0, 10)}})
		f.mustLock(t, h, f2, LockReq{Owner: owner(h, n), Ranges: []LockRange{x(0, 10)}})
		f.unlock(t, h, f1, owner(h, n), br(0, 10))
		if rec, err := f.Record(ctx, h.Client); err != nil || len(rec.Shards) != 1 || rec.Shards[0].Shard != 1 {
			t.Fatalf("host %s still holding a lock: %+v, %v; want its record kept, listing the lock's shard", n, rec.Shards, err)
		}
		f.unlock(t, h, f2, owner(h, n), br(0, 10))
		if _, err := f.Record(ctx, h.Client); !errors.Is(err, ErrStaleClient) {
			t.Errorf("host %s after its last unlock: %v, want its record gone", n, err)
		}
	}
	h, w := f.host(t, "holder", 1), f.host(t, "waiter", 1)
	f.mustLock(t, h, f1, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}})
	f.mustLock(t, w, f1, LockReq{Owner: owner(w, "w"), Ranges: []LockRange{x(0, 10)}, Wait: WaitCallback})
	if _, err := f.Locks().CancelLock(ctx, w, f1, owner(w, "w"), br(0, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Record(ctx, w.Client); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a host whose only waiter was cancelled: %v, want its record gone", err)
	}
	f.unlock(t, h, f1, owner(h, "h"), br(0, 10))

	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	o := f.mustOpen(t, a, f1, OpenRequest{Access: rd | wr})
	f.mustLock(t, a, f1, LockReq{Owner: owner(a, "lo"), Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	f.close(t, a, o)
	if err := f.Locks().ReleaseLockOwner(ctx, a, owner(a, "lo")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Record(ctx, a.Client); err != nil {
		t.Errorf("an NFSv4 client holding nothing after releasing its owner of locks: %v, want its record kept", err)
	}
}

// An NLM host's last unlock racing its next lock either keeps the record,
// which then holds the lock, or deletes it and refuses the lock
// ErrStaleClient: no lock is ever held under a deleted record.
func TestNLMLastUnlockRacesLock(t *testing.T) {
	f := newFixture(t)
	for i := range 300 {
		h := f.host(t, fmt.Sprint("h", i), 1)
		first, next := anyFile(), anyFile()
		f.mustLock(t, h, first, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}})
		var err error
		var wg sync.WaitGroup
		wg.Go(func() { f.unlock(t, h, first, owner(h, "h"), br(0, 10)) })
		wg.Go(func() { _, err = f.lock(h, next, LockReq{Owner: owner(h, "h"), Ranges: []LockRange{x(0, 10)}}) })
		wg.Wait()
		_, gone := f.Record(ctx, h.Client)
		switch held := f.holder(t, next, br(0, 10)) != nil; {
		case err == nil && (gone != nil || !held):
			t.Fatalf("round %d: the lock was granted, held %v, and the record read %v", i, held, gone)
		case err != nil && (!errors.Is(err, ErrStaleClient) || !errors.Is(gone, ErrStaleClient) || held):
			t.Fatalf("round %d: the lock was refused %v, held %v, and the record read %v", i, err, held, gone)
		}
	}
}

func num(n uint32) Seqs { return Seqs{Owner: &n} }

// An owner's sequence takes its requests in order across files. A stale
// number is ErrBadSeqID and advances nothing, nor does a request naming a
// released open or a sequence its open has not reached; a refused open and a
// refused lock each advance it.
func TestOwnerSequence(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "b", 1)
	f1, f2 := anyFile(), anyFile()
	oo := owner(a, "open-owner")
	o1 := f.mustOpen(t, a, f1, OpenRequest{Access: rd | wr, Deny: wr, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, a, refOf(o1), num(2)); err != nil {
		t.Fatal(err)
	}
	o2 := f.mustOpen(t, a, f2, OpenRequest{Access: rd, Owner: oo, Seqs: num(3)})
	if _, err := f.open(a, f2, OpenRequest{Access: rd, Owner: oo, Seqs: num(3)}); !errors.Is(err, ErrBadSeqID) {
		t.Fatalf("a stale number: %v, want ErrBadSeqID", err)
	}
	denied := anyFile()
	f.mustOpen(t, f.caller(t, smb("pc")), denied, OpenRequest{Access: rd, Deny: wr})
	if _, err := f.open(a, denied, OpenRequest{Access: wr, Owner: oo, Seqs: num(4)}); !errors.Is(err, ErrShareViolation) {
		t.Fatalf("an open against another client's deny-write: %v, want ErrShareViolation", err)
	}
	if _, err := f.Opens().Close(ctx, a, refOf(o2), num(5)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Opens().Close(ctx, a, refOf(o2), num(6)); !errors.Is(err, ErrBadState) {
		t.Fatalf("a close of a released open: %v, want ErrBadState", err)
	}
	ahead := o1.Ref
	ahead.Seq += 5
	if _, err := f.Opens().Downgrade(ctx, a, OpenRef{Kind: RefOpened, State: ahead}, num(6), rd|wr, wr); !errors.Is(err, ErrBadState) {
		t.Fatalf("a downgrade naming a sequence the open has not reached: %v, want ErrBadState", err)
	}
	f.mustLock(t, b, f1, LockReq{Owner: owner(b, "b"), Ranges: []LockRange{x(0, 10)}})
	_, err := f.lock(a, f1, LockReq{Owner: owner(a, "lo"), Open: refOf(o1), Ranges: []LockRange{x(0, 10)}, Seqs: Seqs{Owner: ptr(uint32(6)), NewOwner: 1}})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("a lock introducing an owner, against the host's: %v, want ErrLocked", err)
	}
	if _, err := f.Opens().Downgrade(ctx, a, refOf(o1), num(7), rd, 0); err != nil {
		t.Fatalf("the owner's next number after the refusals: %v; want the refused open and lock alone to have advanced it", err)
	}

	// A granted lock introducing an owner starts that owner's own sequence,
	// and each lock and unlock raises its lock state's reference sequence.
	lo := owner(a, "lo2")
	res := f.mustLock(t, a, f1, LockReq{Owner: lo, Open: refOf(o1), Ranges: []LockRange{sh(20, 10)}, Seqs: Seqs{Owner: ptr(uint32(8)), NewOwner: 1}})
	byLock := LockReq{Owner: lo, Open: OpenRef{Kind: RefLocked, State: res.Ref}, Ranges: []LockRange{sh(40, 10)}}
	if byLock.Seqs = num(1); !errors.Is(errOf(f.lock(a, f1, byLock)), ErrBadSeqID) {
		t.Errorf("the new owner of locks reusing its first number: want ErrBadSeqID")
	}
	byLock.Seqs = num(2)
	locked := f.mustLock(t, a, f1, byLock).Ref
	if byLock.Seqs = num(3); !errors.Is(errOf(f.lock(a, f1, byLock)), ErrOldState) {
		t.Errorf("a lock naming the lock state as first answered, after a lock: want ErrOldState")
	}
	if _, err := f.Locks().Unlock(ctx, a, f1, OpenRef{Kind: RefLocked, State: locked}, lo, []ByteRange{br(40, 10)}, num(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Locks().Unlock(ctx, a, f1, OpenRef{Kind: RefLocked, State: locked}, lo, []ByteRange{br(20, 10)}, num(5)); !errors.Is(err, ErrOldState) {
		t.Errorf("an unlock naming the lock state as of its last lock, after an unlock: %v, want ErrOldState", err)
	}

	// Releasing an owner of locks, once it holds none, and the client's end,
	// drop their sequences.
	locked.Seq = 0
	if _, err := f.Locks().Unlock(ctx, a, f1, OpenRef{Kind: RefLocked, State: locked}, lo, []ByteRange{br(20, 10)}, num(6)); err != nil {
		t.Fatal(err)
	}
	if err := f.Locks().ReleaseLockOwner(ctx, a, lo); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.seqs[locksOwner(lo)]; ok {
		t.Error("the released owner of locks kept its sequence")
	}
	if err := f.Expire(ctx, a.Client); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.seqs[opensOwner(oo)]; ok {
		t.Error("the expired client's owner of opens kept its sequence")
	}
}

// A request another principal makes through the owner's state is not the
// owner's: refused ErrNotYours, it advances nothing, and the owner's next
// request keeps its number.
func TestNotYoursAdvancesNoSequence(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.Principal{}))
	oo := owner(a, "open-owner")
	o := f.mustOpen(t, a, anyFile(), OpenRequest{Access: rd, Owner: oo, Seqs: num(1)})
	if _, err := f.Opens().ConfirmOpen(ctx, a, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	other := Caller{Client: a.Client, Principal: entity.NewUser(), Protocol: a.Protocol}
	if _, err := f.Opens().Close(ctx, other, refOf(o), num(3)); !errors.Is(err, ErrNotYours) {
		t.Fatalf("a close by another principal: %v, want ErrNotYours", err)
	}
	if _, err := f.Opens().Close(ctx, a, refOf(o), num(3)); err != nil {
		t.Errorf("the owner's close with the number the refusal carried: %v", err)
	}
}

// Two requests of one owner carrying one number never both run: the
// second, arriving between the first's check and its answer, is told to
// retry, and after the answer it is out of sequence.
func TestOwnerRequestsAreSerial(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	for i := range 300 {
		oo := owner(a, fmt.Sprint("open-owner-", i))
		o := f.mustOpen(t, a, anyFile(), OpenRequest{Access: rd, Owner: oo, Seqs: num(1)})
		if _, err := f.Opens().ConfirmOpen(ctx, a, refOf(o), num(2)); err != nil {
			t.Fatal(err)
		}
		var errs [2]error
		var wg sync.WaitGroup
		for j := range errs {
			wg.Go(func() { _, errs[j] = f.open(a, anyFile(), OpenRequest{Access: rd, Owner: oo, Seqs: num(3)}) })
		}
		wg.Wait()
		if errs[0] == nil && errs[1] == nil {
			t.Fatalf("round %d: two opens of one owner numbered 3 were both granted", i)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrDelay) && !errors.Is(err, ErrBadSeqID) {
				t.Fatalf("round %d: the second open: %v, want ErrDelay or ErrBadSeqID", i, err)
			}
		}
	}
}

// A call that raises a reference's sequence answers the new one, which the
// client presents next: a confirm, a downgrade and an unlock each.
func TestCallsAnswerTheNewSequence(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr, Owner: owner(a, "oo"), Seqs: num(1)})
	opened := func(seq uint32) OpenRef {
		st := o.Ref
		st.Seq = seq
		return OpenRef{Kind: RefOpened, State: st, Handle: handle(file.ID)}
	}
	seq, err := f.Opens().ConfirmOpen(ctx, a, opened(o.Ref.Seq), num(2))
	if err != nil {
		t.Fatal(err)
	}
	if seq, err = f.Opens().Downgrade(ctx, a, opened(seq), num(3), rd, 0); err != nil {
		t.Fatalf("a downgrade naming the sequence the confirm answered: %v", err)
	}
	if err := f.Checks().CheckIO(ctx, a, opened(seq), br(0, 1), false); err != nil {
		t.Errorf("a read naming the sequence the downgrade answered: %v", err)
	}

	lo := owner(a, "lo")
	res := f.mustLock(t, a, file, LockReq{Owner: lo, Open: opened(seq), Ranges: []LockRange{sh(0, 10), sh(20, 10)}})
	locked := OpenRef{Kind: RefLocked, State: res.Ref}
	seq, err = f.Locks().Unlock(ctx, a, file, locked, lo, []ByteRange{br(0, 10)}, Seqs{})
	if err != nil || seq != res.Ref.Seq+1 {
		t.Fatalf("an unlock: sequence %d, %v; want %d", seq, err, res.Ref.Seq+1)
	}
	locked.State.Seq = seq
	if _, err := f.Locks().Unlock(ctx, a, file, locked, lo, []ByteRange{br(20, 10)}, Seqs{}); err != nil {
		t.Errorf("an unlock naming the sequence the last unlock answered: %v", err)
	}
}

func errOf(_ LockResult, err error) error { return err }

func errOf2(_ uint32, err error) error { return err }

func ptr[T any](v T) *T { return &v }

// Until its confirm, an owner's one open is usable by nothing else: a
// close, a downgrade or a lock under it is ErrBadState and advances nothing,
// and a new open from the owner, whatever its number, replaces it, the first
// released and the confirm asked again.
func TestUnconfirmedOwner(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	file := anyFile()
	oo := owner(a, "open-owner")
	res, err := f.Opens().Open(ctx, a, file, OpenRequest{Access: rd | wr, Deny: wr, Owner: oo, Seqs: num(1)})
	if err != nil || !res.Confirm {
		t.Fatalf("a new owner's open: Confirm %v, %v", res.Confirm, err)
	}
	first := res.Open
	if _, err := f.Opens().Close(ctx, a, refOf(first), num(2)); !errors.Is(err, ErrBadState) {
		t.Errorf("a close under the unconfirmed owner: %v, want ErrBadState", err)
	}
	if _, err := f.Opens().Downgrade(ctx, a, refOf(first), num(2), rd, 0); !errors.Is(err, ErrBadState) {
		t.Errorf("a downgrade under the unconfirmed owner: %v, want ErrBadState", err)
	}
	lock := LockReq{Owner: owner(a, "lo"), Open: refOf(first), Ranges: []LockRange{x(0, 10)}, Seqs: Seqs{Owner: ptr(uint32(2)), NewOwner: 1}}
	if _, err := f.lock(a, file, lock); !errors.Is(err, ErrBadState) {
		t.Errorf("a lock under the unconfirmed owner: %v, want ErrBadState", err)
	}

	res, err = f.Opens().Open(ctx, a, anyFile(), OpenRequest{Access: rd, Owner: oo, Seqs: num(40)})
	if err != nil || !res.Confirm {
		t.Fatalf("a second open from the unconfirmed owner: Confirm %v, %v; want granted, the confirm asked again", res.Confirm, err)
	}
	if _, err := f.open(f.caller(t, smb("pc")), file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open of the first open's file once it was replaced: %v; want its deny-write gone", err)
	}
	if _, err := f.Opens().ConfirmOpen(ctx, a, refOf(res.Open), num(41)); err != nil {
		t.Errorf("the confirm of the replacing open, numbered after it: %v", err)
	}
}

// An owner holds one open of a file: its second open of the file is that
// open, upgraded, its modes widened and its sequence raised, judged against
// the other opens alone; another owner's open is another open.
func TestOwnerUpgradesItsOpen(t *testing.T) {
	f := newFixture(t)
	a, s := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.caller(t, smb("pc"))
	file := anyFile()
	oo := owner(a, "open-owner")
	first := f.mustOpen(t, a, file, OpenRequest{Access: rd, Deny: wr, Owner: oo})
	up := f.mustOpen(t, a, file, OpenRequest{Access: wr, Owner: oo})
	if up.Ref.Token != first.Ref.Token || up.Ref.Seq != first.Ref.Seq+1 || up.Mode.Access != rd|wr || up.Mode.Deny != wr {
		t.Fatalf("the owner's second open: %+v, want the first upgraded to read and write, denying write", up)
	}
	if err := f.Checks().CheckIO(ctx, a, refOf(first), br(0, 1), true); err != nil {
		t.Errorf("a write through the upgraded open: %v", err)
	}
	if _, err := f.open(s, file, OpenRequest{Access: rd}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(a, file, OpenRequest{Access: rd, Deny: rd, Owner: oo}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("an upgrade denying read beside another client's reader: %v, want ErrShareViolation", err)
	}
	other := f.mustOpen(t, a, file, OpenRequest{Access: rd, Owner: owner(a, "another")})
	if other.Ref.Token == first.Ref.Token {
		t.Error("another owner's open of the file is the first owner's")
	}
	if r := f.close(t, a, first); r.Orphan || r.Unlink != nil {
		t.Errorf("the close of the upgraded open: %+v", r)
	}
}

// A new owner of opens is confirmed before anything else: I/O through its
// open is refused until ConfirmOpen, which raises the open's reference
// sequence, and only the owner's first open asks for it.
func TestConfirmOpen(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	file := anyFile()
	oo := owner(a, "open-owner")
	res, err := f.Opens().Open(ctx, a, file, OpenRequest{Access: rd, Owner: oo, Seqs: num(1)})
	if err != nil || !res.Confirm {
		t.Fatalf("a new owner's open: Confirm %v, %v; want a confirm asked", res.Confirm, err)
	}
	o := res.Open
	read := func(ref StateRef) error {
		return f.Checks().CheckIO(ctx, a, OpenRef{Kind: RefOpened, State: ref, Handle: handle(file.ID)}, br(0, 1), false)
	}
	if err := read(o.Ref); !errors.Is(err, ErrBadState) {
		t.Errorf("a read before the confirm: %v, want ErrBadState", err)
	}
	if _, err := f.Opens().ConfirmOpen(ctx, a, refOf(o), num(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Opens().ConfirmOpen(ctx, a, refOf(o), num(3)); !errors.Is(err, ErrBadState) {
		t.Errorf("a second confirm: %v, want ErrBadState", err)
	}
	if err := read(o.Ref); !errors.Is(err, ErrOldState) {
		t.Errorf("a read with the reference as opened, after the confirm: %v, want ErrOldState", err)
	}
	current := o.Ref
	current.Seq = 0
	if err := read(current); err != nil {
		t.Errorf("a read after the confirm: %v", err)
	}
	if res, err := f.Opens().Open(ctx, a, anyFile(), OpenRequest{Access: rd, Owner: oo, Seqs: num(3)}); err != nil || res.Confirm {
		t.Errorf("a confirmed owner's open: Confirm %v, %v; want none asked", res.Confirm, err)
	}
}

// I/O may name a lock state, as a client holding locks does: it is judged
// as I/O through the open the lock state was taken under, used only by that
// open's opener and only for the access it holds, and its own owner's locks
// never refuse it.
func TestIOThroughALockState(t *testing.T) {
	f := newFixture(t)
	n, s := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.caller(t, smb("pc"))
	file := anyFile()
	ro := f.mustOpen(t, n, file, OpenRequest{Access: rd})
	res := f.mustLock(t, n, file, LockReq{Owner: owner(n, "lo"), Open: refOf(ro), Ranges: []LockRange{sh(0, 10)}})
	os := f.mustOpen(t, s, file, OpenRequest{Access: rd | wr})
	sres := f.mustLock(t, s, file, LockReq{Owner: smbOwner(s, os), Open: refOf(os), Ranges: []LockRange{x(100, 10)}})
	io := func(who Caller, st StateRef, off uint64, write bool) error {
		return f.Checks().CheckIO(ctx, who, OpenRef{Kind: RefLocked, State: st, Handle: handle(file.ID)}, br(off, 1), write)
	}
	if err := io(n, res.Ref, 5, false); err != nil {
		t.Errorf("a read through the lock state: %v", err)
	}
	if err := io(n, res.Ref, 5, true); !errors.Is(err, ErrAccess) {
		t.Errorf("a write through the lock state of a read-only open: %v, want ErrAccess", err)
	}
	if err := io(Caller{Client: n.Client, Principal: entity.NewUser()}, res.Ref, 5, false); !errors.Is(err, ErrNotYours) {
		t.Errorf("a read through the lock state by another principal: %v, want ErrNotYours", err)
	}
	if err := io(s, sres.Ref, 105, true); err != nil {
		t.Errorf("a write through the lock state of the exclusive lock it holds: %v", err)
	}
	if err := io(n, res.Ref, 105, false); !errors.Is(err, ErrLocked) {
		t.Errorf("a read through another open's lock state under the exclusive lock: %v, want ErrLocked", err)
	}
}

// Every lock call names its caller's client and acts only on that client's
// owners and lock states, a test of a lock included: an owner of another
// client is ErrNotYours, a lock
// under an open by a principal not its opener is ErrNotYours, and a lock state
// named with another owner is ErrBadState.
func TestLockCallsNameTheirClient(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.host(t, "b", 1)
	file := anyFile()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr})
	lo := owner(a, "lo")
	res := f.mustLock(t, a, file, LockReq{Owner: lo, Open: refOf(o), Ranges: []LockRange{x(0, 10)}})
	for name, err := range map[string]error{
		"Lock":             errOf(f.lock(b, file, LockReq{Owner: lo, Ranges: []LockRange{x(0, 10)}})),
		"Unlock":           errOf2(f.Locks().Unlock(ctx, b, file, none, lo, []ByteRange{br(0, 10)}, Seqs{})),
		"CancelLock":       func() error { _, err := f.Locks().CancelLock(ctx, b, file, lo, br(0, 10)); return err }(),
		"WaitAnswered":     f.Locks().WaitAnswered(ctx, b, LockWaiter{File: file.ID, Owner: lo, Open: o.Ref.Token, Range: x(0, 10)}, true),
		"ReleaseLockOwner": f.Locks().ReleaseLockOwner(ctx, b, lo),
		"TestLock":         func() error { _, err := f.Locks().TestLock(ctx, b, file, lo, br(0, 10), true); return err }(),
	} {
		if !errors.Is(err, ErrNotYours) {
			t.Errorf("%s by another client: %v, want ErrNotYours", name, err)
		}
	}
	thief := Caller{Client: a.Client, Principal: entity.NewUser()}
	if _, err := f.lock(thief, file, LockReq{Owner: owner(a, "lo2"), Open: refOf(o), Ranges: []LockRange{x(20, 10)}}); !errors.Is(err, ErrNotYours) {
		t.Errorf("a lock under the open by another principal: %v, want ErrNotYours", err)
	}
	other := owner(a, "lo2")
	if _, err := f.lock(a, file, LockReq{Owner: other, Open: OpenRef{Kind: RefLocked, State: res.Ref}, Ranges: []LockRange{x(20, 10)}}); !errors.Is(err, ErrBadState) {
		t.Errorf("a lock naming lo's lock state as another owner: %v, want ErrBadState", err)
	}
	if _, err := f.Locks().Unlock(ctx, a, file, OpenRef{Kind: RefLocked, State: res.Ref}, other, []ByteRange{br(0, 10)}, Seqs{}); !errors.Is(err, ErrBadState) {
		t.Errorf("an unlock naming lo's lock state as another owner: %v, want ErrBadState", err)
	}
	if f.holder(t, file, br(0, 10)) == nil {
		t.Error("lo's lock was released by a call that was refused")
	}
}
