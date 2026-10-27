package model

import (
	"cmp"
	"context"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// refuseOther is the journal's answer to a call through tag on id when
// another share has the file: refused while it holds anything, otherwise
// accepted.
func refuseOther(j *Model, id journalv2.FileID, tag journalv2.ShareTag) error {
	if f := j.files[id]; f != nil && f.tag != tag && f.holdsAnything() {
		return journalv2.ErrOtherShare
	}
	return nil
}

// offerAll is the offer a journal played by model j makes for file id through
// through: every dirty run at or below it, with the bytes it holds.
func offerAll(j *Model, id journalv2.FileID, through journalv2.Version) journalv2.Offer {
	st := j.current(id)
	o := journalv2.Offer{ID: id}
	bytes := offered{}
	for _, r := range st.runs() {
		if r.offloaded || r.ver.Compare(through) > 0 {
			continue
		}
		if n := len(o.Dirty); n > 0 && o.Dirty[n-1].Version == r.ver && end(o.Dirty[n-1].Extent) == r.ext.Off {
			o.Dirty[n-1].Extent.Len += r.ext.Len
		} else {
			o.Dirty = append(o.Dirty, journalv2.OfferedExtent{Extent: r.ext, Version: r.ver})
		}
		if len(o.Dirty) == 1 || r.ver.Compare(o.Oldest) < 0 {
			o.Oldest = r.ver
		}
		o.Newest = maxVersion(o.Newest, r.ver)
		for off := r.ext.Off; off < end(r.ext); off++ {
			bytes[off] = st.held[off].b
		}
	}
	o.Offered = bytes
	return o
}

// loseUnsynced makes the state of a journal played by model j, just crashed
// and reopened, a random one of those the crash allows: each uncertain write
// or fill split at a random point, and each piece and uncertain record kept or
// lost.
func loseUnsynced(j *Model, rng *rand.Rand) {
	for _, id := range sortedIDs(j) {
		f := j.files[id]
		if !f.unresolved {
			continue
		}
		var recs []*record
		for _, r := range f.recs {
			if r.uncertain && len(r.data) > 1 && rng.IntN(2) == 0 {
				k := 1 + rng.IntN(len(r.data)-1)
				recs = append(recs, r.piece(0, k), r.piece(k, len(r.data)))
				continue
			}
			recs = append(recs, r)
		}
		f.recs = recs
		before := slices.Clone(recs) // adopt filters f.recs, and so recs, in place
		durable := map[*record]bool{}
		for _, r := range recs {
			durable[r] = !r.uncertain // adopt makes the pieces it keeps durable
		}
		candidate{items: f.items(), mask: rng.Uint32()}.adopt(f)
		keepPrefixes(f, before, durable)
	}
}

// keepPrefixes makes the journal's state one a crash can leave: a write comes
// back as a prefix of itself, since a later piece's segment is taken only once
// the earlier piece's was sealed and synced. Of the pieces in before, the
// state f was resolved from, every piece after one that was lost goes too,
// except one durable before the crash.
func keepPrefixes(f *file, before []*record, durable map[*record]bool) {
	have := map[*record]bool{}
	for _, r := range f.recs {
		have[r] = true
	}
	pieces := slices.Clone(before)
	slices.SortFunc(pieces, func(a, b *record) int { return cmp.Compare(a.ext.Off, b.ext.Off) })
	cut := map[uint64]bool{}
	for _, r := range pieces {
		if r.data == nil {
			continue
		}
		switch {
		case durable[r]:
			// Durable before the crash, and so is every piece before it.
		case cut[r.seq]:
			delete(have, r)
		case !have[r]:
			cut[r.seq] = true
		}
	}
	f.recs = slices.DeleteFunc(f.recs, func(r *record) bool { return !have[r] })
	f.countLive()
}

// played is a Target played by a second model: it answers every call as the
// RFC allows, and after a crash keeps a random state of those the crash
// allows. It is the honest journal the runner's own tests drive.
//
// It keeps a file's tag while its records may be on disk, as a journal does:
// a call through another share is refused until a repack has run since the
// file last took a record and no longer holds anything, and accepted after.
type played struct {
	t     testing.TB
	j     *Model
	floor journalv2.Version

	// dropped holds the files a repack ran over after they held nothing:
	// their records are gone, and another share may claim them.
	dropped map[journalv2.FileID]bool
}

func newPlayed(t testing.TB) *played {
	return &played{t: t, j: New(), dropped: map[journalv2.FileID]bool{}}
}

// refuse is the journal's answer to a call through tag on id when another
// share has the file. A call it does not refuse may append a record, so the
// file is no longer one a repack dropped.
func (p *played) refuse(id journalv2.FileID, tag journalv2.ShareTag) error {
	f := p.j.files[id]
	if f != nil && f.tag != tag && (f.holdsAnything() || !p.dropped[id]) {
		return journalv2.ErrOtherShare
	}
	delete(p.dropped, id)
	return nil
}

// Repack drops the records of every file that holds nothing.
func (p *played) Repack(context.Context) error {
	for id, f := range p.j.files {
		if !f.holdsAnything() {
			p.dropped[id] = true
		}
	}
	return nil
}

func (p *played) Share(i int) Handle                  { return playedHandle{p, tagOf(i)} }
func (p *played) View(i int) View                     { return modelView{p.j, tagOf(i)} }
func (p *played) Floor() journalv2.Version            { return p.floor }
func (p *played) Reopen() error                       { return p.restart(0) }
func (p *played) Crash(_ CrashKind, arg uint64) error { return p.restart(arg) }

// restart crashes the journal and reopens it with a floor above every version
// issued, keeping a state the crash allows that seed chooses.
func (p *played) restart(seed uint64) error {
	p.must(p.j.Crash())
	p.floor = journalv2.Version{Counter: p.j.issued.Counter + 1}
	p.must(p.j.Reopen(p.floor))
	loseUnsynced(p.j, rand.New(rand.NewPCG(seed, ^seed)))
	return nil
}

// must fails the test when the model playing the journal refuses its own
// answer: the played journal, not the journal under test, is wrong.
func (p *played) must(err error) {
	p.t.Helper()
	if err != nil {
		p.t.Fatalf("the played journal refused its own answer: %v", err)
	}
}

// issue returns the next version the journal issues: above every version it
// issued, every version on disk and the floor.
func (p *played) issue() journalv2.Version {
	j := p.j
	return journalv2.Version{Counter: maxVersion(maxVersion(j.issued, j.onDisk()), j.floor).Counter + 1}
}

type playedHandle struct {
	p   *played
	tag journalv2.ShareTag
}

func (h playedHandle) WriteAt(_ context.Context, id journalv2.FileID, off int64, b []byte, mtime int64) (journalv2.Version, error) {
	var v journalv2.Version
	err := h.p.refuse(id, h.tag)
	switch {
	case err == nil && len(b) == 0:
		err = journalv2.ErrZeroLength
	case err == nil:
		v = h.p.issue()
	}
	h.p.must(h.p.j.WriteAt(h.tag, id, off, b, mtime, v, err))
	return v, err
}

func (h playedHandle) Sync(_ context.Context, ids ...journalv2.FileID) error {
	var err error
	for _, id := range ids {
		err = cmp.Or(err, h.p.refuse(id, h.tag))
	}
	h.p.must(h.p.j.Sync(h.tag, ids, err))
	return err
}

func (h playedHandle) ReadAt(ctx context.Context, id journalv2.FileID, off int64, b []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	if err := h.p.refuse(id, h.tag); err != nil {
		return 0, nil, nil, journalv2.Version{}, err
	}
	return modelView{h.p.j, h.tag}.ReadAt(ctx, id, off, b)
}

func (h playedHandle) Fill(_ context.Context, id journalv2.FileID, off int64, b []byte, asOf, v journalv2.Version) error {
	err := h.p.refuse(id, h.tag)
	if err == nil && h.p.j.current(id).change.Compare(asOf) > 0 {
		err = journalv2.ErrStaleFill
	}
	h.p.must(h.p.j.Fill(h.tag, id, off, b, asOf, v, err))
	return err
}

func (h playedHandle) Offload(ctx context.Context, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn func(context.Context, journalv2.Offer, func([]journalv2.Extent) error) error) error {
	j := h.p.j
	if err := h.p.refuse(id, h.tag); err != nil {
		h.p.must(j.EndOffer(h.tag, id, through, nil, err))
		return err
	}
	o := offerAll(j, id, through)
	if len(o.Dirty) == 0 {
		h.p.must(j.EndOffer(h.tag, id, through, nil, nil))
		return nil
	}
	h.p.must(j.Offer(h.tag, id, through, limit, widen, o))
	ended := false
	report := func(es []journalv2.Extent) error {
		var err error
		for _, e := range es {
			switch {
			case ended:
				err = journalv2.ErrOfferEnded
			case e.Len <= 0:
				err = cmp.Or(err, journalv2.ErrZeroLength)
			case !slices.ContainsFunc(o.Dirty, func(d journalv2.OfferedExtent) bool { _, ok := intersect(d.Extent, e); return ok }):
				err = journalv2.ErrNotOffered
			}
		}
		h.p.must(j.Report(h.tag, id, es, err))
		return err
	}
	err := fn(ctx, o, report)
	ended = true
	h.p.must(j.EndOffer(h.tag, id, through, err, err))
	return err
}

func (h playedHandle) Release(_ context.Context, id journalv2.FileID, es []journalv2.Extent) error {
	err := h.p.refuse(id, h.tag)
	if err == nil {
		st := h.p.j.current(id)
		for off := range st.held {
			if !st.bit[off] && slices.ContainsFunc(es, func(e journalv2.Extent) bool { return covers(e, off) }) {
				err = journalv2.ErrNotOffloaded
				break
			}
		}
	}
	h.p.must(h.p.j.Release(h.tag, id, es, err))
	return err
}

// remove answers a removal: refused through another share, and for a
// zero-length extent, otherwise given the next version.
func (h playedHandle) remove(id journalv2.FileID, zero bool, apply func(v journalv2.Version, err error) error) (journalv2.Version, error) {
	var v journalv2.Version
	err := h.p.refuse(id, h.tag)
	switch {
	case err == nil && zero:
		err = journalv2.ErrZeroLength
	case err == nil:
		v = h.p.issue()
	}
	h.p.must(apply(v, err))
	return v, err
}

func (h playedHandle) Truncate(_ context.Context, id journalv2.FileID, size int64) (journalv2.Version, error) {
	return h.remove(id, false, func(v journalv2.Version, err error) error { return h.p.j.Truncate(h.tag, id, size, v, err) })
}

func (h playedHandle) Deallocate(_ context.Context, id journalv2.FileID, e journalv2.Extent) (journalv2.Version, error) {
	return h.remove(id, e.Len == 0, func(v journalv2.Version, err error) error { return h.p.j.Deallocate(h.tag, id, e, v, err) })
}

func (h playedHandle) Delete(_ context.Context, id journalv2.FileID) (journalv2.Version, error) {
	return h.remove(id, false, func(v journalv2.Version, err error) error { return h.p.j.Delete(h.tag, id, v, err) })
}

func (h playedHandle) Settle(id journalv2.FileID, v journalv2.Version) {
	h.p.must(h.p.j.Settle(h.tag, id, v))
}
