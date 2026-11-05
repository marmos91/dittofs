package segment

// fullToHand reports whether a full-size sealed segment is queued for a
// retirement the worker will run: the retirer is started and not stopped, and
// the queue holds a segment that recycles. A retirer that is not started
// counts nothing to hand, because its queue never drains. It takes r.mu and
// nothing else, and is called with no lock of the pool held.
func (r *Retirer) fullToHand() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started && !r.stoppedLocked() && len(r.full) > 0
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
