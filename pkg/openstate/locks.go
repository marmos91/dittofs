package openstate

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/ranges"
	"github.com/marmos91/dittofs/pkg/openstate/sequence"
)

// maxRanges is the most ranges one lock or unlock request names.
//
// ponytail: 64 ranges a request bounds the step a Lock holds at O(64·n) in
// the file's locks. Raise it if a client is seen sending longer lists that it
// cannot split.
const maxRanges = 64

// Lock is one byte-range lock, held against the file.
type Lock struct {
	Owner LockOwner
	Open  uint64 // the token of the open it was taken under, whose close releases it; zero: none
	Range LockRange
}

// LockOwner is who holds a lock. Two owners are one when their Client and
// Name are, so two owners of one client conflict like two clients.
type LockOwner struct {
	Client entity.ClientID
	Name   []byte // the owner the client names, at most 1,024 bytes, compared and never interpreted

	// Stack: its locks stack, an exclusive one conflicts with its own, and
	// they are mandatory; else they merge, never conflict with each other,
	// and are advisory.
	//
	// ponytail: one flag carries two rules that go together in every protocol
	// served, as SMB's locks are the only ones that stack and the only ones
	// that are mandatory. Split it into two flags when a protocol is served
	// whose locks stack but are advisory, or merge but are mandatory.
	Stack bool
}

// LockRange is one range of a lock request; one request may mix shared and
// exclusive ranges.
type LockRange struct {
	ByteRange
	Exclusive bool
}

// WaitKind is how a conflicting lock request waits.
type WaitKind uint8

const (
	WaitNone     WaitKind = iota
	WaitPoll              // the client asks again; a freed range is reserved for its next request
	WaitCallback          // granted in the step that frees the range, and reported through Sink.LockWaitOver
)

// LockReq is one lock request: every range in one step, all or none.
type LockReq struct {
	Owner   LockOwner
	Open    OpenRef // the open, or the owner's lock state, it is taken under; RefAnonymous: none
	Ranges  []LockRange
	Wait    WaitKind // only a single-range request waits
	Seqs    Seqs
	Reclaim bool          // locks the owner held before its shard lost its state; a reclaim never waits
	Replay  *LockSequence // its place in its open's lock replay; nil: none
}

// LockResult is a granted or waiting lock; a conflict is a *LockedError.
type LockResult struct {
	Waiting bool     // a callback waiter was recorded: granted later through Sink.LockWaitOver
	Ref     StateRef // the lock state the owner's locks are held under; Seq is its sequence as of the answer
}

// LockWaiter is a lock request that conflicted and asked to wait.
type LockWaiter struct {
	File     entity.FileID
	Owner    LockOwner
	Open     uint64 // the open's token; zero: the lock names no open
	Range    LockRange
	Wait     WaitKind
	Deadline time.Duration // poll only: one lease period after the owner's last poll, or after its range freed
	Reserved bool          // poll only: the range is free and held for this owner until Deadline
	Replay   *LockSequence // the request's place in its open's lock replay, stored when it is granted; nil: none
}

// Locks is the view of byte-range locks and their waiters; it runs at the
// file's primary.
type Locks interface {
	// Lock grants every range of r or none. A conflict is a *LockedError
	// naming the first holder found; a single-range request that waits
	// records a waiter, by poll answered the conflict, by callback answered
	// Waiting. Past 64 ranges it is ErrInvalid. In grace only a reclaim is
	// granted, judged against what was reclaimed before it; outside grace a
	// reclaim is answered with the lock state holding every range asked.
	Lock(ctx context.Context, who Caller, f FileKey, r LockReq) (LockResult, error)
	// CancelLock drops o's waiter on f for r, or reports that its lock was
	// granted first.
	CancelLock(ctx context.Context, who Caller, f FileKey, o LockOwner, r ByteRange) (granted bool, err error)
	// WaitAnswered takes the client's answer to a LockWaitOver grant: a
	// refusal releases the lock as an unlock would, but for the parts the
	// owner's own lock or unlock of them answered first, which it leaves.
	WaitAnswered(ctx context.Context, who Caller, w LockWaiter, refused bool) error
	// TestLock is the lock that would refuse o's lock of r, or nil; it
	// changes nothing. o is the caller's client's owner. It is admitted as a
	// lock that is no reclaim is, so in grace it is ErrGrace.
	TestLock(ctx context.Context, who Caller, f FileKey, o LockOwner, r ByteRange, exclusive bool) (*Lock, error)
	// Unlock releases rs in order. A stacking owner stops at the first range
	// it holds no lock of exactly, with ErrRangeNotLocked, the ranges before
	// it staying released; a merging owner releases whatever it holds of
	// each. It returns the lock state's new reference sequence. replay is
	// its place in its open's lock replay, as a lock's is; nil: none.
	Unlock(ctx context.Context, who Caller, f FileKey, ref OpenRef, o LockOwner, rs []ByteRange, q Seqs, replay *LockSequence) (seq uint32, err error)
	// ReleaseLockOwner drops o's lock states at this primary, with their
	// waiters and o's sequence; while o holds a lock, on any file, it is
	// ErrLocksHeld and changes nothing. The waiters the release frees are
	// served under each file's admission lock, which the table takes itself:
	// the service must not hold one when calling.
	ReleaseLockOwner(ctx context.Context, who Caller, o LockOwner) error
}

type locks struct{ t *Table }

// Locks is the view of byte-range locks and their waiters.
func (t *Table) Locks() Locks { return locks{t} }

// lockState is one owner's locks under one open of one file, or under none.
type lockState struct {
	owner  LockOwner   // its Name a copy of its own
	open   *open       // nil: none
	ref    StateRef    // Seq is its current sequence
	locks  []LockRange // a merging owner's are disjoint, and none touches another of its type
	offers []offer     // callback grants whose answer is outstanding
}

// offer is a callback grant whose answer is outstanding.
type offer struct {
	asked LockRange   // the grant, which WaitAnswered matches
	left  []ByteRange // what of it the owner's own lock or unlock has not answered
}

// lockSeq is the index r names in op's lock replay and the value a lock
// granted under it stores there; ok is false for a request naming none, or
// an open keeping none.
func lockSeq(op *open, r *LockSequence) (i int, v uint8, ok bool) {
	if r == nil || op == nil || op.Durability == entity.DurabilityNone || r.Index < 1 || r.Index > 64 {
		return 0, 0, false
	}
	return int(r.Index - 1), 0x10 | r.Seq&0xF, true
}

// lockSeqRecord is what a request under op naming q stores once it
// succeeds: rec, the open's record with q's sequence set, for a persistent
// open to write with its locks (nil otherwise), and stored, which sets it
// in memory once the write is done.
func (t *Table) lockSeqRecord(e *entry, op *open, q *LockSequence) (rec *entity.OpenRecord, stored func()) {
	i, v, ok := lockSeq(op, q)
	if !ok {
		return nil, func() {}
	}
	if op.persistent() {
		x := t.recordOf(e, op)
		x.LockSeq[i] = v
		rec = &x
	}
	return rec, func() { op.Durable.LockSeq[i] = v }
}

// resetLockSeq invalidates index i of op's lock replay, under e's step, for
// a request under another sequence than the one stored there: the request
// is then judged as a new one, and a later request wrapping round to the
// old sequence is no replay. A persistent open's record follows by the
// timer, the open unsaved until it is written.
func (t *Table) resetLockSeq(e *entry, op *open, i int) {
	if op.Durable.LockSeq[i] == 0 {
		return
	}
	op.Durable.LockSeq[i] = 0
	if op.persistent() {
		op.unsaved = true
		t.idx.Lock()
		t.parked[e.file] = struct{}{}
		t.idx.Unlock()
	}
}

// dropOffers takes r out of every offer of a merging owner, and forgets an
// offer once nothing of it is left: the owner's own lock or unlock of r
// answered the host first, so a later refusal releases nothing of r. A
// stacking owner's grants are answered by dropSurplusOffer instead.
func (ls *lockState) dropOffers(r ByteRange) {
	kept := ls.offers[:0]
	for _, o := range ls.offers {
		var left []ByteRange
		for _, l := range o.left {
			left = append(left, ranges.Minus(l, r)...)
		}
		if o.left = left; len(left) > 0 {
			kept = append(kept, o)
		}
	}
	ls.offers = kept
}

// dropSurplusOffer is a stacking owner's unlock of exactly r answering one of
// its grants of r, which happens only once its locks of r are fewer than those
// grants: while another lock of r remains the unlock is taken as that lock's,
// so each grant stays whole for its refusal to release. A grant is a lock of
// its own, so an unlock of any other range leaves it whole.
func (ls *lockState) dropSurplusOffer(r ByteRange) {
	held, offered := 0, 0
	for _, l := range ls.locks {
		if l.ByteRange == r {
			held++
		}
	}
	for _, o := range ls.offers {
		if o.asked.ByteRange == r {
			offered++
		}
	}
	if offered > held {
		i := slices.IndexFunc(ls.offers, func(o offer) bool { return o.asked.ByteRange == r })
		ls.offers = slices.Delete(ls.offers, i, i+1)
	}
}

// unoffered is what of rs none of offers covers: that part another offer's
// own answer decides, so a refusal releases only the rest. A stacking owner's
// grants are separate locks, which unlock removes only whole, so for it rs is
// left as it is.
//
// decision: a merging owner's refusal keeps what another grant to it covers in
// the mode the later grant left it, as the grant merged it in and nothing
// records the mode it replaced. This fails open when a shared grant over an
// accepted exclusive one is refused: the overlap stays shared, so another
// owner is granted it shared. Overturned by a client that refuses a downgrade
// it raced against its own exclusive grant.
func (ls *lockState) unoffered(rs []ByteRange, offers []offer) []ByteRange {
	if ls.owner.Stack {
		return rs
	}
	for _, o := range offers {
		for _, ol := range o.left {
			var rest []ByteRange
			for _, r := range rs {
				rest = append(rest, ranges.Minus(r, ol)...)
			}
			rs = rest
		}
	}
	return rs
}

func (ls *lockState) openToken() uint64 { return openToken(ls.open) }

// openToken is the token naming o; zero for none.
func openToken(o *open) uint64 {
	if o == nil {
		return 0
	}
	return o.Ref.Token
}

// lock is ls's lock of r. Its owner's name is the table's: a refusal copies
// nothing.
func (ls *lockState) lock(r LockRange) *Lock {
	return &Lock{Owner: ls.owner, Open: ls.openToken(), Range: r}
}

// add takes r into ls: a stacking owner's locks stack; a merging owner's lock
// replaces what it held of r, and joins its locks of r's type that it touches.
func (ls *lockState) add(r LockRange) {
	if ls.owner.Stack {
		ls.locks = append(ls.locks, r)
		return
	}
	ls.cut(r.ByteRange)
	kept := ls.locks[:0]
	for _, l := range ls.locks {
		if j, ok := ranges.Join(l.ByteRange, r.ByteRange); ok && l.Exclusive == r.Exclusive {
			r.ByteRange = j
			continue
		}
		kept = append(kept, l)
	}
	ls.locks = append(kept, r)
}

// cut releases whatever ls holds of b.
func (ls *lockState) cut(b ByteRange) {
	var kept []LockRange
	for _, l := range ls.locks {
		for _, p := range ranges.Minus(l.ByteRange, b) {
			kept = append(kept, LockRange{p, l.Exclusive})
		}
	}
	ls.locks = kept
}

// unlock releases rs in order, as Unlock does.
func (ls *lockState) unlock(rs []ByteRange) error {
	for _, r := range rs {
		if !ls.owner.Stack {
			ls.dropOffers(r)
			ls.cut(r)
			continue
		}
		i := slices.IndexFunc(ls.locks, func(l LockRange) bool { return l.ByteRange == r })
		if i < 0 {
			return ErrRangeNotLocked
		}
		ls.locks = slices.Delete(ls.locks, i, i+1)
		ls.dropSurplusOffer(r)
	}
	return nil
}

func sameOwner(a, b LockOwner) bool { return a.Client == b.Client && bytes.Equal(a.Name, b.Name) }

func locksOwner(o LockOwner) sequence.Owner {
	return sequence.Owner{Client: o.Client, Kind: sequence.Locks, Name: string(o.Name)}
}

// conflicts reports whether owner o's lock r conflicts with h, held or asked
// for by holder: they share a byte and one is exclusive, and they are two
// owners' or, a stacking owner's, r is exclusive.
func conflicts(o LockOwner, r LockRange, holder LockOwner, h LockRange) bool {
	if !ranges.Overlap(r.ByteRange, h.ByteRange) || !r.Exclusive && !h.Exclusive {
		return false
	}
	return !sameOwner(o, holder) || o.Stack && r.Exclusive
}

func lapsed(w *LockWaiter, now time.Duration) bool { return w.Wait == WaitPoll && now >= w.Deadline }

// lockState is o's lock state under open id on e; nil: none.
func (e *entry) lockState(o LockOwner, id uint64) *lockState {
	for _, ls := range e.lstates {
		if sameOwner(ls.owner, o) && ls.openToken() == id {
			return ls
		}
	}
	return nil
}

// waiterOf is the index of o's waiter on e, or -1: an owner holds at most one
// per file.
func (e *entry) waiterOf(o LockOwner) int {
	return slices.IndexFunc(e.waiters, func(w *LockWaiter) bool { return sameOwner(w.Owner, o) })
}

// blocker is the lock that refuses owner o's lock r on e, or nil: a held lock
// r conflicts with; a reservation it conflicts with; or, where r conflicts
// with the request of one of the first upto waiters, a lock of another owner
// that r overlaps and that blocks that waiter. So a stream of shared locks on
// the range blocking a waiter cannot starve it. o's own waiter is never in
// its way.
//
// decision: a waiter bars only the ranges of the locks that block it, not
// its whole range: over the whole range, one client holding one byte and
// parking a whole-file exclusive waiter under a second owner would refuse
// every other client's lock on the file for as long as it renews. The
// ceiling: shared locks taken beside the blocker, inside the waiter's range,
// can keep an exclusive waiter waiting for as long as they overlap in time.
// Widen the barrier to a bounded margin if exclusive lock waits are seen
// starving there.
func (e *entry) blocker(o LockOwner, r LockRange, upto int) *Lock {
	for _, ls := range e.lstates {
		for _, l := range ls.locks {
			if conflicts(o, r, ls.owner, l) {
				return ls.lock(l)
			}
		}
	}
	for i, w := range e.waiters {
		if sameOwner(o, w.Owner) || !conflicts(o, r, w.Owner, w.Range) {
			continue
		}
		if w.Reserved {
			return &Lock{Owner: w.Owner, Open: w.Open, Range: w.Range}
		}
		if i >= upto {
			continue
		}
		for _, ls := range e.lstates {
			for _, l := range ls.locks {
				if !sameOwner(o, ls.owner) && ranges.Overlap(r.ByteRange, l.ByteRange) && conflicts(w.Owner, w.Range, ls.owner, l) {
					return ls.lock(l)
				}
			}
		}
	}
	return nil
}

// lockedOut is ErrLocked for I/O r through the open of token id (zero:
// anonymous) across a range a mandatory lock on e forbids it: any I/O but its
// own open's under an exclusive lock, and every write, its own open's too,
// under a shared one. A stacking owner's locks are the mandatory ones.
func (e *entry) lockedOut(id uint64, r ByteRange, write bool) error {
	for _, ls := range e.lstates {
		if !ls.owner.Stack {
			continue
		}
		own := id != 0 && ls.openToken() == id
		for _, l := range ls.locks {
			if ranges.Overlap(l.ByteRange, r) && (write && (!own || !l.Exclusive) || !write && l.Exclusive && !own) {
				return ErrLocked
			}
		}
	}
	return nil
}

// lockStateFor is o's lock state under op (nil: none) on e, minted if it has
// none while o's client is current here: in op's shard, or in s, which is
// first listed in the client's record, for one under no open.
func (t *Table) lockStateFor(ctx context.Context, e *entry, o LockOwner, op *open, s ShardID) (ls *lockState, minted bool, err error) {
	var id uint64
	if op != nil {
		id, s = op.Ref.Token, op.Ref.Shard
	}
	if ls := e.lockState(o, id); ls != nil {
		return ls, false, nil
	}
	if op == nil {
		if err := t.markShard(ctx, o.Client, s); err != nil {
			return nil, false, err
		}
	}
	ref, err := t.mintUnder(e, o.Client, s, StateLock, func(StateRef) error {
		if !t.current(o.Client) {
			return ErrStaleClient
		}
		// An owner is counted once, by its first lock state, never refused
		// here: a lock took its place at reserveLock, and an installed open's
		// owner is never refused.
		k := ownerKey{o.Client, string(o.Name)}
		if t.owners[k] == 0 {
			_ = t.chargeLocked(o.Client, cappedOwners, 1, false)
		}
		t.owners[k]++
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	o.Name = slices.Clone(o.Name)
	ls = &lockState{owner: o, open: op, ref: ref}
	if e.lstates == nil {
		e.lstates = map[uint64]*lockState{}
	}
	e.lstates[ref.Token] = ls
	return ls, true, nil
}

// dropLockState drops ls from e, with its locks and its waiter.
func (t *Table) dropLockState(e *entry, ls *lockState) {
	for i := len(e.waiters) - 1; i >= 0; i-- {
		if w := e.waiters[i]; sameOwner(w.Owner, ls.owner) && w.Open == ls.openToken() {
			t.unwait(e, i)
		}
	}
	delete(e.lstates, ls.ref.Token)
	t.release(e, ls.ref)
	t.idx.Lock()
	defer t.idx.Unlock()
	c := ls.owner.Client
	_ = t.chargeLocked(c, cappedRanges, -len(ls.locks), false)
	k := ownerKey{c, string(ls.owner.Name)}
	if t.owners[k]--; t.owners[k] <= 0 {
		delete(t.owners, k)
		_ = t.chargeLocked(c, cappedOwners, -1, false)
	}
}

// reserveLock takes r's places, under op with lease key key, on e: the
// ranges it adds, a new owner, and a waiter where another holder's caching
// grant would make it wait, refused together ErrDelay past a cap. It runs
// before r starts a recall, which can close another client's disconnected
// durable open, so a refusal changes nothing. A waiter behind a lock alone
// takes no place here: it starts no recall, so it is charged, and refused,
// where it is added, as nothing changes before it. The places count beside
// the charges made where the state is added, which take them with no
// refusal: a request of the client racing r near a cap is refused meanwhile,
// which fails closed. rangesBack gives the ranges' places back, which r's step
// calls once its own ranges are charged, before it serves the file's
// waiters, so a waiter of the same client is not judged against them twice;
// release gives back the rest at the end of the step. The owner's place is
// a reference to it, held until release, so a state of the owner dropping
// elsewhere meanwhile neither frees its place nor lets the mint charge it
// again past the cap. waiter reports whether a waiter's place was taken.
func (t *Table) reserveLock(e *entry, r LockReq, op *open, key LeaseKey) (rangesBack, release func(), waiter bool, err error) {
	c := r.Owner.Client
	ls := e.lockState(r.Owner, openToken(op))
	cur := lockState{owner: r.Owner}
	if ls != nil {
		cur.locks = ls.locks
	}
	if r.Wait != WaitNone && e.waiterOf(r.Owner) < 0 {
		for _, g := range e.grants {
			waiter = waiter || !g.heldBy(c, key, op) && g.fsm.Kind != GrantNone
		}
	}
	var n [len(perClient)]int
	n[cappedRanges] = max(len(cur.adding(r.Ranges...))-len(cur.locks), 0)
	if waiter {
		n[cappedWaiters] = 1
	}
	t.idx.Lock()
	defer t.idx.Unlock()
	ok := ownerKey{c, string(r.Owner.Name)}
	if t.owners[ok] == 0 {
		n[cappedOwners] = 1
	}
	for k, m := range n {
		if t.overLocked(c, capped(k), m) {
			return nil, nil, false, &DelayError{DelayResources}
		}
	}
	for k, m := range n {
		_ = t.chargeLocked(c, capped(k), m, false)
	}
	t.owners[ok]++
	rangesBack = sync.OnceFunc(func() { _ = t.charge(c, cappedRanges, -n[cappedRanges], false) })
	return rangesBack, func() {
		rangesBack()
		_ = t.charge(c, cappedWaiters, -n[cappedWaiters], false)
		t.idx.Lock()
		defer t.idx.Unlock()
		if t.owners[ok]--; t.owners[ok] <= 0 {
			delete(t.owners, ok)
			_ = t.chargeLocked(c, cappedOwners, -1, false)
		}
	}, waiter, nil
}

// relock sets ls's locks to next, charging its client for the ranges next
// adds, refused past the cap where capped, and writing them first where ls
// is under a persistent open, so a refusal of either changes nothing.
// rec, when set, is the open's new record, written with the locks.
func (t *Table) relock(ctx context.Context, ls *lockState, next []LockRange, capped bool, rec *entity.OpenRecord) error {
	n := len(next) - len(ls.locks)
	if err := t.charge(ls.owner.Client, cappedRanges, n, capped); err != nil {
		return err
	}
	if err := t.keepLocks(ctx, ls.open, ls.owner, ls.locks, next, rec); err != nil {
		_ = t.charge(ls.owner.Client, cappedRanges, -n, false)
		return err
	}
	ls.locks = next
	return nil
}

// unwait drops e's waiter i, which its client no longer holds.
func (t *Table) unwait(e *entry, i int) *LockWaiter {
	w := e.waiters[i]
	e.waiters = slices.Delete(e.waiters, i, i+1)
	_ = t.charge(w.Owner.Client, cappedWaiters, -1, false)
	return w
}

// tidy drops ls once nothing keeps it: no lock, no waiter, and no open, whose
// close would.
func (t *Table) tidy(e *entry, ls *lockState) {
	waiting := slices.ContainsFunc(e.waiters, func(w *LockWaiter) bool {
		return sameOwner(w.Owner, ls.owner) && w.Open == ls.openToken()
	})
	if ls.open == nil && len(ls.locks) == 0 && !waiting {
		t.dropLockState(e, ls)
	}
}

// dropWaiter drops e's waiter i, and its lock state if nothing else keeps it.
func (t *Table) dropWaiter(e *entry, i int) *LockWaiter {
	w := t.unwait(e, i)
	if ls := e.lockState(w.Owner, w.Open); ls != nil {
		t.tidy(e, ls)
	}
	return w
}

// serve considers e's waiters, under its step: a poll waiter past its
// deadline is dropped, then each waiter in arrival order whose request
// nothing refuses now, no lock and no caching grant another holder holds, is
// served, a callback waiter granted its lock and reported to out, a poll
// waiter its range reserved for a lease period.
func (t *Table) serve(ctx context.Context, e *entry, out *outbox) {
	if out.timer {
		if !slices.Contains(out.serve, e.file) {
			out.serve = append(out.serve, e.file)
		}
		return
	}
	now := t.d.Clock.Elapsed()
	for i := 0; i < len(e.waiters); {
		if lapsed(e.waiters[i], now) {
			t.dropWaiter(e, i)
			continue
		}
		i++
	}
	for i := 0; i < len(e.waiters); {
		w := e.waiters[i]
		switch {
		case w.Reserved || e.blocker(w.Owner, w.Range, i) != nil || e.cachedByOther(w):
		case w.Wait == WaitPoll:
			w.Reserved, w.Deadline = true, now+leasePeriod
		default:
			t.unwait(e, i)
			granted := true
			if ls := e.lockState(w.Owner, w.Open); ls != nil {
				// A refused write, or a lock past its client's ranges, is a
				// lock not held, answered not granted.
				rec, stored := t.lockSeqRecord(e, ls.open, w.Replay)
				if granted = t.relock(ctx, ls, ls.adding(w.Range), true, rec) == nil; granted {
					stored()
					ls.offers = append(ls.offers, offer{w.Range, []ByteRange{w.Range.ByteRange}}) // only a callback waiter reaches here
				}
				t.tidy(e, ls)
			}
			out.over = append(out.over, waitOver{*w, granted})
			continue
		}
		i++
	}
}

// serveFiles serves the waiters of files, for the timer: an open whose
// Timeout passed since its session ended is closed, a recall past its
// deadline is ended, a poll waiter past its deadline is dropped and the
// waiters behind it are considered, and a range the timer freed is granted.
// Each file's step takes the file's admission lock first, since it may grant
// or revoke, and holds no other table lock.
func (t *Table) serveFiles(ctx context.Context, sink Sink, files []entity.FileID) {
	for _, f := range files {
		var out outbox
		unlock := t.order(f)
		_ = t.step(f, func(e *entry) error {
			t.timeouts(ctx, e, &out)
			t.lapse(ctx, e, &out)
			t.serve(ctx, e, &out)
			if !slices.ContainsFunc(e.waiters, func(w *LockWaiter) bool { return w.Wait == WaitPoll }) {
				t.idx.Lock()
				delete(t.polls, f)
				t.idx.Unlock()
			}
			return nil
		})
		unlock()
		out.raise(t, sink)
	}
}

// lockTarget finds what ref names on e for who: an open; a lock state and its
// open; or nothing, for an anonymous ref. cur is the current sequence of what
// it names, which the caller judges.
func (t *Table) lockTarget(e *entry, who Caller, ref OpenRef) (op *open, ls *lockState, cur uint32, err error) {
	ref.State.Seq = 0
	switch ref.Kind {
	case RefAnonymous:
		return nil, nil, 0, nil
	case RefLocked:
		if _, err := t.resolve(who.Client, e.file, ref.State); err != nil {
			return nil, nil, 0, err
		}
		if ls = e.lstates[ref.State.Token]; ls == nil {
			return nil, nil, 0, ErrBadState
		}
		return ls.open, ls, ls.ref.Seq, nil
	}
	_, k, err := t.findOpen(who, ref)
	if err != nil {
		return nil, nil, 0, err
	}
	if op = e.opens[k.token]; op == nil || op.parked != 0 {
		return nil, nil, 0, ErrBadState // a disconnected open is used again only once reconnected
	}
	return op, nil, op.Ref.Seq, nil
}

func (v locks) Lock(ctx context.Context, who Caller, f FileKey, r LockReq) (LockResult, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return LockResult{}, err
	}
	if len(r.Ranges) == 0 || len(r.Ranges) > maxRanges || len(r.Owner.Name) > maxName {
		return LockResult{}, ErrInvalid
	}
	if r.Owner.Client != who.Client {
		return LockResult{}, ErrNotYours
	}
	if len(r.Ranges) > 1 || r.Reclaim {
		r.Wait = WaitNone
	}
	var res LockResult
	var out outbox
	err = t.step(f.ID, func(e *entry) error {
		op, ls, cur, err := t.lockTarget(e, who, r.Open)
		if err != nil {
			return err
		}
		if ls != nil && !sameOwner(ls.owner, r.Owner) {
			return ErrBadState
		}
		// A lock under an open, not yet under a lock state, introduces its
		// owner of locks, and is numbered by the open's owner.
		introduces := ls == nil && op != nil
		seq := locksOwner(r.Owner)
		if introduces {
			seq = opensOwner(op.Owner)
		}
		return t.sequenced(seq, r.Seqs.Owner, seqOther, func(bool) (err error) {
			if err := seqErr(cur, r.Open.State.Seq); err != nil {
				return err
			}
			if op != nil && !t.mayUse(op, who) {
				return ErrNotYours
			}
			if op != nil && !t.confirmed(op) {
				return ErrBadState // the owner's sequence refuses it only when numbered
			}
			if i, v, ok := lockSeq(op, r.Replay); ok {
				if op.Durable.LockSeq[i] == v {
					// A replay is answered, never applied again, in grace too.
					if ls := e.lockState(r.Owner, openToken(op)); ls != nil {
						res.Ref = ls.ref
					}
					return nil
				}
				t.resetLockSeq(e, op, i) // another sequence is no replay of the stored one
			}
			// Admitted under the step, so grace is judged as it stands when
			// the lock is decided, and a refusal answers the owner's number.
			grace, err := t.admit(who, f.Shard, r.Reclaim)
			switch {
			case err != nil:
				return err
			case r.Reclaim && !grace:
				err = reclaimHeld(e, r, op, &res)
			default:
				res, err = t.lock(ctx, e, f.Shard, r, op, &out)
			}
			if err == nil && introduces && r.Seqs.Owner != nil {
				t.smu.Lock()
				t.seqs.Answer(locksOwner(r.Owner), r.Seqs.NewOwner, sequence.Granted)
				t.smu.Unlock()
			}
			if grace && errors.Is(err, ErrLocked) {
				err = ErrReclaimConflict
			}
			return err
		})
	})
	out.raise(t, sink)
	return res, err
}

// reclaimHeld answers a reclaim of locks in a shard that lost nothing, under
// e's step: the owner's lock state under op, when it holds every range of r
// as asked, else ErrNoReclaim. Like a resend answered granted, it settles a
// callback grant of the range still offered to a merging owner. A stacking
// owner's reclaim answers none of its grants, as lock answers none for a
// reclaim in grace: it names a lock of r, which need not be the grant's, and
// removes none, so the grant stays whole for its refusal to release.
func reclaimHeld(e *entry, r LockReq, op *open, res *LockResult) error {
	ls := e.lockState(r.Owner, openToken(op))
	if ls == nil {
		return ErrNoReclaim
	}
	for _, rg := range r.Ranges {
		if !slices.ContainsFunc(ls.locks, func(l LockRange) bool {
			return l.Exclusive == rg.Exclusive && len(ranges.Minus(rg.ByteRange, l.ByteRange)) == 0
		}) {
			return ErrNoReclaim
		}
	}
	for _, rg := range r.Ranges {
		if ls.owner.Stack {
			break
		}
		ls.dropOffers(rg.ByteRange) // the reclaim answers the callback granted: its refusal releases nothing
	}
	res.Ref = ls.ref
	return nil
}

// lock decides r, under op (nil: none), on e under its step: every range
// granted, or a conflict at the first range refused, recording a waiter for a
// single range that waits. The owner's earlier waiter on the file is replaced,
// a callback one reported not granted and the waiters it barred served, unless
// r repeats its request and still waits, which keeps its place.
//
// Before any of that, every caching grant on the file another holder holds,
// another client or the owner's client under another lease key, is recalled
// to none; while one still caches, r is never refused for a
// conflict: a callback request waits, and any other is ErrDelay.
func (t *Table) lock(ctx context.Context, e *entry, s ShardID, r LockReq, op *open, out *outbox) (LockResult, error) {
	t.serve(ctx, e, out) // a range the timer freed goes to its waiters before this request is judged
	now := t.d.Clock.Elapsed()
	id := openToken(op)
	var key LeaseKey
	if op != nil {
		key = op.lease
	}
	rangesBack, release, reserved, err := t.reserveLock(e, r, op, key)
	if err != nil {
		return LockResult{}, err
	}
	defer release()
	recall := t.recallOthers(ctx, e, func(g *grant) bool { return g.heldBy(r.Owner.Client, key, op) }, func(*grant) GrantKind { return GrantNone }, out)
	var de *DelayError
	if errors.As(recall, &de) && (de.Cause != DelayRecall || r.Wait != WaitCallback) {
		return LockResult{}, recall
	}
	var held *Lock
	for i, rg := range r.Ranges {
		if held = e.blocker(r.Owner, rg, len(e.waiters)); held != nil {
			break
		}
		// an earlier range of this same list is held by the time this one is judged
		if j := slices.IndexFunc(r.Ranges[:i], func(p LockRange) bool { return conflicts(r.Owner, rg, r.Owner, p) }); j >= 0 {
			held = &Lock{Owner: r.Owner, Open: id, Range: r.Ranges[j]}
			break
		}
	}
	free := held == nil && recall == nil
	var ls *lockState
	var minted bool
	if free {
		// The lock state is made, then the locks written, before anything
		// else changes, so a refused write changes nothing: not the owner's
		// earlier waiter, nor its lock state, a minted one dropped again.
		var err error
		if ls, minted, err = t.lockStateFor(ctx, e, r.Owner, op, s); err != nil {
			return LockResult{}, err
		}
		// The request's place in its open's lock replay is stored with its
		// locks, once for every range, and only when they are granted.
		rec, stored := t.lockSeqRecord(e, op, r.Replay)
		// Held before the earlier waiter goes, so its drop keeps ls.
		if err := t.relock(ctx, ls, ls.adding(r.Ranges...), false, rec); err != nil {
			if minted {
				t.dropLockState(e, ls)
			}
			return LockResult{}, err
		}
		stored()
	}
	rangesBack() // charged by relock, or never added: serve must not count them twice
	prev := e.waiterOf(r.Owner)
	repeat := prev >= 0 && r.Wait == e.waiters[prev].Wait && r.Ranges[0] == e.waiters[prev].Range && id == e.waiters[prev].Open
	if prev >= 0 && (free || !repeat) {
		if w := t.dropWaiter(e, prev); w.Wait == WaitCallback {
			out.over = append(out.over, waitOver{*w, false})
		}
		if !free { // a grant serves them after its ranges are added, below
			t.serve(ctx, e, out) // the waiter it replaced may have barred the ones behind it
		}
	}
	switch {
	case free:
		for _, rg := range r.Ranges {
			if ls.owner.Stack {
				break // a stacking owner's lock is another lock, answering no grant
			}
			ls.dropOffers(rg.ByteRange) // this answer supersedes the callback's: its refusal releases nothing
		}
		if !minted {
			var err error
			if ls.ref.Seq, err = t.advance(e, ls.ref); err != nil {
				return LockResult{}, err
			}
		}
		t.serve(ctx, e, out)
		return LockResult{Ref: ls.ref}, nil
	case r.Wait == WaitNone:
		return LockResult{}, &LockedError{*held}
	case repeat:
		if w := e.waiters[prev]; w.Wait == WaitPoll {
			w.Deadline = now + leasePeriod
		}
	default:
		ls, minted, err := t.lockStateFor(ctx, e, r.Owner, op, s)
		if err != nil {
			return LockResult{}, err
		}
		if err := t.charge(r.Owner.Client, cappedWaiters, 1, !reserved); err != nil {
			if minted {
				t.dropLockState(e, ls)
			}
			return LockResult{}, err
		}
		w := &LockWaiter{File: e.file, Owner: ls.owner, Open: id, Range: r.Ranges[0], Wait: r.Wait}
		if q := r.Replay; q != nil {
			w.Replay = &LockSequence{q.Index, q.Seq}
		}
		if w.Wait == WaitPoll {
			w.Deadline = now + leasePeriod
			t.idx.Lock()
			t.polls[e.file] = struct{}{}
			t.idx.Unlock()
		}
		e.waiters = append(e.waiters, w)
	}
	if r.Wait == WaitPoll {
		return LockResult{}, &LockedError{*held}
	}
	return LockResult{Waiting: true, Ref: e.lockState(r.Owner, id).ref}, nil
}

func (v locks) CancelLock(ctx context.Context, who Caller, f FileKey, o LockOwner, r ByteRange) (bool, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return false, err
	}
	if o.Client != who.Client {
		return false, ErrNotYours
	}
	granted := false
	var out outbox
	_ = t.step(f.ID, func(e *entry) error {
		if i := e.waiterOf(o); i >= 0 && e.waiters[i].Range.ByteRange == r {
			t.dropWaiter(e, i)
			t.serve(ctx, e, &out)
			return nil
		}
		for _, ls := range e.lstates {
			granted = granted || sameOwner(ls.owner, o) && slices.ContainsFunc(ls.locks, func(l LockRange) bool {
				return len(ranges.Minus(r, l.ByteRange)) == 0
			})
		}
		return nil
	})
	out.raise(t, sink)
	t.dropIdle(ctx, o.Client)
	return granted, nil
}

func (v locks) WaitAnswered(ctx context.Context, who Caller, w LockWaiter, refused bool) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	if w.Owner.Client != who.Client {
		return ErrNotYours
	}
	var out outbox
	err = t.step(w.File, func(e *entry) error {
		// decision: only what of a grant is still offered is the callback's to
		// give back: a part the owner's own lock or unlock answered first
		// releases nothing, as Linux lockd ignores a GRANTED reply for a block
		// it has already unlinked. Overturned by a client that refuses a grant
		// it was also answered for synchronously and expects the refusal to
		// release it.
		ls := e.lockState(w.Owner, w.Open)
		i := -1
		if ls != nil {
			i = slices.IndexFunc(ls.offers, func(o offer) bool { return o.asked == w.Range })
		}
		if i < 0 {
			return nil
		}
		o := ls.offers[i]
		others := slices.Delete(slices.Clone(ls.offers), i, i+1)
		if refused {
			// An answer is never refused for the ranges a refusal splits.
			next, _ := ls.removing(ls.unoffered(o.left, others))
			if err := t.relock(ctx, ls, next.locks, false, nil); err != nil {
				return err // the grant stays offered, for the client's answer again
			}
			ls.offers = others
			t.tidy(e, ls)
			t.serve(ctx, e, &out)
		} else {
			ls.offers = others
			if !ls.owner.Stack { // a stacking owner's grants are separate locks
				for _, l := range o.left {
					ls.dropOffers(l) // what the owner accepted, no other refusal releases
				}
			}
		}
		return nil
	})
	out.raise(t, sink)
	t.dropIdle(ctx, who.Client)
	return err
}

func (v locks) TestLock(ctx context.Context, who Caller, f FileKey, o LockOwner, r ByteRange, exclusive bool) (*Lock, error) {
	t := v.t
	if o.Client != who.Client {
		return nil, ErrNotYours
	}
	if err := t.staleClient(ctx, who.Client); err != nil {
		return nil, err
	}
	var held *Lock
	err := t.step(f.ID, func(e *entry) error {
		// In grace the locks still to be reclaimed are not held yet, so a
		// test is refused as a lock is.
		if _, err := t.admit(who, f.Shard, false); err != nil {
			return err
		}
		held = e.blocker(o, LockRange{r, exclusive}, len(e.waiters))
		return nil
	})
	return held, err
}

func (v locks) Unlock(ctx context.Context, who Caller, f FileKey, ref OpenRef, o LockOwner, rs []ByteRange, q Seqs, replay *LockSequence) (uint32, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return 0, err
	}
	if len(rs) > maxRanges || len(o.Name) > maxName {
		return 0, ErrInvalid
	}
	if o.Client != who.Client {
		return 0, ErrNotYours
	}
	var seq uint32
	var out outbox
	err = t.step(f.ID, func(e *entry) error {
		op, ls, cur, err := t.lockTarget(e, who, ref)
		switch {
		case err != nil:
			return err
		case ls == nil:
			ls = e.lockState(o, openToken(op))
		case !sameOwner(ls.owner, o):
			return ErrBadState
		}
		return t.sequenced(locksOwner(o), q.Owner, seqOther, func(bool) error {
			if err := seqErr(cur, ref.State.Seq); err != nil {
				return err
			}
			if op != nil && !t.mayUse(op, who) {
				return ErrNotYours
			}
			if i, v, ok := lockSeq(op, replay); ok {
				if op.Durable.LockSeq[i] == v {
					return nil // a replay is answered, never applied again
				}
				t.resetLockSeq(e, op, i)
			}
			if ls == nil {
				if o.Stack && len(rs) > 0 {
					return ErrRangeNotLocked
				}
				return nil
			}
			// An unlock splitting a range is refused past the client's
			// ranges, as a lock is; one releasing nothing writes nothing.
			// Its place in the open's lock replay is stored only when every
			// range is released.
			next, err := ls.removing(rs)
			rec, stored := (*entity.OpenRecord)(nil), func() {}
			if err == nil {
				rec, stored = t.lockSeqRecord(e, op, replay)
			}
			if werr := t.relock(ctx, ls, next.locks, true, rec); werr != nil {
				return werr
			}
			stored()
			ls.offers = next.offers
			if err == nil {
				ls.ref.Seq, err = t.advance(e, ls.ref)
				seq = ls.ref.Seq
			}
			t.tidy(e, ls)
			t.serve(ctx, e, &out)
			return err
		})
	})
	out.raise(t, sink)
	t.dropIdle(ctx, who.Client)
	return seq, err
}

func (v locks) ReleaseLockOwner(ctx context.Context, who Caller, o LockOwner) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	if err := t.staleClient(ctx, who.Client); err != nil {
		return err
	}
	if o.Client != who.Client {
		return ErrNotYours
	}
	var files []entity.FileID
	for _, h := range t.heldBy(o.Client) {
		if h.ref.Type == StateLock && !slices.Contains(files, h.file) {
			files = append(files, h.file)
		}
	}
	// Both passes visit the files in a fixed order.
	slices.SortFunc(files, func(a, b entity.FileID) int { return bytes.Compare(a[:], b[:]) })
	holds := func(e *entry) error {
		for _, ls := range e.lstates {
			if sameOwner(ls.owner, o) && len(ls.locks) > 0 {
				return ErrLocksHeld
			}
		}
		return nil
	}
	for _, f := range files {
		if err := t.step(f, holds); err != nil {
			return err
		}
	}
	// decision: the owner is judged holding no lock file by file, then
	// released file by file, so a lock it is granted between the two passes
	// is refused here too, after the files before it were released. The
	// owner's requests are serial, so only a client racing its own release
	// meets it. Hold every file's step across both passes if one is seen.
	out := outbox{timer: true} // a release names no file, so no admission lock is held for its grants
	for _, f := range files {
		err = t.step(f, func(e *entry) error {
			if err := holds(e); err != nil {
				return err
			}
			for _, ls := range e.lstates {
				if sameOwner(ls.owner, o) {
					t.dropLockState(e, ls)
				}
			}
			t.serve(ctx, e, &out)
			return nil
		})
		if err != nil {
			break
		}
	}
	if err == nil {
		t.smu.Lock()
		delete(t.seqs, locksOwner(o))
		t.smu.Unlock()
	}
	out.raise(t, sink)
	t.serveFiles(ctx, sink, out.serve)
	t.dropIdle(ctx, o.Client)
	return err
}

// dropIdle deletes client c's record once c holds nothing here, if a restart
// notice is how a loss of its state reaches it: an NLM host's record goes with
// its last lock, so the hosts a failover notifies are the ones holding
// something. c is then no longer current here, and its next call connects it
// again. The check and the drop share the index lock, so no state is minted
// for c between them: a lock asked for c once it is dropped is ErrStaleClient,
// never granted under a record about to go, and the client step orders the
// delete with the Connect that answers it.
//
// decision: an NLM host's record goes with its last lock, because the
// records are the restart-notice list and a failover must notify no host
// holding nothing. The cost: a host that locks and unlocks one range in a
// loop pays a record write, its Connect, and a delete per cycle, and each
// lock after a delete is first refused ErrStaleClient for its adapter to
// connect it again. Keep the record through a lease period of idleness,
// swept by the timer, if NLM lock churn shows those writes in a profile. A
// record outlives idleness while a grace still waits on the host's reclaim,
// since the host holds nothing in a shard until it reclaims there. One left
// idle when grace ends stays until a later call of the host's leaves it idle.
// Until then each restart lists it in its shards: it is told again, the
// restart-notice number is raised for it, it may reclaim there, and those
// shards' graces run their full lease period for it. Drop the idle records a
// grace listed once the grace ends, if NLM hosts are seen holding graces open.
//
// decision: the record is deleted without asking whether a restart notice to
// the host is outstanding, since the table records none. A failed delete
// leaves the record, which only adds a host to the next failover's notices.
// Check for an outstanding notice once one is recorded.
func (t *Table) dropIdle(ctx context.Context, c entity.ClientID) {
	t.cmu.Lock()
	notice := t.known[c].recovery == entity.RecoverNotice
	t.cmu.Unlock()
	if !notice {
		return
	}
	// The grace is judged under the client step, the order ServeShard takes
	// it in, so a grace begun meanwhile is seen waiting on the host.
	t.cstep.Lock()
	defer t.cstep.Unlock()
	t.cmu.Lock()
	now := t.d.Clock.Elapsed()
	for _, sh := range t.shards {
		if _, ok := sh.reclaim[c]; ok && sh.inGrace(now) {
			t.cmu.Unlock()
			return // a grace still waits on its reclaim
		}
	}
	t.cmu.Unlock()
	t.idx.Lock()
	idle := !t.holdsAnyLocked(c)
	if idle {
		t.cmu.Lock()
		delete(t.known, c)
		delete(t.leases, c)
		t.cmu.Unlock()
	}
	t.idx.Unlock()
	if idle {
		_ = t.d.Records.Clients().Delete(ctx, c)
	}
}
