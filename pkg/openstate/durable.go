package openstate

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/grantfsm"
)

// persistent reports whether o is a persistent open, whose every change is
// written before it is answered.
func (o *open) persistent() bool { return o.Durability == entity.DurabilityPersistent }

// recordOf is o's record, with the lease covering it, on e.
func (t *Table) recordOf(e *entry, o *open) entity.OpenRecord {
	r := o.record()
	if g := e.grants[o.Grant.Token]; g != nil && g.id == o.Grant && g.key != (LeaseKey{}) {
		r.Lease = &entity.Lease{Key: [16]byte(g.key), State: smbState(g.fsm.Kind), Epoch: g.fsm.Epoch, V2: g.fsm.V2}
	}
	return r
}

// persist writes o's record over its stored one, under e's step.
func (t *Table) persist(ctx context.Context, e *entry, o *open) error {
	return t.d.Records.Opens().Put(ctx, o.share, t.recordOf(e, o))
}

// persistLease writes the record of each persistent open on e that g
// covers, so each carries the lease's kind and epoch as they now stand.
//
// decision: after an acknowledgement or a recall's timeout, which only
// narrow a lease, a failed write is not answered: the stored kind is then
// wider than the one held, which a failover reinstalls and the next conflict
// recalls again. Answer it, and keep the change unapplied, if a failover is
// seen leaving a client told it may cache what it gave back.
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
	for _, h := range t.heldBy(c) {
		if h.ref.Type != StateOpen {
			continue
		}
		_ = t.step(h.file, func(e *entry) error {
			switch o := e.opens[h.ref.Token]; {
			case o == nil || o.Session != s || o.parked != 0:
			case o.Durability == entity.DurabilityNone:
				t.closeByTable(ctx, e, o, &out)
			default:
				o.parked = t.d.Clock.Elapsed() + o.Durable.Timeout
				t.idx.Lock()
				t.parked[e.file] = struct{}{}
				t.idx.Unlock()
			}
			return nil
		})
	}
	out.raise(t, sink)
	t.serveFiles(ctx, sink, out.serve)
	return nil
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
		o.Client == who.Client && o.Principal == who.Principal &&
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
	var res OpenResult
	err := t.step(f, func(e *entry) error {
		o := e.opens[k.token]
		if o == nil || !o.reconnects(who, m, t.d.Clock.Elapsed()) {
			return ErrNoReclaim
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
		res.Open = o.Open
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

// timeouts closes each open of e whose Timeout passed since its session
// ended, as a close does, under e's step and the file's admission lock,
// since a close may grant a waiter; the file leaves the timer's set once it
// holds no disconnected open.
func (t *Table) timeouts(ctx context.Context, e *entry, out *outbox) {
	now, kept := t.d.Clock.Elapsed(), false
	for _, o := range e.opens {
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
	if !o.owed {
		return
	}
	o.owed = false
	t.cmu.Lock()
	defer t.cmu.Unlock()
	sh := t.shards[o.Ref.Shard]
	if sh == nil {
		return
	}
	if sh.owed[o.Client]--; sh.owed[o.Client] <= 0 {
		delete(sh.owed, o.Client)
	}
	sh.settle()
}

// install puts every persistent open of shard s of share into the
// table with its locks and its lease, disconnected since now, before s
// answers anything: so its deny modes, locks and lease conflict as they did
// until it is reconnected or its Timeout passes. An open whose client the
// shard's records do not name is left to its record. sh is the shard's new
// serving, not yet installed.
//
// ponytail: the references minted here carry the instance bits of no
// shard, since s is installed after them, so a miss on one once its state
// is gone reads stale rather than bad. Mint under sh's instance if an
// adapter is found telling the two apart for a FileId or a lease.
func (t *Table) install(ctx context.Context, share entity.ShareID, s ShardID, sh *served, clients map[entity.ClientID]entity.ClientRecord) error {
	ops := t.d.Records.Opens()
	for d, err := range ops.Durable(ctx, share, uint64(s)) {
		if err != nil {
			return err
		}
		rec, err := ops.Get(ctx, share, d.File, d.Open)
		if err != nil {
			return err
		}
		cl, ok := clients[rec.Client]
		if rec.Durability != entity.DurabilityPersistent || !ok {
			continue
		}
		locks, err := ops.Locks(ctx, share, d.File, d.Open)
		if err != nil {
			return err
		}
		if err := t.reinstate(ctx, share, s, sh, cl, rec, locks); err != nil {
			return err
		}
	}
	return nil
}

// reinstate is install of one open, rec, of client cl, with its locks.
func (t *Table) reinstate(ctx context.Context, share entity.ShareID, s ShardID, sh *served, cl entity.ClientRecord, rec entity.OpenRecord, locks []entity.LockRecord) error {
	t.cmu.Lock()
	if _, ok := t.known[cl.ID]; !ok {
		t.leases[cl.ID] = t.leaseEnd(cl.Protocol)
		t.know(cl)
	}
	t.cmu.Unlock()
	o := &open{share: share, durable: true, parked: t.d.Clock.Elapsed() + rec.Timeout, owed: cl.Recovery == entity.RecoverReconnect, Open: Open{
		ID: rec.ID, Client: rec.Client, Principal: rec.Principal, File: rec.File, Via: rec.Via, Name: rec.Name,
		Mode:       OpenMode{Access: rec.Access, Deny: rec.Deny, Privileged: rec.Privileged},
		Delete:     DeleteMark{OnClose: rec.DeleteOnClose, By: rec.DeleteBy},
		Suspended:  rec.Suspended,
		Durability: rec.Durability, Session: rec.Session, Durable: &DurableKeys{Timeout: rec.Timeout, Create: rec.CreateGUID},
	}}
	if rec.Lease != nil {
		o.lease = LeaseKey(rec.Lease.Key)
	}
	return t.step(o.File, func(e *entry) error {
		if err := t.mintOpen(e, o, s); err != nil {
			return err
		}
		if l := rec.Lease; l != nil {
			if err := t.reinstateLease(e, o, s, l); err != nil {
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
		t.idx.Lock()
		t.parked[e.file] = struct{}{}
		t.idx.Unlock()
		if o.owed {
			sh.owed[o.Client]++
		}
		return nil
	})
}

// reinstateLease covers o, just installed on e, with its lease l: the one
// already installed under l's key, or a new one of l's kind and epoch.
func (t *Table) reinstateLease(e *entry, o *open, s ShardID, l *entity.Lease) error {
	for _, g := range e.grants {
		if g.client == o.Client && g.key == o.lease {
			o.Grant = g.id
			return nil
		}
	}
	k := kindOf(l.State)
	ref, err := t.mintUnder(e, o.Client, s, StateGrant, func(StateRef) error {
		if k != GrantNone {
			t.granted[o.Client]++
		}
		return nil
	})
	if err != nil {
		return err
	}
	g := &grant{id: GrantID{s, ref.Instance, ref.Token}, client: o.Client, key: o.lease,
		fsm: grantfsm.Grant{Kind: k, Epoch: l.Epoch, V2: l.V2}, counted: k != GrantNone}
	if e.grants == nil {
		e.grants = map[uint64]*grant{}
	}
	e.grants[ref.Token] = g
	o.Grant = g.id
	return nil
}

// keepLocks writes locks as ls's stored locks where its open is persistent:
// each is written at its offset under its position, then each stored lock
// of ls's owner not written over is deleted, so a write that stops part way
// leaves more locks stored, never fewer.
//
// decision: a lock's position names it among its owner's under one open,
// with no ID drawn, and one owner's positions may land on another's at the
// same offset. SMB, the one protocol with persistent opens, names one owner
// per open. Draw IDs from NextID if a protocol is served whose persistent
// opens hold several owners' locks.
//
// ponytail: only Lock and Unlock write a persistent open's locks; a lock a
// callback waiter is granted when a range frees, and one its refusal gives
// back, are held in memory only, so a failover loses or keeps them as the
// last request left them. Write them in the serving step, which then needs
// the call's context, if persistent opens are seen waiting on locks.
func (t *Table) keepLocks(ctx context.Context, ls *lockState, locks []LockRange) error {
	o := ls.open
	if o == nil || !o.persistent() {
		return nil
	}
	ops, f := t.d.Records.Opens(), o.opened()
	stored, err := ops.Locks(ctx, o.share, f, o.ID)
	if err != nil {
		return err
	}
	owner := entity.LockOwner{Client: ls.owner.Client, Owner: ls.owner.Name}
	for i, l := range locks {
		r := entity.LockRecord{ID: entity.LockID(i + 1), Open: o.ID, Owner: owner, Offset: l.Offset, Length: l.Length, Exclusive: l.Exclusive}
		if err := ops.SetLocks(ctx, o.share, f, o.ID, nil, []entity.LockRecord{r}, nil); err != nil {
			return err
		}
	}
	for _, r := range stored {
		over := int(r.ID) <= len(locks) && locks[r.ID-1].Offset == r.Offset
		if !over && sameOwner(LockOwner{Client: r.Owner.Client, Name: r.Owner.Owner}, ls.owner) {
			if err := ops.SetLocks(ctx, o.share, f, o.ID, nil, nil, []entity.LockRecord{r}); err != nil {
				return err
			}
		}
	}
	return nil
}
