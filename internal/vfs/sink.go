package vfs

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// The filesystem is open state's Sink: the table raises each of these after
// the step that raised it has released every table lock, on a goroutine that
// may still hold the file's admission lock. So the filesystem hands each to a
// path the client registered on a goroutine of its own and returns: the
// table's goroutine never waits on a client, and a method that took that lock
// itself, or through a table call that serves waiters, would wait on its own
// caller. An event with no path to go by is dropped: a recall then ends at its
// deadline, as one the client never answered.
//
// A recall of a lease or an oplock, an SMB break, goes by the opens it
// covers, never by the client: to the session holding one of them, else to a
// session of the client signed in as one of their openers, else nowhere. A
// ClientGuid travels in clear, so anyone can connect under a machine's; a
// path kept per SMB client is one a stranger could move.
//
// decision: every other callback, a delegation's recall, a revocation, a lock
// wait's end, goes by the path the client's latest Connect registered, which
// for an SMB client is its latest session's, so a stranger connected under a
// machine's ClientGuid may receive them. They carry no cached data: a
// revocation tells of state ended already, and a lock wait's end tells only
// that a range was granted, so the stranger can hold up the machine's lock
// wait, not read its data. Overturned by routing a lock wait's end by its
// open, as a break goes.
//
// ponytail: one goroutine per callback, so two raised for one client may
// reach it out of order. Queue each client's callbacks in order if an
// adapter is found acting on a revocation before the recall it ends.

// callbackDeadline bounds each callback: the recall deadline, past which a
// client's answer changes nothing.
const callbackDeadline = 35 * time.Second

// grantRetry is how long a lock grant the client did not answer waits before
// it is sent again.
//
// ponytail: a fixed pause, retried until the client answers, its path ends,
// or the filesystem stops, one goroutine per grant
// meanwhile. Back off, and bound the goroutines, if hosts that stay down
// are seen holding grants.
var grantRetry = 10 * time.Second

// ClientEnded tells the client its state ended, then drops its path.
//
// ponytail: on one node the table has already released the client's state
// everywhere, so the client is only told; with peers, end it at each primary
// of shards through Peers.ClientState first.
func (fs *Filesystem) ClientEnded(c entity.ClientID, shards []openstate.ShardID, why openstate.RevokeReason) {
	fs.deliver(fs.latest(c), func(ctx context.Context, cb vfs.Callbacks) {
		cb.Revoked(ctx, vfs.Revocation{Client: c, Reason: why})
	})
	fs.reg.unregister(c)
}

// Closed runs nothing yet.
//
// ponytail: until release is built, a close open state made itself is not
// run: an orphan's content stays held. Run it as Close's release is run.
func (fs *Filesystem) Closed(f entity.FileID, r openstate.CloseResult) {}

// LockWaitOver tells the waiting owner's client, and hands its answer to a
// grant back to open state, under the file's admission lock. A grant the
// client gives no answer to is sent again, through the client's latest path,
// as the lock stays granted until it answers.
func (fs *Filesystem) LockWaitOver(w openstate.LockWaiter, granted bool) {
	c := w.Owner.Client
	fs.deliver(fs.latest(c), func(ctx context.Context, cb vfs.Callbacks) {
		refused, err := cb.LockWaitOver(ctx, w, granted)
		for granted && err != nil {
			select {
			case <-fs.stop.Done():
				return
			case <-time.After(grantRetry):
			}
			if cb = fs.reg.latest(c); cb == nil {
				return
			}
			refused, err = grantAgain(fs.stop, cb, w)
		}
		if granted {
			unlock := fs.Admission(w.File)
			_ = fs.lockState.WaitAnswered(fs.stop, openstate.Caller{Client: c}, w, refused)
			unlock()
		}
	})
}

// grantAgain sends w's grant once more, bounded as every callback is.
func grantAgain(stop context.Context, cb vfs.Callbacks, w openstate.LockWaiter) (bool, error) {
	ctx, cancel := context.WithTimeout(stop, callbackDeadline)
	defer cancel()
	return cb.LockWaitOver(ctx, w, true)
}

func (fs *Filesystem) RecallGrant(g openstate.CachingGrant) {
	to := openstate.GrantNone
	if g.Recall != nil {
		to = g.Recall.To
	}
	fs.deliver(func(ctx context.Context) vfs.Callbacks {
		opens, err := fs.grants.Openers(ctx, openstate.Caller{Client: g.Client}, g.ID)
		switch {
		case err != nil:
			return nil // gone since: nothing is left to recall
		case len(opens) == 0:
			return fs.reg.latest(g.Client) // a delegation, the client's own
		}
		return fs.reg.breakPath(g.Client, opens)
	}, func(ctx context.Context, cb vfs.Callbacks) { _ = cb.RecallGrant(ctx, g, to) })
}

func (fs *Filesystem) RecallAny(c entity.ClientID, keep int) {
	fs.deliver(fs.latest(c), func(ctx context.Context, cb vfs.Callbacks) { _ = cb.RecallAny(ctx, keep) })
}

func (fs *Filesystem) Revoked(r openstate.Revocation) {
	fs.deliver(fs.latest(r.Client), func(ctx context.Context, cb vfs.Callbacks) { cb.Revoked(ctx, r) })
}

// Notify delivers nothing yet.
//
// decision: a watch's changes are not delivered until Watch is built, since
// delivery must first drop a watch no longer held and check that the watch's
// holder may traverse to each change, and nothing records a watch to check
// against. Nothing raises one meanwhile, as no watch can be set. Deliver it
// with Watch.
func (fs *Filesystem) Notify(c entity.ClientID, w openstate.WatchID, ch []openstate.Change) {}

// GraceEnded runs nothing yet.
//
// ponytail: until release is built, a shard's pending releases are not run
// when its grace ends. Run them here with release.
func (fs *Filesystem) GraceEnded(share entity.ShareID, sh openstate.ShardID) {}

// deliver runs send on a goroutine of its own with the callbacks path finds
// there, under a context ending at callbackDeadline or at Stop. With no path
// found, or once the filesystem is stopped, it sends nothing.
func (fs *Filesystem) deliver(path func(context.Context) vfs.Callbacks, send func(ctx context.Context, cb vfs.Callbacks)) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.state == stopped {
		return
	}
	fs.sending.Go(func() {
		ctx, cancel := context.WithTimeout(fs.stop, callbackDeadline)
		defer cancel()
		if cb := path(ctx); cb != nil {
			send(ctx, cb)
		}
	})
}

// latest is a path for deliver: client c's latest path as it is now.
func (fs *Filesystem) latest(c entity.ClientID) func(context.Context) vfs.Callbacks {
	cb := fs.reg.latest(c)
	return func(context.Context) vfs.Callbacks { return cb }
}

// registry is each client's callback paths at this node: the one its latest
// Connect registered, with the session it came on, and an SMB client's per
// session, with the principal each session signed in as.
type registry struct {
	mu   sync.Mutex
	byID map[entity.ClientID]latestPath
	smb  map[entity.ClientID]map[entity.SessionRef]session
}

// latestPath is the path a client's latest Connect registered, and its
// session; zero for a client that names none.
type latestPath struct {
	sess entity.SessionRef
	cb   vfs.Callbacks
}

// session is an SMB session's path.
type session struct {
	principal entity.Principal
	cb        vfs.Callbacks
}

func newRegistry() *registry {
	return &registry{byID: map[entity.ClientID]latestPath{}, smb: map[entity.ClientID]map[entity.SessionRef]session{}}
}

// register makes cb client id's latest path and, for a non-zero sess, that
// session's, signed in as p. A reboot's ended ID has already gone, through
// ClientEnded.
func (r *registry) register(id entity.ClientID, sess entity.SessionRef, p entity.Principal, cb vfs.Callbacks) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[id] = latestPath{sess, cb}
	if sess == (entity.SessionRef{}) {
		return
	}
	if r.smb[id] == nil {
		r.smb[id] = map[entity.SessionRef]session{}
	}
	r.smb[id][sess] = session{p, cb}
}

// dropSession drops session sess's path of client id, and the client's
// latest path when that session registered it, so nothing goes by a
// session that has ended.
func (r *registry) dropSession(id entity.ClientID, sess entity.SessionRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.smb[id], sess)
	if l, ok := r.byID[id]; ok && l.sess == sess {
		delete(r.byID, id)
	}
}

// unregister drops every path of client id.
func (r *registry) unregister(id entity.ClientID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	delete(r.smb, id)
}

// latest is client id's latest path, or nil.
func (r *registry) latest(id entity.ClientID) vfs.Callbacks {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id].cb
}

// breakPath is the path a break of a grant covering opens, of client id,
// goes by: the session holding one of them, else a session of the client
// signed in as one of their openers, else nil.
func (r *registry) breakPath(id entity.ClientID, opens []openstate.Open) vfs.Callbacks {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessions := r.smb[id]
	for _, o := range opens {
		if s, ok := sessions[o.Session]; ok {
			return s.cb
		}
	}
	for _, o := range opens {
		for _, s := range sessions {
			if s.principal == o.Principal {
				return s.cb
			}
		}
	}
	return nil
}
