package openstate

import (
	"context"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/openstate/grantfsm"
)

// persistent reports whether o is a persistent open, whose every change is
// written before it is answered.
func (o *open) persistent() bool { return o.Durability == entity.DurabilityPersistent }

// recordOf is o's record, with the lease covering it, on e.
func (t *Table) recordOf(e *entry, o *open) entity.OpenRecord {
	r := o.record()
	if g := e.grants[o.Grant.Token]; g != nil && g.id == o.Grant && g.key != (LeaseKey{}) {
		r.Lease = &entity.Lease{Key: [16]byte(g.key), ParentKey: [16]byte(g.parent), State: smbState(g.fsm.Kind), Epoch: g.fsm.Epoch, V2: g.fsm.V2}
	}
	return r
}

// persist writes o's record over its stored one, under e's step.
func (t *Table) persist(ctx context.Context, e *entry, o *open) error {
	err := t.d.Records.Opens().Put(ctx, o.share, t.recordOf(e, o))
	if err == nil {
		o.unsaved = false
	}
	return err
}

// save is persist for a change already made, which no caller can be
// answered for: a failed write marks o unsaved, and the timer writes it
// again on each sweep. Meanwhile the file is offered no grant and o is not
// reconnected, so no client is told what a failover would undo.
func (t *Table) save(ctx context.Context, e *entry, o *open) {
	if t.persist(ctx, e, o) == nil {
		return
	}
	o.unsaved = true
	t.idx.Lock()
	t.parked[e.file] = struct{}{}
	t.idx.Unlock()
}

// unsaved reports whether an open of e holds a change its record lacks.
func (e *entry) unsaved() bool {
	for _, o := range e.opens {
		if o.unsaved {
			return true
		}
	}
	return false
}

// persistLease writes the record of each persistent open on e that g
// covers, so each carries the lease's kind and epoch as they now stand.
func (t *Table) persistLease(ctx context.Context, e *entry, g *grant) error {
	for _, h := range e.opens {
		if h.persistent() && h.Grant == g.id {
			if err := t.persist(ctx, e, h); err != nil {
				return err
			}
		}
	}
	return nil
}

// persistLeaseAs is persistLease with g as fsm would leave it, written
// before g is changed so a refused write changes nothing.
func (t *Table) persistLeaseAs(ctx context.Context, e *entry, g *grant, fsm grantfsm.Grant) error {
	keep := g.fsm
	g.fsm = fsm
	defer func() { g.fsm = keep }()
	return t.persistLease(ctx, e, g)
}

// cloneFSM is f with its own copy of the recall in flight, which its
// methods change in place.
func cloneFSM(f grantfsm.Grant) grantfsm.Grant {
	if f.Recall != nil {
		r := *f.Recall
		f.Recall = &r
	}
	return f
}

// handleCached reports whether o is covered by a grant holding handle
// caching, the one under which a durable open is kept.
func (e *entry) handleCached(o *open) bool {
	g := e.grants[o.Grant.Token]
	return g != nil && g.id == o.Grant && g.fsm.Kind&GrantHandle != 0
}

// sessionKey names one session of one client.
//
// ponytail: an ended session is remembered for a lease period, against a
// call that names it still in flight, and a call slower than that can
// attach an open to it. Keep it until the client ends if such a call is
// seen.
type sessionKey struct {
	client  entity.ClientID
	session entity.SessionRef
}

// endedLocked reports whether session s of client c has ended here, so an
// open may no longer be attached to it; the caller holds idx.
func (t *Table) endedLocked(c entity.ClientID, s entity.SessionRef) bool {
	_, gone := t.ended[sessionKey{c, s}]
	return s != (entity.SessionRef{}) && gone
}

// persistOpen writes o, just granted on e, where it is persistent, and the
// persistent opens the lease covering it covers, whose kind the open may
// have widened. A failed write takes o back out, as if it was refused; a
// widened kind is left in memory, where it is wider than what the client was
// told and what is stored.
//
// decision: a failed write's record, which the store may still have
// committed, is not deleted: the store is failing writes, and the next
// start installs the record and closes it at its Timeout. Delete it here if
// a store is found failing one write and not the next.
func (t *Table) persistOpen(ctx context.Context, e *entry, o *open) error {
	err := error(nil)
	if g := e.grants[o.Grant.Token]; g != nil && g.id == o.Grant {
		err = t.persistLease(ctx, e, g)
	}
	if err == nil && o.persistent() && (o.Grant == (GrantID{}) || e.grants[o.Grant.Token] == nil) {
		err = t.persist(ctx, e, o)
	}
	if err != nil {
		t.uncover(e, o)
		t.dropOpen(e, o)
		return err
	}
	o.durable = o.durable || o.persistent()
	return nil
}

// smbState is k as an SMB lease state numbers it: read 1, handle 2, write 4.
func smbState(k GrantKind) uint8 {
	var s uint8
	if k&GrantRead != 0 {
		s |= 1
	}
	if k&GrantHandle != 0 {
		s |= 2
	}
	if k&GrantWrite != 0 {
		s |= 4
	}
	return s
}

// kindOf is the GrantKind an SMB lease state names.
func kindOf(s uint8) GrantKind {
	var k GrantKind
	if s&1 != 0 {
		k |= GrantRead
	}
	if s&2 != 0 {
		k |= GrantHandle
	}
	if s&4 != 0 {
		k |= GrantWrite
	}
	return k
}

func (v clients) Disconnect(ctx context.Context, c entity.ClientID, s entity.SessionRef) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	// A disconnect names a client, not a file, so the waiters its closes
	// free are served after it, under each file's admission lock.
	out := outbox{timer: true}
	// The session is marked ended in the critical section that lists the
	// client's state, so an open minted for it is either listed here or
	// refused at its mint.
	t.idx.Lock()
	t.ended[sessionKey{c, s}] = t.d.Clock.Elapsed()
	t.idx.Unlock()
	for _, h := range t.heldBy(c) {
		if h.ref.Type != StateOpen {
			continue
		}
		_ = t.step(h.file, func(e *entry) error {
			switch o := e.opens[h.ref.Token]; {
			case o == nil || o.Session != s || o.parked != 0:
			case o.Durability == entity.DurabilityNone, !o.persistent() && !e.handleCached(o):
				// A durable open is kept only under handle caching, which
				// it was granted with and a break may have taken.
				t.closeByTable(ctx, e, o, &out)
			default:
				t.park(e, o, &out)
			}
			return nil
		})
	}
	out.raise(t, sink)
	t.serveFiles(ctx, sink, out.serve)
	return nil
}

// park keeps o, of e, disconnected until its Timeout passes. Its waiters
// go, a callback one answered not granted: no session is left to hold
// what they would be granted.
func (t *Table) park(e *entry, o *open, out *outbox) {
	o.parked = t.d.Clock.Elapsed() + o.Durable.Timeout
	for i := 0; i < len(e.waiters); {
		w := e.waiters[i]
		if w.Owner.Client != o.Client || w.Open != o.Ref.Token {
			i++
			continue
		}
		t.dropWaiter(e, i)
		if w.Wait == WaitCallback {
			out.over = append(out.over, waitOver{*w, false})
		}
	}
	t.idx.Lock()
	t.parked[e.file] = struct{}{}
	t.idx.Unlock()
}

// reconnects reports whether who may reconnect o at now, presenting m.
//
// decision: the client is matched on every reconnect, an oplock's included,
// as RFC 22 has every dialect served send its ClientGuid; MS-SMB2 3.3.5.9.7
// matches it only for a lease, and smbtorture's durable_v2_open reopen1a
// reconnects an oplock under a new ClientGuid. Match it only under a lease
// if that test is to pass.
func (o *open) reconnects(who Caller, m ReconnectMatch, now time.Duration) bool {
	return o.Durable != nil && o.parked != 0 && now < o.parked &&
		o.Client == who.Client && o.Principal == who.Principal && o.share == m.Share &&
		o.Durable.Create == m.Create && o.lease == m.Lease
}

func (v opens) Reconnect(ctx context.Context, who Caller, id entity.OpenID, m ReconnectMatch, s entity.SessionRef) (OpenResult, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return OpenResult{}, err
	}
	t.idx.Lock()
	k, ok := t.byID[id]
	f := t.refs[k].file
	t.idx.Unlock()
	if !ok {
		return OpenResult{}, ErrNoReclaim
	}
	if _, begun := t.instance(k.shard); !begun {
		return OpenResult{}, ErrDelay // its shard is being installed
	}
	var res OpenResult
	err := t.step(f, func(e *entry) error {
		o := e.opens[k.token]
		t.idx.Lock()
		ended := t.endedLocked(who.Client, s)
		t.idx.Unlock()
		switch {
		case o == nil || ended || !o.reconnects(who, m, t.d.Clock.Elapsed()):
			return ErrNoReclaim
		case o.unsaved:
			return ErrDelay // its record is written again by the timer first
		}
		if o.persistent() {
			rec := t.recordOf(e, o)
			rec.Session = s
			if err := t.d.Records.Opens().Put(ctx, o.share, rec); err != nil {
				return err
			}
		}
		o.Session, o.parked = s, 0
		t.unowe(o)
		t.cmu.Lock()
		delete(t.adopted, o.Client) // it came back: its installed state is its own again
		t.cmu.Unlock()
		res.Open = o.public()
		if g := e.grants[o.Grant.Token]; g != nil && g.id == o.Grant {
			p := g.public(e.file)
			res.Grant = &p
		}
		return nil
	})
	return res, err
}

func (v opens) Flushed(ctx context.Context, who Caller, id entity.OpenID, loss uint64) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	return t.withOpen(who, OpenRef{Kind: RefOpened, Open: id}, nil, seqOther, func(_ *entry, o *open) error {
		if loss <= o.LossSeen {
			return nil
		}
		o.LossSeen = loss
		return ErrLost
	})
}

// timeouts writes again each record of e a failed write left unsaved, and
// closes each open whose Timeout passed since its session ended, as a close
// does, under e's step and the file's admission lock, since a close may
// grant a waiter; the file leaves the timer's set once it holds neither.
func (t *Table) timeouts(ctx context.Context, e *entry, out *outbox) {
	now, kept := t.d.Clock.Elapsed(), false
	for _, o := range e.opens {
		if o.unsaved {
			t.save(ctx, e, o)
		}
		kept = kept || o.unsaved
		switch {
		case o.parked == 0:
		case now >= o.parked:
			t.closeByTable(ctx, e, o, out)
		default:
			kept = true
		}
	}
	if !kept {
		t.idx.Lock()
		delete(t.parked, e.file)
		t.idx.Unlock()
	}
}

// unowe ends o's place in its shard's grace: its client no longer waits on
// it, as it reconnected or closed.
func (t *Table) unowe(o *open) {
	sh := o.owedTo
	if sh == nil {
		return
	}
	o.owedTo = nil
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if sh.owed[o.Client]--; sh.owed[o.Client] <= 0 {
		delete(sh.owed, o.Client)
	}
	sh.settle()
}

// unadopt drops the client c that install made known here, once it holds
// no state and never came back to claim what was installed for it.
func (t *Table) unadopt(c entity.ClientID) {
	t.idx.Lock()
	defer t.idx.Unlock()
	if t.holdsAnyLocked(c) {
		return
	}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if _, ok := t.adopted[c]; ok {
		delete(t.adopted, c)
		delete(t.known, c)
		delete(t.leases, c)
	}
}

// install puts every persistent open of shard s of share into the
// table with its locks, its lease and its file's delete pending,
// disconnected since now, before s answers anything: so its deny modes,
// locks and lease conflict as they did until it is reconnected or its
// Timeout passes, which closes it as a close does. Only a client the
// shard's records name and that recovers by reconnecting is waited for by
// grace; an open of any other client, ended or with no record, is kept
// only to its Timeout. sh is the shard's new serving, not yet installed;
// what the installed leases raise goes to out.
//
// ponytail: the references minted here carry the instance bits of no
// shard, since s is installed after them, so a miss on one once its state
// is gone reads stale rather than bad. Mint under sh's instance if an
// adapter is found telling the two apart for a FileId or a lease.
func (t *Table) install(ctx context.Context, share entity.ShareID, s ShardID, sh *served, clients map[entity.ClientID]entity.ClientRecord, out *outbox) error {
	ops := t.d.Records.Opens()
	for d, err := range ops.Durable(ctx, share, uint64(s)) {
		if err != nil {
			return err
		}
		rec, err := ops.Get(ctx, share, d.File, d.Open)
		if err != nil {
			return err
		}
		if rec.Durability != entity.DurabilityPersistent {
			continue
		}
		locks, err := ops.Locks(ctx, share, d.File, d.Open)
		if err != nil {
			return err
		}
		dp, held, err := ops.DeletePending(ctx, share, d.File)
		if err != nil {
			return err
		}
		var pending *entity.DeleteRecord
		if held {
			pending = &dp
		}
		cl, named := clients[rec.Client]
		if !named {
			cl = entity.ClientRecord{ID: rec.Client, Protocol: entity.ProtocolSMB}
		}
		if err := t.reinstate(ctx, share, s, sh, cl, rec, locks, pending, out); err != nil {
			return err
		}
	}
	return nil
}

// reinstate is install of one open, rec, of client cl, with its locks and
// its file's delete pending, if it has one.
func (t *Table) reinstate(ctx context.Context, share entity.ShareID, s ShardID, sh *served, cl entity.ClientRecord, rec entity.OpenRecord, locks []entity.LockRecord, pending *entity.DeleteRecord, out *outbox) error {
	t.cmu.Lock()
	if _, ok := t.known[cl.ID]; !ok {
		t.leases[cl.ID] = t.leaseEnd(cl.Protocol)
		t.know(cl)
		t.adopted[cl.ID] = struct{}{}
	}
	t.cmu.Unlock()
	o := &open{share: share, durable: true, parked: t.d.Clock.Elapsed() + rec.Timeout, Open: Open{
		ID: rec.ID, Client: rec.Client, Principal: rec.Principal, File: rec.File, Via: rec.Via, Name: rec.Name,
		Mode:       OpenMode{Access: rec.Access, Deny: rec.Deny, Privileged: rec.Privileged},
		Delete:     DeleteMark{OnClose: rec.DeleteOnClose, By: rec.DeleteBy},
		Suspended:  rec.Suspended,
		Durability: rec.Durability, Session: rec.Session, Durable: &DurableKeys{Timeout: rec.Timeout, Create: rec.CreateGUID},
	}}
	if cl.Recovery == entity.RecoverReconnect {
		o.owedTo = sh
	}
	if rec.Lease != nil {
		o.lease = LeaseKey(rec.Lease.Key)
	}
	return t.step(o.File, func(e *entry) error {
		if err := t.mintOpen(e, o, s); err != nil {
			return err
		}
		o.adopted = true
		if l := rec.Lease; l != nil {
			if err := t.reinstateLease(e, o, s, l, out); err != nil {
				return err
			}
		}
		for _, l := range locks {
			ls, _, err := t.lockStateFor(ctx, e, LockOwner{Client: o.Client, Name: l.Owner.Owner, Stack: true}, o, s)
			if err != nil {
				return err
			}
			ls.locks = append(ls.locks, LockRange{ByteRange{Offset: l.Offset, Length: l.Length}, l.Exclusive})
		}
		if e.pending == nil {
			e.pending = pending
		}
		t.idx.Lock()
		t.parked[e.file] = struct{}{}
		t.idx.Unlock()
		if o.owedTo != nil {
			t.cmu.Lock()
			sh.owed[o.Client]++
			t.cmu.Unlock()
		}
		return nil
	})
}

// reinstateLease covers o, just installed on e, with its lease l: the one
// already installed under l's key, or a new one of l's kind and epoch,
// counted in its client's budget as any grant is.
func (t *Table) reinstateLease(e *entry, o *open, s ShardID, l *entity.Lease, out *outbox) error {
	for _, g := range e.grants {
		if g.client == o.Client && g.key == o.lease {
			o.Grant = g.id
			return nil
		}
	}
	k := kindOf(l.State)
	ref, err := t.mintUnder(e, o.Client, s, StateGrant, func(StateRef) error {
		if k == GrantNone {
			return nil
		}
		if t.granted[o.Client]++; t.granted[o.Client] == maxGrants {
			out.full = append(out.full, o.Client)
		}
		return nil
	})
	if err != nil {
		return err
	}
	g := &grant{id: GrantID{s, ref.Instance, ref.Token}, client: o.Client, key: o.lease, parent: LeaseKey(l.ParentKey),
		fsm: grantfsm.Grant{Kind: k, Epoch: l.Epoch, V2: l.V2}, counted: k != GrantNone}
	if e.grants == nil {
		e.grants = map[uint64]*grant{}
	}
	e.grants[ref.Token] = g
	o.Grant = g.id
	return nil
}

// keepLocks writes locks as owner's stored locks under op where op is
// persistent: each is written at its offset under its position, then each
// stored lock of the owner not written over is deleted, so a write that
// stops part way leaves more locks stored, never fewer. It runs before the
// change is made, so a refused write changes nothing held.
//
// decision: a lock's position names it among its owner's under one open,
// with no ID drawn, and one owner's positions may land on another's at the
// same offset. SMB, the one protocol with persistent opens, names one owner
// per open. Draw IDs from NextID if a protocol is served whose persistent
// opens hold several owners' locks.
func (t *Table) keepLocks(ctx context.Context, o *open, lo LockOwner, locks []LockRange) error {
	if o == nil || !o.persistent() {
		return nil
	}
	ops, f := t.d.Records.Opens(), o.opened()
	stored, err := ops.Locks(ctx, o.share, f, o.ID)
	if err != nil {
		return err
	}
	owner := entity.LockOwner{Client: lo.Client, Owner: lo.Name}
	for i, l := range locks {
		r := entity.LockRecord{ID: entity.LockID(i + 1), Open: o.ID, Owner: owner, Offset: l.Offset, Length: l.Length, Exclusive: l.Exclusive}
		if err := ops.SetLocks(ctx, o.share, f, o.ID, nil, []entity.LockRecord{r}, nil); err != nil {
			return err
		}
	}
	for _, r := range stored {
		over := int(r.ID) <= len(locks) && locks[r.ID-1].Offset == r.Offset
		if !over && sameOwner(LockOwner{Client: r.Owner.Client, Name: r.Owner.Owner}, lo) {
			if err := ops.SetLocks(ctx, o.share, f, o.ID, nil, nil, []entity.LockRecord{r}); err != nil {
				return err
			}
		}
	}
	return nil
}

// adding is ls's locks with rs added, as add takes them, leaving ls as it is.
func (ls *lockState) adding(rs ...LockRange) []LockRange {
	n := lockState{owner: ls.owner, locks: slices.Clone(ls.locks)}
	for _, r := range rs {
		n.add(r)
	}
	return n.locks
}

// public is o as its caller sees it, with keys of its own.
func (o *open) public() Open {
	p := o.Open
	if p.Durable != nil {
		k := *p.Durable
		p.Durable = &k
	}
	return p
}

// keepPending writes the delete pending set, through o, or cleared by
// set, while a persistent open holds e's file, before it is applied, so a
// refused write changes nothing; nil set changes nothing.
//
// ponytail: only Update writes it. The delete pending a closing open with
// delete on close leaves its file, while a persistent open stays, is held
// in memory only, so a failover loses it; the persistent open's own mark is
// in its record. Write it in the close's step if such a close is seen
// before a failover.
func (t *Table) keepPending(ctx context.Context, e *entry, o *open, who Caller, set *bool) error {
	if set == nil || *set == (e.pending != nil) {
		return nil
	}
	var by *open
	for _, h := range e.opens {
		if h.persistent() {
			by = h
		}
	}
	switch {
	case by == nil:
		return nil
	case *set:
		return t.d.Records.Opens().SetDeletePending(ctx, o.share, e.file, by.ID, *pendingFrom(o, who.Principal))
	}
	return t.d.Records.Opens().ClearDeletePending(ctx, o.share, e.file)
}

// closeUnreclaimed closes, for each shard whose grace has ended since the
// sweep last looked, every durable open record of the shard that no open
// here holds, as a close would: the record goes, and a file it left with no
// entry and no open is raised Closed as an orphan, for the service to
// release. A persistent record is held by the open installed for it, which
// its Timeout closes. Each record is judged under its file's admission lock and
// step, so an unlink writing the records of the opens held, or a reclaim
// writing its own, never loses one; a failed pass is run again at the next
// sweep.
//
// ponytail: a record of a named stream is judged under the stream's
// admission lock, not its base file's, since the record names only the
// stream. Read the stream's base first if an unlink of a base is seen racing
// the close of its stream's record.
//
// ponytail: a clone's source record is skipped, as nothing here makes one
// yet. Close it at the clone's last batch once clones are built.
func (t *Table) closeUnreclaimed(ctx context.Context, sink Sink) {
	now := t.d.Clock.Elapsed()
	due := map[ShardID]*served{}
	t.cmu.Lock()
	for s, sh := range t.shards {
		if sh.unclosed && !sh.inGrace(now) {
			sh.unclosed = false
			due[s] = sh
		}
	}
	t.cmu.Unlock()
	for s, sh := range due {
		if err := t.closeShardRecords(ctx, sink, s, sh.share); err != nil {
			t.cmu.Lock()
			sh.unclosed = true
			t.cmu.Unlock()
		}
	}
}

// closeShardRecords is closeUnreclaimed of shard s of share.
func (t *Table) closeShardRecords(ctx context.Context, sink Sink, s ShardID, share entity.ShareID) error {
	ops := t.d.Records.Opens()
	for d, err := range ops.Durable(ctx, share, uint64(s)) {
		if err != nil {
			return err
		}
		rec, err := ops.Get(ctx, share, d.File, d.Open)
		if merrs.IsNotFoundError(err) {
			continue // closed since the listing
		}
		if err != nil {
			return err
		}
		if rec.Kind == entity.OpenCloneSource {
			continue
		}
		var orphan entity.FileID
		unlock := t.order(d.File)
		err = t.step(d.File, func(*entry) error {
			t.idx.Lock()
			_, held := t.byID[d.Open]
			t.idx.Unlock()
			if held {
				return nil
			}
			o, err := ops.Delete(ctx, share, d.File, d.Open)
			if merrs.IsNotFoundError(err) {
				return nil
			}
			orphan = o.File
			return err
		})
		unlock()
		if err != nil {
			return err
		}
		if !orphan.IsZero() {
			sink.Closed(orphan, CloseResult{Orphan: true})
		}
	}
	return nil
}
