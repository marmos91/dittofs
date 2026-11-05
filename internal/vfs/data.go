package vfs

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/internal/vfs/quota"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The Data area: reads, writes, commits, hole punching and seeking. Each is
// admitted to the share its handle names, judged through the reference it
// carries, and runs under the file's admission lock from that judgement to
// the I/O it admits, so no open, lock or grant that conflicts lands between
// them. A change of content is staged under the file's accept lock too, and
// answered with the WCC read in that one hold. No sync runs under either: a
// commit, a stable write's included, runs once the admission lock is
// released, so an open, a close or a lock on the file never waits on one.

// Read reads the file ref names into dst from off, and answers how many
// verified bytes it read and whether they reach the file's end. The engine
// may answer n bytes and then the error for the chunk after them; both are
// answered, and the bytes that failed are never padded.
func (fs *Filesystem) Read(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, dst []byte) (int, bool, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, false, err
	}
	defer done()
	_, f, unlock, release, err := fs.io(ctx, c, ref, vfs.OpRead, off, int64(len(dst)), false)
	if err != nil {
		return 0, false, err
	}
	defer release()
	defer unlock()
	n, err := fs.engine.Read(ctx, f.Share, f.ID, off, dst)
	return n, err == nil && off+int64(n) >= f.Size, storeErr(err)
}

// Write stages src at off in the file ref names, and answers the engine's
// verifier, sampled before the bytes were staged, with the file's WCC. What
// its commit will charge, the bytes it lands in holes and past the size its
// staged writes leave, is reserved against the quotas charging the file's
// owner, group and project before anything changes, and refused ErrQuota at
// once past them. A stable write is answered once the engine's Commit over
// its extent has, which runs after the file's admission lock is released. A Commit answering
// another verifier means a loss may have taken the write between staging and
// the sync, and a client does not consult a stable reply's verifier, so the
// write is answered ErrDelay (DelayCommit) for the client to resend, never
// as stable.
func (fs *Filesystem) Write(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, src []byte, st vfs.Stability) (vfs.WriteResult, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.WriteResult{}, err
	}
	defer done()
	a, f, unlock, release, err := fs.io(ctx, c, ref, vfs.OpWrite, off, int64(len(src)), true)
	if err != nil {
		return vfs.WriteResult{}, err
	}
	defer release()
	var res *quota.Reservation
	add, err := fs.charges(ctx, f, off, int64(len(src)))
	if err == nil {
		res, err = fs.reserve(ctx, a, charged(f), entity.Usage{Bytes: add})
	}
	if err != nil {
		unlock()
		return vfs.WriteResult{}, err
	}
	var wr vfs.WriteResult
	// decision: a write passes no suspended times: open state keeps the
	// times an open suspends but answers them to no caller, so a write
	// through such an open still moves mtime and ctime. Pass them once
	// CheckIO answers the open's.
	opts := WriteOpts{
		// The reservation travels with the staged bytes: the engine's Done,
		// on the commit that charges them or on their drop, releases it,
		// never this call's return. A write the engine refused it never
		// calls, so that one is released here.
		Done: res.Release,
	}
	wr.WCC, err = fs.stage(ctx, c, a, ref.Handle, f, off, true, func() (err error) {
		wr.Verifier, err = fs.engine.Write(ctx, f.Share, f.ID, off, src, opts)
		return err
	})
	unlock()
	if err != nil {
		res.Release()
		return vfs.WriteResult{}, err
	}
	if stable(a, ref, st) {
		var v vfs.Verifier
		if v, _, err = fs.engine.Commit(ctx, f.Share, f.ID, off, int64(len(src))); err == nil && v != wr.Verifier {
			return vfs.WriteResult{}, &vfs.Delay{Cause: vfs.DelayCommit}
		}
	}
	wr.N = len(src)
	return wr, storeErr(err)
}

// charges is what a write of n bytes at off to f charges once committed:
// the bytes it lands in holes below f's size, as the engine's allocation
// answers them, and those past it; a gap it leaves past the size is a hole
// and charges nothing. A range the allocation answers no progress over is
// counted as holes.
//
// ponytail: a write below the size reads the allocation over its range on
// every write, whether or not any quota limits it. Skip the read for a
// charge no quota limits once it shows in a write profile.
func (fs *Filesystem) charges(ctx context.Context, f vfs.File, off, n int64) (int64, error) {
	end := min(off+n, f.Size)
	add := max(0, off+n-max(off, f.Size))
	for at := off; at < end; {
		sp, err := fs.engine.Allocation(ctx, f.Share, f.ID, at)
		if err != nil {
			return 0, storeErr(err)
		}
		next := min(sp.Offset+sp.Length, end)
		if next <= at {
			return add + end - at, nil
		}
		if sp.Class == entity.Hole {
			add += next - at
		}
		at = next
	}
	return add, nil
}

// stable reports whether a write asked st through ref must be stable before
// it is answered: when it asks so, and through any open on a continuously
// available share, whatever it asks.
//
// decision: a persistent open's write is made stable by its share's flag
// alone, as open state answers no open's durability. That holds while a
// persistent open is granted only on a continuously available share, as SMB
// grants one; overturned by a persistent open granted on any other share.
func stable(a *admitted, ref vfs.OpenRef, st vfs.Stability) bool {
	opened := ref.Kind == openstate.RefOpened || ref.Kind == openstate.RefLocked
	return st != vfs.Unstable || opened && a.policy.SMB&entity.SMBContinuouslyAvailable != 0
}

// Commit makes length bytes from off of the file ref names stable, length 0
// to its end, and answers the engine's verifier, sampled once the sync
// completed, which runs holding none of the file's locks. Through an SMB open
// it is a flush: open state answers ErrLost, once for the open, when content
// written while it was open was lost, judged under the admission lock taken
// again once the sync completed. A failed sync answers its own error, and
// still records the file's loss sequence for the open, so a loss is answered
// at most once, whether as ErrLost or as the failure; a retry, ErrDelay, or a
// cancelled call records nothing, so the retried flush still answers ErrLost.
//
// decision: a commit decides no right and asks open state nothing but an SMB
// flush's loss: it changes no content, only makes what was staged durable,
// so any caller admitted to the share may sync a file it can name. Overturned
// if syncing on demand is found to be a load a caller must hold a right for.
func (fs *Filesystem) Commit(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off, length int64) (vfs.Verifier, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.Verifier{}, err
	}
	defer done()
	if off < 0 || length < 0 {
		return vfs.Verifier{}, vfs.ErrInvalid
	}
	a, unlock, release, err := fs.opened(ctx, c, ref)
	if err != nil {
		return vfs.Verifier{}, err
	}
	defer release()
	f, err := fs.file(ctx, a, ref.Handle)
	if err == nil {
		err = content(f)
	}
	unlock()
	if err != nil {
		return vfs.Verifier{}, err
	}
	v, loss, err := fs.engine.Commit(ctx, f.Share, f.ID, off, length)
	if err != nil {
		err = storeErr(err)
		if errors.Is(err, vfs.ErrDelay) || ctx.Err() != nil {
			return vfs.Verifier{}, err
		}
		// decision: any other failure counts as the report of the file's
		// loss, since the engine does not name a failed window's error. So a
		// flush failing for another reason, after a loss from a corrupt
		// record that failed no sync, marks that loss seen without ErrLost.
		// Overturned once the engine's Commit names the failed window's
		// error, when only that one should record the loss.
		loss = fs.engine.LossSeq(f.Share, f.ID)
	}
	if c.Protocol == entity.ProtocolSMB && ref.Kind == openstate.RefOpened {
		unlock, lerr := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(f.ID)}})
		if lerr != nil {
			return vfs.Verifier{}, errContended
		}
		defer unlock()
		if ferr := fs.opens.Flushed(ctx, a.who(), ref.Open, loss); err == nil && ferr != nil {
			return vfs.Verifier{}, openErr(ferr)
		}
	}
	if err != nil {
		return vfs.Verifier{}, err
	}
	return v, nil
}

// Deallocate punches a hole of length bytes from off in the file ref names,
// checked and ordered as a write is, and answers its WCC; an append-only file
// refuses any, ErrPerm, as one never appends.
func (fs *Filesystem) Deallocate(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off, length int64) (vfs.WCC, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.WCC{}, err
	}
	defer done()
	a, f, unlock, release, err := fs.io(ctx, c, ref, vfs.OpDeallocate, off, length, true)
	if err != nil {
		return vfs.WCC{}, err
	}
	defer release()
	defer unlock()
	return fs.stage(ctx, c, a, ref.Handle, f, off, false, func() error {
		return fs.engine.Deallocate(ctx, f.Share, f.ID, off, length)
	})
}

// Seek answers the first offset at or after off of the file ref names that
// holds data, or a hole, as what asks, from the engine's allocation, or the
// file's size when it finds none before the end, which an adapter reads as
// end of file. From an offset at or past the size, judged against the size
// read under the file's admission lock, it answers ErrPastEnd.
func (fs *Filesystem) Seek(ctx context.Context, c vfs.Call, ref vfs.OpenRef, off int64, what vfs.SeekWhat) (int64, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	_, f, unlock, release, err := fs.io(ctx, c, ref, vfs.OpSeek, off, 0, false)
	if err != nil {
		return 0, err
	}
	defer release()
	defer unlock()
	if off >= f.Size {
		return 0, vfs.ErrPastEnd
	}
	for off < f.Size {
		sp, err := fs.engine.Allocation(ctx, f.Share, f.ID, off)
		if err != nil {
			return 0, storeErr(err)
		}
		if (sp.Class == entity.Hole) == (what == vfs.SeekHole) {
			return off, nil
		}
		end := sp.Offset + sp.Length
		if end <= off {
			break
		}
		off = end
	}
	return f.Size, nil
}

// io admits I/O of n bytes from off to the file ref names for op, takes the
// file's admission lock and reads it, then judges the caller. A write is
// ErrReadOnly while the share or the client holds the caller to read-only,
// whatever it comes through. Through an open or a lock state open state alone
// judges it, refusing any principal but the opener, NotYours, and any access
// the open was not granted: a write starting below the file's size wants
// write data, and one starting at or past it append data, so one holding
// append data without write data can only append, ErrAccess below the size,
// judged against the size read under the lock that every change of size takes.
// Anonymous or through a delegation, the chokepoint decides the caller's own
// right on the file first, by the same rule: write data admits a write
// anywhere, and append data alone one at or past the size; then open state
// judges it against every deny mode and mandatory lock held, breaking or
// recalling what conflicts. It answers the admission lock's unlock and the
// call's uncount apart, so a sync can run counted once the lock is dropped.
func (fs *Filesystem) io(ctx context.Context, c vfs.Call, ref vfs.OpenRef, op vfs.Op, off, n int64, write bool) (*admitted, vfs.File, func(), func(), error) {
	if off < 0 || n < 0 {
		return nil, vfs.File{}, nil, nil, vfs.ErrInvalid
	}
	a, unlock, release, err := fs.opened(ctx, c, ref)
	if err != nil {
		return nil, vfs.File{}, nil, nil, err
	}
	fail := func(err error) (*admitted, vfs.File, func(), func(), error) {
		unlock()
		release()
		return nil, vfs.File{}, nil, nil, err
	}
	f, err := fs.file(ctx, a, ref.Handle)
	if err == nil {
		err = content(f)
	}
	if err != nil {
		return fail(err)
	}
	if write && a.caller.ReadOnly != entity.NotNarrowed {
		return fail(vfs.ErrReadOnly)
	}
	want := entity.ReadData
	if write {
		want = entity.WriteData
	}
	if ref.Kind == openstate.RefAnonymous || ref.Kind == openstate.RefDelegated {
		ask := want
		if write && off >= f.Size {
			// Either right admits an append. Asking Rights, which audits
			// nothing, which one the caller holds lets Decide audit the
			// one decision that admits it, or append data's refusal.
			got, err := fs.authz.Rights(ctx, a.caller, ref.Handle, nil, entity.WriteData)
			if err != nil {
				return fail(storeErr(err))
			}
			if got&entity.WriteData == 0 {
				ask = entity.AppendData
			}
		}
		if err := fs.authz.Decide(ctx, a.caller, op, ref.Handle, nil, nil, ask); err != nil {
			return fail(storeErr(err))
		}
	}
	if write && off >= f.Size {
		want = entity.AppendData
	}
	r := openstate.ByteRange{Offset: uint64(off), Length: uint64(n)}
	if err := fs.checks.CheckIO(ctx, a.who(), keyOf(f), ref, r, want); err != nil {
		return fail(openErr(err))
	}
	return a, f, unlock, release, nil
}

// content refuses a file that holds no content: ErrIsDir for a directory,
// ErrInvalid for anything else but a regular file or a named stream.
func content(f vfs.File) error {
	switch {
	case f.IsDir():
		return vfs.ErrIsDir
	case !f.IsRegular() && !f.IsStream():
		return vfs.ErrInvalid
	}
	return nil
}

// stage runs do, a change of the content of f, which h names, at off, which
// may append only when appendable: an immutable file, or an append-only one
// for a change below its size or one that may not append, is ErrPerm before
// anything changes; then an unprivileged caller's change clears setuid and
// setgid in a committed change; then, under f's accept lock, the flags are
// checked again against the size the overlay leaves, the WCC's Before is
// read, do stages the change, and the After is read, all in that one hold.
func (fs *Filesystem) stage(ctx context.Context, c vfs.Call, a *admitted, h vfs.Handle, f vfs.File, off int64, appendable bool, do func() error) (vfs.WCC, error) {
	if flagsRefuse(f, off, appendable) {
		return vfs.WCC{}, vfs.ErrPerm
	}
	if _, err := fs.clearSetID(ctx, a, h, f, off >= f.Size); err != nil {
		return vfs.WCC{}, err
	}
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Accept: []lockorder.Key{lockorder.Key(f.ID)}})
	if err != nil {
		return vfs.WCC{}, errContended
	}
	defer unlock()
	before, err := fs.file(ctx, a, h)
	if err != nil {
		return vfs.WCC{}, err
	}
	if flagsRefuse(before, off, appendable) {
		return vfs.WCC{}, vfs.ErrPerm
	}
	if err := do(); err != nil {
		return vfs.WCC{}, storeErr(err)
	}
	return fs.wcc(ctx, c, a, h, before), nil
}

// flagsRefuse reports whether f's flags refuse a change of its content at off,
// which may append only when appendable: it is immutable, or append-only and
// the change below its size or not appendable.
func flagsRefuse(f vfs.File, off int64, appendable bool) bool {
	return f.IsImmutable() || f.IsAppendOnly() && (!appendable || off < f.Size)
}

// clearSetID clears setuid and setgid on f, which h names, for an
// unprivileged caller, the owner included, changing its content, in a change
// committed before the content's change is staged; it reports whether it
// cleared either. appends says the change starts at or past the size, which
// append data admits as well as write data.
//
// decision: the clear is decided against the file as the caller is now, not
// against the open the change came through, which the store cannot be
// handed: a write through an open whose caller has since lost the right it
// wants, write data, or append data for an append, on a setuid file is
// refused. Overturned once ClearSetID takes an open's grant.
func (fs *Filesystem) clearSetID(ctx context.Context, a *admitted, h vfs.Handle, f vfs.File, appends bool) (bool, error) {
	if a.caller.Privileged || f.ClearSetID() == f.Mode {
		return false, nil
	}
	if _, err := fs.meta.Files().ClearSetID(ctx, a.caller.Identity, h, appends); err != nil {
		return false, storeErr(err)
	}
	return true, nil
}
