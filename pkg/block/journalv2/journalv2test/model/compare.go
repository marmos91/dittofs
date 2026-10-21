package model

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// maxObserved bounds the bytes Compare accepts the journal naming for one
// file in one answer, so a journal reporting an absurd extent fails the
// comparison instead of exhausting memory.
const maxObserved = 1 << 26

type heldByte struct {
	b   byte
	ver version
}

type indexByte struct {
	ver       version
	offloaded bool
}

type sinceByte struct {
	ver   version
	mtime int64
}

// markerKey is a removal marker as compared: its kind, where the source says
// it, its extent, and its version.
type markerKey struct {
	kind journalv2.ChangeKind
	ext  extent
	ver  version
}

// normalMarker returns a marker's key with its extent in one form.
//
// decision: a marker that runs to the end of the file is compared with
// Len = MaxInt64 - Off, and a source writing it with Len 0, or with any length
// reaching past the largest offset, is read the same way, since the handle's
// contract does not fix how such a length is written.
// A delete always covers the whole file, so its extent is not compared at all.
// Tighten this once the journal's Since documents one form.
func normalMarker(kind journalv2.ChangeKind, e extent, v version) markerKey {
	switch {
	case kind == journalv2.ChangeDelete:
		e = extent{Off: 0, Len: toEnd(0)}
	case e.Len == 0 || e.Len >= toEnd(e.Off):
		e.Len = toEnd(e.Off)
	}
	return markerKey{kind, e, v}
}

// observation is what the journal shows of one file, read once and compared
// with every state the model allows.
type observation struct {
	held    map[int64]heldByte // held bytes inside the read windows
	missing map[int64]bool     // missing bytes inside the read windows
	extents map[int64]bool     // bytes Extents names
	asOf    version
	writes  map[int64]sinceByte // bytes Since names as held
	markers []markerKey         // markers Since yields
	bits    map[int64]indexByte // the index's held bytes
	idxMark []markerKey         // the index's markers, with no kind
}

// Compare reads the journal through v, the view of share tag, and returns the
// first way it differs from the model. It compares every file of the share:
// the bytes held and their versions, the missing extents, exactly; the change
// version a read reports; what Since yields; the offloaded bit and markers of
// the index; and which files the share lists and which it lists as dirty.
//
// Answers are compared byte by byte, never extent by extent, because where the
// journal splits held content depends on its records, which the caller cannot
// see.
//
// After a crash, Compare looks for a state the crash allows that the journal
// shows, file by file, and continues from it: the model then holds what the
// journal holds. When none matches, it reports how the state in which every
// uncertain record survived differs.
func (m *Model) Compare(tag shareTag, v View) error {
	const op = "compare"
	if m.crashed {
		return fmt.Errorf("model: compare: called between Crash and Reopen")
	}
	ids := slices.SortedFunc(maps.Keys(m.files), func(a, b fileID) int {
		x, y := a.Bytes(), b.Bytes()
		return slices.Compare(x[:], y[:])
	})
	for _, id := range ids {
		if f := m.files[id]; f.tag == tag {
			if err := m.compareFile(f, v); err != nil {
				return err
			}
		}
	}

	wantFiles, wantDirty := map[fileID]bool{}, map[fileID]bool{}
	for _, id := range ids {
		f := m.files[id]
		if f.tag != tag {
			continue
		}
		st := derive(f, all)
		if len(st.held) > 0 || len(st.markers) > 0 {
			wantFiles[id] = true
		}
		for off := range st.held {
			if !st.bit[off] {
				wantDirty[id] = true
				break
			}
		}
	}
	gotFiles, gotDirty := map[fileID]bool{}, map[fileID]bool{}
	for id := range v.Files() {
		gotFiles[id] = true
	}
	for id := range v.DirtyFiles() {
		gotDirty[id] = true
	}
	if d := setDiff(wantFiles, gotFiles); d != "" {
		return fmt.Errorf("model: %s share %d: Files %s", op, tag, d)
	}
	if d := setDiff(wantDirty, gotDirty); d != "" {
		return fmt.Errorf("model: %s share %d: DirtyFiles %s", op, tag, d)
	}
	return nil
}

func setDiff(want, got map[fileID]bool) string {
	for id := range want {
		if !got[id] {
			return fmt.Sprintf("omits file %x", id.Bytes())
		}
	}
	for id := range got {
		if !want[id] {
			return fmt.Sprintf("lists file %x the model does not", id.Bytes())
		}
	}
	return ""
}

func (m *Model) compareFile(f *file, v View) error {
	const op = "compare"
	obs, err := observe(f, v)
	if err != nil {
		return m.mismatch(op, f.id, "%v", err)
	}
	ceiling := maxVersion(m.maxSeen, m.floor)
	if !f.unresolved {
		if d := differ(obs, derive(f, all), ceiling); d != "" {
			return m.mismatch(op, f.id, "%s", d)
		}
		return nil
	}
	orig, matched := f.recs, false
	defer func() {
		if !matched {
			f.recs = orig // a failed search changes nothing
		}
	}()
	fixed := decide(f, obs)
	var items []*record
	for _, r := range f.items() {
		if _, ok := fixed[r]; !ok {
			items = append(items, r)
		}
	}
	if len(items) > maxUncertain {
		return m.mismatch(op, f.id, "%d uncertain records left undecided, above the %d the model can search", len(items), maxUncertain)
	}
	all := uint32(1)<<len(items) - 1
	first := ""
	for mask := all; ; mask-- {
		c := candidate{fixed, items, mask}
		d := m.issuedAbove(f, c)
		if d == "" {
			d = differ(obs, c.derive(f), ceiling)
		}
		if d == "" {
			c.adopt(f)
			matched = true
			return nil
		}
		if mask == all {
			first = d
		}
		if mask == 0 {
			break
		}
	}
	return m.mismatch(op, f.id, "no state the crash allows matches; with every undecided record kept: %s", first)
}

// issuedAbove returns how candidate c keeps a record the journal cannot have
// kept, or "" when it keeps none: every version issued after a crash exceeds
// every version on disk, so a record at or above the first of them did not
// survive that crash.
func (m *Model) issuedAbove(f *file, c candidate) string {
	for _, r := range f.recs {
		if r.uncertain && c.took(r) && r.bound != (version{}) && r.ver.Compare(r.bound) >= 0 {
			return fmt.Sprintf("keeps a %s at %v, but the journal issued %v after the crash", r.kind, r.ver, r.bound)
		}
	}
	return ""
}

// observe reads file f through v. It reads the journal around every extent any
// record of f ever wrote and every extent the journal says it holds, one byte
// wider on each side, so a held byte, a missing byte and a widened missing
// extent are all seen. It fails on answers that no state could explain.
func observe(f *file, v View) (*observation, error) {
	o := &observation{
		held: map[int64]heldByte{}, missing: map[int64]bool{}, extents: map[int64]bool{},
		writes: map[int64]sinceByte{}, bits: map[int64]indexByte{},
	}

	es, err := v.Extents(f.id)
	if err != nil {
		return nil, fmt.Errorf("Extents: %w", err)
	}
	var windows []extent
	for _, e := range es {
		if e.Len <= 0 || e.Len > maxObserved || len(o.extents)+int(e.Len) > maxObserved {
			return nil, fmt.Errorf("Extents names %+v", e)
		}
		for off := e.Off; off < end(e); off++ {
			o.extents[off] = true
		}
		windows = append(windows, e)
	}
	for _, r := range f.recs {
		if r.data != nil {
			windows = append(windows, r.ext)
		}
	}
	if len(windows) == 0 {
		windows = append(windows, extent{Off: 0, Len: 1})
	}
	for i, w := range windows {
		lo, hi := max(w.Off-1, 0), min(end(w), toEnd(0)-1)+1
		windows[i] = extent{Off: lo, Len: hi - lo}
	}
	windows = merge(windows)

	read := false
	for _, w := range windows {
		p := make([]byte, w.Len)
		_, missing, held, asOf, err := v.ReadAt(f.id, w.Off, p)
		if err != nil {
			return nil, fmt.Errorf("ReadAt %+v: %w", w, err)
		}
		if !read {
			o.asOf, read = asOf, true
		}
		for _, h := range held {
			piece, ok := intersect(h.Extent, w)
			if !ok || piece != h.Extent {
				return nil, fmt.Errorf("ReadAt %+v reports %+v held, outside the read", w, h.Extent)
			}
			for off := piece.Off; off < end(piece); off++ {
				if _, dup := o.held[off]; dup {
					return nil, fmt.Errorf("ReadAt %+v reports byte %d held twice", w, off)
				}
				o.held[off] = heldByte{p[off-w.Off], h.Version}
			}
		}
		for _, e := range missing {
			piece, ok := intersect(e, w)
			if !ok || piece != e {
				return nil, fmt.Errorf("ReadAt %+v reports %+v missing, outside the read", w, e)
			}
			for off := piece.Off; off < end(piece); off++ {
				if _, dup := o.held[off]; dup || o.missing[off] {
					return nil, fmt.Errorf("ReadAt %+v reports byte %d both held and missing, or missing twice", w, off)
				}
				o.missing[off] = true
			}
		}
		for off := w.Off; off < end(w); off++ {
			if _, ok := o.held[off]; !ok && !o.missing[off] {
				return nil, fmt.Errorf("ReadAt %+v reports byte %d neither held nor missing", w, off)
			}
		}
	}

	var last version
	for c, err := range v.Since(f.id, version{}) {
		if err != nil {
			return nil, fmt.Errorf("Since: %w", err)
		}
		if c.Version.Compare(last) < 0 {
			return nil, fmt.Errorf("Since yields %v after %v, out of version order", c.Version, last)
		}
		last = c.Version
		switch c.Kind {
		case journalv2.ChangeWrite:
			if c.Extent.Len <= 0 || c.Extent.Len > maxObserved || len(o.writes)+int(c.Extent.Len) > maxObserved {
				return nil, fmt.Errorf("Since names %+v", c.Extent)
			}
			for off := c.Extent.Off; off < end(c.Extent); off++ {
				if _, dup := o.writes[off]; dup {
					return nil, fmt.Errorf("Since names byte %d held twice", off)
				}
				o.writes[off] = sinceByte{c.Version, c.Mtime}
			}
		case journalv2.ChangeTruncate, journalv2.ChangeDeallocate, journalv2.ChangeDelete:
			o.markers = append(o.markers, normalMarker(c.Kind, c.Extent, c.Version))
		default:
			return nil, fmt.Errorf("Since yields a change of kind %d, which the model never makes", c.Kind)
		}
	}

	entries, err := v.Index(f.id)
	if err != nil {
		return nil, fmt.Errorf("Index: %w", err)
	}
	for _, e := range entries {
		switch e.Kind {
		case journalv2.EntryHeld:
			if e.Extent.Len <= 0 || e.Extent.Len > maxObserved || len(o.bits)+int(e.Extent.Len) > maxObserved {
				return nil, fmt.Errorf("Index names %+v held", e.Extent)
			}
			for off := e.Extent.Off; off < end(e.Extent); off++ {
				if _, dup := o.bits[off]; dup {
					return nil, fmt.Errorf("Index holds byte %d twice", off)
				}
				o.bits[off] = indexByte{e.Version, e.Offloaded}
			}
		case journalv2.EntryMarker:
			k := normalMarker(0, e.Extent, e.Version)
			o.idxMark = append(o.idxMark, k)
		}
	}
	return o, nil
}

// merge returns the extents of es sorted and with overlapping or adjacent ones
// joined.
func merge(es []extent) []extent {
	slices.SortFunc(es, func(a, b extent) int { return cmp.Compare(a.Off, b.Off) })
	var out []extent
	for _, e := range es {
		if n := len(out); n > 0 && e.Off <= end(out[n-1]) {
			hi := max(end(out[n-1]), end(e))
			out[n-1].Len = hi - out[n-1].Off
			continue
		}
		out = append(out, e)
	}
	return out
}

// differ returns the first way the observation differs from st, or "" when it
// does not. ceiling is the highest version issued or the floor, whichever is
// higher: no answer of the journal names a change above it.
func differ(o *observation, st *fileState, ceiling version) string {
	offs := slices.Sorted(maps.Keys(st.held))
	for _, off := range offs {
		c := st.held[off]
		switch got, ok := o.held[off]; {
		case !ok:
			return fmt.Sprintf("byte %d is held at %v, but the journal reports it missing", off, c.rec.ver)
		case got.b != c.b:
			return fmt.Sprintf("byte %d reads %q, not %q", off, got.b, c.b)
		case got.ver != c.rec.ver:
			return fmt.Sprintf("byte %d is served at %v, not %v", off, got.ver, c.rec.ver)
		case !o.extents[off]:
			return fmt.Sprintf("byte %d is held, but Extents omits it", off)
		}
	}
	for _, off := range slices.Sorted(maps.Keys(o.held)) {
		if _, ok := st.held[off]; !ok {
			return fmt.Sprintf("byte %d is served, but the model does not hold it", off)
		}
	}
	for _, off := range slices.Sorted(maps.Keys(o.extents)) {
		if _, ok := st.held[off]; !ok {
			return fmt.Sprintf("Extents names byte %d, which the model does not hold", off)
		}
	}
	// decision: asOf is required between the model's change version and the
	// highest version issued or the floor, not equal to the change version. A
	// file with no index entry answers with its floor bucket, which may be
	// higher, and buckets start at the floor after a reopen. Require equality
	// once the journal documents that a file with an entry answers with that
	// entry's version exactly.
	switch {
	case o.asOf.Compare(st.change) < 0:
		return fmt.Sprintf("a read reports the file changed at %v, older than %v", o.asOf, st.change)
	case o.asOf.Compare(ceiling) > 0:
		return fmt.Sprintf("a read reports the file changed at %v, above every version issued and the floor (%v)", o.asOf, ceiling)
	}

	nWrites := 0
	for _, off := range offs {
		c := st.held[off]
		if c.rec.ver == (version{}) {
			continue
		}
		nWrites++
		want := sinceByte{ver: c.rec.ver}
		if c.rec.kind == kindWrite {
			want.mtime = c.rec.mtime
		}
		if got, ok := o.writes[off]; !ok || got != want {
			return fmt.Sprintf("Since names byte %d as %+v (present %t), not %+v", off, got, ok, want)
		}
	}
	if len(o.writes) != nWrites {
		return fmt.Sprintf("Since names %d held bytes, the model holds %d", len(o.writes), nWrites)
	}

	var want, wantIdx []markerKey
	for _, r := range st.markers {
		want = append(want, normalMarker(changeKind(r.kind), r.ext, r.ver))
		wantIdx = append(wantIdx, normalMarker(0, r.ext, r.ver))
	}
	// A delete's extent is not compared in Since; the index has no kind to
	// tell a delete by, so its marker is compared as one over the whole file.
	for i := range wantIdx {
		if want[i].kind == journalv2.ChangeDelete {
			wantIdx[i].ext = extent{Off: 0, Len: toEnd(0)}
		}
	}
	if d := markersDiffer("Since", o.markers, want); d != "" {
		return d
	}
	if d := markersDiffer("the index", o.idxMark, wantIdx); d != "" {
		return d
	}

	for _, off := range offs {
		got, ok := o.bits[off]
		switch {
		case !ok:
			return fmt.Sprintf("byte %d is held, but the index has no held entry for it", off)
		case got.ver != st.held[off].rec.ver:
			return fmt.Sprintf("byte %d is held at %v in the index, not %v", off, got.ver, st.held[off].rec.ver)
		case got.offloaded != st.bit[off]:
			return fmt.Sprintf("byte %d has offloaded bit %t in the index, not %t", off, got.offloaded, st.bit[off])
		}
	}
	if len(o.bits) != len(st.held) {
		return fmt.Sprintf("the index holds %d bytes, the model %d", len(o.bits), len(st.held))
	}
	return ""
}

func markersDiffer(source string, got, want []markerKey) string {
	order := func(a, b markerKey) int {
		if c := a.ver.Compare(b.ver); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ext.Off, b.ext.Off); c != 0 {
			return c
		}
		return cmp.Compare(a.kind, b.kind)
	}
	got, want = slices.SortedFunc(slices.Values(got), order), slices.SortedFunc(slices.Values(want), order)
	if !slices.Equal(got, want) {
		return fmt.Sprintf("%s has markers %+v, the model %+v", source, got, want)
	}
	return ""
}
