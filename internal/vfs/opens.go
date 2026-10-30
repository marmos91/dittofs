package vfs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/vfs/admit"
	"github.com/marmos91/dittofs/internal/vfs/authz"
	"github.com/marmos91/dittofs/internal/vfs/lockorder"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
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
	switch {
	case req.Reconnect != nil:
		return fs.reconnect(ctx, a, c, req)
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
// seen's, or, to create, the directory's create locks, and looks the name up
// again under them: again is set when it moved.
func (fs *Filesystem) openLocked(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest, seen *vfs.File) (_ vfs.OpenResult, again bool, _ error) {
	set := creating(req.Dir)
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
	defer unlock()
	f, err := fs.entryIf(ctx, a, req.Dir, req.Name)
	if err != nil || (f == nil) != (seen == nil) || f != nil && f.ID != seen.ID {
		return vfs.OpenResult{}, err == nil, err
	}
	if f != nil {
		if req.Disposition == vfs.CreateNew {
			return vfs.OpenResult{}, false, vfs.ErrExist
		}
		r, err := fs.grant(ctx, a, c, req, entity.NewHandle(a.share.ID, f.ID), *f, false)
		return r, false, err
	}
	if !creates(req.Disposition) {
		return vfs.OpenResult{}, false, vfs.ErrNotFound
	}
	cr, err := fs.createLocked(ctx, a, c, vfs.OpOpen, req.Dir, req.Name, req.Create)
	if err != nil {
		return vfs.OpenResult{}, false, err
	}
	// The new file's admission lock is taken last, out of the one order:
	// no other call can name the file yet.
	defer fs.Admission(cr.File.ID)()
	r, err := fs.grant(ctx, a, c, req, cr.Handle, cr.File, true)
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
	defer unlock()
	f, err := fs.meta.Files().Get(ctx, a.caller.Identity, req.H)
	switch {
	case err != nil:
		return vfs.OpenResult{}, storeErr(err)
	case req.Disposition == vfs.CreateNew:
		return vfs.OpenResult{}, vfs.ErrExist
	}
	return fs.grant(ctx, a, c, req, req.H, f, false)
}

// grant decides and records the open of f, which h names, holding f's
// admission lock: the rights it asks, decided through the directory it was
// resolved through, a delete on close asking delete and an overwrite write
// data; for a maximal open every right the caller holds besides; then open
// state's deny modes, which a handle-caching grant over a conflict breaks
// first, answering ErrDelay; then an overwrite's truncate, under f's accept
// lock. created is set for a file the open made.
//
// decision: an open that created its file is granted what it asks without
// deciding it on the new file, as the add right on its directory was
// decided: a create asking write with a mode granting none, as an extracting
// archiver sends, is granted, as POSIX's open with O_CREAT is. Its maximal
// access is still the file's own. Overturned by an inherited ACL found to
// deny its creator a right the create needs.
func (fs *Filesystem) grant(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest, h vfs.Handle, f vfs.File, created bool) (vfs.OpenResult, error) {
	overwrite := overwrites(req.Disposition) && !created
	if overwrite && f.IsDir() {
		return vfs.OpenResult{}, vfs.ErrIsDir
	}
	var via vfs.Handle
	if len(req.Name) > 0 {
		via = req.Dir
	}
	access := req.Access &^ vfs.MaximumAllowed
	if req.DeleteOnClose {
		access |= entity.Delete
	}
	if overwrite {
		access |= entity.WriteData
	}
	max, err := fs.authz.Rights(ctx, a.caller, h, via, entity.EntryRights)
	if err != nil {
		return vfs.OpenResult{}, storeErr(err)
	}
	if req.Access&vfs.MaximumAllowed != 0 {
		if max == 0 {
			return vfs.OpenResult{}, vfs.ErrAccess
		}
		access |= max
	}
	if !created {
		if err := fs.authz.Decide(ctx, a.caller, vfs.OpOpen, h, via, req.Name, access); err != nil {
			return vfs.OpenResult{}, storeErr(err)
		}
	}
	who := a.who()
	o, err := fs.opens.Open(ctx, who, keyOf(f), openstate.OpenRequest{
		Unlinked: f.Nlink == 0 && !f.IsDir(), Via: fileOf(via), Name: req.Name,
		Privileged: req.Backup && a.caller.Privileged, Access: access, Deny: req.Deny, DeleteOnClose: req.DeleteOnClose,
		Loss: fs.engine.LossSeq(f.ID), Owner: req.Owner, Seqs: req.Seqs,
		Lease: req.Lease, Parent: req.Parent, Epoch: req.Epoch, Want: req.Want,
		SetID: f.Mode&(entity.ModeSetuid|entity.ModeSetgid) != 0, Reclaim: req.Reclaim,
		Durability: req.Durability, Durable: openstate.DurableKeys{Timeout: req.Timeout, Create: req.CreateID, Instance: req.Instance},
		Session: c.Session, Replay: req.Replay, MaxRead: max&entity.ReadData != 0, Channel: c.Channel.Seq,
	})
	if err != nil {
		return vfs.OpenResult{}, openErr(err)
	}
	ref := vfs.OpenRef{Kind: openstate.RefOpened, State: o.Open.Ref, Open: o.Open.ID, Handle: h}
	if overwrite {
		if err := fs.truncate(ctx, a, h, f, entity.Attrs{}.WithSize(0)); err != nil {
			// The open is undone. An unlinked f it alone held is released
			// here, under the admission lock already held, as a close
			// releases it.
			if cr, cerr := fs.opens.Close(ctx, who, ref, req.Seqs); cerr == nil && cr.Orphan {
				if rerr := fs.release(ctx, keyOf(f)); rerr != nil {
					fs.log.Warn("vfs: a release after a failed overwrite failed", "file", f.ID, "err", rerr)
				}
			}
			return vfs.OpenResult{}, err
		}
	}
	file, err := fs.joined(ctx, f)
	if overwrite {
		file, err = fs.file(ctx, a, h)
	}
	if err != nil {
		return vfs.OpenResult{}, err
	}
	return vfs.OpenResult{Ref: ref, File: file, MaximalAccess: max, Grant: o.Grant, Confirm: o.Confirm}, nil
}

// reconnect attaches the durable open req names to the call's session, as
// open state matches it, holding no lock: it changes no file.
func (fs *Filesystem) reconnect(ctx context.Context, a *admitted, c vfs.Call, req vfs.OpenRequest) (vfs.OpenResult, error) {
	m := openstate.ReconnectMatch{Share: a.share.ID, Create: req.CreateID, Lease: req.Lease}
	o, err := fs.opens.Reconnect(ctx, a.who(), req.Reconnect.Open, m, c.Session)
	if err != nil {
		return vfs.OpenResult{}, openErr(err)
	}
	h := entity.NewHandle(a.share.ID, o.Open.File)
	var via vfs.Handle
	if !o.Open.Via.IsZero() {
		via = entity.NewHandle(a.share.ID, o.Open.Via)
	}
	f, err := fs.file(ctx, a, h)
	if err != nil {
		return vfs.OpenResult{}, err
	}
	max, err := fs.authz.Rights(ctx, a.caller, h, via, entity.EntryRights)
	if err != nil {
		return vfs.OpenResult{}, storeErr(err)
	}
	ref := vfs.OpenRef{Kind: openstate.RefOpened, State: o.Open.Ref, Open: o.Open.ID, Handle: h}
	return vfs.OpenResult{Ref: ref, File: f, MaximalAccess: max, Grant: o.Grant}, nil
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
	a, unlock, release, err := fs.opened(ctx, c, ref)
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
	a, unlock, release, err := fs.opened(ctx, c, ref)
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
	a, unlock, release, err := fs.opened(ctx, c, ref)
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
// no open, BadState. Open state refuses the open to any client but its own
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
func (fs *Filesystem) opened(ctx context.Context, c vfs.Call, ref vfs.OpenRef) (*admitted, func(), func(), error) {
	if err := onSession(c, &ref); err != nil {
		return nil, nil, nil, err
	}
	a, release, err := fs.admitHandle(ctx, c, ref.Handle)
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
func (fs *Filesystem) closed(ctx context.Context, share entity.ShareID, f entity.FileID, cr openstate.CloseResult) *vfs.WCC {
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
// span about 7s. One still delayed after them, or when the filesystem stops
// first, is refused. Overturned by a recall found to outlast them; then
// retry when open state reports the recall resolved.
const unlinkRetries = 8

// unlinkClosed is attempt try of the deferred unlink d records for f, and
// answers the directory's WCC when it ran. One delayed is retried in the
// background, answering nil. One refused, or delayed past unlinkRetries, is
// logged at Warn and counted, and f keeps its name: its client has closed,
// and no reply reaches it. A release failing once the unlink has committed
// is logged as a failed release; the unlink ran.
func (fs *Filesystem) unlinkClosed(ctx context.Context, share entity.ShareID, f entity.FileID, d entity.DeleteRecord, try int) *vfs.WCC {
	r, err := fs.deferredUnlink(ctx, share, f, d)
	var failed releaseFailed
	var delay *vfs.Delay
	switch {
	case errors.As(err, &failed):
		fs.log.Warn("vfs: a release after a deferred unlink failed", "file", f, "err", err)
	case errors.As(err, &delay) && try < unlinkRetries:
		wait := min(50*time.Millisecond<<try, 2*time.Second)
		fs.background(func(ctx context.Context) {
			t := time.NewTimer(wait)
			defer t.Stop()
			select {
			case <-t.C:
				defer fs.count(share)()
				fs.unlinkClosed(ctx, share, f, d, try+1)
			case <-ctx.Done():
				fs.refused(f, ctx.Err())
			}
		})
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
// privileged when the marking open was, not through the marking open's
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
	a.caller.Privileged = a.caller.Privileged || d.Privileged
	return a, nil
}

// releaser marks a file being released, so no open lands between the
// release's check of its holders and the engine's Delete: held reports the
// file open, or its release already under way, and changes nothing; done
// ends the mark with the release's outcome.
type releaser interface {
	Releasing(ctx context.Context, f openstate.FileKey) (done func(released bool), held bool, err error)
}

// noMark is the releaser until open state marks releases.
//
// ponytail: marks nothing and finds no open, so an open landing between a
// release and the engine's Delete is not refused, and a pending release run
// at grace's end deletes the content of a file a reconnected persistent open
// holds; the engine's release transaction re-checks the holders the store
// records, which only the real engine does. Replace it with open state's
// Opens.Releasing, built on fix/openstate-stream-deny, its ErrFileOpen and
// ErrReleasing answered as held, once that branch lands on storage-v2.
type noMark struct{}

func (noMark) Releasing(context.Context, openstate.FileKey) (func(bool), bool, error) {
	return func(bool) {}, false, nil
}

// release runs the release of the file key names, holding its admission
// lock: marked as being released, then the engine's Delete, whose
// transaction re-checks the holders. A file found held is left to the close
// or release holding it.
//
// ponytail: the pending releases a release writes for the file's named
// streams are run by the next start's recovery, not after it, as Delete
// reports none. Run each here once the engine's Delete reports them.
func (fs *Filesystem) release(ctx context.Context, key openstate.FileKey) error {
	done, held, err := fs.marks.Releasing(ctx, key)
	if err != nil || held {
		return openErr(err)
	}
	err = fs.engine.Delete(ctx, key.ID)
	done(err == nil)
	return storeErr(err)
}

// releaseAlone is release of file f of share, by a caller holding none of
// f's locks: it takes f's admission lock first. A file already gone is
// released already.
func (fs *Filesystem) releaseAlone(ctx context.Context, share entity.ShareID, f entity.FileID) error {
	rec, err := fs.meta.Files().Get(ctx, system, entity.NewHandle(share, f))
	switch {
	case errors.Is(storeErr(err), vfs.ErrStale):
		return nil
	case err != nil:
		return storeErr(err)
	}
	defer fs.Admission(f)()
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
		unlock := fs.Admission(id)
		err := fs.release(ctx, openstate.FileKey{Share: share, Shard: s, ID: id})
		unlock()
		if err != nil {
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
