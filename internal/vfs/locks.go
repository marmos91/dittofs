package vfs

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The Locking area's byte-range locks. Each is admitted to the share its
// handle names and runs under the file's admission lock, the one every read
// and write holds from its conflict check to its I/O, so a lock is ordered
// wholly before or after each of them. Open state decides the rest: the
// owner's client and sequence, the open's opener, grace, and the conflict
// with every protocol's locks. A lock that waits holds nothing here: it is
// answered ErrBlocked at once, and its end reaches the client through
// LockWaitOver.
//
// decision: a lock under no open, NLM's, is decided as the caller's own
// right to the I/O it guards: read data for a shared range or a test, write
// data for an exclusive one, as a lock daemon opens the file to lock it.
// Through an open or a lock state, open state's opener check is the whole
// check, as the open was authorised for its access when granted; an unlock
// or a cancel decides no right, as it only gives up what the owner holds.
// Overturned if a client is seen locking through an open a range of access
// the open was not granted.

// Lock takes every range of r, or none, for r's owner on the file ref
// names, and answers the lock state's reference. A conflict is ErrLocked
// naming the holder; a single range asked to wait by callback is recorded
// and answered ErrBlocked.
func (fs *Filesystem) Lock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, r vfs.LockReq) (vfs.StateRef, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.StateRef{}, err
	}
	defer done()
	exclusive := false
	for _, x := range r.Ranges {
		exclusive = exclusive || x.Exclusive
	}
	a, key, unlock, release, err := fs.lockable(ctx, c, ref, vfs.OpLock, exclusive)
	if err != nil {
		return vfs.StateRef{}, err
	}
	defer release()
	defer unlock()
	res, err := fs.lockState.Lock(ctx, a.who(), key, openstate.LockReq{
		Owner: r.Owner, Open: ref, Ranges: r.Ranges, Wait: r.Wait, Seqs: r.Seqs, Reclaim: r.Reclaim, Replay: r.Replay,
	})
	if err == nil && res.Waiting {
		return vfs.StateRef{}, vfs.ErrBlocked
	}
	return res.Ref, openErr(err)
}

// CancelLock drops o's waiter for r on the file ref names, or reports that
// its lock was granted first: an adapter answers a waiting request other
// than as granted only once this has returned.
func (fs *Filesystem) CancelLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange) (bool, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	a, key, unlock, release, err := fs.lockable(ctx, c, ref, vfs.OpCancelLock, false)
	if err != nil {
		return false, err
	}
	defer release()
	defer unlock()
	granted, err := fs.lockState.CancelLock(ctx, a.who(), key, o, r)
	return granted, openErr(err)
}

// TestLock is the lock that would refuse o's lock of r on the file ref
// names, or nil; it changes nothing.
func (fs *Filesystem) TestLock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, r vfs.ByteRange, exclusive bool) (*vfs.Lock, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	a, key, unlock, release, err := fs.lockable(ctx, c, ref, vfs.OpTestLock, false)
	if err != nil {
		return nil, err
	}
	defer release()
	defer unlock()
	l, err := fs.lockState.TestLock(ctx, a.who(), key, o, r, exclusive)
	return l, openErr(err)
}

// Unlock releases o's rs in order on the file ref names, and answers the
// lock state's new sequence. The waiters it frees are served under the
// admission lock it holds.
func (fs *Filesystem) Unlock(ctx context.Context, c vfs.Call, ref vfs.OpenRef, o vfs.LockOwner, rs []vfs.ByteRange, q vfs.Seqs, replay *vfs.LockSequence) (uint32, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	a, key, unlock, release, err := fs.lockable(ctx, c, ref, vfs.OpUnlock, false)
	if err != nil {
		return 0, err
	}
	defer release()
	defer unlock()
	seq, err := fs.lockState.Unlock(ctx, a.who(), key, ref, o, rs, q, replay)
	return seq, openErr(err)
}

// lockable admits a lock call for op through ref, takes the file's
// admission lock and reads the file, answering it as open state keys it. A
// directory holds no byte-range locks: ErrIsDir.
// Under no open, a lock or a test is decided as read data, or write data
// when exclusive; a cancel or an unlock decides nothing.
func (fs *Filesystem) lockable(ctx context.Context, c vfs.Call, ref vfs.OpenRef, op vfs.Op, exclusive bool) (*admitted, openstate.FileKey, func(), func(), error) {
	a, unlock, release, err := fs.opened(ctx, c, ref)
	if err != nil {
		return nil, openstate.FileKey{}, nil, nil, err
	}
	fail := func(err error) (*admitted, openstate.FileKey, func(), func(), error) {
		unlock()
		release()
		return nil, openstate.FileKey{}, nil, nil, err
	}
	f, err := fs.meta.Files().Get(ctx, a.caller.Identity, ref.Handle)
	if err != nil {
		return fail(storeErr(err))
	}
	if f.IsDir() {
		return fail(vfs.ErrIsDir)
	}
	if ref.Kind == openstate.RefAnonymous && (op == vfs.OpLock || op == vfs.OpTestLock) {
		want := entity.ReadData
		if exclusive {
			want = entity.WriteData
		}
		if err := fs.authz.Decide(ctx, a.caller, op, ref.Handle, nil, nil, want); err != nil {
			return fail(storeErr(err))
		}
	}
	return a, keyOf(f), unlock, release, nil
}
