package openstate

import (
	"context"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate/conflict"
)

// Open is one open of one file, used only by its opener. Its durable form,
// written only while it keeps an unlinked file alive, is an
// entity.OpenRecord.
type Open struct {
	ID        entity.OpenID // 40 bits from NextID, unique across the installation, never reused
	Client    entity.ClientID
	Principal entity.Principal // the opener, mapped before squashing; any other caller through the open is refused
	File      entity.FileID    // the file whose table holds it: a named stream's base file
	Stream    entity.FileID    // the named stream it opens; zero: File itself
	Via       entity.FileID    // the directory it was opened through; zero: opened by handle
	Mode      OpenMode
	Ref       StateRef // the reference naming it; Seq is the sequence as of the answer
	Delete    DeleteMark
	Suspended entity.TimeMask // times I/O through it leaves alone
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

// OpenRequest is what an open asks for.
type OpenRequest struct {
	// Share and Shard are where the file lives, as the service resolved it:
	// the shard names the open's reference, and the share keys its durable
	// record.
	Share         entity.ShareID
	Shard         ShardID
	Unlinked      bool          // the file has no entry, as the service resolved it: the open is written durable
	Stream        entity.FileID // the named stream opened, whose base file the call names; zero: none
	Via           entity.FileID // the directory it was opened through; zero: by handle
	Name          []byte        // its name in Via, which a deferred delete unlinks
	Privileged    bool
	Access, Deny  entity.AccessMask
	DeleteOnClose bool
}

// OpenResult is what an open answers.
type OpenResult struct {
	Open Open
}

// OpenUpdate is a change made through an open; nil or zero fields change
// nothing.
type OpenUpdate struct {
	Delete          *bool // set or clear the file's delete pending, the disposition
	Suspend, Resume entity.TimeMask
}

// OpenRef names the open a call acts through: by its reference, as an NFSv4
// stateid carries it, by its ID, as an SMB FileId carries it, or by neither,
// which is the anonymous open of NFSv3 I/O and of NFSv4's anonymous stateid.
type OpenRef struct {
	Ref  StateRef      // Type StateOpen, with the sequence presented
	ID   entity.OpenID // nonzero: the open of that ID, at its current sequence
	File entity.FileID // the file the request's handle names; zero: none, which an anonymous open may not be
}

func (r OpenRef) anonymous() bool { return r.Ref.Type == 0 && r.ID == 0 }

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
	Open(ctx context.Context, who Caller, f entity.FileID, r OpenRequest) (OpenResult, error)
	// Downgrade narrows an open's access and deny to masks within them.
	Downgrade(ctx context.Context, who Caller, o OpenRef, access, deny entity.AccessMask) error
	// Update sets or clears the file's delete pending, through an open
	// holding delete access, and suspends or resumes the open's times.
	Update(ctx context.Context, who Caller, o entity.OpenID, u OpenUpdate) error
	// Close ends an open, and reports what the service runs after it.
	Close(ctx context.Context, who Caller, o OpenRef) (CloseResult, error)
	// Durable is the records of f's opens, for the unlink
	// that may take f's last entry to write in its transaction. The service
	// calls commit once, true when that transaction committed and took the
	// last entry, so it wrote the records; only then does the table hold
	// the opens durable. It holds f's admission lock throughout.
	Durable(ctx context.Context, f entity.FileID) (recs []entity.OpenRecord, commit func(committed bool), err error)
}

type opens struct{ t *Table }

// Opens is the view of opens and deny modes.
func (t *Table) Opens() Opens { return opens{t} }

// open is an Open and what the table holds beside it.
type open struct {
	Open
	share   entity.ShareID
	name    []byte
	durable bool // its record is in the store
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
	return &entity.DeleteRecord{Parent: o.Via, Name: o.name, By: by, Access: o.Mode.Access, Privileged: o.Mode.Privileged}
}

func (v opens) Open(ctx context.Context, who Caller, f entity.FileID, r OpenRequest) (OpenResult, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return OpenResult{}, err
	}
	if r.DeleteOnClose && r.Access&entity.Delete == 0 {
		return OpenResult{}, ErrAccess
	}
	n, err := t.d.Records.NextID(ctx, kvstore.IDOpen)
	if err != nil {
		return OpenResult{}, err
	}
	o := &open{share: r.Share, name: slices.Clone(r.Name), Open: Open{
		ID: entity.OpenID(n), Client: who.Client, Principal: who.Principal, File: f, Stream: r.Stream, Via: r.Via,
		Mode: OpenMode{Access: r.Access, Deny: r.Deny, Privileged: r.Privileged},
	}}
	if r.DeleteOnClose {
		o.Delete = DeleteMark{OnClose: true, By: who.Principal}
	}
	err = t.step(f, func(e *entry) error {
		if e.pending != nil {
			return ErrDeletePending
		}
		for _, h := range e.opens {
			if !conflict.Shares(r.Access, r.Deny, h.Mode.Access, h.Mode.Deny) {
				return ErrShareViolation
			}
		}
		if err := t.markShard(ctx, who.Client, r.Shard); err != nil {
			return err
		}
		if err := t.mintOpen(e, o, r.Shard); err != nil {
			return err
		}
		if e.unlinked || r.Unlinked {
			// The file has no entry and this open holds it: it is written
			// durable before the answer, guarding the File against the
			// release, which finds its holders only in the store.
			if err := t.d.Records.Opens().Put(ctx, o.share, o.record()); err != nil {
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
		t.byID[o.ID] = refKey{s, ref.Token}
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
	t.idx.Lock()
	defer t.idx.Unlock()
	if ref.ID == 0 {
		h, err := t.resolveLocked(who.Client, ref.File, ref.Ref)
		return h.file, refKey{ref.Ref.Shard, ref.Ref.Token}, err
	}
	k, ok := t.byID[ref.ID]
	if !ok {
		return entity.FileID{}, refKey{}, ErrBadState
	}
	h := t.refs[k]
	switch {
	case !ref.File.IsZero() && h.file != ref.File:
		return entity.FileID{}, refKey{}, ErrBadState
	case h.client != who.Client:
		return entity.FileID{}, refKey{}, ErrNotYours
	}
	return h.file, k, nil
}

// withOpen runs fn under the step of the file of the open ref names, once
// the open is found again there and who may use it: its opener, or its
// client's owner where the client protects its state.
func (t *Table) withOpen(who Caller, ref OpenRef, fn func(e *entry, o *open) error) error {
	if ref.anonymous() {
		return ErrBadState
	}
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
		if !t.mayUse(o, who) {
			return ErrNotYours
		}
		return fn(e, o)
	})
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

func (v opens) Downgrade(ctx context.Context, who Caller, ref OpenRef, access, deny entity.AccessMask) error {
	t := v.t
	if _, err := t.running(); err != nil {
		return err
	}
	return t.withOpen(who, ref, func(e *entry, o *open) error {
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
	return t.withOpen(who, OpenRef{ID: id}, func(e *entry, o *open) error {
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

func (v opens) Close(ctx context.Context, who Caller, ref OpenRef) (CloseResult, error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return CloseResult{}, err
	}
	var res CloseResult
	err := t.withOpen(who, ref, func(e *entry, o *open) (err error) {
		if res.Orphan, err = t.unrecord(ctx, o); err != nil {
			return err
		}
		res.Unlink = t.closeOpen(e, o)
		return nil
	})
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

// closeOpen drops o from e, its file's entry, under its step. A close with
// delete on close makes the file delete pending, and the last close of a
// delete-pending file returns the pending unlink.
func (t *Table) closeOpen(e *entry, o *open) *entity.DeleteRecord {
	if o.Delete.OnClose && e.pending == nil {
		e.pending = pendingFrom(o, o.Delete.By)
	}
	t.dropOpen(e, o)
	if len(e.opens) > 0 {
		return nil
	}
	p := e.pending
	e.pending = nil
	return p
}

// decision: the step is released between the records' snapshot and the
// commit, so a close or an open landing between them would leave a record
// of a closed open, or a volatile open of a file with no entry. The service
// keeps the file's admission lock from Durable to commit, as it does for
// every mutating call, so none lands there. Run the unlink under the step,
// as a callback, if a caller is found that unlinks without that lock.
func (v opens) Durable(ctx context.Context, f entity.FileID) ([]entity.OpenRecord, func(bool), error) {
	t := v.t
	if _, err := t.running(); err != nil {
		return nil, nil, err
	}
	var recs []entity.OpenRecord
	err := t.step(f, func(e *entry) error {
		for _, o := range e.opens {
			recs = append(recs, o.record())
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
		_ = t.step(f, func(e *entry) error {
			for _, rec := range recs {
				for _, o := range e.opens {
					if o.ID == rec.ID {
						o.durable = true
					}
				}
			}
			e.unlinked = true
			return nil
		})
	}
	return recs, commit, nil
}
