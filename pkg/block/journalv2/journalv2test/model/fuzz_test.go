package model

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// driver plays a journal that always answers as the RFC allows, from bytes a
// fuzzer chooses, and drives the model with its answers.
type driver struct {
	t    *testing.T
	m    *Model
	data []byte

	// given is every byte any write or fill handed the model, per file,
	// version and offset: the only bytes the model may ever report.
	given map[journalv2.FileID]map[journalv2.Version]map[int64]map[byte]bool
}

func (d *driver) next() byte {
	if len(d.data) == 0 {
		return 0
	}
	b := d.data[0]
	d.data = d.data[1:]
	return b
}

var fuzzFiles = []journalv2.FileID{fid(1), fid(2), fid(3), fid(4)}

// pick returns a file and the share a call on it goes through. Files 1 and 2
// are share A's and file 3 share B's; file 4 is called through either, so
// another share's call on a file is reached too.
func (d *driver) pick() (journalv2.FileID, journalv2.ShareTag) {
	b := d.next()
	id := fuzzFiles[int(b)%len(fuzzFiles)]
	switch {
	case id == fid(3), id == fid(4) && b&0x80 != 0:
		return id, shareB
	}
	return id, shareA
}

// offset is small, or close to the largest file offset.
func (d *driver) offset(n int64) int64 {
	b := d.next()
	if b == 0xff {
		return math.MaxInt64 - 64 - n
	}
	return int64(b % 64)
}

func (d *driver) issue() journalv2.Version {
	return journalv2.Version{Counter: maxVersion(d.m.maxSeen, d.m.floor).Counter + 1}
}

// other is the answer of a call through tag on id when another share has the
// file: refused while it holds anything, otherwise accepted.
func (d *driver) other(id journalv2.FileID, tag journalv2.ShareTag) error {
	if f := d.m.files[id]; f != nil && f.tag != tag && f.holdsAnything() {
		return journalv2.ErrOtherShare
	}
	return nil
}

func (d *driver) give(id journalv2.FileID, v journalv2.Version, off int64, p []byte) {
	if d.given[id] == nil {
		d.given[id] = map[journalv2.Version]map[int64]map[byte]bool{}
	}
	if d.given[id][v] == nil {
		d.given[id][v] = map[int64]map[byte]bool{}
	}
	for i, b := range p {
		at := d.given[id][v][off+int64(i)]
		if at == nil {
			at = map[byte]bool{}
			d.given[id][v][off+int64(i)] = at
		}
		at[b] = true
	}
}

func (d *driver) must(err error) {
	d.t.Helper()
	if err != nil {
		d.t.Fatalf("the model refused an answer the RFC allows: %v", err)
	}
}

func (d *driver) step() {
	id, tag := d.pick()
	switch d.next() % 11 {
	case 0: // write
		n := int64(d.next()%16) + 1
		off := d.offset(n)
		p := fill(d.next(), int(n))
		p[0] ^= d.next()
		if err := d.other(id, tag); err != nil {
			d.must(d.m.WriteAt(tag, id, off, p, 0, journalv2.Version{}, err))
			return
		}
		v := d.issue()
		d.give(id, v, off, p)
		d.must(d.m.WriteAt(tag, id, off, p, int64(d.next()), v, nil))

	case 1: // sync
		d.m.Sync(tag, id)

	case 2: // fill
		n := int64(d.next()%16) + 1
		off := d.offset(n)
		p := fill(d.next(), int(n))
		v := journalv2.Version{Counter: uint64(d.next()) % (d.m.maxSeen.Counter + 1)}
		asOf := maxVersion(d.m.maxSeen, d.m.floor)
		if d.next()&1 == 0 {
			asOf = journalv2.Version{}
		}
		err := d.other(id, tag)
		if err == nil && d.m.current(id).change.Compare(asOf) > 0 {
			err = journalv2.ErrStaleFill
		}
		if err == nil {
			d.give(id, v, off, p)
		}
		d.must(d.m.Fill(tag, id, off, p, asOf, v, err))

	case 3: // offload: offer every dirty run, report some of it
		if f := d.m.files[id]; f == nil || f.tag != tag {
			return
		}
		through := d.m.maxSeen
		st := d.m.current(id)
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
		d.must(d.m.Offer(tag, id, through, 0, 0, o))
		if d.next()&1 == 0 && d.other(id, tag) == nil {
			v := d.issue()
			p := fill(d.next(), 4)
			d.give(id, v, 0, p)
			d.must(d.m.WriteAt(tag, id, 0, p, 0, v, nil))
		}
		var reported []journalv2.Extent
		for _, p := range o.Dirty {
			if d.next()&1 == 0 {
				reported = append(reported, p.Extent)
			}
		}
		d.must(d.m.Report(tag, id, reported, nil))
		d.m.EndOffer(tag, id)

	case 4: // release
		n := int64(d.next()%32) + 1
		e := journalv2.Extent{Off: d.offset(n), Len: n}
		err := d.other(id, tag)
		if err == nil {
			st := d.m.current(id)
			for off := range st.held {
				if covers(e, off) && !st.bit[off] {
					err = journalv2.ErrNotOffloaded
					break
				}
			}
		}
		d.must(d.m.Release(tag, id, []journalv2.Extent{e}, err))

	case 5: // truncate
		size := d.offset(0)
		if err := d.other(id, tag); err != nil {
			d.must(d.m.Truncate(tag, id, size, journalv2.Version{}, err))
			return
		}
		d.must(d.m.Truncate(tag, id, size, d.issue(), nil))

	case 6: // deallocate
		n := int64(d.next() % 16)
		e := journalv2.Extent{Off: d.offset(n), Len: n}
		err := d.other(id, tag)
		if err == nil && n == 0 {
			err = journalv2.ErrZeroLength
		}
		if err != nil {
			d.must(d.m.Deallocate(tag, id, e, journalv2.Version{}, err))
			return
		}
		d.must(d.m.Deallocate(tag, id, e, d.issue(), nil))

	case 7: // delete
		if err := d.other(id, tag); err != nil {
			d.must(d.m.Delete(tag, id, journalv2.Version{}, err))
			return
		}
		d.must(d.m.Delete(tag, id, d.issue(), nil))

	case 8: // settle
		d.m.Settle(tag, id, journalv2.Version{Counter: uint64(d.next()) % (d.m.maxSeen.Counter + 1)})

	case 9: // crash, reopen, and a journal that kept a random allowed state
		d.crash()

	case 10: // the model agrees with itself
		for _, tag := range []journalv2.ShareTag{shareA, shareB} {
			d.must(d.m.Compare(tag, modelView{d.m, tag}))
		}
	}
}

// crash crashes the model, and resolves it against a journal that kept a
// state the crash allows, chosen by the fuzzer: Compare must accept it and
// continue from it.
func (d *driver) crash() {
	// The search for the state the journal kept doubles with each uncertain
	// record; keep it small so the fuzzer spends its time on sequences.
	const searched = 8
	for _, f := range d.m.files {
		if uncertainAt(f) > searched {
			d.m.Sync(f.tag, f.id)
		}
		if uncertainAt(f) > searched {
			return // settled markers alone exceed it; crash later
		}
	}
	journal := clone(d.m)
	d.must(d.m.Crash())
	d.must(journal.Crash())
	floor := d.issue()
	d.must(d.m.Reopen(floor))
	d.must(journal.Reopen(floor))

	rng := rand.New(rand.NewPCG(uint64(d.next()), uint64(d.next())))
	for _, f := range journal.files {
		if f.unresolved {
			items := f.items()
			candidate{items: items, mask: rng.Uint32() & (1<<len(items) - 1)}.adopt(f)
		}
	}
	for _, tag := range []journalv2.ShareTag{shareA, shareB} {
		d.must(d.m.Compare(tag, modelView{journal, tag}))
	}
	for _, tag := range []journalv2.ShareTag{shareA, shareB} {
		d.must(journal.Compare(tag, modelView{d.m, tag}))
	}
}

// uncertainAt counts what a crash now would leave uncertain in f.
func uncertainAt(f *file) int {
	n := 0
	for _, r := range f.recs {
		if !r.durable || r.kind.removal() && r.settled && !r.markerGone {
			n++
		}
	}
	return n
}

// clone returns a model with its own copy of every file and record of m.
func clone(m *Model) *Model {
	c := New()
	c.seq, c.maxSeen, c.floor, c.crashed = m.seq, m.maxSeen, m.floor, m.crashed
	for id, f := range m.files {
		g := &file{id: f.id, tag: f.tag, unresolved: f.unresolved}
		for _, r := range f.recs {
			copied := *r
			g.recs = append(g.recs, &copied)
		}
		c.files[id] = g
	}
	return c
}

// check asserts the model reports only bytes it was given: every held byte is
// one some write or fill handed it, at that offset and version.
func (d *driver) check() {
	for id, f := range d.m.files {
		if f.unresolved {
			continue
		}
		for off, c := range d.m.current(id).held {
			if !d.given[id][c.rec.ver][off][c.b] {
				d.t.Fatalf("file %x byte %d holds %q at %v, which no write or fill gave", id.Bytes(), off, c.b, c.rec.ver)
			}
		}
	}
}

// FuzzModel drives random sequences of every operation through the model
// alone, answering each as the RFC allows. The model must accept every such
// answer, never panic, and never hold a byte it was not given.
func FuzzModel(f *testing.F) {
	f.Add([]byte{0, 0, 3, 'a', 0, 9, 1, 0, 0, 0, 2, 0, 3, 0, 0, 9, 1, 2})
	f.Add([]byte{0, 0, 0xff, 7, 'z', 0, 5, 9, 0, 5, 1, 0, 9, 3, 4, 0, 4, 0, 6, 8, 1, 0, 0, 10})
	f.Add([]byte{3, 0, 1, 15, 'q', 0, 3, 3, 1, 1, 2, 3, 2, 'w', 0, 4, 4, 2, 0, 9, 5, 6})
	f.Add([]byte{0, 0, 4, 3, 'b', 0, 0, 3, 1, 1, 3, 3, 0, 0, 4, 4, 5, 0, 7, 8, 7, 0, 7, 9, 0, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := &driver{t: t, m: New(), data: data, given: map[journalv2.FileID]map[journalv2.Version]map[int64]map[byte]bool{}}
		for steps := 0; len(d.data) > 0 && steps < 256; steps++ {
			d.step()
			d.check()
		}
	})
}
