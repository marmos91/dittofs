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
// may still hold the file's admission lock. So the filesystem hands each to
// the Callbacks the client registered, on a goroutine of its own, and returns: the
// table's goroutine never waits on a client, and a method that took that lock
// itself, or through a table call that serves waiters, would wait on its own
// caller. An event for a client with no Callbacks here is dropped: a recall
// then ends at its deadline, as one the client never answered. A callback
// that fails, or finds no Callbacks or session to go to, is counted in
// vfs_callback_errors_total by its kind.
//
// A recall of a lease or an oplock, an SMB break, names the session it goes
// to, picked by the opens it covers, never by the client: the session holding
// one of them, else a session of the client signed in as one of their
// openers, else none, and the break waits for one to sign in until its
// deadline. A ClientGuid travels in clear, so anyone can connect under a
// machine's and register its Callbacks; the session is the filesystem's to
// pick, and the adapter only delivers on it.
//
// decision: every other callback, a delegation's recall, a revocation, a lock
// wait's end, a recall-any, names no session: the adapter delivers it by the
// state or waiter it names. So a stranger whose Connect under a machine's
// ClientGuid registered the Callbacks may receive them. They carry no cached
// data: a revocation tells of state ended already, and a lock wait's end
// tells only that a range was granted, so the stranger can hold up the
// machine's lock wait, not read its data. Overturned if an SMB lock
// completion is seen held up this way: then a lock wait's end names its
// waiter's session as a break does.
//
// ponytail: one goroutine per callback, so two raised for one client may
// reach it out of order. Queue each client's callbacks in order if an
// adapter is found acting on a revocation before the recall it ends.

// callbackDeadline bounds each callback: the lease or oplock break deadline,
// past which a client's answer changes nothing.
const callbackDeadline = 35 * time.Second

// grantRetry is how long a lock grant the client did not answer waits before
// it is sent again, and how often a recall not yet sent, or whose send
// failed, looks for a new path to send it through.
//
// ponytail: a fixed pause, a grant retried until the client answers or ends
// or the filesystem stops, one goroutine per grant meanwhile; an SMB client
// with no session holds its goroutine, unsent, until one signs in or the
// client ends. Back off, and bound the goroutines, if hosts that stay down
// are seen holding grants.
var grantRetry = 10 * time.Second

// ClientEnded tells the client its state ended, then drops its path.
//
// ponytail: on one node the table has already released the client's state
// everywhere, so the client is only told; with peers, end it at each primary
// of shards through Peers.ClientState first.
func (fs *Filesystem) ClientEnded(c entity.ClientID, shards []openstate.ShardID, why openstate.RevokeReason) {
	fs.deliver(fs.latest("Revoked", c), func(ctx context.Context, cb vfs.Callbacks) {
		cb.Revoked(ctx, vfs.Revocation{Client: c, Reason: why})
	})
	fs.reg.unregister(c)
}

// LockWaitOver tells the waiting owner's client, and hands its answer to a
// grant back to open state, under the file's admission lock. A grant the
// client gives no answer to is sent again, through the client's Callbacks
// as they are then, as the lock stays granted until it answers or the client
// ends; to an SMB client, only once it has a session to receive it.
func (fs *Filesystem) LockWaitOver(w openstate.LockWaiter, granted bool) {
	c := w.Owner.Client
	fs.deliver(fs.latest("LockWaitOver", c), func(ctx context.Context, cb vfs.Callbacks) {
		refused, err := cb.LockWaitOver(ctx, w, granted)
		for granted && err != nil {
			fs.callbackErrors.WithLabelValues("LockWaitOver").Inc()
			for {
				select {
				case <-fs.stop.Done():
					return
				case <-time.After(grantRetry):
				}
				if !fs.reg.sessionless(c) {
					break
				}
			}
			if cb = fs.counted("LockWaitOver", fs.reg.latest(c)); cb == nil {
				return
			}
			refused, err = grantAgain(fs.stop, cb, w)
		}
		if err != nil {
			fs.callbackErrors.WithLabelValues("LockWaitOver").Inc()
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

// RecallGrant sends the recall through the client's Callbacks, to the
// session its break goes to. One with nowhere to go, or whose send fails,
// looks again every grantRetry, and is sent once the client registers other
// Callbacks or the break finds another session, until the callback's
// deadline or the grant is gone: a client that reconnects, or signs in
// again, is recalled there. A recall with nowhere to go at first is counted
// once.
//
// decision: only the grant's end or the deadline stops it, not an answer to
// the recall: nothing here tells whether one is still in flight. An answer
// comes from an opener's session, one the break went to already, so it is
// sent again only after a send that failed yet reached the client. Have
// Openers report the recall in flight if an adapter is seen misreading such
// a break.
func (fs *Filesystem) RecallGrant(g openstate.CachingGrant) {
	to := openstate.GrantNone
	if g.Recall != nil {
		to = g.Recall.To
	}
	fs.background(func(stop context.Context) {
		ctx, cancel := context.WithTimeout(stop, callbackDeadline)
		defer cancel()
		var tried recallRoute // the route last sent through; zero: none yet
		for first := true; ; first = false {
			opens, err := fs.grants.Openers(ctx, g.ID)
			if err != nil {
				return // gone since: nothing is left to recall
			}
			r := fs.reg.recallRoute(g.Client, opens)
			if first {
				fs.counted("RecallGrant", r.cb)
			}
			if r.cb != nil && !r.same(tried) {
				if r.cb.RecallGrant(ctx, r.sess, g, to) == nil {
					return
				}
				fs.callbackErrors.WithLabelValues("RecallGrant").Inc()
				tried = r
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(grantRetry):
			}
		}
	})
}

// recallRoute is where a recall goes: the client's Callbacks, the generation
// that registered them, and the session to deliver on, zero for a
// delegation.
type recallRoute struct {
	cb   vfs.Callbacks
	gen  uint64
	sess entity.SessionRef
}

// same reports whether r and o go the same way: through Callbacks one
// registration made, to one session. Callbacks need not be comparable.
func (r recallRoute) same(o recallRoute) bool { return r.gen == o.gen && r.sess == o.sess }

func (fs *Filesystem) RecallAny(c entity.ClientID, keep int) {
	fs.deliver(fs.latest("RecallAny", c), func(ctx context.Context, cb vfs.Callbacks) {
		if cb.RecallAny(ctx, keep) != nil {
			fs.callbackErrors.WithLabelValues("RecallAny").Inc()
		}
	})
}

func (fs *Filesystem) Revoked(r openstate.Revocation) {
	fs.deliver(fs.latest("Revoked", r.Client), func(ctx context.Context, cb vfs.Callbacks) { cb.Revoked(ctx, r) })
}

// Notify delivers nothing yet.
//
// decision: a watch's changes are not delivered until Watch is built, since
// delivery must first drop a watch no longer held and check that the watch's
// holder may traverse to each change, and nothing records a watch to check
// against. Nothing raises one meanwhile, as no watch can be set. Deliver it
// with Watch.
func (fs *Filesystem) Notify(c entity.ClientID, w openstate.WatchID, ch []openstate.Change) {}

// deliver runs send in the background with the Callbacks path finds there,
// under a context ending at callbackDeadline or at Stop. With none found, or
// once the filesystem is stopped, it sends nothing.
func (fs *Filesystem) deliver(path func(context.Context) vfs.Callbacks, send func(ctx context.Context, cb vfs.Callbacks)) {
	fs.background(func(stop context.Context) {
		ctx, cancel := context.WithTimeout(stop, callbackDeadline)
		defer cancel()
		if cb := path(ctx); cb != nil {
			send(ctx, cb)
		}
	})
}

// background runs run on a goroutine of its own, which Stop ends and waits
// for, under a context ending at Stop; once the filesystem is stopped it runs
// nothing, answering false.
func (fs *Filesystem) background(run func(ctx context.Context)) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.state == stopped {
		return false
	}
	fs.sending.Go(func() { run(fs.stop) })
	return true
}

// latest is a path for deliver: client c's Callbacks as they are now, a
// callback of kind dropped for want of them counted.
func (fs *Filesystem) latest(kind string, c entity.ClientID) func(context.Context) vfs.Callbacks {
	cb := fs.counted(kind, fs.reg.latest(c))
	return func(context.Context) vfs.Callbacks { return cb }
}

// counted is cb, counting a callback of kind dropped when it is nil.
func (fs *Filesystem) counted(kind string, cb vfs.Callbacks) vfs.Callbacks {
	if cb == nil {
		fs.callbackErrors.WithLabelValues(kind).Inc()
	}
	return cb
}

// registry is, at this node, each client's Callbacks, which its latest
// Connect registered; each SMB client's sessions, with the principal each
// signed in as; and the client each NLM host last connected as.
//
// ponytail: an NLM host's entry stays once its client has ended, one per host
// ever connected. Drop it with the client's Callbacks if host churn shows it.
type registry struct {
	mu  sync.Mutex
	gen uint64 // raised by each register, naming the Callbacks it registered
	cb  map[entity.ClientID]registered
	smb map[entity.ClientID]map[entity.SessionRef]entity.Principal // kept, empty, once an SMB client's last session ends
	nlm map[string]entity.ClientID
}

// registered is a client's Callbacks and the register that made them.
type registered struct {
	cb  vfs.Callbacks
	gen uint64
}

func newRegistry() *registry {
	return &registry{cb: map[entity.ClientID]registered{}, smb: map[entity.ClientID]map[entity.SessionRef]entity.Principal{}, nlm: map[string]entity.ClientID{}}
}

// register makes cb client id's Callbacks and, for a non-zero sess, records
// that session, signed in as p. A reboot's ended ID has already gone, through
// ClientEnded. For a non-empty NLM host, it drops the Callbacks of the client
// the host connected as before when that is another: open state deletes an
// idle host's record without raising ClientEnded, and the host's next Connect
// makes a new client. It registers nothing for a host already connected as a
// newer client: IDs only grow, so id is one open state has deleted, and a
// Connect that got it registering late must not displace the live one.
func (r *registry) register(id entity.ClientID, host string, sess entity.SessionRef, p entity.Principal, cb vfs.Callbacks) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.nlm[host]; ok && old != id {
		if old > id {
			return
		}
		delete(r.cb, old)
	}
	if host != "" {
		r.nlm[host] = id
	}
	r.gen++
	r.cb[id] = registered{cb, r.gen}
	if sess == (entity.SessionRef{}) {
		return
	}
	if r.smb[id] == nil {
		r.smb[id] = map[entity.SessionRef]entity.Principal{}
	}
	r.smb[id][sess] = p
}

// dropSession forgets session sess of client id, so no break goes to it. The
// client's Callbacks stay until the client ends: its other sessions, and its
// durable opens, outlive this one.
func (r *registry) dropSession(id entity.ClientID, sess entity.SessionRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.smb[id], sess)
}

// sessionless reports whether client id is an SMB client with no session
// left to receive a callback.
func (r *registry) sessionless(id entity.ClientID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.smb[id]
	return ok && len(s) == 0
}

// unregister drops client id's Callbacks and sessions.
func (r *registry) unregister(id entity.ClientID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cb, id)
	delete(r.smb, id)
}

// latest is client id's Callbacks, or nil.
func (r *registry) latest(id entity.ClientID) vfs.Callbacks {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cb[id].cb
}

// recallRoute is where a recall of a grant covering opens, of client id,
// goes. With no opens, a delegation, it goes to the client naming no
// session. Otherwise, a break, it goes to the session holding one of them,
// else to a session of the client signed in as one of their openers, else
// nowhere: the zero route.
func (r *registry) recallRoute(id entity.ClientID, opens []openstate.Open) recallRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg := r.cb[id]
	if len(opens) == 0 || reg.cb == nil {
		return recallRoute{cb: reg.cb, gen: reg.gen}
	}
	sessions := r.smb[id]
	for _, o := range opens {
		if _, ok := sessions[o.Session]; ok {
			return recallRoute{reg.cb, reg.gen, o.Session}
		}
	}
	// ponytail: an open whose session is live on another node is not
	// followed there; the break goes to a session here signed in as an
	// opener, or nowhere. Route it through Peers.Recall to the node its
	// SessionRef names once the filesystem runs in a cluster.
	for _, o := range opens {
		for s, p := range sessions {
			if p == o.Principal {
				return recallRoute{reg.cb, reg.gen, s}
			}
		}
	}
	return recallRoute{}
}
