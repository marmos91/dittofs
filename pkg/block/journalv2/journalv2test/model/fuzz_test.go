package model

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// driver runs the model under test in lockstep with a second model that plays
// the journal, from bytes a fuzzer chooses. The journal answers every call as
// the RFC allows; both models are given the answer, and after every step the
// model must show exactly what the journal holds. After a crash the journal
// keeps a random state of those the crash allows, each uncertain write or fill
// split at most once at a random point, which the model must find.
//
// The journal is the model's own code, so this shows that the model finds any
// state a crash leaves and keeps answering as that state does. It does not
// show that the model's precedence matches the RFC: the table tests do.
type driver struct {
	t       *testing.T
	m       *Model // under test
	journal *Model // plays the journal
	data    []byte

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

// issue returns the next version the journal issues: above every version it
// issued, every version on disk and the floor, but not above a fill's version
// a crash may have lost, so the bound such a fill must respect is reached.
func (d *driver) issue() journalv2.Version {
	return journalv2.Version{Counter: maxVersion(maxVersion(d.journal.issued, d.journal.onDisk()), d.journal.floor).Counter + 1}
}

// other is the journal's answer to a call through tag on id when another
// share has the file: refused while it holds anything, otherwise accepted.
func (d *driver) other(id journalv2.FileID, tag journalv2.ShareTag) error {
	if f := d.journal.files[id]; f != nil && f.tag != tag && f.holdsAnything() {
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

// both applies a call to the journal, which must take its own answer, and
// then to the model, which must accept it.
func (d *driver) both(call func(m *Model) error) {
	d.t.Helper()
	if err := call(d.journal); err != nil {
		d.t.Fatalf("the journal refused its own answer: %v", err)
	}
	if err := call(d.m); err != nil {
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
		mtime := int64(d.next())
		if err := d.other(id, tag); err != nil {
			d.both(func(m *Model) error { return m.WriteAt(tag, id, off, p, mtime, journalv2.Version{}, err) })
			return
		}
		v := d.issue()
		d.give(id, v, off, p)
		d.t.Logf("write %s share %d %s at %s", fmtFile(id), tag, fmtExtent(extent{Off: off, Len: n}), fmtVersion(v))
		d.both(func(m *Model) error { return m.WriteAt(tag, id, off, p, mtime, v, nil) })

	case 1: // sync
		d.t.Logf("sync %s", fmtFile(id))
		d.both(func(m *Model) error { return m.Sync(tag, []journalv2.FileID{id}, nil) })

	case 2: // fill, never below a release or removal the caller made over its bytes
		n := int64(d.next()%16) + 1
		off := d.offset(n)
		p := fill(d.next(), int(n))
		// Usually an older version, sometimes one above every version the
		// journal issued, which a crash and the next version issued test.
		v := journalv2.Version{Counter: uint64(d.next()) % (d.journal.maxSeen.Counter + 1)}
		if b := d.next(); b < 32 {
			v = journalv2.Version{Counter: d.journal.maxSeen.Counter + 1 + uint64(b)}
		}
		// What the caller released, or removed and settled, is in the records
		// of one model or the other: a crash may have taken it from the
		// journal's while the model kept it, unseen, or the other way round.
		// A fill below an unsettled marker is decided, and is fuzzed.
		for _, mm := range []*Model{d.journal, d.m} {
			if f := mm.files[id]; f != nil {
				for _, r := range f.recs {
					if _, ok := intersect(r.ext, extent{Off: off, Len: n}); ok && (r.kind == kindRelease || r.kind.removal() && r.settled) {
						v = maxVersion(v, r.ver)
					}
				}
			}
		}
		asOf := maxVersion(d.journal.maxSeen, d.journal.floor)
		if d.next()&1 == 0 {
			asOf = journalv2.Version{}
		}
		err := d.other(id, tag)
		if err == nil && d.journal.current(id).change.Compare(asOf) > 0 {
			err = journalv2.ErrStaleFill
		}
		if err == nil {
			d.give(id, v, off, p)
		}
		d.t.Logf("fill %s %s at %s as of %s -> %v", fmtFile(id), fmtExtent(extent{Off: off, Len: n}), fmtVersion(v), fmtVersion(asOf), err)
		d.both(func(m *Model) error { return m.Fill(tag, id, off, p, asOf, v, err) })

	case 3: // offload: offer every dirty run, report some of it
		if f := d.journal.files[id]; f == nil || f.tag != tag {
			return
		}
		through := d.journal.maxSeen
		st := d.journal.current(id)
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
		d.both(func(m *Model) error { return m.Offer(tag, id, through, 0, 0, o) })
		if d.next()&1 == 0 && d.other(id, tag) == nil {
			v := d.issue()
			p := fill(d.next(), 4)
			d.give(id, v, 0, p)
			d.both(func(m *Model) error { return m.WriteAt(tag, id, 0, p, 0, v, nil) })
		}
		var reported []journalv2.Extent
		for _, p := range o.Dirty {
			if d.next()&1 == 0 {
				reported = append(reported, p.Extent)
			}
		}
		d.t.Logf("offload %s through %s offered %s reported %s", fmtFile(id), fmtVersion(through), fmtOffered(o.Dirty), fmtExtents(reported))
		d.both(func(m *Model) error { return m.Report(tag, id, reported, nil) })
		d.both(func(m *Model) error { return m.EndOffer(tag, id, through, nil, nil) })

	case 4: // release
		n := int64(d.next()%32) + 1
		e := journalv2.Extent{Off: d.offset(n), Len: n}
		err := d.other(id, tag)
		if err == nil {
			st := d.journal.current(id)
			for off := range st.held {
				if covers(e, off) && !st.bit[off] {
					err = journalv2.ErrNotOffloaded
					break
				}
			}
		}
		d.t.Logf("release %s %s -> %v", fmtFile(id), fmtExtent(e), err)
		d.both(func(m *Model) error { return m.Release(tag, id, []journalv2.Extent{e}, err) })

	case 5: // truncate
		size := d.offset(0)
		if err := d.other(id, tag); err != nil {
			d.both(func(m *Model) error { return m.Truncate(tag, id, size, journalv2.Version{}, err) })
			return
		}
		v := d.issue()
		d.t.Logf("truncate %s to %d at %s", fmtFile(id), size, fmtVersion(v))
		d.both(func(m *Model) error { return m.Truncate(tag, id, size, v, nil) })

	case 6: // deallocate
		n := int64(d.next() % 16)
		e := journalv2.Extent{Off: d.offset(n), Len: n}
		err := d.other(id, tag)
		if err == nil && n == 0 {
			err = journalv2.ErrZeroLength
		}
		if err != nil {
			d.both(func(m *Model) error { return m.Deallocate(tag, id, e, journalv2.Version{}, err) })
			return
		}
		v := d.issue()
		d.t.Logf("deallocate %s %s at %s", fmtFile(id), fmtExtent(e), fmtVersion(v))
		d.both(func(m *Model) error { return m.Deallocate(tag, id, e, v, nil) })

	case 7: // delete
		if err := d.other(id, tag); err != nil {
			d.both(func(m *Model) error { return m.Delete(tag, id, journalv2.Version{}, err) })
			return
		}
		v := d.issue()
		d.t.Logf("delete %s at %s", fmtFile(id), fmtVersion(v))
		d.both(func(m *Model) error { return m.Delete(tag, id, v, nil) })

	case 8: // settle
		v := journalv2.Version{Counter: uint64(d.next()) % (d.journal.maxSeen.Counter + 1)}
		d.t.Logf("settle %s through %s", fmtFile(id), fmtVersion(v))
		d.both(func(m *Model) error { return m.Settle(tag, id, v) })

	case 9, 10: // crash and reopen
		d.crash()
	}
}

// crash crashes both, and lets the journal keep a random state of those the
// crash allows: each uncertain write or fill split at a random point, and each
// piece and uncertain record kept or lost. The model finds it at the next
// Compare.
func (d *driver) crash() {
	// The search doubles with each record the bytes cannot decide; keep it
	// small so the fuzzer spends its time on sequences.
	const searched = 8
	for _, f := range d.journal.files {
		if uncertainAt(f) > searched {
			id, tag := f.id, f.tag
			d.both(func(m *Model) error { return m.Sync(tag, []journalv2.FileID{id}, nil) })
		}
		if uncertainAt(f) > searched {
			return // settled markers alone exceed it; crash later
		}
	}
	// The floor carries the version limit: above every version issued, not
	// above a fill's version, so a fill above every version issued can be
	// lost and its version issued again.
	floor := journalv2.Version{Counter: d.journal.issued.Counter + 1}
	d.t.Logf("crash, reopen with floor %s", fmtVersion(floor))
	d.both(func(m *Model) error { return m.Crash() })
	d.both(func(m *Model) error { return m.Reopen(floor) })

	rng := rand.New(rand.NewPCG(uint64(d.next()), uint64(d.next())))
	for _, f := range d.journal.files {
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
		candidate{items: f.items(), mask: rng.Uint32()}.adopt(f)
	}

	// Issue a version before the model compares the files the crash left
	// uncertain, on a file no crash touches, so the bound it sets on them is
	// checked.
	v := d.issue()
	p := []byte{d.next()}
	d.give(fid(5), v, 0, p)
	d.t.Logf("write %s share %d [0,1) at %s", fmtFile(fid(5)), shareA, fmtVersion(v))
	d.both(func(m *Model) error { return m.WriteAt(shareA, fid(5), 0, p, 0, v, nil) })
	d.both(func(m *Model) error { return m.Sync(shareA, []journalv2.FileID{fid(5)}, nil) })
}

// uncertainAt counts what a crash now would leave uncertain in f.
func uncertainAt(f *file) int {
	n := 0
	for _, r := range f.recs {
		if !r.durable || crashMayDropMarker(r) {
			n++
		}
	}
	return n
}

// compare checks the model against the journal, share by share, and that it
// holds no byte it was not given.
func (d *driver) compare() {
	for _, tag := range []journalv2.ShareTag{shareA, shareB} {
		if err := d.m.Compare(d.t.Context(), tag, modelView{d.journal, tag}); err != nil {
			d.t.Fatalf("the model does not show what the journal holds: %v", err)
		}
	}
	for id, f := range d.m.files {
		if f.unresolved {
			continue
		}
		for off, c := range d.m.current(id).held {
			if !d.given[id][c.rec.ver][off][c.b] {
				d.t.Fatalf("%s byte %d holds %q at %s, which no write or fill gave", fmtFile(id), off, c.b, fmtVersion(c.rec.ver))
			}
		}
	}
}

// clone returns a model with its own copy of every file and record of m.
func clone(m *Model) *Model {
	c := New()
	c.seq, c.issued, c.maxSeen, c.floor, c.crashed = m.seq, m.issued, m.maxSeen, m.floor, m.crashed
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

// FuzzModel drives random sequences of every operation through the model and
// a second model playing the journal, crashes included. The model must accept
// every answer the RFC allows, never panic, never hold a byte it was not
// given, and show what the journal holds after every step.
func FuzzModel(f *testing.F) {
	f.Add([]byte{0, 0, 3, 'a', 0, 9, 1, 0, 0, 0, 2, 0, 3, 0, 0, 9, 1, 2})
	f.Add([]byte{0, 0, 0xff, 7, 'z', 0, 5, 9, 0, 5, 1, 0, 9, 3, 4, 0, 4, 0, 6, 8, 1, 0, 0, 10})
	f.Add([]byte{3, 0, 1, 15, 'q', 0, 3, 3, 1, 1, 2, 3, 2, 'w', 0, 4, 4, 2, 0, 9, 5, 6})
	f.Add([]byte{0, 0, 4, 3, 'b', 0, 0, 3, 1, 1, 3, 3, 0, 0, 4, 4, 5, 0, 7, 8, 7, 0, 7, 9, 0, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := &driver{t: t, m: New(), journal: New(), data: data, given: map[journalv2.FileID]map[journalv2.Version]map[int64]map[byte]bool{}}
		for steps := 0; len(d.data) > 0 && steps < 256; steps++ {
			d.step()
			d.compare()
		}
	})
}

func fmtExtents(es []journalv2.Extent) string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = fmtExtent(e)
	}
	return "[" + strings.Join(out, " ") + "]"
}

func fmtOffered(os []journalv2.OfferedExtent) string {
	out := make([]string, len(os))
	for i, o := range os {
		out[i] = fmtExtent(o.Extent) + " " + fmtVersion(o.Version)
	}
	return "[" + strings.Join(out, ", ") + "]"
}
