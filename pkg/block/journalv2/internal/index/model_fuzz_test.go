// The model-based fuzz drives the index through the same sequence of steps
// the running journal would take, comparing the model against it after every
// one. It runs in this external test package because the model imports the
// journal's public package, which will import the index once the journal is
// wired, so an internal test of package index could not import both.
package index_test

import (
	"context"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/index"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/model"
)

// Small shared builders, named once.
func ext(off, n int64) core.Extent { return core.Extent{Off: off, Len: n} }
func ver(n uint64) core.Version    { return core.Version{Counter: n} }
func fid(n byte) core.FileID       { return core.NewFileID([16]byte{15: n}) }

// fidOf converts a public FileID to the core one the index takes, which is
// the same value under another name.
func fidOf(id journalv2.FileID) core.FileID { return core.NewFileID(id.Bytes()) }

// shareA and shareB are the two tags the driver holds, as a journal with two
// shares attached.
const (
	shareA core.ShareTag = 1
	shareB core.ShareTag = 2
)

// fuzzFiles are the files the driver works on.
var fuzzFiles = []core.FileID{fid(1), fid(2), fid(3), fid(4)}

// indexView serves the model from the index-backed harness: the reads, the
// file lists and the index entries, all from the read-out and the span, as
// the wired journal's inspection would.
type indexView struct {
	mi  *index.ModelIndex
	tag journalv2.ShareTag
}

var _ model.View = indexView{}

// ReadAt serves p from the harness' records: each piece the held set places
// is read from the record that holds it, at the payload offset its skip
// names. A piece the index places outside its record's payload is a bug the
// model cannot even phrase, so it is reported as an error of the read.
func (v indexView) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (n int, missing []journalv2.Extent, held []journalv2.Held, asOf journalv2.Version, err error) {
	e := core.Extent{Off: off, Len: int64(len(p))}
	placed, missingE, asOf := v.mi.Read(fidOf(id), e)
	for _, pl := range placed {
		rec, ok := v.mi.Payload(pl.Record)
		if !ok || uint64(pl.Skip)+uint64(pl.Extent.Len) > uint64(len(rec.Data)) {
			return 0, nil, nil, asOf, fmt.Errorf("index places %v in record %v from skip %d, outside its payload", pl.Extent, pl.Record, pl.Skip)
		}
		copy(p[pl.Extent.Off-off:], rec.Data[pl.Skip:uint64(pl.Skip)+uint64(pl.Extent.Len)])
		held = append(held, journalv2.Held{Extent: pl.Extent, Version: pl.Version})
	}
	return len(p), slices.Clone(missingE), held, asOf, nil
}

// Extents returns the held extents of id, contiguous ones merged.
func (v indexView) Extents(id journalv2.FileID) ([]journalv2.Extent, error) {
	var out []journalv2.Extent
	for _, e := range v.mi.File(fidOf(id)) {
		if e.Kind != index.EntryHeld {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Off+out[n-1].Len == e.Extent.Off {
			out[n-1].Len += e.Extent.Len
			continue
		}
		out = append(out, e.Extent)
	}
	return out, nil
}

// Since maps the index's changes onto the journal's, one for one, the mtime
// of every write read from the record it names.
func (v indexView) Since(ctx context.Context, id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
	return func(yield func(journalv2.Change, error) bool) {
		for _, c := range v.mi.Since(fidOf(id), from) {
			out := journalv2.Change{Kind: c.Kind, Extent: c.Extent, Version: c.Version, Clone: c.Clone}
			if c.Kind == core.ChangeWrite {
				if rec, ok := v.mi.Payload(c.Record); ok {
					out.Mtime = rec.Mtime
				}
			}
			if !yield(out, nil) {
				return
			}
		}
	}
}

// Files lists the share's files: those the read-out sees under this tag with
// a held entry or a marker. A file holding only retained content has nothing
// the share serves or owes a caller, so it is not listed, as the model's
// wantFiles expects.
func (v indexView) Files() iter.Seq[journalv2.FileID] {
	return func(yield func(journalv2.FileID) bool) {
		for id, es := range v.mi.All() {
			if v.visible(es, false) && !yield(journalv2.NewFileID(id.Bytes())) {
				return
			}
		}
	}
}

// DirtyFiles lists the share's files holding content whose offloaded bit is
// unset. The times are zero, as the harness has no clock.
func (v indexView) DirtyFiles() iter.Seq2[journalv2.FileID, time.Time] {
	return func(yield func(journalv2.FileID, time.Time) bool) {
		for id, es := range v.mi.All() {
			if v.visible(es, true) && !yield(journalv2.NewFileID(id.Bytes()), time.Time{}) {
				return
			}
		}
	}
}

// visible reports whether the entries the read-out handed over belong to
// this share and, when dirty is set, whether the file holds dirty content: a
// foreign tag answers false, nothing listed or nothing dirty false too.
func (v indexView) visible(es []index.Entry, dirty bool) bool {
	var listed, isDirty bool
	for _, e := range es {
		if e.Tag != core.ShareTag(v.tag) {
			return false
		}
		switch e.Kind {
		case index.EntryHeld:
			listed = true
			if !e.Offloaded {
				isDirty = true
			}
		case index.EntryMarker:
			listed = true
		}
	}
	if !dirty {
		return listed
	}
	return listed && isDirty
}

// Index maps the read-out onto the journal's inspection entries, dropping the
// fields the public entry does not carry: Skip, By, Removal and Clone. This
// is the read-out the model consumes.
func (v indexView) Index(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
	var out []journalv2.IndexEntry
	for _, e := range v.mi.File(fidOf(id)) {
		out = append(out, journalv2.IndexEntry{
			Kind:      journalv2.EntryKind(e.Kind),
			Extent:    e.Extent,
			Version:   e.Version,
			Tag:       e.Tag,
			Location:  journalv2.RecordRef{Segment: e.Record.Segment, Offset: int64(e.Record.Offset)},
			Offloaded: e.Offloaded,
		})
	}
	return out, nil
}

// The offered reader: every byte it holds reads, and the first byte it does
// not hold fails with ErrNotOffered, as the offer's probe check requires.
type offeredBytes map[int64]byte

func (o offeredBytes) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		b, ok := o[off+int64(i)]
		if !ok {
			if i > 0 {
				return i, nil
			}
			return 0, journalv2.ErrNotOffered
		}
		p[i] = b
	}
	return len(p), nil
}

// versioned is one extent with the version the operation that named it drew.
type versioned struct {
	e core.Extent
	v core.Version
}

// fuzzDriver is one fuzz run: the harness, the model compared with it, the
// input, the log, and the driver's own memory of its releases and removals,
// which later fills are raised to stay off the model's undecided corners.
type fuzzDriver struct {
	mi  *index.ModelIndex
	m   *model.Model
	in  []byte
	i   int
	log []string
	// err is the first error a step found, so the run stops at it.
	err error
	// released are the extents each release removed, with the highest version
	// it released them at; removed the removals, marked settled ones.
	released map[core.FileID][]versioned
	removed  map[core.FileID][]versioned
	settled  map[core.FileID]core.Version // highest settle version, any tag (an over-approximation is harmless)
}

// next returns the next input byte, and 0 once the input is exhausted.
func (d *fuzzDriver) next() byte {
	if d.i >= len(d.in) {
		return 0
	}
	b := d.in[d.i]
	d.i++
	return b
}

// done reports whether the input is exhausted.
func (d *fuzzDriver) done() bool { return d.i >= len(d.in) }

// offset decodes one extent offset from the input: 0xff names the high end
// of the file less n, so a write near the top of the file is driven; anything
// else is the byte modulo 64.
func (d *fuzzDriver) offset(n int64) int64 {
	if b := d.next(); b == 0xff {
		return math.MaxInt64 - 64 - n
	} else {
		return int64(b % 64)
	}
}

// payload builds one record's bytes: the salt plus the index of each byte, so
// every byte of a record differs from every other and a wrong skip shows up
// as wrong bytes read.
func payload(salt byte, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = salt + byte(i)
	}
	return p
}

// overlaps reports whether two extents share a byte.
func overlaps(a, b core.Extent) bool {
	return a.Off < b.Off+b.Len && b.Off < a.Off+a.Len
}

// toEnd is the extent from off to the end of the file.
func toEnd(off int64) core.Extent { return core.Extent{Off: off, Len: math.MaxInt64 - off} }

// runModelFuzz drives at most 256 steps decoded from the input, comparing the
// index with the model after every one, and returns the step log with the
// first difference found.
func runModelFuzz(data []byte) ([]string, error) {
	d := &fuzzDriver{
		mi:       index.NewModelIndex(),
		in:       data,
		released: map[core.FileID][]versioned{},
		removed:  map[core.FileID][]versioned{},
		settled:  map[core.FileID]core.Version{},
	}
	d.m = model.New()
	for range 256 {
		if d.done() {
			break
		}
		d.step()
		if d.err != nil {
			return d.log, d.err
		}
		for _, tag := range []journalv2.ShareTag{shareA, shareB} {
			if err := d.m.Compare(context.Background(), tag, indexView{d.mi, tag}); err != nil {
				return d.log, err
			}
		}
		if err := d.mi.CheckAccounting(); err != nil {
			return d.log, err
		}
	}
	return d.log, nil
}

// step decodes and runs one operation on both the harness and the model.
func (d *fuzzDriver) step() {
	b := d.next()
	id := fuzzFiles[int(b)%len(fuzzFiles)]
	tag := journalv2.ShareTag(shareA)
	if id == fuzzFiles[2] || (id == fuzzFiles[3] && b&0x80 != 0) {
		tag = journalv2.ShareTag(shareB)
	}
	jid := journalv2.NewFileID(id.Bytes())
	issued := d.mi.Issued()
	names := [...]string{"write", "fill", "offload", "release", "truncate", "deallocate", "delete", "settle", "write"}
	op := int(d.next()) % len(names)
	d.log = append(d.log, fmt.Sprintf("%s file %d through share %d", names[op], id.Bytes()[15], tag))
	switch op {
	case 0, 8: // write
		n := int64(d.next())%32 + 1
		off := d.offset(n)
		salt := d.next()
		mtime := int64(d.next())
		retain := d.next()&1 == 1
		p := payload(salt, int(n))
		v, err := d.mi.Write(core.ShareTag(tag), id, off, p, mtime, retain)
		d.record("wrote", ext(off, n), v, nil, err)
		if d.mis(d.m.WriteAt(tag, jid, off, p, mtime, v, err)) {
			return
		}

	case 1: // fill
		n := int64(d.next())%32 + 1
		off := d.offset(n)
		salt := d.next()
		vb := d.next()
		ab := d.next() % 3
		p := payload(salt, int(n))
		v := ver(uint64(vb) % (issued.Counter + 1))
		// The fill's version is raised to every release and settled removal
		// over its bytes, keeping the call off the model's undecided corner:
		// the running journal never fills below what it released.
		for _, rel := range d.released[id] {
			if overlaps(rel.e, ext(off, n)) && rel.v.Compare(v) > 0 {
				v = rel.v
			}
		}
		for _, rem := range d.removed[id] {
			if rem.v.Compare(d.settled[id]) <= 0 && overlaps(rem.e, ext(off, n)) && rem.v.Compare(v) > 0 {
				v = rem.v
			}
		}
		var asOf core.Version
		switch ab {
		case 0:
			asOf = d.mi.AsOf(id)
		case 1:
			asOf = core.Version{}
		default:
			asOf = issued
		}
		err := d.mi.Fill(core.ShareTag(tag), id, off, p, v, asOf)
		d.record("filled", ext(off, n), v, nil, err)
		if d.mis(d.m.Fill(tag, jid, off, p, asOf, v, err)) {
			return
		}
		d.log[len(d.log)-1] += fmt.Sprintf(", as of v%d", asOf.Counter)

	case 2: // offload
		d.offload(tag, id, jid)

	case 3: // release
		n := int64(d.next())%32 + 1
		off := d.offset(n)
		out, err := d.mi.Release(core.ShareTag(tag), id, ext(off, n))
		if err == nil {
			for _, h := range out {
				d.released[id] = append(d.released[id], versioned{h.Extent, h.Version})
			}
		}
		d.record("released", ext(off, n), core.Version{}, out, err)
		d.mis(d.m.Release(tag, jid, []journalv2.Extent{ext(off, n)}, err))

	case 4: // truncate
		size := d.offset(0)
		v, err := d.mi.Truncate(core.ShareTag(tag), id, size)
		if err == nil {
			d.removed[id] = append(d.removed[id], versioned{toEnd(size), v})
		}
		d.record("truncated to", ext(size, 0), v, nil, err)
		d.mis(d.m.Truncate(tag, jid, size, v, err))

	case 5: // deallocate
		n := int64(d.next() % 16)
		off := d.offset(n)
		v, err := d.mi.Deallocate(core.ShareTag(tag), id, ext(off, n))
		if err == nil {
			d.removed[id] = append(d.removed[id], versioned{ext(off, n), v})
		}
		d.record("deallocated", ext(off, n), v, nil, err)
		d.mis(d.m.Deallocate(tag, jid, ext(off, n), v, err))

	case 6: // delete
		v, err := d.mi.Delete(core.ShareTag(tag), id)
		if err == nil {
			d.removed[id] = append(d.removed[id], versioned{toEnd(0), v})
		}
		d.record("deleted", toEnd(0), v, nil, err)
		d.mis(d.m.Delete(tag, jid, v, err))

	case 7: // settle
		v := ver(uint64(d.next()) % (issued.Counter + 1))
		d.mi.Settle(core.ShareTag(tag), id, v)
		d.record("settled", core.Extent{}, v, nil, nil)
		if d.mis(d.m.Settle(tag, jid, v)) {
			return
		}
		if v.Compare(d.settled[id]) > 0 {
			d.settled[id] = v
		}
	}
}

// record notes one step's outcome in the log: what it touched, the version it
// drew, and the error the index returned, if any.
func (d *fuzzDriver) record(what string, e core.Extent, v core.Version, out any, err error) {
	line := fmt.Sprintf("  %s", what)
	if e.Len > 0 || e.Off > 0 {
		line += fmt.Sprintf(" %v", e)
	}
	if v != (core.Version{}) {
		line += fmt.Sprintf(" at v%d", v.Counter)
	}
	if n, ok := out.([]core.Held); ok {
		line += fmt.Sprintf(" (%d pieces)", len(n))
	}
	if err != nil {
		line += fmt.Sprintf(", refused: %v", err)
	}
	d.log = append(d.log, line)
}

// mis reports the error to the log and the driver's stop, if the model found
// one. A misuse is the driver's bug, kept out of the fuzz's own answer: the
// planted-bug test checks for it separately.
func (d *fuzzDriver) mis(err error) bool {
	if err == nil {
		return false
	}
	d.log = append(d.log, "  "+err.Error())
	if d.err == nil {
		d.err = err
	}
	return true
}

// offload drives one offload on both sides: the offer the journal would make
// of the file's dirty content, the reports the input chooses, and the end of
// the offer. A write between the offer and the reports exercises the rule
// that a report marks only what it offered, not what arrived meanwhile.
func (d *fuzzDriver) offload(tag journalv2.ShareTag, id core.FileID, jid journalv2.FileID) {
	through := d.mi.Issued()
	inter := d.next()&1 == 1
	salt := byte(0)
	if inter {
		salt = d.next()
	}
	if err := d.mi.Check(core.ShareTag(tag), id); err != nil {
		d.log = append(d.log, fmt.Sprintf("  offload refused: %v", err))
		if d.mis(d.m.EndOffer(tag, jid, through, nil, err)) {
			return
		}
		return
	}
	// The dirty extents, contiguous ones of one version merged, and their
	// bytes read through the view.
	type span struct {
		e core.Extent
		v core.Version
	}
	var dirty []span
	for _, en := range d.mi.File(id) {
		if en.Kind != index.EntryHeld || en.Offloaded || en.Tag != core.ShareTag(tag) {
			continue
		}
		if n := len(dirty); n > 0 && dirty[n-1].e.Off+dirty[n-1].e.Len == en.Extent.Off && dirty[n-1].v == en.Version {
			dirty[n-1].e.Len += en.Extent.Len
			continue
		}
		dirty = append(dirty, span{en.Extent, en.Version})
	}
	if len(dirty) == 0 {
		d.log = append(d.log, "  offered nothing")
		d.mis(d.m.EndOffer(tag, jid, through, nil, nil))
		return
	}
	o := offeredBytes{}
	view := indexView{d.mi, tag}
	oldest, newest := dirty[0].v, dirty[0].v
	for _, s := range dirty {
		if s.v.Compare(oldest) < 0 {
			oldest = s.v
		}
		if s.v.Compare(newest) > 0 {
			newest = s.v
		}
		p := make([]byte, s.e.Len)
		if _, _, _, _, err := view.ReadAt(context.Background(), jid, s.e.Off, p); err != nil {
			d.mis(err)
			return
		}
		for i := range p {
			o[s.e.Off+int64(i)] = p[i]
		}
	}
	offer := journalv2.Offer{ID: jid, Oldest: oldest, Newest: newest, Offered: o}
	for _, s := range dirty {
		offer.Dirty = append(offer.Dirty, journalv2.OfferedExtent{Extent: s.e, Version: s.v})
	}
	d.log = append(d.log, fmt.Sprintf("  offered %d dirty extents, versions v%d..v%d", len(dirty), oldest.Counter, newest.Counter))
	if d.mis(d.m.Offer(tag, jid, through, 0, 0, offer)) {
		return
	}
	// Sometimes a write inside the offer, as the running callback would let
	// a concurrent one through.
	if inter {
		p := payload(salt, 4)
		v, err := d.mi.Write(core.ShareTag(tag), id, 0, p, 0, false)
		d.record("wrote during offer", ext(0, 4), v, nil, err)
		if d.mis(d.m.WriteAt(tag, jid, 0, p, 0, v, err)) {
			return
		}
	}
	// The reports: each dirty extent is skipped, whole, or partly reported,
	// shaped from the bytes captured before the offer's write, so a report
	// never names the newer content.
	var reported []journalv2.Extent
	for _, s := range dirty {
		switch d.next() % 3 {
		case 0:
			continue
		case 1:
			reported = append(reported, s.e)
		default:
			start := s.e.Off + int64(d.next())%s.e.Len
			length := 1 + int64(d.next())%(s.e.Off+s.e.Len-start)
			reported = append(reported, ext(start, length))
		}
	}
	for _, e := range reported {
		if err := d.mi.Mark(core.ShareTag(tag), id, e, oldest, newest); err != nil {
			// A mark sets the bits first and can refuse only at the record's
			// note; going on would leave the index marked and the model not,
			// so the run stops on the refusal and names it.
			d.mis(err)
			return
		}
	}
	d.log = append(d.log, fmt.Sprintf("  reported %d extents", len(reported)))
	if d.mis(d.m.Report(tag, jid, reported, nil)) {
		return
	}
	d.mis(d.m.EndOffer(tag, jid, through, nil, nil))
}

// FuzzIndexModel finds operations whose index answer the reference model
// disagrees with: any divergence in the bytes served, the change version a
// read reports, what Since yields, the offloaded bits the index keeps, which
// files a share lists as its own or as dirty, or the index's own counts,
// fails with the step log.
func FuzzIndexModel(f *testing.F) {
	for _, seed := range modelSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		log, err := runModelFuzz(data)
		if err != nil {
			t.Fatalf("after steps:\n%s\ngot: %v", strings.Join(log, "\n"), err)
		}
	})
}

// modelSeeds are the hand-written seeds. Each names a scenario the model must
// see whole; the coverage-guided engine mutates from them and keeps whatever
// it finds.
var modelSeeds = [][]byte{
	// Three-way split: write [0,20) at v1, then write [5,9) at v2, which
	// splits the first piece in three. The tail's payload offset is where a
	// split bug shows: a tail that keeps the head's skip serves wrong bytes.
	{0, 0, 19, 0, 'a', 1, 0, 0, 0, 3, 5, 'b', 2, 0},

	// Partial report, release, refill: write [0,16); an offload offering the
	// whole span, reporting [4,12); a release of [4,12); then a fill of
	// [0,16) whose version is raised to the release's, so the model allows it.
	{0, 0, 15, 0, 'c', 3, 0, 0, 2, 0, 2, 4, 7, 0, 3, 7, 4, 0, 1, 15, 0, 'd', 255, 0},

	// Removals, settle, other share, retain, big offset: a retained write;
	// a deallocate; a truncate; a settle; a write through share B on file 4;
	// share A refused on file 4; a delete of file 1; a write near the top.
	{0, 0, 31, 0, 'e', 5, 1, 0, 5, 4, 8, 0, 4, 20, 0, 7, 255, 0x83, 0, 3, 0, 'f', 6, 0, 3, 0, 3, 0, 'g', 7, 0, 0, 6, 1, 0, 7, 0xff, 'h', 8, 0},

	// Stale fill, marker mask: file 3 under share B, written [10,20); then
	// truncated to 12; a fill with a zero asOf must be refused as stale; a
	// fill at v0 with the file's own asOf is accepted and masked by the
	// marker the truncate left.
	{2, 0, 9, 10, 'i', 9, 0, 2, 4, 12, 2, 1, 9, 10, 'j', 1, 1, 2, 1, 9, 10, 'k', 0, 0},

	// Write during offload: write [0,8); an offer; a write of [0,4) at v2
	// inside it; then a report of the whole [0,8). Only [4,8) may be marked.
	{0, 0, 7, 0, 'l', 1, 0, 0, 2, 1, 'm', 1},
}

// plantInputs returns every seed plus sixty-four generated inputs, so the
// planted-bug test runs a corpus the size of a CI pass without fuzzing.
func plantInputs() [][]byte {
	out := slices.Clone(modelSeeds)
	rng := rand.New(rand.NewPCG(4, 12))
	for range 64 {
		in := make([]byte, 160)
		for i := range in {
			in[i] = byte(rng.IntN(256))
		}
		out = append(out, in)
	}
	return out
}

// TestModelFuzzInputsPass runs every seed and generated input once on the
// default build: each must pass. A driver that reported spurious divergence
// would make the planted-bug test vacuous, so this pins the driver's own
// soundness.
func TestModelFuzzInputsPass(t *testing.T) {
	for i, in := range plantInputs() {
		log, err := runModelFuzz(in)
		if err != nil {
			t.Fatalf("input %d failed:\nsteps:\n%s\ngot: %v", i, strings.Join(log, "\n"), err)
		}
	}
}

// TestModelFuzzFilesInDistinctBuckets guards the fuzz's floor-bucket
// decision, recorded at the driver: the four files must sit in distinct
// buckets, or a fill of one can be refused by another's settled removal.
func TestModelFuzzFilesInDistinctBuckets(t *testing.T) {
	seen := make(map[int]bool, len(fuzzFiles))
	for _, id := range fuzzFiles {
		b := index.BucketOf(id)
		if seen[b] {
			t.Fatalf("two fuzz files share bucket %d, which the distinct-bucket decision forbids", b)
		}
		seen[b] = true
	}
}

// decision: the fuzz drives writes, fills, offloads with their reports,
// releases, truncates, deallocates, deletes and settles — the operations the
// model itself owns — and leaves out the rest. Sync changes nothing but
// durability, which matters only across a crash; a crash, a reopen and the
// rebuild belong to the rebuild's own property tests, and the model-based
// crash search needs records on disk; the model has no unmark, so one is
// never driven; clone targets, Forget, strand, failed sync windows, repoint
// and repack change nothing the model holds an opinion about (see its package
// doc); and the harness's 2^40 bounds never refuse, so capacity has no rule
// to break. Add an operation when the model gains one.

// decision: the four fuzz files sit in distinct floor buckets, because the
// index keeps no per-file change version once a file's entry has dropped —
// a file holding only filled content answers its bucket's version. In a
// shared bucket that version can stand above the file's own last change,
// and the model's comparison then requires the exact change version where
// the index owes only the bucket's. Whether the index or the model is right
// is an open question for the RFC; keep the files in distinct buckets until
// it is settled, and remove the constraint with the answer.
