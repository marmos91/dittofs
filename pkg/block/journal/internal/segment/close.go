package segment

import (
	"errors"
	"sync"
)

// fullToHand reports whether a full-size sealed segment is queued for a
// retirement the worker will run: the retirer is started and the queue holds a
// segment that recycles (Stop empties the queue, so a stopped retirer holds
// none). A retirer that is not started counts nothing to hand, because its
// queue never drains. It takes r.mu and
// nothing else, and is called with no lock of the pool held.
func (r *Retirer) fullToHand() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started && len(r.full) > 0
}

// retiredToHand asks the attached retirer whether a full-size retired segment
// is to hand, outside p.mu; false when no retirer is attached.
func (p *Pool) retiredToHand() bool {
	p.mu.Lock()
	r := p.retirer
	p.mu.Unlock()
	if r == nil {
		return false
	}
	return r.fullToHand()
}

// attachRetirer records r as the pool's source of recycled spares; NewRetirer
// calls it once. A second, different retirer is a caller bug and panics.
func (p *Pool) attachRetirer(r *Retirer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retirer != nil && p.retirer != r {
		panic("segment: a pool takes recycled spares from one retirer")
	}
	p.retirer = r
}

// GroupConfig names the parts of one journal's segment set that Close closes.
// Streams, Pool and Cache are required (NewGroup panics on nil); Idle and
// Retirer may be nil.
type GroupConfig struct {
	Streams *Streams
	Idle    *IdleSealer
	Pool    *Pool
	Retirer *Retirer
	Cache   *Descriptors
}

// Group is the segment set a journal runs: its streams, its background
// workers and its descriptor cache, closed together in the order the journal's
// close requires.
//
// decision: the close lives on this composition rather than on the ledger Set,
// because the Set knows none of these parts. Withdraw it if the Set ever owns
// the streams and the cache it would have to close.
type Group struct {
	cfg  GroupConfig
	once sync.Once
	err  error
}

// NewGroup returns the group over c. It panics when Streams, Pool or Cache is
// nil: a close that skips one would leave its workers or descriptors running.
func NewGroup(c GroupConfig) *Group {
	if c.Streams == nil || c.Pool == nil || c.Cache == nil {
		panic("segment: a group needs streams, a pool and a cache")
	}
	return &Group{cfg: c}
}

// Close closes the set in the order that keeps every part valid while another
// still uses it: the streams first, so new appends and reads fail with the
// closed error while in-flight appends, rolls, syncs and seals finish; then
// the idle sealer, the retirer and the pool, so every background worker is
// joined before the segments it touches are closed; last the cache, whose
// Shutdown waits for every reader's reference and then closes every
// descriptor. The pool is told to make nothing before the retirer stops, whose
// drop of its queue wakes it, and the retirer is joined before the pool, so no
// recycle lands in a stopped pool mid-close. Errors are joined. Close is idempotent and safe
// concurrently: a second call waits for the first and returns nil.
func (g *Group) Close() error {
	first := false
	g.once.Do(func() {
		first = true
		var errs []error
		errs = append(errs, g.cfg.Streams.Close())
		if g.cfg.Idle != nil {
			g.cfg.Idle.Stop()
		}
		g.cfg.Pool.Closing()
		if g.cfg.Retirer != nil {
			g.cfg.Retirer.Stop()
		}
		g.cfg.Pool.Stop()
		errs = append(errs, g.cfg.Cache.Shutdown())
		g.err = errors.Join(errs...)
	})
	if first {
		return g.err
	}
	return nil
}
