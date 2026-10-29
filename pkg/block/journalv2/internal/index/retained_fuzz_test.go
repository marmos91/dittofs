package index

import (
	"cmp"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// rbyte is one byte of retained content: where it is, the record and payload
// offset it comes from, its version and bit, and the version that superseded
// it. A file's retained content is a multiset of them, which is what the set
// must keep whatever pieces it cuts it into.
type rbyte struct {
	off       int64
	record    RecordAt
	at        uint32
	version   core.Version
	offloaded bool
	by        core.Version
}

func compareRbyte(a, b rbyte) int {
	return cmp.Or(
		cmp.Compare(a.off, b.off),
		a.version.Compare(b.version),
		a.by.Compare(b.by),
		cmp.Compare(a.record.Segment, b.record.Segment),
		cmp.Compare(a.record.Offset, b.record.Offset),
		cmp.Compare(a.at, b.at),
		cmp.Compare(btoi(a.offloaded), btoi(b.offloaded)),
	)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rbytes expands retained pieces into their bytes, sorted.
func rbytes(rs []Retained) []rbyte {
	var out []rbyte
	for _, r := range rs {
		for i := range r.Extent.Len {
			out = append(out, rbyte{r.Extent.Off + i, r.Record, r.Skip + uint32(i), r.Version, r.Offloaded, r.By})
		}
	}
	slices.SortFunc(out, compareRbyte)
	return out
}

// retainedOps drives a file and a per-byte oracle with the same operations,
// decoded from data, and compares them after each. Writes that retain join
// one of two open windows, each of which either syncs, dropping what its
// records retained, or fails as a unit, handed over shuffled. Plain writes,
// fills, marks, releases, partial drops and repoints land between them, so a
// failed record is often partly superseded by a surviving write, in the other
// window or none, or partly released. A window of retaining writes alone must
// fail back to exactly the held set before it.
func retainedOps(t *testing.T, data []byte) {
	var f file
	var o byteOracle
	var kept []rbyte
	// The file's sets feed the per-segment table, so every step can compare
	// the counters against the pieces both sides hold.
	f = *newFile(new(segments))
	next, version := uint32(0), uint64(100)
	fresh := func() RecordAt { next++; return rec(core.SegmentID(next), next%4*1024) }
	take := func() byte {
		if len(data) == 0 {
			return 0
		}
		b := data[0]
		data = data[1:]
		return b
	}
	span := func() core.Extent {
		b := take()
		off := int64(b>>1) % oracleSize
		n := int64(take()) % (oracleSize - off + 1)
		if b&1 == 1 {
			off &^= 3
			n = min(n&^3, oracleSize-off)
		}
		return ext(off, n)
	}
	inside := func(off int64, e core.Extent) bool { return off >= e.Off && off < e.Off+e.Len }
	// Two windows are open at once, as a failed segment's window and the next
	// segment's are, so a retaining write can survive one window's failure and
	// then sync or fail in its own.
	var windows [2][]WindowRecord
	var before []Piece
	pure, pureWin := false, 0
	rng := rand.New(rand.NewPCG(uint64(len(data)), 7))

	for step := 0; len(data) > 0; step++ {
		op, e := take()%10, span()
		// Only retaining writes to one window and reads keep that window
		// pure; failing it is the op that checks it.
		if op != 1 && op != 6 && op != 9 && len(windows[0])+len(windows[1]) > 0 {
			pure = false
		}
		switch op {
		case 0, 1: // write; a retaining write joins one of the windows
			version++
			p := Placed{Extent: e, Record: fresh(), Skip: uint32(take()), Version: ver(version)}
			if op == 1 && e.Len > 0 {
				w := int(take() % 2)
				if len(windows[0])+len(windows[1]) == 0 {
					before, pure, pureWin = f.held.pieces(nil), true, w
				} else if w != pureWin {
					pure = false
				}
				windows[w] = append(windows[w], WindowRecord{Record: p.Record, Version: p.Version})
				for i := e.Off; i < e.Off+e.Len; i++ {
					if b := o[i]; b.held {
						kept = append(kept, rbyte{i, b.record, b.at, b.version, b.offloaded, p.Version})
					}
				}
				f.writeRetain(p)
			} else {
				f.write(p)
			}
			o.put(Piece{Extent: p.Extent, Record: p.Record, Skip: p.Skip, Version: p.Version})
		case 2: // fill
			v := ver(uint64(take()%4 + 1))
			p := Placed{Extent: e, Record: fresh(), Skip: uint32(take()), Version: v}
			f.held.fill(p)
			for i := range e.Len {
				if !o[e.Off+i].held {
					o.put(Piece{Extent: ext(e.Off+i, 1), Record: p.Record, Skip: p.Skip + uint32(i), Version: v, Offloaded: true})
				}
			}
		case 3: // mark every version inside e
			f.held.markOffloaded(e, ver(0), ver(version))
			for i := e.Off; i < e.Off+e.Len; i++ {
				o[i].offloaded = o[i].offloaded || o[i].held
			}
		case 4: // release
			all := true
			for i := e.Off; i < e.Off+e.Len; i++ {
				all = all && (!o[i].held || o[i].offloaded)
			}
			_, err := f.held.release(e, nil)
			if all != (err == nil) || err != nil && !errors.Is(err, ErrNotOffloaded) {
				t.Fatalf("step %d: release(%+v) = %v, oracle all offloaded %v", step, e, err, all)
			}
			if all {
				for i := range e.Len {
					o[e.Off+i].held = false
				}
			}
		case 5: // a window syncs: what its records retained leaves
			wi := int(take() % 2)
			window := windows[wi]
			for _, w := range window {
				f.retained.drop(whole, bySuperseder(w.Version), nil)
				kept = slices.DeleteFunc(kept, func(b rbyte) bool { return b.by == w.Version })
			}
			if len(window) > 0 {
				windows[wi], pure = window[:0], false
			}
		case 6: // a window fails, handed over in any order
			wi := int(take() % 2)
			window := windows[wi]
			win := slices.Clone(window)
			rng.Shuffle(len(win), func(i, j int) { win[i], win[j] = win[j], win[i] })
			var bys []core.Version
			for _, r := range f.retained.list(nil) {
				bys = append(bys, r.By)
			}
			got, covers := f.failWindow(win, nil)
			inWindow := func(v core.Version) bool {
				return slices.ContainsFunc(window, func(w WindowRecord) bool { return w.Version == v })
			}
			if !slices.IsSortedFunc(covers, core.Version.Compare) || len(slices.CompactFunc(slices.Clone(covers), func(a, b core.Version) bool { return a == b })) != len(covers) || slices.ContainsFunc(covers, inWindow) {
				t.Fatalf("step %d: covering versions %+v are not sorted, distinct and outside the window %+v", step, covers, window)
			}
			// A superseder the call handed out, new to the retained set and
			// not the window's own, is one the caller must be told about.
			for _, r := range f.retained.list(nil) {
				if !slices.Contains(bys, r.By) && !inWindow(r.By) && !slices.Contains(covers, r.By) {
					t.Fatalf("step %d: %+v was handed to %v, which failWindow did not name in %+v", step, r, r.By, covers)
				}
			}
			var want []rbyte
			order := slices.Clone(window)
			slices.SortFunc(order, func(a, b WindowRecord) int { return b.Version.Compare(a.Version) })
			for _, w := range order {
				var was [oracleSize]bool
				for i := range o {
					if o[i].held && o[i].record == w.Record {
						was[i], o[i].held = true, false
					}
				}
				kept = slices.DeleteFunc(kept, func(b rbyte) bool { return b.record == w.Record })
				// Where the record was held, the newest byte it retained is
				// held again, and any older one it retained there is now
				// superseded by that one.
				var newest [oracleSize]int
				for i := range newest {
					newest[i] = -1
				}
				for j, b := range kept {
					if b.by == w.Version && was[b.off] && (newest[b.off] < 0 || b.version.Compare(kept[newest[b.off]].version) > 0) {
						newest[b.off] = j
					}
				}
				for i, j := range newest {
					if j >= 0 {
						b := kept[j]
						o.put(Piece{Extent: ext(int64(i), 1), Record: b.record, Skip: b.at, Version: b.version, Offloaded: b.offloaded})
					}
				}
				for j := range kept {
					if b := &kept[j]; b.by == w.Version && was[b.off] && newest[b.off] != j {
						b.by = kept[newest[b.off]].version
					}
				}
				kept = slices.DeleteFunc(kept, func(b rbyte) bool { return b.by == w.Version && was[b.off] })
			}
			// Once the whole window has failed, what it superseded and could
			// not hold again is the newer content's over it, or, under no
			// newer content, reported.
			for j := range kept {
				b := &kept[j]
				if !slices.ContainsFunc(window, func(w WindowRecord) bool { return w.Version == b.by }) {
					continue
				}
				if h := o[b.off]; h.held && h.version.Compare(b.version) > 0 {
					b.by = h.version
				} else {
					want = append(want, *b)
				}
			}
			slices.SortFunc(want, compareRbyte)
			if g := rbytes(got); !slices.Equal(g, want) {
				t.Fatalf("step %d: failWindow returned\n got  %+v\n want %+v", step, g, want)
			}
			if pure && wi == pureWin && len(window) > 0 {
				if got := f.held.pieces(nil); !slices.Equal(got, before) {
					t.Fatalf("step %d: a failed window did not restore the held set:\n got  %+v\n want %+v", step, got, before)
				}
			}
			if len(window) > 0 {
				windows[wi], pure = window[:0], false
			}
		case 7: // an offload commit drops what is retained inside e
			f.retained.drop(e, func(Retained) bool { return true }, nil)
			kept = slices.DeleteFunc(kept, func(b rbyte) bool { return inside(b.off, e) })
		case 8: // repack carries forward the record retained at e's first byte
			i := slices.IndexFunc(kept, func(b rbyte) bool { return inside(b.off, e) })
			if i < 0 {
				break
			}
			from, to, skip := kept[i].record, fresh(), uint32(take())
			f.retained.repoint(e, from, to, skip)
			for j := range kept {
				if b := &kept[j]; b.record == from && inside(b.off, e) {
					b.record, b.at = to, skip+uint32(b.off-e.Off)
				}
			}
		case 9: // read
			gotHeld, gotMissing := f.held.span(e, nil, nil)
			wantHeld, wantMissing := o.span(e)
			if !slices.Equal(gotHeld, wantHeld) || !slices.Equal(gotMissing, wantMissing) {
				t.Fatalf("step %d: span(%+v):\n got  %+v %+v\n want %+v %+v", step, e, gotHeld, gotMissing, wantHeld, wantMissing)
			}
		}
		if got, want := f.held.pieces(nil), o.pieces(); !slices.Equal(got, want) {
			t.Fatalf("step %d (op %d over %+v): held\n got  %+v\n want %+v", step, op, e, got, want)
		}
		list := f.retained.list(nil)
		if !slices.IsSortedFunc(list, compareRetained) || slices.ContainsFunc(list, func(r Retained) bool { return r.Extent.Len <= 0 }) {
			t.Fatalf("step %d: retained pieces out of order or empty: %+v", step, list)
		}
		slices.SortFunc(kept, compareRbyte)
		if got := rbytes(list); !slices.Equal(got, kept) {
			t.Fatalf("step %d (op %d over %+v): retained\n got  %+v\n want %+v", step, op, e, got, kept)
		}
		// The per-segment counters must equal a walk of both sets: retained
		// bytes count toward the record's own segment, so a supersede counted
		// against the superseder's segment, or a re-hold that kept them
		// retained, shows here.
		walk := map[core.SegmentID]SegmentBytes{}
		walkFile(&f, walk)
		for seg := core.SegmentID(1); seg <= core.SegmentID(next); seg++ {
			if b := f.held.acct.bytes(seg); b != walk[seg] {
				t.Fatalf("step %d (op %d over %+v): segment %d counts %+v, walk says %+v", step, op, e, seg, b, walk[seg])
			}
		}
	}
}

// TestRetainedMatchesByteOracle runs long random operation sequences against
// the per-byte oracle. It catches a retained piece lost or kept twice by a
// split, a read served from retained content, a failed window that holds back
// the wrong version or holds older content over a surviving write, and a
// retained piece of a failed record left behind.
func TestRetainedMatchesByteOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 6))
	for range 200 {
		data := make([]byte, 2000)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		retainedOps(t, data)
	}
}

func FuzzRetained(f *testing.F) {
	f.Add([]byte{0, 0, 40, 0, 1, 4, 20, 0, 1, 10, 30, 0, 6, 0, 0, 9, 0, 96})
	f.Add([]byte{1, 0, 40, 0, 1, 8, 16, 0, 0, 12, 4, 0, 6, 0, 0, 1, 0, 20, 0, 5, 0, 0})
	f.Fuzz(retainedOps)
}
