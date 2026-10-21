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

// String prints the marker as the caller names it: truncate [5,EOF) @v2.
func (k markerKey) String() string {
	name := map[journalv2.ChangeKind]string{
		journalv2.ChangeTruncate:   "truncate",
		journalv2.ChangeDeallocate: "deallocate",
		journalv2.ChangeDelete:     "delete",
	}[k.kind]
	if name == "" {
		name = "marker"
	}
	return fmt.Sprintf("%s %s @%s", name, fmtExtent(k.ext), fmtVersion(k.ver))
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
	since   []sinceView         // what Since yields, from each version read
	bits    map[int64]indexByte // the index's held bytes
	idxMark []markerKey         // the index's markers, with no kind
}

// sinceView is what Since(id, from) yielded.
type sinceView struct {
	from    version
	writes  map[int64]sinceByte // bytes named as held
	markers []markerKey
}

// sinceFroms returns the versions Compare reads Since from for f: the start,
// and each version a record of f carries, so that a journal yielding a change
// at from, or ignoring from, is seen.
//
// ponytail: Since is read once per version, at most 8 of them spread over the
// file's versions, each read costing a walk of the file. Read every version if
// a journal bug in the from filter ever slips between the sampled ones.
func sinceFroms(f *file) []version {
	var vs []version
	for _, r := range f.recs {
		vs = append(vs, r.ver)
	}
	slices.SortFunc(vs, version.Compare)
	vs = slices.Compact(vs)
	const most = 8
	if len(vs) > most {
		picked := make([]version, 0, most)
		for i := range most {
			picked = append(picked, vs[i*(len(vs)-1)/(most-1)])
		}
		vs = slices.Compact(picked)
	}
	return append([]version{{}}, vs...)
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
// record the crash left in doubt was kept differs, or why that state cannot be.
func (m *Model) Compare(tag shareTag, v View) error {
	const op = "compare"
	if m.crashed {
		return m.misuse(op, fileID{}, "called between Crash and Reopen")
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
		return m.mismatch(op, fileID{}, "share %d: Files %s", tag, d)
	}
	if d := setDiff(wantDirty, gotDirty); d != "" {
		return m.mismatch(op, fileID{}, "share %d: DirtyFiles %s", tag, d)
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
	obs, err := observe(f, v, sinceFroms(f))
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
		return m.misuse(op, f.id, "%d records the crash left in doubt that the journal's bytes cannot settle, above the %d the model can search; sync the file more often", len(items), maxUncertain)
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
	return m.mismatch(op, f.id, "no state the crash allows matches what the journal shows; with every record in doubt kept: %s", first)
}

// issuedAbove returns how candidate c keeps a record the journal cannot have
// kept, or "" when it keeps none: every version issued after a crash exceeds
// every version on disk, so a record at or above the first of them did not
// survive that crash.
func (m *Model) issuedAbove(f *file, c candidate) string {
	for _, r := range f.recs {
		if r.uncertain && c.took(r) && r.bound != (version{}) && r.ver.Compare(r.bound) >= 0 {
			return fmt.Sprintf("keeps a %s at %s, but the journal issued %s after the crash, and every version issued exceeds every version on disk", r.kind, fmtVersion(r.ver), fmtVersion(r.bound))
		}
	}
	return ""
}

// observe reads file f through v. It reads the journal around every extent any
// record of f ever wrote and every extent the journal says it holds, one byte
// wider on each side, so a held byte, a missing byte and a widened missing
// extent are all seen. It fails on answers that no state could explain.
func observe(f *file, v View, froms []version) (*observation, error) {
	o := &observation{
		held: map[int64]heldByte{}, missing: map[int64]bool{}, extents: map[int64]bool{},
		bits: map[int64]indexByte{},
	}

	es, err := v.Extents(f.id)
	if err != nil {
		return nil, fmt.Errorf("Extents: %w", err)
	}
	var windows []extent
	for _, e := range es {
		if e.Len <= 0 || e.Len > maxObserved || len(o.extents)+int(e.Len) > maxObserved {
			return nil, fmt.Errorf("Extents names %s", fmtExtent(e))
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
			return nil, fmt.Errorf("ReadAt %s: %w", fmtExtent(w), err)
		}
		if !read {
			o.asOf, read = asOf, true
		}
		for _, h := range held {
			piece, ok := intersect(h.Extent, w)
			if !ok || piece != h.Extent {
				return nil, fmt.Errorf("ReadAt %s reports %s held, outside the read", fmtExtent(w), fmtExtent(h.Extent))
			}
			for off := piece.Off; off < end(piece); off++ {
				if _, dup := o.held[off]; dup {
					return nil, fmt.Errorf("ReadAt %s reports byte %d held twice", fmtExtent(w), off)
				}
				o.held[off] = heldByte{p[off-w.Off], h.Version}
			}
		}
		for _, e := range missing {
			piece, ok := intersect(e, w)
			if !ok || piece != e {
				return nil, fmt.Errorf("ReadAt %s reports %s missing, outside the read", fmtExtent(w), fmtExtent(e))
			}
			for off := piece.Off; off < end(piece); off++ {
				if _, dup := o.held[off]; dup || o.missing[off] {
					return nil, fmt.Errorf("ReadAt %s reports byte %d both held and missing, or missing twice", fmtExtent(w), off)
				}
				o.missing[off] = true
			}
		}
		for off := w.Off; off < end(w); off++ {
			if _, ok := o.held[off]; !ok && !o.missing[off] {
				return nil, fmt.Errorf("ReadAt %s reports byte %d neither held nor missing: missing must be exact", fmtExtent(w), off)
			}
		}
	}

	for _, from := range froms {
		sv, err := readSince(f.id, v, from)
		if err != nil {
			return nil, err
		}
		o.since = append(o.since, sv)
	}

	entries, err := v.Index(f.id)
	if err != nil {
		return nil, fmt.Errorf("Index: %w", err)
	}
	for _, e := range entries {
		switch e.Kind {
		case journalv2.EntryHeld:
			if e.Extent.Len <= 0 || e.Extent.Len > maxObserved || len(o.bits)+int(e.Extent.Len) > maxObserved {
				return nil, fmt.Errorf("Index names %s held", fmtExtent(e.Extent))
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

// readSince reads Since(id, from) through v.
func readSince(id fileID, v View, from version) (sinceView, error) {
	sv := sinceView{from: from, writes: map[int64]sinceByte{}}
	var last version
	for c, err := range v.Since(id, from) {
		if err != nil {
			return sv, fmt.Errorf("Since from %s: %w", fmtVersion(from), err)
		}
		if c.Version.Compare(last) < 0 {
			return sv, fmt.Errorf("Since from %s yields %s after %s, out of version order", fmtVersion(from), fmtVersion(c.Version), fmtVersion(last))
		}
		last = c.Version
		switch c.Kind {
		case journalv2.ChangeWrite:
			if c.Extent.Len <= 0 || c.Extent.Len > maxObserved || len(sv.writes)+int(c.Extent.Len) > maxObserved {
				return sv, fmt.Errorf("Since from %s names %s", fmtVersion(from), fmtExtent(c.Extent))
			}
			for off := c.Extent.Off; off < end(c.Extent); off++ {
				if _, dup := sv.writes[off]; dup {
					return sv, fmt.Errorf("Since from %s names byte %d held twice", fmtVersion(from), off)
				}
				sv.writes[off] = sinceByte{c.Version, c.Mtime}
			}
		case journalv2.ChangeTruncate, journalv2.ChangeDeallocate, journalv2.ChangeDelete:
			sv.markers = append(sv.markers, normalMarker(c.Kind, c.Extent, c.Version))
		default:
			return sv, fmt.Errorf("Since from %s yields a change of kind %d, which the model never makes", fmtVersion(from), c.Kind)
		}
	}
	return sv, nil
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
// higher: no answer of the journal names a change above it. Each difference
// names the bytes, as the run of them that differ the same way, and the rule
// the journal broke where one is.
func differ(o *observation, st *fileState, ceiling version) string {
	offs := slices.Sorted(maps.Keys(st.held))
	if d := firstRun(offs, func(off int64) string {
		c := st.held[off]
		switch got, ok := o.held[off]; {
		case !ok && c.rec.durable && c.rec.kind == kindWrite:
			return fmt.Sprintf("held by %s, but the journal reports them missing: a synced write was lost", heldBy(c))
		case !ok:
			return fmt.Sprintf("held by %s, but the journal reports them missing", heldBy(c))
		case got.ver != c.rec.ver:
			return fmt.Sprintf("held by %s, but the journal serves them at %s", heldBy(c), fmtVersion(got.ver))
		case got.b != c.b:
			return fmt.Sprintf("held by %s, but the journal serves other bytes there", heldBy(c))
		case !o.extents[off]:
			return "held, but Extents omits them"
		}
		return ""
	}); d != "" {
		return d
	}
	if d := firstRun(slices.Sorted(maps.Keys(o.held)), func(off int64) string {
		if _, ok := st.held[off]; !ok {
			return fmt.Sprintf("served at %s, but the model holds nothing there: removed, released or never written", fmtVersion(o.held[off].ver))
		}
		return ""
	}); d != "" {
		return d
	}
	if d := firstRun(slices.Sorted(maps.Keys(o.extents)), func(off int64) string {
		if _, ok := st.held[off]; !ok {
			return "named by Extents, but the model holds nothing there"
		}
		return ""
	}); d != "" {
		return d
	}

	// decision: asOf is required between the model's change version and the
	// highest version issued or the floor, not equal to the change version. A
	// file with no index entry answers with its floor bucket, which may be
	// higher, and buckets start at the floor after a reopen. Require equality
	// once the journal documents that a file with an entry answers with that
	// entry's version exactly.
	switch {
	case o.asOf.Compare(st.change) < 0:
		return fmt.Sprintf("a read reports the file changed at %s, older than %s, its newest change: a stale fill would be accepted", fmtVersion(o.asOf), fmtVersion(st.change))
	case o.asOf.Compare(ceiling) > 0:
		return fmt.Sprintf("a read reports the file changed at %s, above every version issued and the floor (%s)", fmtVersion(o.asOf), fmtVersion(ceiling))
	}

	for _, sv := range o.since {
		if d := sinceDiffers(sv, st, offs); d != "" {
			return d
		}
	}

	var wantIdx []markerKey
	for _, r := range st.markers {
		k := normalMarker(0, r.ext, r.ver)
		if r.kind == kindDelete {
			// The index has no kind to tell a delete by, so its marker is
			// compared as one over the whole file.
			k.ext = extent{Off: 0, Len: toEnd(0)}
		}
		wantIdx = append(wantIdx, k)
	}
	if d := markersDiffer("the index", o.idxMark, wantIdx); d != "" {
		return d
	}

	if d := firstRun(offs, func(off int64) string {
		got, ok := o.bits[off]
		want := st.held[off]
		switch {
		case !ok:
			return "held, but the index has no held entry for them"
		case got.ver != want.rec.ver:
			return fmt.Sprintf("held at %s, but the index names %s", fmtVersion(want.rec.ver), fmtVersion(got.ver))
		case got.offloaded && !st.bit[off]:
			return fmt.Sprintf("dirty at %s, but the index marks them offloaded: a release would evict the only copy", fmtVersion(want.rec.ver))
		case !got.offloaded && st.bit[off]:
			return fmt.Sprintf("offloaded at %s, but the index marks them dirty", fmtVersion(want.rec.ver))
		}
		return ""
	}); d != "" {
		return d
	}
	return firstRun(slices.Sorted(maps.Keys(o.bits)), func(off int64) string {
		if _, ok := st.held[off]; !ok {
			return "named held by the index, but the model holds nothing there"
		}
		return ""
	})
}

// sinceDiffers returns how what Since yielded from sv.from differs from st:
// every held byte above from as a write, with its version and mtime, and every
// live marker above from.
func sinceDiffers(sv sinceView, st *fileState, offs []int64) string {
	from := fmtVersion(sv.from)
	want := func(off int64) (sinceByte, bool) {
		c := st.held[off]
		if c.rec.ver.Compare(sv.from) <= 0 {
			return sinceByte{}, false
		}
		w := sinceByte{ver: c.rec.ver}
		if c.rec.kind == kindWrite {
			w.mtime = c.rec.mtime
		}
		return w, true
	}
	if d := firstRun(offs, func(off int64) string {
		w, ok := want(off)
		got, named := sv.writes[off]
		switch {
		case ok && !named:
			return fmt.Sprintf("held at %s, but Since from %s omits them", fmtVersion(w.ver), from)
		case !ok && named:
			return fmt.Sprintf("held at or below %s, but Since from %s names them at %s", from, from, fmtVersion(got.ver))
		case ok && got != w:
			return fmt.Sprintf("written at %s with mtime %d, but Since from %s names them at %s with mtime %d", fmtVersion(w.ver), w.mtime, from, fmtVersion(got.ver), got.mtime)
		}
		return ""
	}); d != "" {
		return d
	}
	if d := firstRun(slices.Sorted(maps.Keys(sv.writes)), func(off int64) string {
		if _, ok := st.held[off]; !ok {
			return fmt.Sprintf("named held by Since from %s, but the model holds nothing there", from)
		}
		return ""
	}); d != "" {
		return d
	}

	var markers []markerKey
	for _, r := range st.markers {
		if r.ver.Compare(sv.from) > 0 {
			markers = append(markers, normalMarker(changeKind(r.kind), r.ext, r.ver))
		}
	}
	return markersDiffer("Since from "+from, sv.markers, markers)
}

// firstRun returns the first of offs for which problem says something, with
// the run of contiguous offsets after it for which it says the same, as
// "bytes [a,b): problem"; "" when it says nothing for any.
func firstRun(offs []int64, problem func(int64) string) string {
	for i, off := range offs {
		p := problem(off)
		if p == "" {
			continue
		}
		e := extent{Off: off, Len: 1}
		for j := i + 1; j < len(offs) && offs[j] == end(e) && problem(offs[j]) == p; j++ {
			e.Len++
		}
		return fmt.Sprintf("bytes %s: %s", fmtExtent(e), p)
	}
	return ""
}

// heldBy names the record a held byte is read from, as the caller made it.
func heldBy(c cell) string {
	state := "an unsynced"
	if c.rec.durable {
		state = "a synced"
	}
	return fmt.Sprintf("%s %s at %s", state, c.rec.kind, fmtVersion(c.rec.ver))
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
		return fmt.Sprintf("%s has markers %v, the model %v", source, got, want)
	}
	return ""
}
