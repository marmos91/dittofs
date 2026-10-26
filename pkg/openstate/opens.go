package openstate

import (
	"context"
	"errors"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate/conflict"
	"github.com/marmos91/dittofs/pkg/openstate/sequence"
)

// Open is one open of one file, used only by its opener. Its durable form,
// written only while it keeps an unlinked file alive, is an
// entity.OpenRecord.
type Open struct {
	ID        entity.OpenID // only an SMB open's, or a durable one's, whose record is keyed by it: 40 bits from NextID, unique across the installation, never reused; zero: an NFS open, named by Ref
	Client    entity.ClientID
	Principal entity.Principal // the opener, mapped before squashing; any other caller through the open is refused
	File      entity.FileID    // the file whose table holds it: a named stream's base file
	Stream    entity.FileID    // the named stream it opens; zero: File itself
	Via       entity.FileID    // the directory it was opened through; zero: opened by handle
	Name      []byte           // its entry in Via, the one a delete pending removes; nil: opened by handle
	Owner     LockOwner        // the owner of opens, which holds this one open of the file; zero: the protocol has none
	Mode      OpenMode
	Ref       StateRef // the reference naming it; Seq is the sequence as of the answer
	Grant     GrantID  // the grant covering it: its client's lease under its key, or its oplock; zero: none
	Delete    DeleteMark
	Suspended entity.TimeMask // times I/O through it leaves alone
	LossSeen  uint64          // the file's loss sequence it last reported; volatile
}

// OpenMode is what an open may do, and what it refuses other opens.
type OpenMode struct {
	Access     entity.AccessMask // granted at open
	Deny       entity.AccessMask // the access it refuses other opens
	Privileged bool              // backup or restore intent its caller was privileged for
}

// DeleteMark is an open whose close makes the file delete pending.
type DeleteMark struct {
	OnClose bool
	By      entity.Principal // who asked for it; the deferred unlink runs as it
}

// FileKey names the file a call acts on, with where it lives as the service
// resolved it: the shard names a reference minted for it, and the share keys
// its durable records.
type FileKey struct {
	Share entity.ShareID
	Shard ShardID
	ID    entity.FileID
}

// OpenRequest is what an open asks for.
type OpenRequest struct {
	Unlinked      bool          // the file has no entry, as the service resolved it: the open is written durable
	Stream        entity.FileID // the named stream opened, whose base file the call names; zero: none
	Via           entity.FileID // the directory it was opened through; zero: by handle
	Name          []byte        // its name in Via, which a deferred delete unlinks
	Privileged    bool
	Access, Deny  entity.AccessMask
	DeleteOnClose bool
	Loss          uint64    // the file's loss sequence as the service read it before the call: the open has seen every loss up to it
	Owner         LockOwner // the owner of opens, of the caller's client, which numbers its requests in Seqs; zero: the protocol has none
	Seqs          Seqs
	Lease         LeaseKey  // the key of the lease to cover it; zero: none
	Epoch         *uint16   // a v2 lease request's epoch, which a new lease starts from; nil: a v1 lease or an oplock; an existing lease keeps its own
	Want          GrantKind // the grant kind asked for; none: no grant
	SetID         bool      // the file's mode has the setuid or setgid bit, as the service read it: no write caching is offered
}

// Seqs is the owner sequence numbers a request presents; zero where the
// protocol has none. A reference's own sequence travels in OpenRef.
type Seqs struct {
	Owner    *uint32 // the owner's number: the owner of locks', or the owner of opens' for an open, a downgrade, a close or a lock introducing a new owner of locks
	NewOwner uint32  // a lock introducing a new owner of locks: that owner's first number
}

// OpenResult is what an open answers.
type OpenResult struct {
	Open    Open
	Grant   *CachingGrant // the grant covering the open, or offered with it; nil: none
	Confirm bool          // the open's owner is new: it must ConfirmOpen before any other request
}

// OpenUpdate is a change made through an open; nil or zero fields change
// nothing.
type OpenUpdate struct {
	Delete          *bool // set or clear the file's delete pending, the disposition
	Suspend, Resume entity.TimeMask
}

// OpenRef names what a call acts through, by Kind: an open, a lock state, a
// delegation, or the anonymous open of NFSv3 I/O and of NFSv4's anonymous
// stateid. An NFS reference names it by State, with the sequence presented;
// an SMB one by Open, its FileId's ID, at its current sequence. Handle is
// the file handle the request carried, whose file must be the state's.
type OpenRef struct {
	Kind   RefKind
	State  StateRef      // NFS: the open, lock state or delegation; SMB: only the shard
	Open   entity.OpenID // SMB: the open; zero for NFS
	Handle entity.Handle // nil: none, which an anonymous open may not be
}

// RefKind is what an OpenRef names.
type RefKind uint8

const (
	RefOpened RefKind = iota + 1
	RefLocked
	RefDelegated
	RefAnonymous
)

// file is the file r's handle names; zero when it carries none, and
// ErrBadState for a handle that names no file.
func (r OpenRef) file() (entity.FileID, error) {
	if len(r.Handle) == 0 {
		return entity.FileID{}, nil
	}
	_, f, err := r.Handle.Parse()
	if err != nil {
		return entity.FileID{}, ErrBadState
	}
	return f, nil
}

// CloseResult is what a close leaves the service to run: open state never
// releases or unlinks a file itself.
type CloseResult struct {
	Orphan bool // the file has no entry and no open: the service runs its release
	// Unlink is the deferred delete of a delete-pending file: the entry the
	// marking open was opened through, which the service unlinks, as By,
	// authorised by the marking open's grant.
	Unlink *entity.DeleteRecord
}

// Opens is the view of opens and deny modes; it runs at the file's primary.
type Opens interface {
	// Open grants an open of f, or refuses it ErrShareViolation against a
	// deny mode either way and ErrDeletePending on a delete-pending file; it
	// never grants less than was asked. An owner of opens holds one open of
	// a file: its second open of it upgrades the first.
	Open(ctx context.Context, who Caller, f FileKey, r OpenRequest) (OpenResult, error)
	// Downgrade narrows an open's access and deny to masks within them, and
	// returns the open's new reference sequence.
	Downgrade(ctx context.Context, who Caller, o OpenRef, q Seqs, access, deny entity.AccessMask) (seq uint32, err error)
	// ConfirmOpen confirms the new owner of the open o names, its request
	// numbered in q, and returns the open's new reference sequence; until it,
	// the owner's requests and I/O through its opens are refused.
	ConfirmOpen(ctx context.Context, who Caller, o OpenRef, q Seqs) (seq uint32, err error)
	// Update sets or clears the file's delete pending, through an open
	// holding delete access, and suspends or resumes the open's times.
	Update(ctx context.Context, who Caller, o entity.OpenID, u OpenUpdate) error
	// Close ends an open, and reports what the service runs after it.
	Close(ctx context.Context, who Caller, o OpenRef, q Seqs) (CloseResult, error)
	// Durable is the records of f's opens, for the unlink that may take
	// f's last entry to write in its transaction; an open with no ID is
	// issued one for its record. The service calls commit once, true when
	// that transaction committed and took the last entry, so it wrote the
	// records; only then does the table hold the opens durable. It holds f's
	// admission lock throughout.
	Durable(ctx context.Context, f FileKey) (recs []entity.OpenRecord, commit func(committed bool), err error)
}

type opens struct{ t *Table }

// Opens is the view of opens and deny modes.
func (t *Table) Opens() Opens { return opens{t} }

// open is an Open and what the table holds beside it.
type open struct {
	Open
	share   entity.ShareID
	lease   LeaseKey // the lease key it was opened under; zero: none
	durable bool     // its record is in the store
}

// opened is the file o opens: its named stream, or else its file.
func (o *open) opened() entity.FileID {
	if !o.Stream.IsZero() {
		return o.Stream
	}
	return o.File
}

func (o *open) record() entity.OpenRecord {
	return entity.OpenRecord{
		ID: o.ID, Client: o.Client, Principal: o.Principal, File: o.opened(), Via: o.Via,
		Access: o.Mode.Access, Deny: o.Mode.Deny, Privileged: o.Mode.Privileged,
		DeleteOnClose: o.Delete.OnClose, DeleteBy: o.Delete.By, Suspended: o.Suspended,
		Nonce: o.Ref.Token, StateSeq: o.Ref.Seq,
	}
}

// pendingFrom is the delete pending o makes, as principal by.
func pendingFrom(o *open, by entity.Principal) *entity.DeleteRecord {
	return &entity.DeleteRecord{Parent: o.Via, Name: o.Name, By: by, Access: o.Mode.Access, Privileged: o.Mode.Privileged}
}

func (v opens) Open(ctx context.Context, who Caller, f FileKey, r OpenRequest) (OpenResult, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return OpenResult{}, err
	}
	var res OpenResult
	if len(r.Owner.Name) > maxName {
		return OpenResult{}, ErrInvalid
	}
	if len(r.Owner.Name) > 0 && r.Owner.Client != who.Client {
		return OpenResult{}, ErrNotYours
	}
	owner := opensOwner(r.Owner)
	var out outbox
	err = t.sequenced(owner, r.Seqs.Owner, seqOpen, func(replaces bool) (err error) {
		if replaces {
			t.closeOpensOf(ctx, r.Owner, &out)
		}
		res, err = t.grantOpen(ctx, who, f, r, &out)
		return err
	})
	out.raise(t, sink)
	if err == nil && r.Seqs.Owner != nil {
		t.smu.Lock()
		res.Confirm = t.seqs.Unconfirmed(owner)
		t.smu.Unlock()
	}
	return res, err
}

// grantOpen grants an open of f, or refuses it, and offers it a grant. A
// deny-mode conflict breaks the handle caching covering the opens it meets
// before it is refused, and another holder's caching grant the open conflicts
// with is recalled; either answers ErrDelay until it ends.
func (t *Table) grantOpen(ctx context.Context, who Caller, f FileKey, r OpenRequest, out *outbox) (OpenResult, error) {
	if r.DeleteOnClose && r.Access&entity.Delete == 0 {
		return OpenResult{}, ErrInvalid
	}
	var id entity.OpenID
	if who.Protocol == entity.ProtocolSMB {
		// An SMB FileId names the open across a reconnect.
		var err error
		if id, err = t.nextOpenID(ctx); err != nil {
			return OpenResult{}, err
		}
	}
	r.Owner.Name = slices.Clone(r.Owner.Name)
	o := &open{share: f.Share, lease: r.Lease, Open: Open{
		ID: id, Client: who.Client, Principal: who.Principal, File: f.ID, Stream: r.Stream, Via: r.Via, Name: slices.Clone(r.Name), Owner: r.Owner,
		Mode: OpenMode{Access: r.Access, Deny: r.Deny, Privileged: r.Privileged}, LossSeen: r.Loss,
	}}
	if r.DeleteOnClose {
		o.Delete = DeleteMark{OnClose: true, By: who.Principal}
	}
	var res OpenResult
	err := t.step(f.ID, func(e *entry) error {
		if e.pending != nil {
			return ErrDeletePending
		}
		if r.Lease != (LeaseKey{}) {
			t.idx.Lock()
			kf, held := t.keys[clientKey{who.Client, r.Lease}]
			t.idx.Unlock()
			if held && kf != f.ID {
				return ErrInvalid // a key names one grant, on one file
			}
		}
		granted, access, deny := o, r.Access, r.Deny
		if up := e.openOf(r.Owner, r.Stream); up != nil {
			if !t.mayUse(up, who) {
				return ErrNotYours
			}
			granted, access, deny = up, up.Mode.Access|r.Access, up.Mode.Deny|r.Deny
		}
		own := func(g *grant) bool { return g.heldBy(who.Client, r.Lease, granted) }
		var hs []*open
		for _, h := range e.opens {
			if h != granted && !conflict.Shares(access, deny, h.Mode.Access, h.Mode.Deny) {
				hs = append(hs, h)
			}
		}
		if err := t.breakHandles(e, hs, own, out); err != nil {
			return err
		}
		if err := t.recallOthers(e, own, func(*grant) GrantKind { return conflict.GrantKeeps(access, deny) }, out); err != nil {
			return err
		}
		if granted != o {
			if err := t.upgrade(e, granted, who, r, access, deny); err != nil {
				return err
			}
		} else if err := t.addOpen(ctx, e, f, o, r); err != nil {
			return err
		}
		res.Grant = t.offer(e, f, who, granted, r)
		res.Open = granted.Open
		return nil
	})
	if err != nil {
		return OpenResult{}, err
	}
	return res, nil
}

// addOpen adds o, a new open of f, to e under its step: its shard listed in
// its client's record, its reference minted, and its record written durable
// where the file has no entry.
func (t *Table) addOpen(ctx context.Context, e *entry, f FileKey, o *open, r OpenRequest) error {
	if err := t.markShard(ctx, o.Client, f.Shard); err != nil {
		return err
	}
	if err := t.mintOpen(e, o, f.Shard); err != nil {
		return err
	}
	if !e.unlinked && !r.Unlinked {
		return nil
	}
	// The file has no entry and this open holds it: it is written durable
	// before the answer, guarding the File against the release, which finds
	// its holders only in the store. Its record is keyed by an ID, which an
	// NFS open takes now.
	err := error(nil)
	if o.ID == 0 {
		o.ID, err = t.nextOpenID(ctx)
	}
	if err == nil {
		err = t.d.Records.Opens().Put(ctx, o.share, o.record())
	}
	if err != nil {
		t.dropOpen(e, o)
		return err
	}
	o.durable = true
	return nil
}

// openOf is owner's open of e's file, or of its stream s, on e; nil for none
// or for a protocol with no owner of opens.
func (e *entry) openOf(owner LockOwner, s entity.FileID) *open {
	if len(owner.Name) == 0 {
		return nil
	}
	for _, o := range e.opens {
		if o.Stream == s && sameOwner(o.Owner, owner) {
			return o
		}
	}
	return nil
}

// upgrade widens up, its owner's open of e's file, to access and deny, which
// were judged against every other open, under e's step, raising its
// reference's sequence.
func (t *Table) upgrade(e *entry, up *open, who Caller, r OpenRequest, access, deny entity.AccessMask) error {
	seq, err := t.advance(e, up.Ref)
	if err != nil {
		return err
	}
	up.Mode.Access, up.Mode.Deny, up.Ref.Seq = access, deny, seq
	if r.DeleteOnClose {
		up.Delete = DeleteMark{OnClose: true, By: who.Principal}
	}
	return nil
}

// mintOpen mints o's reference and adds o to e, file o.File's entry, under
// its step, if o's client is current here; the check and the mint share the
// index lock, so an expiry that drops the client either sees the open or
// refuses it.
func (t *Table) mintOpen(e *entry, o *open, s ShardID) error {
	ref, err := t.mintUnder(e, o.Client, s, StateOpen, func(ref StateRef) error {
		if !t.current(o.Client) {
			return ErrStaleClient
		}
		if o.ID != 0 {
			t.byID[o.ID] = refKey{s, ref.Token}
		}
		if !o.Via.IsZero() {
			t.via[o.Via]++
		}
		return nil
	})
	if err != nil {
		return err
	}
	o.Ref = ref
	if e.opens == nil {
		e.opens = map[uint64]*open{}
	}
	e.opens[ref.Token] = o
	return nil
}

// dropOpen removes o from e, its file's entry, under its step: its
// reference, its ID and its count under Via.
func (t *Table) dropOpen(e *entry, o *open) {
	delete(e.opens, o.Ref.Token)
	t.release(e, o.Ref)
	t.idx.Lock()
	defer t.idx.Unlock()
	delete(t.byID, o.ID)
	if !o.Via.IsZero() {
		if t.via[o.Via]--; t.via[o.Via] <= 0 {
			delete(t.via, o.Via)
		}
	}
}

// markShard lists shard s in client c's record, once: it commits before the
// answer granting c its first state there. Done is zero until the table
// holds each shard's grace instance.
func (t *Table) markShard(ctx context.Context, c entity.ClientID, s ShardID) error {
	t.cmu.Lock()
	k, ok := t.known[c]
	listed := slices.Contains(k.shards, s)
	t.cmu.Unlock()
	if !ok {
		return ErrStaleClient
	}
	if listed {
		return nil
	}
	rec, err := t.d.Records.Clients().Get(ctx, c)
	if err != nil {
		return staleIfGone(err)
	}
	if !slices.ContainsFunc(rec.Shards, func(m entity.ShardMark) bool { return m.Shard == s }) {
		if err := t.d.Records.Clients().MarkShard(ctx, c, entity.ShardMark{Shard: s}); err != nil {
			return staleIfGone(err)
		}
	}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if k, ok := t.known[c]; ok {
		k.shards = append(k.shards, s)
		t.known[c] = k
	}
	return nil
}

// findOpen finds the open ref names for who: its file and its reference's
// key. A reference is judged as resolve judges it; an ID that names no open
// is ErrBadState.
func (t *Table) findOpen(who Caller, ref OpenRef) (entity.FileID, refKey, error) {
	if ref.Kind != RefOpened {
		return entity.FileID{}, refKey{}, ErrBadState
	}
	f, err := ref.file()
	if err != nil {
		return entity.FileID{}, refKey{}, err
	}
	t.idx.Lock()
	defer t.idx.Unlock()
	if ref.Open == 0 {
		h, err := t.resolveLocked(who.Client, f, ref.State)
		return h.file, refKey{ref.State.Shard, ref.State.Token}, err
	}
	k, ok := t.byID[ref.Open]
	if !ok {
		return entity.FileID{}, refKey{}, ErrBadState
	}
	h := t.refs[k]
	switch {
	case !f.IsZero() && h.file != f:
		return entity.FileID{}, refKey{}, ErrBadState
	case h.client != who.Client:
		return entity.FileID{}, refKey{}, ErrNotYours
	}
	return h.file, k, nil
}

// withOpen runs fn under the step of the file of the open ref names, once
// the open is found again there, as the request of the open's owner numbered
// q (nil: unnumbered), of kind req. The reference's
// sequence is judged in the owner's sequence, and who must be able to use the
// open: its opener, or its client's owner where the client protects its state.
func (t *Table) withOpen(who Caller, ref OpenRef, q *uint32, req seqReq, fn func(e *entry, o *open) error) error {
	presented := ref.State.Seq
	ref.State.Seq = 0
	f, _, err := t.findOpen(who, ref)
	if err != nil {
		return err
	}
	return t.step(f, func(e *entry) error {
		// Found again under the step, so the sequence judged is the one the
		// change applies to.
		_, k, err := t.findOpen(who, ref)
		if err != nil {
			return err
		}
		o := e.opens[k.token]
		if o == nil {
			return ErrBadState
		}
		return t.sequenced(opensOwner(o.Owner), q, req, func(bool) error {
			if err := seqErr(o.Ref.Seq, presented); err != nil {
				return err
			}
			if !t.mayUse(o, who) {
				return ErrNotYours
			}
			return fn(e, o)
		})
	})
}

// seqReq is what a numbered request is to its owner's sequence.
type seqReq uint8

const (
	seqOther   seqReq = iota // any request but these two
	seqConfirm               // the owner's confirm
	seqOpen                  // an open, which replaces an unconfirmed owner's
)

func opensOwner(o LockOwner) sequence.Owner {
	return sequence.Owner{Client: o.Client, Kind: sequence.Opens, Name: string(o.Name)}
}

// sequenced runs fn as owner o's request numbered n, or as an unnumbered one
// for a nil n: a number other than the one o's sequence expects is
// ErrBadSeqID and runs nothing, and fn's answer then advances the sequence
// unless it is exempt. The owner is marked in flight from its check to its
// answer, so a second numbered request of the owner arriving between them is
// ErrDelay, and its retry is judged once the first has advanced the
// sequence: a check and the change it admits never separate.
//
// Until an owner of opens is confirmed, its open is usable by its confirm
// alone: any other request naming its state is ErrBadState and advances
// nothing, and a new open of the owner replaces it, taken as the owner's
// first whatever its number; fn is told it replaces, and releases the
// unconfirmed open first.
func (t *Table) sequenced(o sequence.Owner, n *uint32, req seqReq, fn func(replaces bool) error) error {
	if n == nil {
		return fn(false)
	}
	t.smu.Lock()
	if _, busy := t.inflight[o]; busy {
		t.smu.Unlock()
		return ErrDelay
	}
	replaces := false
	if t.seqs.Unconfirmed(o) {
		switch req {
		case seqOpen:
			delete(t.seqs, o)
			replaces = true
		case seqOther:
			t.smu.Unlock()
			return ErrBadState
		}
	}
	if !t.seqs.Check(o, *n) {
		t.smu.Unlock()
		return ErrBadSeqID
	}
	t.inflight[o] = struct{}{}
	t.smu.Unlock()
	err := fn(replaces)
	a := sequence.Granted
	switch {
	case errors.Is(err, ErrStaleClient), errors.Is(err, ErrStaleState), errors.Is(err, ErrBadState), errors.Is(err, ErrNotYours), errors.Is(err, ErrBadSeqID):
		// The request named no record of the owner's, as the client counts
		// it, or was another principal's, which may not move the owner's
		// sequence.
		a = sequence.Exempt
	case err != nil:
		a = sequence.Refused
	}
	t.smu.Lock()
	t.seqs.Answer(o, *n, a)
	delete(t.inflight, o)
	t.smu.Unlock()
	return err
}

// confirmed reports whether o's owner has confirmed itself, or numbers no
// requests and has nothing to confirm.
func (t *Table) confirmed(o *open) bool {
	t.smu.Lock()
	defer t.smu.Unlock()
	return !t.seqs.Unconfirmed(opensOwner(o.Owner))
}

// mayUse reports whether who may act through o.
func (t *Table) mayUse(o *open, who Caller) bool {
	if who.Principal == o.Principal {
		return true
	}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	owner := t.known[o.Client].owner
	return !owner.IsZero() && who.Principal == owner
}

func (v opens) Downgrade(ctx context.Context, who Caller, ref OpenRef, q Seqs, access, deny entity.AccessMask) (uint32, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return 0, err
	}
	var seq uint32
	err := t.withOpen(who, ref, q.Owner, seqOther, func(e *entry, o *open) (err error) {
		if access&^o.Mode.Access != 0 || deny&^o.Mode.Deny != 0 {
			return ErrInvalid
		}
		if seq, err = t.advance(e, o.Ref); err != nil {
			return err
		}
		o.Mode.Access, o.Mode.Deny = access, deny
		o.Ref.Seq = seq
		return nil
	})
	return seq, err
}

func (v opens) Update(ctx context.Context, who Caller, id entity.OpenID, u OpenUpdate) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	return t.withOpen(who, OpenRef{Kind: RefOpened, Open: id}, nil, seqOther, func(e *entry, o *open) error {
		if u.Delete != nil {
			if o.Mode.Access&entity.Delete == 0 {
				return ErrAccess
			}
			switch {
			case !*u.Delete:
				e.pending = nil
			case e.pending == nil:
				e.pending = pendingFrom(o, who.Principal)
			}
		}
		o.Suspended = o.Suspended&^u.Resume | u.Suspend
		return nil
	})
}

func (v opens) ConfirmOpen(ctx context.Context, who Caller, ref OpenRef, q Seqs) (uint32, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return 0, err
	}
	var seq uint32
	err := t.withOpen(who, ref, q.Owner, seqConfirm, func(e *entry, o *open) error {
		k := opensOwner(o.Owner)
		t.smu.Lock()
		unconfirmed := t.seqs.Unconfirmed(k)
		t.smu.Unlock()
		if !unconfirmed {
			return ErrBadState // an owner with nothing to confirm
		}
		n, err := t.advance(e, o.Ref)
		if err != nil {
			return err
		}
		o.Ref.Seq, seq = n, n
		t.smu.Lock()
		t.seqs.Confirm(k)
		t.smu.Unlock()
		return nil
	})
	return seq, err
}

func (v opens) Close(ctx context.Context, who Caller, ref OpenRef, q Seqs) (CloseResult, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return CloseResult{}, err
	}
	var res CloseResult
	var out outbox
	err = t.withOpen(who, ref, q.Owner, seqOther, func(e *entry, o *open) (err error) {
		if res.Orphan, err = t.unrecord(ctx, o); err != nil {
			return err
		}
		res.Unlink = t.closeOpen(e, o, &out)
		return nil
	})
	out.raise(t, sink)
	return res, err
}

// unrecord deletes o's durable record, if it has one, and reports whether
// that left its file orphaned.
func (t *Table) unrecord(ctx context.Context, o *open) (bool, error) {
	if !o.durable {
		return false, nil
	}
	orphaned, err := t.d.Records.Opens().Delete(ctx, o.share, o.opened(), o.ID)
	return orphaned.File != entity.FileID{}, err
}

// closeOpen drops o from e, its file's entry, under its step, with the lock
// states under it, their locks and their waiters, and the grant covering it
// if it was the last open covered; the waiters either frees go to out. A close with delete on close makes the file delete pending, and the
// last close of a delete-pending file returns the pending unlink.
func (t *Table) closeOpen(e *entry, o *open, out *outbox) *entity.DeleteRecord {
	if o.Delete.OnClose && e.pending == nil {
		e.pending = pendingFrom(o, o.Delete.By)
	}
	freed := false
	for _, ls := range e.lstates {
		if ls.open == o {
			t.dropLockState(e, ls)
			freed = true
		}
	}
	freed = t.uncover(e, o) || freed
	t.dropOpen(e, o)
	if freed {
		t.serve(e, out)
	}
	if len(e.opens) > 0 {
		return nil
	}
	p := e.pending
	e.pending = nil
	return p
}

// decision: the step is released between the records' snapshot and the
// commit. The service keeps the file's admission lock from Durable to
// commit, so no call's open or close lands there; a timer step's close,
// which takes no admission lock, can, and commit deletes that open's record.
// Run the unlink under the step, as a callback, if a caller is found that
// unlinks without that lock.
func (v opens) Durable(ctx context.Context, f FileKey) ([]entity.OpenRecord, func(bool), error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return nil, nil, err
	}
	var recs []entity.OpenRecord
	var held []*open
	err := t.step(f.ID, func(e *entry) error {
		for _, o := range e.opens {
			rec := o.record()
			if rec.ID == 0 {
				// An NFS open's record is keyed by an ID it takes now; an
				// aborted unlink leaves the ID unused.
				id, err := t.nextOpenID(ctx)
				if err != nil {
					return err
				}
				rec.ID = id
			}
			recs, held = append(recs, rec), append(held, o)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	commit := func(committed bool) {
		if !committed {
			return
		}
		var orphan bool
		_ = t.step(f.ID, func(e *entry) error {
			for i, o := range held {
				if e.opens[o.Ref.Token] != o {
					// Closed by a timer step since the snapshot, which takes
					// no admission lock: its record is deleted here.
					out, _ := t.d.Records.Opens().Delete(ctx, f.Share, recs[i].File, recs[i].ID)
					orphan = orphan || !out.File.IsZero()
					continue
				}
				o.ID, o.durable = recs[i].ID, true
			}
			e.unlinked = true
			return nil
		})
		if sink, err := t.running(); orphan && err == nil {
			sink.Closed(f.ID, CloseResult{Orphan: true})
		}
	}
	return recs, commit, nil
}

// nextOpenID issues an open's ID, unique across the installation and never
// reused.
func (t *Table) nextOpenID(ctx context.Context) (entity.OpenID, error) {
	n, err := t.d.Records.NextID(ctx, kvstore.IDOpen)
	return entity.OpenID(n), err
}

// closeOpensOf closes owner's opens here, as the table closes an open itself:
// what each close leaves the service to run goes to out.
func (t *Table) closeOpensOf(ctx context.Context, owner LockOwner, out *outbox) {
	for _, h := range t.heldBy(owner.Client) {
		if h.ref.Type != StateOpen {
			continue
		}
		_ = t.step(h.file, func(e *entry) error {
			if o := e.opens[h.ref.Token]; o != nil && sameOwner(o.Owner, owner) {
				t.closeByTable(ctx, e, o, out)
			}
			return nil
		})
	}
}

// closeByTable closes o, of e's file, under its step, with no Close to
// answer: what the close leaves the service to run goes to out.
func (t *Table) closeByTable(ctx context.Context, e *entry, o *open, out *outbox) {
	var r CloseResult
	r.Orphan, _ = t.unrecord(ctx, o)
	r.Unlink = t.closeOpen(e, o, out)
	out.close(e.file, r)
}
