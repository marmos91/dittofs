package durability

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// run is one stream's worker: on each wake it completes the entries a sync
// covered and stamps the ones it has not seen, until Stop closes done.
func (d *Durability) run(st *stream) {
	defer d.wg.Done()
	for {
		select {
		case <-d.done:
			return
		case <-st.kick:
		}
		d.complete(st)
		d.stamp(st)
	}
}

// complete drops every covered entry from the ledger (R87): each is marked
// done under its file's append domain, one domain at a time, so a record whose
// sync landed is never dropped in the gap between a writer's append and the
// accounting it does under that same domain. Only the worker compacts and
// Track only appends, so the indices collected stay valid between the steps.
func (d *Durability) complete(st *stream) {
	st.mu.Lock()
	st.scratch = st.scratch[:0]
	for i := range st.ledger {
		if e := &st.ledger[i]; !e.done && st.covered(e.w.Loc) {
			st.scratch = append(st.scratch, i)
		}
	}
	idx := st.scratch
	st.mu.Unlock()
	if len(idx) == 0 {
		return
	}

	for _, i := range idx {
		st.mu.Lock()
		f := st.ledger[i].w.File
		st.mu.Unlock()
		unlock := d.domains.Lock(f)
		st.mu.Lock()
		st.ledger[i].done = true
		st.mu.Unlock()
		unlock()
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.ledger[:0]
	live := map[core.SegmentID]struct{}{}
	for _, e := range st.ledger {
		if !e.done {
			kept = append(kept, e)
			live[e.w.Loc.Segment] = struct{}{}
		}
	}
	clear(st.ledger[len(kept):]) // drop the references the tail still holds
	st.ledger = kept
	for seg := range st.synced {
		if _, ok := live[seg]; !ok && (!st.haveLast || seg != st.lastSeg) {
			delete(st.synced, seg)
		}
	}
	for seg := range st.failed {
		if _, ok := live[seg]; !ok && (!st.haveLast || seg != st.lastSeg) {
			delete(st.failed, seg)
		}
	}
}

// stamp records when the worker first saw each entry. The clock is read
// outside mu, and only entries that existed before the read are stamped, so a
// stamp is never earlier than the record's arrival.
func (d *Durability) stamp(st *stream) {
	st.mu.Lock()
	n := len(st.ledger)
	need := false
	for i := range n {
		if !st.ledger[i].seenSet {
			need = true
			break
		}
	}
	st.mu.Unlock()
	if !need {
		return
	}
	now := d.clock.Elapsed()
	st.mu.Lock()
	for i := range n {
		if e := &st.ledger[i]; !e.seenSet {
			e.seen, e.seenSet = now, true
		}
	}
	st.mu.Unlock()
}
