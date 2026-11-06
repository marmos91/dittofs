package vfs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/admit"
	"github.com/cubbit/dittofs/internal/vfs/authz"
	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Opens and closes, and the release of a file nothing holds. An open
// resolves or creates its name, decides the rights it asks through the
// directory it resolved through, then has open state judge the deny modes
// held and record it, under the file's admission lock. A close drops the
// open there, then runs what open state left it once that lock is released:
// a deferred unlink, a release.

// errSnapshotOpen answers an open naming a snapshot.
//
// decision: an open of a snapshot is ErrInvalid, as no snapshot can be named
// before RFC 12's store view exists, so every token is one the service never
// issued. Open it read-only through that view once it lands.
var errSnapshotOpen = fmt.Errorf("vfs: no snapshot is served: %w", vfs.ErrInvalid)

// Open opens the file req names, by Dir and Name or by H, creating it as its
// disposition allows, and answers the open, the file and the caller's
// maximal access to it. A reconnect attaches the durable open it names to
// the call's session instead.
func (fs *Filesystem) Open(ctx context.Context, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.OpenResult{}, err
	}
	defer done()
	if req.Snapshot != nil {
		return vfs.OpenResult{}, errSnapshotOpen
	}
	at := req.H
	if len(req.Name) > 0 {
		at = req.Dir
	}
	a, release, err := fs.admitHandle(ctx, c, at)
	if err != nil {
		return vfs.OpenResult{}, err
	}
	defer release()
	if a == nil {
		return vfs.OpenResult{}, fs.pseudoOpen(ctx, at, req.Disposition)
	}
	if req.Reconnect != nil {
		return fs.reconnect(ctx, a, c, req)
	}
	// The whole open, its lookup, create and truncate included, is decided
	// as the open is: over SMB privileged only with backup or restore intent.
	a.caller = forOpen(a.caller, c.Protocol, req.Backup)
	switch {
	case req.DeleteOnClose && req.Access&entity.Delete == 0:
		// Refused before anything is created, as an open asking delete on
		// close without delete opens nothing.
		return vfs.OpenResult{}, vfs.ErrInvalid
	case req.Access&^(entity.EntryRights|entity.SystemSecurity|vfs.MaximumAllowed) != 0:
		// A right no entry names is granted to no one on an existing file,
		// so a creating open, which decides nothing on the new file, is
		// refused it too.
		return vfs.OpenResult{}, vfs.ErrAccess
	case req.Access&entity.SystemSecurity != 0 && !a.caller.Admin:
		// Only an admin reads or writes the audit list, as SetACL guards it,
		// so a creating open, which decides nothing on the new file, cannot
		// hand it to another caller either.
		return vfs.OpenResult{}, vfs.ErrAccess
	case len(req.Name) == 0:
		return fs.openHandle(ctx, a, c, req)
	}
	for {
		seen, err := fs.entryIf(ctx, a, req.Dir, req.Name)
		if err != nil {
			return vfs.OpenResult{}, err
		}
		r, again, err := fs.openLocked(ctx, a, c, req, seen)
		if !again {
			return r, err
		}
	}
}

// openLocked is one attempt of an open by name, seen the file the name named
// when looked up, nil for none. It holds the directory's admission lock and
// seen's, or, to create, the directory's create locks and the admission lock
// of the ID the new file will take, and looks the name up again under them:
// again is set when it moved.
func (fs *Filesystem) openLocked(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest, seen *vfs.File) (_ vfs.OpenResult, again bool, _ error) {
	id := entity.NewFileID()
	set := creating(req.Dir)
	set.Admit = append(set.Admit, lockorder.Key(id))
	if seen != nil {
		k := lockorder.Key(seen.ID)
		set = lockorder.Set{Admit: []lockorder.Key{lockorder.Key(fileOf(req.Dir)), k}}
		if overwrites(req.Disposition) {
			set.Accept = []lockorder.Key{k}
		}
	}
	unlock, err := fs.locks.Take(ctx, set)
	if err != nil {
		return vfs.OpenResult{}, false, errContended
	}
	var undone func()
	defer func() { runAfter(undone) }() // deferred first, so it runs after unlock
	defer unlock()
	f, err := fs.entryIf(ctx, a, req.Dir, req.Name)
	if err != nil || (f == nil) != (seen == nil) || f != nil && f.ID != seen.ID {
		return vfs.OpenResult{}, err == nil, err
	}
	if f != nil {
		if req.Disposition == vfs.CreateNew {
			return vfs.OpenResult{}, false, vfs.ErrExist
		}
		r, after, err := fs.grant(ctx, a, c, req, entity.NewHandle(a.share.ID, f.ID), *f, false)
		undone = after
		return r, false, err
	}
	if !creates(req.Disposition) {
		return vfs.OpenResult{}, false, vfs.ErrNotFound
	}
	cr, err := fs.createLocked(ctx, a, c, vfs.OpOpen, req.Dir, req.Name, req.Create, id)
	if err != nil {
		return vfs.OpenResult{}, false, err
	}
	r, after, err := fs.grant(ctx, a, c, req, cr.Handle, cr.File, true)
	undone = after
	if err == nil {
		r.Created, r.Dir = true, &cr.Dir
	}
	return r, false, err
}

// openHandle is an open by handle, which creates nothing, under the file's
// admission lock and, to overwrite it, its accept lock.
func (fs *Filesystem) openHandle(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	k := lockorder.Key(fileOf(req.H))
	set := lockorder.Set{Admit: []lockorder.Key{k}}
	if overwrites(req.Disposition) {
		set.Accept = []lockorder.Key{k}
	}
	unlock, err := fs.locks.Take(ctx, set)
	if err != nil {
		return vfs.OpenResult{}, errContended
	}
	var undone func()
	defer func() { runAfter(undone) }() // deferred first, so it runs after unlock
	defer unlock()
	f, err := fs.meta.Files().Get(ctx, a.caller.Identity, req.H)
	switch {
	case err != nil:
		return vfs.OpenResult{}, storeErr(err)
	case req.Disposition == vfs.CreateNew:
		return vfs.OpenResult{}, vfs.ErrExist
	}
	r, undone, err := fs.grant(ctx, a, c, req, req.H, f, false)
	return r, err
}

// runAfter runs what an undone open left to run once its locks are released,
// if anything.
func runAfter(f func()) {
	if f != nil {
		f()
	}
}

// grant decides and records the open of f, which h names, holding f's
// admission lock: the rights it asks, decided through the directory it was
// resolved through, an overwrite asking write data besides; for a maximal
// open every right the caller holds besides; then open state's deny modes,
// which a handle-caching grant over a conflict breaks first, answering
// ErrDelay; then an overwrite's truncate, under f's accept lock. created is
// set for a file the open made. Over SMB the open is decided with the
// caller's privilege only on a backup or restore intent, and the maximal
// access it answers never with it. An open whose answer fails once open
// state recorded it is undone: an open the owner held goes back to what it
// held, and a new open is closed. What the undo leaves, a deferred unlink or
// a release, runs as after a Close (RFC 17 §5.3): through the func grant
// returns, which its caller runs once it has released its locks.
//
// decision: an open that created its file is granted what it asks without
// deciding it on the new file, as the add right on its directory was
// decided (RFC 19 §3.1): a create asking write with a mode granting none, as an extracting
// archiver sends, is granted, as POSIX's open with O_CREAT is. Its maximal
// access is still the file's own. Overturned by an inherited ACL found to
// deny its creator a right the create needs.
func (fs *Filesystem) grant(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest, h vfs.Handle, f vfs.File, created bool) (vfs.OpenResult, func(), error) {
	overwrite := overwrites(req.Disposition) && !created
	if overwrite && f.IsDir() {
		return vfs.OpenResult{}, nil, vfs.ErrIsDir
	}
	var via vfs.Handle
	if len(req.Name) > 0 {
		via = req.Dir
	}
	access := req.Access &^ vfs.MaximumAllowed
	if overwrite {
		access |= entity.WriteData
	}
	cp := a.caller
	max, err := fs.authz.Rights(ctx, cp, h, via, entity.EntryRights)
	if err != nil {
		return vfs.OpenResult{}, nil, storeErr(err)
	}
	maximal := max
	if cp.Privileged && c.Protocol == entity.ProtocolSMB {
		if maximal, err = fs.authz.Rights(ctx, forOpen(cp, c.Protocol, false), h, via, entity.EntryRights); err != nil {
			return vfs.OpenResult{}, nil, storeErr(err)
		}
	}
	if req.Access&vfs.MaximumAllowed != 0 {
		if max == 0 {
			return vfs.OpenResult{}, nil, vfs.ErrAccess
		}
		access |= max
	}
	if !created {
		if err := fs.authz.Decide(ctx, cp, vfs.OpOpen, h, via, req.Name, access); err != nil {
			return vfs.OpenResult{}, nil, storeErr(err)
		}
	}
	who := a.who()
	// decision: the request's Conn is left empty, as vfs.Call carries no
	// principals of the other sessions signed in on the caller's connection:
	// an SMB open joins an existing lease only when its own principal is one
	// of the lease's openers', and any other is granted no grant. That fails
	// closed, costing a second user on one connection the shared cache.
	// Fill Conn once OpenRequest carries those principals, from the SMB adapter.
	o, err := fs.opens.Open(ctx, who, keyOf(f), openstate.OpenRequest{
		Unlinked: f.Nlink == 0 && !f.IsDir(), Via: fileOf(via), Name: req.Name,
		Privileged: req.Backup && a.caller.Privileged, Access: access, Deny: req.Deny, DeleteOnClose: req.DeleteOnClose,
		Loss: fs.engine.LossSeq(f.Share, f.ID), Owner: req.Owner, Seqs: req.Seqs,
		Lease: req.Lease, Parent: req.Parent, Epoch: req.Epoch, Want: req.Want,
		SetID: f.Mode&(entity.ModeSetuid|entity.ModeSetgid) != 0, Reclaim: req.Reclaim,
		Durability: req.Durability, Durable: openstate.DurableKeys{Timeout: req.Timeout, Create: req.CreateID, Instance: req.Instance},
		Session: c.Session, Replay: req.Replay, MaxRead: max&entity.ReadData != 0, Channel: c.Channel.Seq,
	})
	if err != nil {
		return vfs.OpenResult{}, nil, openErr(err)
	}
	ref := vfs.OpenRef{Kind: openstate.RefOpened, State: o.Open.Ref, Open: o.Open.ID, Handle: h}
	file, err := fs.openedFile(ctx, a, h, f, overwrite)
	if err != nil {
		// No client holds the open's reference to undo it, so it is undone
		// here, on a context the call's end does not cancel, though Drain's
		// deadline does. The open is undone, not closed: it quotes no owner
		// number, as the open took the request's, and an open the owner held
		// goes back to what it held, a new open ending with its delete on
		// close dropped.
		actx, cancel := fs.detached(ctx)
		cr, cerr := fs.opens.Abandon(actx, who, ref, o.Prior)
		cancel()
		if cerr != nil {
			fs.log.Warn("vfs: a failed open not closed", "file", f.ID, "err", cerr)
			return vfs.OpenResult{}, nil, err
		}
		// A deferred unlink the undo left, a file marked delete pending while
		// the open was recorded, and the release of an f it alone held run as
		// after a Close, once the caller's locks are released: both take
		// them.
		return vfs.OpenResult{}, func() { fs.closed(ctx, f.Share, f.ID, cr) }, err
	}
	if overwrite && via != nil {
		fs.changedIn(ctx, via, openstate.Change{What: openstate.ChangeSize | openstate.ChangeTimes, Name: req.Name})
	}
	return vfs.OpenResult{Ref: ref, File: file, MaximalAccess: maximal, Grant: o.Grant, Confirm: o.Confirm}, nil, nil
}

// openedFile is the file an open of f, which h names, answers: truncated first
// for an overwrite, then read again.
func (fs *Filesystem) openedFile(ctx context.Context, a *admitted, h vfs.Handle, f vfs.File, overwrite bool) (vfs.File, error) {
	if !overwrite {
		return fs.joined(ctx, f)
	}
	if err := fs.truncate(ctx, a, h, f, entity.Attrs{}.WithSize(0)); err != nil {
		return vfs.File{}, err
	}
	return fs.file(ctx, a, h)
}

// reconnect attaches the durable open req names to the call's session, as
// open state matches it, the attach holding no lock: it changes no file. Its maximal
// access is the caller's without privilege over SMB, as a new open answers
// it. The reads after the attach run on a context the call's end does not
// cancel, though Drain's deadline does, so a client dropping mid-reconnect
// does not fail them.
//
// decision: a failure reading the file, its overlay or its rights after
// open state attached the open answers an error for a reconnect that
// happened, and the open is closed again, as a durable open that times out
// is: the client's retry is refused as no reclaim and it loses the durable
// handle, but nothing it cannot reach stays held. One cut short by Drain's
// deadline stays attached, as Drain leaves every open to the next start.
// Open state has no way to park the open again; park it on that failure
// once open state offers it, if such failures are seen.
func (fs *Filesystem) reconnect(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	m := openstate.ReconnectMatch{Share: a.share.ID, Create: req.CreateID, Lease: req.Lease}
	o, err := fs.opens.Reconnect(ctx, a.who(), req.Reconnect.Open, m, c.Session)
	if err != nil {
		return vfs.OpenResult{}, openErr(err)
	}
	ctx, cancel := fs.detached(ctx)
	defer cancel()
	h := entity.NewHandle(a.share.ID, o.Open.File)
	var via vfs.Handle
	if !o.Open.Via.IsZero() {
		via = entity.NewHandle(a.share.ID, o.Open.Via)
	}
	ref := vfs.OpenRef{Kind: openstate.RefOpened, State: o.Open.Ref, Open: o.Open.ID, Handle: h}
	f, err := fs.file(ctx, a, h)
	var max entity.AccessMask
	if err == nil {
		max, err = fs.authz.Rights(ctx, forOpen(a.caller, c.Protocol, false), h, via, entity.EntryRights)
		err = storeErr(err)
	}
	if err != nil {
		if ctx.Err() != nil {
			// Drain's deadline ended the reads: the open stays attached, as
			// Drain leaves every open to the next start's grace.
			return vfs.OpenResult{}, err
		}
		// No client holds the reference to close it.
		unlock := fs.Admission(o.Open.File)
		cr, cerr := fs.opens.Close(ctx, a.who(), ref, vfs.Seqs{})
		unlock()
		if cerr != nil {
			fs.log.Warn("vfs: a failed reconnect not closed", "file", o.Open.File, "err", cerr)
		} else {
			fs.closed(ctx, a.share.ID, o.Open.File, cr)
		}
		return vfs.OpenResult{}, err
	}
	return vfs.OpenResult{Ref: ref, File: f, MaximalAccess: max, Grant: o.Grant}, nil
}

// detached is ctx without the call's own end, for what a call finishes once
// open state recorded its open, an undo or a reconnect's close: Drain's
// deadline and Stop still end it, as they end every call.
func (fs *Filesystem) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(fs.stop, cancel)
	return ctx, func() { stop(); cancel() }
}

// uncancelled is detached keeping ctx's deadline, for what a close or a
// name's removal leaves: a client dropping does not end it, while the
// call's deadline, Drain's and Stop still do (RFC 17 §5.3). A deferred
// unlink the call's deadline ends is refused and counted, as any refused
// one is.
func (fs *Filesystem) uncancelled(ctx context.Context) (context.Context, context.CancelFunc) {
	d, ok := ctx.Deadline()
	ctx, cancel := fs.detached(ctx)
	if !ok {
		return ctx, cancel
	}
	ctx, end := context.WithDeadline(ctx, d)
	return ctx, func() { end(); cancel() }
}

// forOpen is c as an open over protocol p is decided: over SMB, which
// confines privilege to a handle opened with backup or restore intent,
// privileged only with that intent.
func forOpen(c authz.Caller, p entity.Protocol, backup bool) authz.Caller {
	if p == entity.ProtocolSMB && !backup {
		c.Privileged = false
	}
	return c
}

// pseudoOpen answers an open in the pseudo-filesystem, h a pseudo directory.
//
// decision: an open there is refused, as every node is a directory no client
// may change: ErrReadOnly for one that may create, as a change there is,
// ErrIsDir otherwise, as NFSv4 never opens a directory and SMB never reaches
// the pseudo-filesystem. Overturned by an adapter opening a pseudo directory
// to list it.
func (fs *Filesystem) pseudoOpen(ctx context.Context, h vfs.Handle, d vfs.Disposition) error {
	if _, err := fs.pseudoNode(ctx, h); err != nil {
		return err
	}
	if creates(d) {
		return vfs.ErrReadOnly
	}
	return vfs.ErrIsDir
}

// creates reports whether d creates a file the name does not name.
func creates(d vfs.Disposition) bool {
	return d == vfs.CreateNew || d == vfs.OpenOrCreate || d == vfs.OverwriteOrCreate || d == vfs.Supersede
}

// overwrites reports whether d truncates a file the name names.
func overwrites(d vfs.Disposition) bool {
	return d == vfs.Overwrite || d == vfs.OverwriteOrCreate || d == vfs.Supersede
}

// Downgrade narrows the open ref names to access and deny, and answers its
// new reference sequence.
func (fs *Filesystem) Downgrade(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs, access vfs.Access, deny vfs.Access) (uint32, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	a, unlock, release, err := fs.opened(ctx, c, ref, false)
	if err != nil {
		return 0, err
	}
	defer release()
	defer unlock()
	seq, err := fs.opens.Downgrade(ctx, a.who(), ref, q, access, deny)
	return seq, openErr(err)
}

// ConfirmOpen confirms the new owner of the open ref names, and answers the
// open's new reference sequence.
func (fs *Filesystem) ConfirmOpen(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (uint32, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	a, unlock, release, err := fs.opened(ctx, c, ref, false)
	if err != nil {
		return 0, err
	}
	defer release()
	defer unlock()
	seq, err := fs.opens.ConfirmOpen(ctx, a.who(), ref, q)
	return seq, openErr(err)
}

// Close ends the open ref names, under its file's admission lock, then, with
// that lock released, runs what open state left: the deferred unlink of a
// delete-pending file, answered with its directory's WCC, and the release of
// a file left with no name and no open.
func (fs *Filesystem) Close(ctx context.Context, c vfs.Call, ref vfs.OpenRef, q vfs.Seqs) (*vfs.WCC, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	a, unlock, release, err := fs.opened(ctx, c, ref, true)
	if err != nil {
		return nil, err
	}
	defer release()
	cr, err := fs.opens.Close(ctx, a.who(), ref, q)
	unlock()
	if err != nil {
		return nil, openErr(err)
	}
	return fs.closed(ctx, a.share.ID, fileOf(ref.Handle), cr), nil
}

// opened admits a call through the open ref names to the share of the
// handle it carries, and takes that file's admission lock. It answers the
// lock's release, then the call's uncount: a pseudo directory's handle holds
// no open, BadState. releases is a call that only releases state, as admit
// takes it. Open state refuses the open to any client but its own
// and any principal but its opener, NotYours.
//
// decision: an NFSv4.0 call quoting an NFSv4.1 client's open is not refused
// here for its client. The client record's Adapter bytes carry the minor
// version that confirmed it, which the NFSv4 adapter reads before it names
// the call's client, refusing a stateid of a client confirmed under the
// other minor version itself, so no such call reaches the service; the
// service never reads those bytes. What the service refuses is a delegation
// presented on no session (onSession). Overturned if a second NFSv4 adapter
// is ever written that does not read them.
func (fs *Filesystem) opened(ctx context.Context, c vfs.Call, ref vfs.OpenRef, releases bool) (*admitted, func(), func(), error) {
	if err := onSession(c, &ref); err != nil {
		return nil, nil, nil, err
	}
	a, release, err := fs.admitHandleFor(ctx, c, ref.Handle, releases)
	if err == nil && a == nil {
		err = &vfs.StateError{Kind: vfs.StateBadState}
	}
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(fileOf(ref.Handle))}})
	if err != nil {
		release()
		return nil, nil, nil, errContended
	}
	return a, unlock, release, nil
}

// closed runs what a close left file f of share, a close of the service's or
// one open state made itself, holding none of f's locks: the deferred unlink
// of a delete-pending f, then the release of an f left with no name and no
// open. It answers the directory's WCC when the deferred unlink ran at once.
// Both run on the call, on a context a client dropping does not cancel and
// the call's deadline still ends, so a client dropping once the close
// committed neither loses the delete on close nor leaves f's content held
// until the next start.
func (fs *Filesystem) closed(ctx context.Context, share entity.ShareID, f entity.FileID, cr openstate.CloseResult) *vfs.WCC {
	ctx, cancel := fs.uncancelled(ctx)
	defer cancel()
	var w *vfs.WCC
	if d := cr.Unlink; d != nil {
		w = fs.unlinkClosed(ctx, share, f, *d, 0)
	}
	if cr.Orphan {
		if err := fs.releaseAlone(ctx, share, f); err != nil {
			fs.log.Warn("vfs: a release after a last close failed", "file", f, "err", err)
		}
	}
	return w
}

// unlinkRetries is how many times a deferred unlink answered ErrDelay is
// tried again.
//
// decision: a deferred unlink open state delays, while it recalls a grant or
// breaks a lease, is tried again in the background, the first after 50ms and
// each later one after twice the wait before it, at most 2s: eight retries
// span about 7s. One met by grace is tried every 2s until the grace ends, as
// no client is left to retry it. One still delayed after the retries, or
// when the filesystem stops first, is refused. Overturned by a recall found
// to outlast them; then retry when open state reports the recall resolved.
//
// While a retry waits, the file is neither delete pending nor unlinked:
// open state handed the mark back at the close. A new open of it is
// granted, and the retry then removes the name from under that open; a
// create of its name is refused ErrExist rather than ErrDeletePending; a
// rename of it succeeds, and the retry then finds no entry, or another file
// created under the name, and removes nothing, the delete lost without being
// counted. Overturned by a delete on close found lost this way; then keep
// the file delete pending in open state until the service reports the
// unlink's outcome.
const unlinkRetries = 8

// unlinkRetry is the wait before a delayed deferred unlink's first retry,
// and unlinkWait the longest wait, which each try met by grace waits.
var unlinkRetry, unlinkWait = 50 * time.Millisecond, 2 * time.Second

// unlinkClosed is attempt try of the deferred unlink d records for f, and
// answers the directory's WCC when it ran. One delayed is retried in the
// background, answering nil. One refused, or delayed past unlinkRetries, is
// logged at Warn and counted, and f keeps its name: its client has closed,
// and no reply reaches it.
func (fs *Filesystem) unlinkClosed(ctx context.Context, share entity.ShareID, f entity.FileID, d entity.DeleteRecord, try int) *vfs.WCC {
	r, err := fs.deferredUnlink(ctx, share, f, d)
	var delay *vfs.Delay
	switch {
	case errors.As(err, &delay) && try < unlinkRetries, errors.Is(err, vfs.ErrGrace):
		next, wait := try+1, min(unlinkRetry<<min(try, 6), unlinkWait)
		if errors.Is(err, vfs.ErrGrace) {
			next, wait = try, unlinkWait // a grace answer spends no retry
		}
		if !fs.background(func(ctx context.Context) {
			t := time.NewTimer(wait)
			defer t.Stop()
			select {
			case <-t.C:
				defer fs.count(share)()
				fs.unlinkClosed(ctx, share, f, d, next)
			case <-ctx.Done():
				fs.refused(f, ctx.Err())
			}
		}) {
			fs.refused(f, err) // stopped: no retry runs
		}
		return nil
	case err != nil:
		fs.refused(f, err)
		return nil
	}
	if r == nil {
		return nil
	}
	return &r.Dir
}

// refused logs and counts a deferred unlink of f refused for err.
func (fs *Filesystem) refused(f entity.FileID, err error) {
	fs.refusals.Inc()
	fs.log.Warn("vfs: a delete on close refused after its last close", "file", f, "err", err)
}

// deferredUnlink is the delete on close d records for file f, run as an
// unlink of d's entry by the principal that marked f, through the
// directory's and f's locks taken afresh. An entry that no longer names f
// removes nothing, answering nil.
//
// decision: the unlink is decided as the marking principal resolved afresh,
// privileged only when the marking open was, not through the marking open's
// grant, which the store's unlink cannot be handed. A principal that lost
// delete-child on the directory, or its grant on the share, since its open is
// refused, and the file keeps its name. Overturned once the store's Unlink
// takes an open's grant.
func (fs *Filesystem) deferredUnlink(ctx context.Context, share entity.ShareID, f entity.FileID, d entity.DeleteRecord) (*vfs.UnlinkResult, error) {
	a, err := fs.marker(ctx, share, d)
	if err != nil {
		return nil, err
	}
	dir := vfs.Target{H: entity.NewHandle(share, d.Parent)}
	for {
		seen, err := fs.entry(ctx, a, dir.H, d.Name)
		switch {
		case errors.Is(err, vfs.ErrNotFound):
			return nil, nil
		case err != nil:
			return nil, err
		case seen.ID != f:
			return nil, nil
		}
		r, again, err := fs.unlinkLocked(ctx, a, vfs.Call{Protocol: entity.ProtocolSMB}, dir, d.Name, seen, vfs.OpUnlink)
		if !again {
			return &r, err
		}
	}
}

// marker is the admission of the principal d names as marking a file of
// share delete on close, resolved on the share as a proven caller is.
func (fs *Filesystem) marker(ctx context.Context, share entity.ShareID, d entity.DeleteRecord) (*admitted, error) {
	sh, p, err := fs.meta.ShareIndex().Share(ctx, share)
	if err != nil {
		return nil, storeErr(err)
	}
	if err := sh.State.Gate(); err != nil {
		return nil, storeErr(err)
	}
	ro := entity.NotNarrowed
	if p.ReadOnly {
		ro = entity.NarrowedByShare
	}
	id := vfs.Identity{Proof: vfs.ProofSecret, Evidence: vfs.Evidence{Principal: d.By}}
	a := &admitted{share: sh, policy: p}
	a.caller = authz.Caller{Share: sh.ID, Proved: id, Protocol: entity.ProtocolSMB}
	a.caller.Identity, a.caller.Mapped, err = admit.Resolve(ctx, principals{fs.meta.Principals()}, id, sh.ID, p, ro)
	if err != nil {
		return nil, storeErr(err)
	}
	// The unlink is privileged only when the marking open was: over SMB an
	// admin's open without backup intent was decided without privilege.
	a.caller.Privileged = d.Privileged
	return a, nil
}

// releaser marks a file being released, so no open lands between the
// release's check of its holders and the engine's Delete: held reports the
// file open, or its release already under way, and changes nothing; done
// ends the mark with the release's outcome.
type releaser interface {
	Releasing(ctx context.Context, f openstate.FileKey) (done func(released bool), held bool, err error)
}

// openMark is the releaser over open state's Opens.Releasing: a file open,
// a clone's hold included, or already being released is held.
type openMark struct{ opens openstate.Opens }

func (m openMark) Releasing(ctx context.Context, f openstate.FileKey) (func(bool), bool, error) {
	done, err := m.opens.Releasing(ctx, f)
	switch {
	case errors.Is(err, openstate.ErrFileOpen), errors.Is(err, openstate.ErrReleasing):
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}
	return done, false, nil
}

// release runs the release of the file key names, holding its admission
// lock: marked as being released, then the engine's Delete, whose
// transaction re-checks the holders. A file found held is left to the close
// or release holding it. The release's outcome goes to the mark's done as
// RFC 17 §5.3 says: committed or aborted, it ends the mark; any other
// failure within the call keeps it, calling no done (RFC 8 §8.1, step 1),
// and the release is left to the next start's recovery. A release that
// cannot finish within the call answers a retry channel, waited on by
// awaitRelease, and release answers as if it were done.
//
// ponytail: the pending releases a release writes for the file's named
// streams are run by the next start's recovery, not after it, as Delete
// reports none. Run each here once the engine's Delete reports them.
func (fs *Filesystem) release(ctx context.Context, key openstate.FileKey) error {
	done, held, err := fs.marks.Releasing(ctx, key)
	if err != nil || held {
		return openErr(err)
	}
	retry, err := fs.engine.Delete(ctx, key.Share, key.ID)
	switch {
	case retry != nil:
		fs.awaitRelease(key, done, retry)
		return nil
	case err == nil:
		done(true)
		return nil
	case errors.Is(err, vfserr.ErrFileOpen):
		done(false)
		return nil
	}
	return storeErr(err)
}

// awaitRelease waits, in the background and never inside Sink.Closed, on the
// retry channel of key's pending release, which keeps its mark meanwhile
// (RFC 17 §5.3): committed or aborted, the outcome goes to done; a clone's
// delay goes to done(false) and the release is run again, marked anew, as a
// call the filesystem admits, so a drain refuses it or joins it and its
// deadline ends it; any other outcome is logged and keeps the mark, leaving
// the release to the next start's recovery. From Drain on no release is run
// again, and the wait ends at Stop without its done (RFC 17 §2.5).
func (fs *Filesystem) awaitRelease(key openstate.FileKey, done func(bool), retry <-chan error) {
	fs.background(func(ctx context.Context) {
		var out error
		select {
		case out = <-retry:
		case <-ctx.Done():
			return
		}
		var d *vfs.Delay
		switch {
		case out == nil:
			done(true)
		case errors.Is(out, vfserr.ErrFileOpen):
			done(false)
		case errors.As(out, &d) && d.Cause == vfs.DelayClone:
			done(false)
			ctx, end, err := fs.enter(ctx)
			if err != nil {
				return
			}
			defer end()
			if err := fs.releaseAlone(ctx, key.Share, key.ID); err != nil {
				fs.log.Warn("vfs: a release run again after a clone failed", "file", key.ID, "err", err)
			}
		default:
			fs.log.Warn("vfs: a pending release did not finish", "file", key.ID, "err", out)
		}
	})
}

// releaseAlone is release of file f of share, by a caller holding none of
// f's locks: it takes f's admission lock first, then reads f, so of two
// releases of one file the second finds it gone. A file already gone is
// released already.
func (fs *Filesystem) releaseAlone(ctx context.Context, share entity.ShareID, f entity.FileID) error {
	defer fs.Admission(f)()
	rec, err := fs.meta.Files().Get(ctx, system, entity.NewHandle(share, f))
	switch {
	case errors.Is(storeErr(err), vfs.ErrStale):
		return nil
	case err != nil:
		return storeErr(err)
	}
	return fs.release(ctx, keyOf(rec))
}

// system is the identity the service reads a file's record as, outside any
// call, to place it: privileged, holding an admin grant.
var system = entity.Identity{Privileged: true, Access: entity.AccessAdmin}

// Closed runs what a close open state made itself left file f, as Close
// runs a close's, on a goroutine of its own, counted against f's share.
//
// ponytail: f's share is found by reading f in each share listed, as the
// event names the file alone, O(shares) reads. Have the table name the share
// with the file once a node serves many shares.
func (fs *Filesystem) Closed(f entity.FileID, r openstate.CloseResult) {
	fs.background(func(ctx context.Context) {
		share, err := fs.shareOf(ctx, f)
		if err != nil {
			fs.log.Warn("vfs: a close open state made is not run", "file", f, "err", err)
			return
		}
		defer fs.count(share)()
		fs.closed(ctx, share, f, r)
	})
}

// shareOf is the listed share holding file f.
func (fs *Filesystem) shareOf(ctx context.Context, f entity.FileID) (entity.ShareID, error) {
	_, shares, err := fs.meta.ShareIndex().List(ctx)
	if err != nil {
		return entity.ShareID{}, storeErr(err)
	}
	for _, sh := range shares {
		if _, err := fs.meta.Files().Get(ctx, system, entity.NewHandle(sh.ID, f)); err == nil {
			return sh.ID, nil
		}
	}
	return entity.ShareID{}, vfs.ErrStale
}

// GraceEnded runs, on a goroutine of its own, the pending release of every
// file of share in its shard s, as nothing ran them before the restart,
// counted against the share.
func (fs *Filesystem) GraceEnded(share entity.ShareID, s openstate.ShardID) {
	fs.background(func(ctx context.Context) { fs.recoverReleases(ctx, share, s) })
}

// recoverReleases is GraceEnded's run of share's pending releases in shard
// s, read from the store's index of them.
func (fs *Filesystem) recoverReleases(ctx context.Context, share entity.ShareID, s openstate.ShardID) {
	defer fs.count(share)()
	for id, err := range fs.meta.Namespace().PendingReleases(ctx, share, uint64(s)) {
		if err != nil {
			fs.log.Warn("vfs: pending releases not read at grace's end", "share", share, "err", err)
			return
		}
		if err := fs.releaseAlone(ctx, share, id); err != nil {
			fs.log.Warn("vfs: a pending release failed at grace's end", "file", id, "err", err)
		}
	}
}

// onSession refuses ref, when it names a delegation, on a call with no
// session: NotYours. A delegation is its client's, not an opener's, and
// NFSv4.0, the one protocol naming a client with no session, is offered
// none, so a delegation's stateid on no session is never its holder's own.
func onSession(c vfs.Call, ref *vfs.OpenRef) error {
	if ref != nil && ref.Kind == vfs.RefDelegated && c.Session == (vfs.SessionRef{}) {
		return &vfs.StateError{Kind: vfs.StateNotYours}
	}
	return nil
}
