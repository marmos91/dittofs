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
	Mode      OpenMode
	Ref       StateRef // the reference naming it; Seq is the sequence as of the answer
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
	Loss          uint64 // the file's loss sequence as the service read it before the call: the open has seen every loss up to it
	Owner         []byte // the owner of opens the request names, which numbers its requests in Seqs; nil: none
	Seqs          Seqs
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
	Confirm bool // the open's owner is new: it must ConfirmOpen before any other request
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
	// never grants less than was asked.
	Open(ctx context.Context, who Caller, f FileKey, r OpenRequest) (OpenResult, error)
	// Downgrade narrows an open's access and deny to masks within them.
	Downgrade(ctx context.Context, who Caller, o OpenRef, q Seqs, access, deny entity.AccessMask) error
	// ConfirmOpen confirms the new owner of the open o names, its request
	// numbered seq, and raises the open's reference sequence; until it, the
	// owner's requests and I/O through its opens are refused.
	ConfirmOpen(ctx context.Context, who Caller, o OpenRef, seq uint32) error
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
	owner   []byte // the owner of opens that made it
	durable bool   // its record is in the store
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
	if _, err := t.running(); err != nil {
		return OpenResult{}, err
	}
	var res OpenResult
	owner := opensOwner(who.Client, r.Owner)
	err := t.sequenced(owner, r.Seqs.Owner, false, func() (err error) {
		res, err = t.grantOpen(ctx, who, f, r)
		return err
	})
	if err == nil && r.Seqs.Owner != nil {
		t.smu.Lock()
		res.Confirm = !t.seqs[owner].Confirmed
		t.smu.Unlock()
	}
	return res, err
}

// grantOpen grants an open of f, or refuses it.
func (t *Table) grantOpen(ctx context.Context, who Caller, f FileKey, r OpenRequest) (OpenResult, error) {
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
	o := &open{share: f.Share, owner: slices.Clone(r.Owner), Open: Open{
		ID: id, Client: who.Client, Principal: who.Principal, File: f.ID, Stream: r.Stream, Via: r.Via, Name: slices.Clone(r.Name),
		Mode: OpenMode{Access: r.Access, Deny: r.Deny, Privileged: r.Privileged}, LossSeen: r.Loss,
	}}
	if r.DeleteOnClose {
		o.Delete = DeleteMark{OnClose: true, By: who.Principal}
	}
	err := t.step(f.ID, func(e *entry) error {
		if e.pending != nil {
			return ErrDeletePending
		}
		for _, h := range e.opens {
			if !conflict.Shares(r.Access, r.Deny, h.Mode.Access, h.Mode.Deny) {
				return ErrShareViolation
			}
		}
		if err := t.markShard(ctx, who.Client, f.Shard); err != nil {
			return err
		}
		if err := t.mintOpen(e, o, f.Shard); err != nil {
			return err
		}
		if e.unlinked || r.Unlinked {
			// The file has no entry and this open holds it: it is written
			// durable before the answer, guarding the File against the
			// release, which finds its holders only in the store. Its record
			// is keyed by an ID, which an NFS open takes now.
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
		}
		return nil
	})
	if err != nil {
		return OpenResult{}, err
	}
	return OpenResult{Open: o.Open}, nil
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
// q (nil: unnumbered) and, if confirming, its confirm. The reference's
// sequence is judged in the owner's sequence, and who must be able to use the
// open: its opener, or its client's owner where the client protects its state.
func (t *Table) withOpen(who Caller, ref OpenRef, q *uint32, confirming bool, fn func(e *entry, o *open) error) error {
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
		return t.sequenced(opensOwner(o.Client, o.owner), q, confirming, func() error {
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

func opensOwner(c entity.ClientID, name []byte) sequence.Owner {
	return sequence.Owner{Client: c, Kind: sequence.Opens, Name: string(name)}
}

// sequenced runs fn as owner o's request numbered n, or as an unnumbered one
// for a nil n: a number other than the one o's sequence expects is
// ErrBadSeqID and runs nothing, and fn's answer then advances the sequence
// unless it is exempt.
func (t *Table) sequenced(o sequence.Owner, n *uint32, confirming bool, fn func() error) error {
	if n == nil {
		return fn()
	}
	t.smu.Lock()
	ok := t.seqs.Check(o, *n, confirming)
	t.smu.Unlock()
	if !ok {
		return ErrBadSeqID
	}
	err := fn()
	a := sequence.Granted
	switch {
	case errors.Is(err, ErrStaleClient), errors.Is(err, ErrBadState), errors.Is(err, ErrNotYours), errors.Is(err, ErrBadSeqID):
		// The request named no record of the owner's, as the client counts
		// it, or was another principal's, which may not move the owner's
		// sequence.
		a = sequence.Exempt
	case err != nil:
		a = sequence.Refused
	}
	t.smu.Lock()
	t.seqs.Answer(o, *n, a)
	t.smu.Unlock()
	return err
}

// confirmed reports whether o's owner has confirmed itself, or numbers no
// requests and has nothing to confirm.
func (t *Table) confirmed(o *open) bool {
	t.smu.Lock()
	defer t.smu.Unlock()
	s, ok := t.seqs[opensOwner(o.Client, o.owner)]
	return !ok || s.Confirmed
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

func (v opens) Downgrade(ctx context.Context, who Caller, ref OpenRef, q Seqs, access, deny entity.AccessMask) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	return t.withOpen(who, ref, q.Owner, false, func(e *entry, o *open) error {
		if access&^o.Mode.Access != 0 || deny&^o.Mode.Deny != 0 {
			return ErrInvalid
		}
		seq, err := t.advance(e, o.Ref)
		if err != nil {
			return err
		}
		o.Mode.Access, o.Mode.Deny = access, deny
		o.Ref.Seq = seq
		return nil
	})
}

func (v opens) Update(ctx context.Context, who Caller, id entity.OpenID, u OpenUpdate) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	return t.withOpen(who, OpenRef{Kind: RefOpened, Open: id}, nil, false, func(e *entry, o *open) error {
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

func (v opens) ConfirmOpen(ctx context.Context, who Caller, ref OpenRef, seq uint32) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	return t.withOpen(who, ref, &seq, true, func(e *entry, o *open) error {
		k := opensOwner(o.Client, o.owner)
		t.smu.Lock()
		s, ok := t.seqs[k]
		t.smu.Unlock()
		if !ok || s.Confirmed {
			return ErrBadState // an owner with nothing to confirm
		}
		n, err := t.advance(e, o.Ref)
		if err != nil {
			return err
		}
		o.Ref.Seq = n
		t.smu.Lock()
		s = t.seqs[k]
		s.Confirmed = true
		t.seqs[k] = s
		t.smu.Unlock()
		return nil
	})
}

func (v opens) Close(ctx context.Context, who Caller, ref OpenRef, q Seqs) (CloseResult, error) {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return CloseResult{}, err
	}
	var res CloseResult
	var out outbox
	err = t.withOpen(who, ref, q.Owner, false, func(e *entry, o *open) (err error) {
		if res.Orphan, err = t.unrecord(ctx, o); err != nil {
			return err
		}
		res.Unlink = t.closeOpen(e, o, &out)
		return nil
	})
	out.raise(sink)
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
// states under it, their locks and their waiters; the waiters that frees go to
// out. A close with delete on close makes the file delete pending, and the
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
