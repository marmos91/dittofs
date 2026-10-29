package openstate

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/conflict"
	"github.com/marmos91/dittofs/pkg/openstate/grantfsm"
)

// GrantKind is what a caching grant lets its holder cache: read, write and
// handle, in any combination holding read, or none.
type GrantKind = grantfsm.Kind

const (
	GrantNone   = grantfsm.None
	GrantRead   = grantfsm.Read
	GrantWrite  = grantfsm.Write
	GrantHandle = grantfsm.Handle
)

// LeaseKey is an SMB lease's key, the client's own: one grant per client,
// key and file covers every open of the file under it.
type LeaseKey [16]byte

// GrantID names a caching grant by its shard and token, as a StateRef does,
// so a call naming it with no file handle resolves from the ID alone. An NFS
// delegation's stateid is its reference, of type StateGrant.
type GrantID struct {
	Shard ShardID
	Token uint64
}

func (g GrantID) ref() StateRef { return StateRef{Type: StateGrant, Shard: g.Shard, Token: g.Token} }

// CachingGrant is a promise that no other holder is using the file in a way
// that conflicts with its Kind, so its holder may cache. It covers every open
// of its client under its Key, an oplock's one open, or, a delegation, none.
//
// decision: it carries nothing of the adapter's until a lease covering a
// persistent open is written; what the adapter keeps with a lease lands with
// that write.
type CachingGrant struct {
	ID     GrantID
	Client entity.ClientID
	File   entity.FileID
	Kind   GrantKind
	Key    LeaseKey // zero: it covers one open, or none
	Epoch  uint16   // a v2 lease's: starts at its request's, raised by every change of Kind sent to the holder
	V2     bool     // a v2 lease, whose Epoch is counted and sent; fixed when the lease is made
	Recall *Recall  // a recall in flight; nil: none
}

// Recall is a recall in flight.
type Recall struct {
	To GrantKind // the kind it goes to; none: a return
}

// Grants is the view of caching grants; it runs at the primary of the shard a
// GrantID names.
type Grants interface {
	// AckRecall is the holder's answer to a recall of g, to kind to; to
	// GrantNone returns a grant that covers no open. With no recall in
	// flight, a grant that covers no open is being returned or narrowed
	// unasked; one that covers opens is ErrNoRecall, and nothing changes,
	// as for an answer after its recall timed out. One claiming caching the
	// grant does not hold, or its recall did not offer, is ErrInvalid: an
	// oplock's recall then ends with the oplock at none, and a lease's stays
	// in flight. The waiters an answer frees are served under the file's
	// admission lock, which the table takes itself: the service must not
	// hold it when calling, though the answer names the file.
	AckRecall(ctx context.Context, who Caller, g GrantID, to GrantKind) error
}

type grants struct{ t *Table }

const (
	// decision: 4,096 grants per client per primary, 16 grants per file and
	// 4,096 recalls not yet handed to the Sink per primary, none measured.
	// Grants pay on files one user works on, and every grant held is memory
	// here and a recall a conflicting client may wait on. Raise them if a
	// measured workload loses grants it would have used.
	maxGrants = 4096
	// keepGrants is what a client reaching the budget is asked to keep.
	//
	// ponytail: a client at its budget is only asked through RecallAny; no
	// grant is recalled by last use, so an NFSv4.0 or SMB client, which cannot
	// be asked, stays at the budget until it closes or returns grants (RFC 14
	// §5.5). Add a per-client last-use list and recall from its tail when one
	// is seen pinned there.
	keepGrants = maxGrants / 2
	maxShared  = 16
	maxUnsent  = 4096

	// coversDeadline is the recall deadline of a grant that covers opens,
	// the one Windows servers use for an SMB lease break; a grant that
	// covers none, a delegation, has one lease period.
	coversDeadline = 35 * time.Second
)

// grant is a caching grant as the table holds it.
type grant struct {
	id      GrantID
	client  entity.ClientID
	key     LeaseKey
	open    *open // an oplock: the one open it covers; nil for a lease and a delegation
	fsm     grantfsm.Grant
	gen     uint32        // raised by each recall started, naming it to the arming of its deadline
	sent    time.Duration // the deadline of the recall in flight, once handed to the Sink; zero: none, or not yet handed
	counted bool          // it is in its client's budget: it caches something
}

type clientKey struct {
	client entity.ClientID
	key    LeaseKey
}

// keyUse is the file a client's lease key is held on, and the opens holding
// it there: a key is held from its first open, granted or not, to its last
// close, and names one file meanwhile.
type keyUse struct {
	file  entity.FileID
	opens int
}

// covers reports whether g covers opens: a lease, or an oplock.
func (g *grant) covers() bool { return g.key != (LeaseKey{}) || g.open != nil }

func (g *grant) deadline() time.Duration {
	if g.covers() {
		return coversDeadline
	}
	return leasePeriod
}

// heldBy reports whether g is the holder's of client c, under lease key key,
// through open o: a lease is its key's, an oplock its one open's, and a
// delegation its client's.
func (g *grant) heldBy(c entity.ClientID, key LeaseKey, o *open) bool {
	switch {
	case g.client != c:
		return false
	case g.key != (LeaseKey{}):
		return g.key == key
	case g.open != nil:
		return g.open == o
	}
	return true
}

// public is g as a caller sees it, on e's file.
func (g *grant) public(f entity.FileID) CachingGrant {
	p := CachingGrant{ID: g.id, Client: g.client, File: f, Kind: g.fsm.Kind, Key: g.key, Epoch: g.fsm.Epoch, V2: g.fsm.V2}
	if r := g.fsm.Recall; r != nil {
		p.Recall = &Recall{To: r.To}
	}
	return p
}

// ownedBy reports whether open h is, beside the grant a holder of client c
// under key key through open o would hold, that holder's own: an open under
// its lease key, its oplock's open, or any open of a delegation's client.
func (h *open) ownedBy(c entity.ClientID, key LeaseKey, o *open, smb bool) bool {
	switch {
	case h.Client != c:
		return false
	case key != (LeaseKey{}):
		return h.lease == key
	case smb:
		return h == o
	}
	return true
}

// recallOthers recalls each grant on e that own does not claim, under e's
// step, to what keeps leaves it, a grant that covers no open to none, and is
// ErrDelay with DelayRecall while any of them holds more than that: its recall
// started here or already in flight, and not ended as it was sent. A request that would start recalls past
// maxUnsent is ErrDelay with DelayResources, and starts and narrows none.
func (t *Table) recallOthers(e *entry, own func(*grant) bool, keeps func(*grant) GrantKind, out *outbox) error {
	type recall struct {
		g       *grant
		allowed GrantKind
	}
	var hit []recall
	starts := 0
	for _, g := range e.grants {
		if own(g) {
			continue
		}
		a := keeps(g)
		if grantfsm.Within(g.fsm.Kind, a) == g.fsm.Kind {
			continue
		}
		if !g.covers() {
			a = GrantNone
		}
		hit = append(hit, recall{g, a})
		if g.fsm.Recall == nil {
			starts++
		}
	}
	if len(hit) == 0 {
		return nil
	}
	if starts > 0 {
		t.idx.Lock()
		full := t.unsent+starts > maxUnsent
		if !full {
			t.unsent += starts
		}
		t.idx.Unlock()
		if full {
			return &DelayError{DelayResources}
		}
	}
	delay := false
	for _, r := range hit {
		if r.g.fsm.Break(r.allowed) {
			t.queueRecall(e, r.g, out)
		}
		delay = delay || r.g.fsm.Recall != nil
	}
	if !delay {
		return nil
	}
	return &DelayError{DelayRecall}
}

// queueRecall adds g's recall in flight, already counted in unsent, to out.
// An SMB break of read caching alone asks no acknowledgement (MS-SMB2
// 3.3.4.6, 3.3.4.7): it ends as it is queued, and nothing waits on it.
func (t *Table) queueRecall(e *entry, g *grant, out *outbox) {
	g.gen++
	g.sent = 0
	out.recalls = append(out.recalls, recallOut{g: g.public(e.file), gen: g.gen})
	if g.covers() && g.fsm.Kind&(GrantWrite|GrantHandle) == 0 {
		_, _ = g.fsm.Ack(g.fsm.Recall.To)
		t.recount(g)
	}
}

// arm starts the deadline of the recall r handed to the Sink, if it is still
// the one in flight, and marks its file for the timer.
func (t *Table) arm(r recallOut) {
	_ = t.step(r.g.File, func(e *entry) error {
		g := e.grants[r.g.ID.Token]
		if g == nil || g.gen != r.gen || g.fsm.Recall == nil {
			return nil
		}
		g.sent = t.d.Clock.Elapsed() + g.deadline()
		t.idx.Lock()
		t.recalling[e.file] = struct{}{}
		t.idx.Unlock()
		return nil
	})
}

// breakHandles answers a deny-mode conflict with the opens hs: while a grant
// with handle caching covers one of them and is not own's, that caching is
// recalled and the answer is ErrDelay; only once none does is the conflict
// ErrShareViolation.
func (t *Table) breakHandles(e *entry, hs []*open, own func(*grant) bool, out *outbox) error {
	if len(hs) == 0 {
		return nil
	}
	covering := func(g *grant) bool {
		for _, h := range hs {
			if h.Grant == g.id {
				return true
			}
		}
		return false
	}
	err := t.recallOthers(e, func(g *grant) bool { return own(g) || !covering(g) }, func(*grant) GrantKind { return ^GrantHandle }, out)
	if err != nil {
		return err
	}
	return ErrShareViolation
}

// offer is the grant covering o, of e's file, after its open by who asking r:
// the lease under r's key, widened if r asks more and nothing refuses it; or a
// new grant of the most of r.Want that nothing refuses; or nil.
func (t *Table) offer(e *entry, f FileKey, who Caller, o *open, r OpenRequest, out *outbox) *CachingGrant {
	smb := who.Protocol == entity.ProtocolSMB
	want := r.Want
	if !smb {
		want &^= GrantHandle // a delegation is never a handle grant
	}
	if r.SetID {
		want &^= GrantWrite
	}
	var mine *grant
	for _, g := range e.grants {
		if g.heldBy(who.Client, r.Lease, o) {
			mine = g
		}
	}
	k := t.offerable(e, who, o, r.Lease, want)
	switch {
	case mine != nil && mine.key != (LeaseKey{}):
		o.Grant = mine.id
		t.idx.Lock()
		budget := t.granted[who.Client] >= maxGrants
		t.idx.Unlock()
		if mine.counted || !budget {
			mine.fsm.Upgrade(want, k)
			if t.recount(mine) {
				out.full = append(out.full, who.Client)
			}
		}
		p := mine.public(e.file)
		return &p
	case mine != nil, k == GrantNone:
		return nil
	}
	var epoch *uint16
	if r.Lease != (LeaseKey{}) {
		epoch = r.Epoch
	}
	ref, err := t.mintUnder(e, who.Client, f.Shard, StateGrant, func(StateRef) error {
		if !t.current(who.Client) || t.granted[who.Client] >= maxGrants {
			return ErrDelay
		}
		if t.granted[who.Client]++; t.granted[who.Client] == maxGrants {
			out.full = append(out.full, who.Client)
		}
		return nil
	})
	if err != nil {
		return nil // a grant is offered, never owed: the open stands without one
	}
	g := &grant{id: GrantID{ref.Shard, ref.Token}, client: who.Client, key: r.Lease, fsm: grantfsm.New(k, epoch), counted: true}
	if smb && r.Lease == (LeaseKey{}) {
		g.open = o
	}
	if g.covers() {
		for _, h := range e.opens {
			if h == o || r.Lease != (LeaseKey{}) && h.Client == who.Client && h.lease == r.Lease {
				h.Grant = g.id
			}
		}
	}
	if e.grants == nil {
		e.grants = map[uint64]*grant{}
	}
	e.grants[ref.Token] = g
	p := g.public(e.file)
	return &p
}

// offerable is the most of want a grant to who, through o under key, may hold
// on e: none to a client fenced, at its budget or with its back channel down,
// on a file with a recall in flight, recalled within a recall deadline, with
// a lock waiter or another holder's byte-range lock, or under 16 grants
// already; and no caching another holder's
// open or grant refuses. The budget is checked where the grant is minted: a
// client at it keeps the grants it holds, and was asked through RecallAny,
// when it reached it, to return some.
//
// ponytail: a file is offered no grant while any recall on it is in flight or
// within one recall deadline of ending, so up to 90 s after a delegation's,
// and while any lock waiter is on it, whatever the request delayed there; a
// grant that would not conflict with it is refused too. Narrow it to grants
// that conflict with the delayed requests when files are measured going
// ungranted after recalls a new grant would not have raced.
func (t *Table) offerable(e *entry, who Caller, o *open, key LeaseKey, want GrantKind) GrantKind {
	caching := 0
	for _, g := range e.grants {
		if g.fsm.Kind != GrantNone {
			caching++
		}
	}
	if want == GrantNone || len(e.waiters) > 0 || caching >= maxShared {
		return GrantNone
	}
	now := t.d.Clock.Elapsed()
	t.cmu.Lock()
	k, ok := t.known[who.Client]
	fenced := now < t.fenced[who.Client]
	t.cmu.Unlock()
	t.idx.Lock()
	recalled := now < t.recalled[e.file]
	t.idx.Unlock()
	if !ok || k.down || fenced || recalled {
		return GrantNone
	}
	smb := who.Protocol == entity.ProtocolSMB
	for _, ls := range e.lstates {
		if len(ls.locks) > 0 && (ls.open == nil && ls.owner.Client != who.Client || ls.open != nil && !ls.open.ownedBy(who.Client, key, o, smb)) {
			return GrantNone // its holder taking locks locally could overlap these
		}
	}
	for _, h := range e.opens {
		if !h.ownedBy(who.Client, key, o, smb) {
			want = grantfsm.Within(want, conflict.GrantKeeps(h.Mode.Access, h.Mode.Deny, key != (LeaseKey{})))
		}
	}
	for _, g := range e.grants {
		switch {
		case g.heldBy(who.Client, key, o):
		case g.fsm.Recall != nil:
			return GrantNone
		case !grantfsm.Compatible(want, g.fsm.Kind):
			want = grantfsm.Within(want, ^GrantWrite)
			if !grantfsm.Compatible(want, g.fsm.Kind) {
				return GrantNone
			}
		}
	}
	return grantfsm.Within(want, want)
}

// uncover drops the grant covering o, closing on e, once o was the last open
// it covered, and reports whether it did.
func (t *Table) uncover(e *entry, o *open) bool {
	g := e.grants[o.Grant.Token]
	if o.Grant == (GrantID{}) || g == nil {
		return false
	}
	for _, h := range e.opens {
		if h != o && h.Grant == g.id {
			return false
		}
	}
	t.dropGrant(e, g)
	return true
}

// dropGrant drops g from e, under its step, with its reference and its count.
// A recall in flight ends with it, so the request it delayed is not
// overtaken by a grant to a holder that closed and opened again.
func (t *Table) dropGrant(e *entry, g *grant) {
	if g.fsm.Recall != nil {
		t.recallEnded(e, g)
	}
	g.fsm.Kind = GrantNone
	t.recount(g)
	delete(e.grants, g.id.Token)
	t.release(e, g.id.ref())
}

// recount keeps g in its client's budget while it caches anything: a grant cut
// to none, kept for the opens it covers, holds none of it. It reports whether
// that brought the client to the budget.
func (t *Table) recount(g *grant) bool {
	held := g.fsm.Kind != GrantNone
	if held == g.counted {
		return false
	}
	g.counted = held
	t.idx.Lock()
	defer t.idx.Unlock()
	if held {
		t.granted[g.client]++
		return t.granted[g.client] == maxGrants
	}
	if t.granted[g.client]--; t.granted[g.client] <= 0 {
		delete(t.granted, g.client)
	}
	return false
}

// recallEnded records that a recall of g on e's file ended: the file is
// offered no grant for one of g's recall deadlines, so the request it delayed
// is not overtaken by a new grant.
func (t *Table) recallEnded(e *entry, g *grant) {
	g.sent = 0
	until := t.d.Clock.Elapsed() + g.deadline()
	t.idx.Lock()
	defer t.idx.Unlock()
	t.recalled[e.file] = max(t.recalled[e.file], until)
}

// lapse ends each recall on e past its deadline, under e's step and the
// file's admission lock, since it adds to the revoked set: a grant that covers
// opens is cut to none and its opens stay; one that covers none is revoked
// into its client's revoked set and reported. Either fences its client from
// grants here for a lease period.
//
// decision: a missed recall recalls none of the client's other grants, though
// a client that misses one may be missing them all; the fence keeps it from
// new grants, and each grant it holds lapses on its own once it conflicts.
// Recall the rest at the first lapse if a client is seen holding grants it no
// longer answers for long past one.
func (t *Table) lapse(e *entry, out *outbox) {
	now := t.d.Clock.Elapsed()
	for _, g := range e.grants {
		if g.fsm.Recall == nil || g.sent == 0 || now < g.sent {
			continue
		}
		t.cmu.Lock()
		t.fenced[g.client] = now + leasePeriod
		t.cmu.Unlock()
		t.recallEnded(e, g)
		if g.covers() {
			g.fsm.Timeout()
			t.recount(g)
			continue
		}
		t.idx.Lock()
		t.revoked[refKey{g.id.Shard, g.id.Token}] = revokedRef{g.client, StateGrant, RevokedGrant}
		t.idx.Unlock()
		t.dropGrant(e, g)
		out.revoked = append(out.revoked, Revocation{Client: g.client, Grant: g.id, Reason: RevokedGrant})
	}
	for _, g := range e.grants {
		if g.sent != 0 {
			return
		}
	}
	t.idx.Lock()
	delete(t.recalling, e.file)
	t.idx.Unlock()
}

// cachedByOther reports whether a grant with read or write caching that
// waiter w's request does not hold is on e, which w waits on.
func (e *entry) cachedByOther(w *LockWaiter) bool {
	var op *open
	if ls := e.lockState(w.Owner, w.Open); ls != nil {
		op = ls.open
	}
	var key LeaseKey
	if op != nil {
		key = op.lease
	}
	for _, g := range e.grants {
		if !g.heldBy(w.Owner.Client, key, op) && g.fsm.Kind != GrantNone {
			return true
		}
	}
	return false
}

func (v grants) AckRecall(ctx context.Context, who Caller, id GrantID, to GrantKind) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	h, err := t.resolve(who.Client, entity.FileID{}, id.ref())
	if err != nil {
		return err
	}
	out := outbox{timer: true}
	err = t.step(h.file, func(e *entry) error {
		if _, err := t.resolve(who.Client, e.file, id.ref()); err != nil {
			return err
		}
		g := e.grants[id.Token]
		if g == nil {
			return ErrBadState
		}
		inFlight := g.fsm.Recall != nil
		if !inFlight && g.covers() {
			return ErrNoRecall
		}
		next, err := g.fsm.Ack(to)
		if err != nil {
			if !inFlight || g.open == nil {
				return ErrInvalid
			}
			g.fsm.Timeout()
			err = ErrInvalid
		}
		switch {
		case next:
			// decision: only a recall's first stage is held to maxUnsent; a
			// later one is queued past it, since refusing it would leave the
			// grant mid-recall with nothing in flight to end it, and the
			// stages are bounded by the grants already recalling. Count them
			// against the bound if stages are seen filling it.
			t.idx.Lock()
			t.unsent++
			t.idx.Unlock()
			t.queueRecall(e, g, &out)
		case inFlight && g.fsm.Recall == nil:
			t.recallEnded(e, g)
		}
		if !g.covers() && g.fsm.Kind == GrantNone {
			t.dropGrant(e, g)
		} else {
			t.recount(g)
		}
		t.serve(e, &out)
		return err
	})
	out.raise(t, sink)
	t.serveFiles(sink, out.serve)
	return err
}
