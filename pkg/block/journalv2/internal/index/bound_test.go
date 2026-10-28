package index

import (
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// newest is the highest version, which every call in these checks accepts.
var newest = core.Version{Counter: 1<<63 - 1}

// walkCount is the entry count measured the slow way: the entries of every
// file the map holds, summed. extentCount must match it, so the checks below
// catch a mutation whose delta the count skipped.
func walkCount(fs *files) int64 {
	n := int64(0)
	for _, id := range walkIDs(fs) {
		fs.view(id, func(f *file) { n += f.entries() })
	}
	return n
}

// walkIDs returns every FileID with an entry, which the count checks need
// because the map gives its keys up only shard by shard.
func walkIDs(fs *files) []core.FileID {
	var ids []core.FileID
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		for id := range s.m {
			ids = append(ids, id)
		}
		s.mu.Unlock()
	}
	return ids
}

// assertIndexRefusal checks a refusal naming the index: ErrNoSpace, and its
// Limit and Unit. A refusal by another limit, or a plain error, would send
// the caller's capacity handling the wrong way.
func assertIndexRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	var e *core.NoSpaceError
	if !errors.As(err, &e) || e.Limit != core.LimitIndex || e.Unit != core.UnitEntries {
		t.Fatalf("err = %v, want a NoSpaceError naming the index in entries", err)
	}
}

// headroom is an unset limit: no admission is refused.
const headroom int64 = 1 << 30

// fillExtent is a small write that fills no held piece, for padding a count
// one entry a call at offsets that never meet.
func fillExtent(off int64) core.Extent { return ext(off*4096, 4096) }

// TestExtentCountCountsHeldRetainedAndMarkers: the count is of held pieces,
// retained extents and removal markers together, so a writeRetain and a
// deallocation raise it even though neither leaves a piece held. One that
// counted held pieces alone would miss two of the three.
func TestExtentCountCountsHeldRetainedAndMarkers(t *testing.T) {
	var fs files
	fs.setExtentLimit(headroom)
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 1))
		return nil
	})
	if c := fs.extentCount(); c != 1 {
		t.Fatalf("after one write, count = %d, want 1", c)
	}
	change(t, &fs, id, func(f *file) error {
		f.writeRetain(write(0, 8, rec(2, 0), 2))
		return nil
	})
	if c := fs.extentCount(); c != 2 { // one held of the new write, one retained
		t.Fatalf("after writeRetain, count = %d, want 2", c)
	}
	if err := fs.update(id, func(f *file) error { return f.deallocate(ext(0, 2), ver(3)) }); err != nil {
		t.Fatalf("deallocate: %v", err)
	}
	// The deallocation narrows the new piece's head away and keeps a
	// marker: the held tail alone, the retained v1 piece and the marker.
	if c := fs.extentCount(); c != 3 {
		t.Fatalf("after deallocate, count = %d, want 3", c)
	}
}

// TestExtentCountMatchesWalk is R20 against every mutation path: random
// operations across files, after each of which the count equals the sum of
// the entries the walk finds. A mutation that skips the delta leaves the
// count ahead or behind the index, and the bound would then refuse the
// wrong calls.
func TestExtentCountMatchesWalk(t *testing.T) {
	var fs files
	fs.setExtentLimit(headroom)
	const nfiles = 8
	var ids [nfiles]core.FileID
	for i := range nfiles {
		ids[i] = fid(byte(i + 1))
	}
	rng := rand.New(rand.NewPCG(1, 2))
	next := func() (RecordAt, uint64) {
		seg := core.SegmentID(1 + rng.IntN(4))
		return rec(seg, uint32(rng.IntN(16))*4), uint64(1 + rng.IntN(16))
	}
	for round := range 4000 {
		id := ids[rng.IntN(nfiles)]
		switch rng.IntN(12) {
		case 0: // write or writeRetain
			r, v := next()
			off := int64(rng.IntN(64)) * 4
			fn := func(f *file) error { f.write(write(off, 4, r, v)); return nil }
			if round%2 == 0 {
				fn = func(f *file) error { f.writeRetain(write(off, 4, r, v)); return nil }
			}
			if err := fs.update(id, fn); err != nil {
				t.Fatal(err)
			}
		case 1: // fill, of a fresh record or one the file holds
			r, v := next()
			off := int64(rng.IntN(64)) * 4
			if err := fs.update(id, func(f *file) error {
				f.fill(Placed{Extent: ext(off, 16), Record: r, Skip: uint32(off), Version: ver(v)})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		case 2: // settle, sometimes before the removals it settles
			fs.settleRemovals(id, ver(uint64(rng.IntN(16))))
		case 3, 4: // deallocate, sometimes of nothing
			err := fs.update(id, func(f *file) error {
				return f.deallocate(ext(int64(rng.IntN(64))*4, int64(rng.IntN(4))), ver(uint64(rng.IntN(16))))
			})
			if err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Fatal(err)
			}
		case 5: // truncate or delete
			if err := fs.update(id, func(f *file) error {
				if rng.IntN(2) == 0 {
					f.truncate(int64(rng.IntN(64))*4, ver(uint64(rng.IntN(16))))
				} else {
					f.delete(ver(uint64(rng.IntN(16)) + 1))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		case 6: // mark, then release what is offloaded
			if err := fs.update(id, func(f *file) error {
				f.held.markOffloaded(ext(0, 512), core.Version{}, newest)
				_, err := f.held.release(ext(0, 512), nil)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		case 7, 8, 9: // hold content under the highest segment, then fail it
			var window []WindowRecord
			fs.view(id, func(f *file) {
				for _, p := range f.held.pieces(nil) {
					window = append(window, WindowRecord{Record: p.Record, Version: p.Version})
				}
			})
			if len(window) > 0 {
				if err := fs.update(id, func(f *file) error {
					f.failWindow(window, nil)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		case 10: // lose a segment, as a recycle: entries can drop
			seg := core.SegmentID(1 + rng.IntN(4))
			if err := fs.update(id, func(f *file) error { f.segmentGone(seg); return nil }); err != nil {
				t.Fatal(err)
			}
		case 11: // claim under a tag, as an append does
			if err := fs.update(id, func(f *file) error { return f.recorded(core.SegmentID(7), core.ShareTag(1), id) }); err != nil {
				t.Fatal(err)
			}
		}
		if c, w := fs.extentCount(), walkCount(&fs); c != w {
			t.Fatalf("round %d: count = %d, walk = %d", round, c, w)
		}
	}
}

// writeAddsMeasured is the delta a write or writeRetain really makes.
func writeAddsMeasured(f *file, p Placed, retain bool) int64 {
	before := f.entries()
	if retain {
		f.writeRetain(p)
	} else {
		f.write(p)
	}
	return f.entries() - before
}

// entriesSnapshot copies every held piece, retained piece and marker of f:
// everything a pre-check could touch, so the pre-check leaving anything
// behind is seen.
func entriesSnapshot(f *file) []Piece {
	ps := f.held.pieces(nil)
	for _, r := range f.retained.list(nil) {
		ps = append(ps, r.Piece)
	}
	for _, m := range f.markers {
		ps = append(ps, Piece{Extent: m.extent, Version: m.version})
	}
	return ps
}

// assertExactPreCheck checks that fn, the pre-check, returns the delta the
// real op makes, want, and changes nothing.
func assertExactPreCheck(t *testing.T, f *file, want int64, fn func(*file) int64) {
	t.Helper()
	before := entriesSnapshot(f)
	if got := fn(f); got != want {
		t.Fatalf("pre-check = %d, want %d", got, want)
	}
	if got := entriesSnapshot(f); !slices.Equal(got, before) {
		t.Fatalf("the pre-check changed the file:\n before %+v\n after  %+v", before, got)
	}
}

// TestWriteAddsIsExact: the pre-check must equal the delta a write or a
// writeRetain makes, on every shape it can meet, and it must change nothing:
// admission runs under the file's read lock, where anything it touched would
// race. A pre-check that under- or over-counted would refuse the wrong
// writes; one that mutated the file it read would corrupt the index.
func TestWriteAddsIsExact(t *testing.T) {
	cases := []struct {
		name   string
		set    func(f *file)
		e      core.Extent
		retain bool
	}{
		{"into a gap", func(f *file) {
			f.write(write(0, 8, rec(1, 0), 1))
			f.write(write(16, 8, rec(1, 64), 2))
		}, ext(8, 8), false},
		{"exactly over one piece", func(f *file) {
			f.write(write(0, 8, rec(1, 0), 1))
		}, ext(0, 8), false},
		{"covering three pieces", func(f *file) {
			for i := range int64(3) {
				f.write(write(i*4, 4, rec(1, uint32(i)*4), uint64(i+1)))
			}
		}, ext(0, 12), false},
		{"a three-way split", func(f *file) {
			f.write(write(0, 12, rec(1, 0), 1))
		}, ext(4, 4), false},
		{"writeRetain over two pieces and a gap", func(f *file) {
			f.write(write(0, 8, rec(1, 0), 1))
			f.write(write(12, 8, rec(1, 64), 2))
		}, ext(4, 12), true},
		{"over one end", func(f *file) {
			f.write(write(0, 8, rec(1, 0), 1))
		}, ext(0, 4), false},
		{"of nothing", func(f *file) {
			f.write(write(0, 8, rec(1, 0), 1))
		}, ext(4, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := write(c.e.Off, c.e.Len, rec(2, 0), 9)
			var probe file
			c.set(&probe)
			want := writeAddsMeasured(&probe, p, c.retain)
			var f file
			c.set(&f)
			assertExactPreCheck(t, &f, want, func(f *file) int64 {
				return f.writeAdds(p.Extent, c.retain)
			})
		})
	}
}

// TestWriteAddsJoinsANeighbour is the one shape the pre-check over-counts by
// design: the second of two placements of one record, contiguous in the file
// and the payload, which a put joins into one entry. Admission hands the
// over-count to commit as the admitted amount, so the committed delta stays
// exact; this check pins that the over-count is what the RFC allows rather
// than hides.
func TestWriteAddsJoinsANeighbour(t *testing.T) {
	b := Placed{Extent: ext(4, 4), Record: rec(1, 0), Skip: 4, Version: ver(1)}
	var f file
	f.write(Placed{Extent: ext(0, 4), Record: rec(1, 0), Version: ver(1)})
	f.write(b)
	if got := f.writeAdds(b.Extent, false); got > 1 {
		t.Fatalf("writeAdds of the joining piece = %d, want at most 1", got)
	}
}

// TestFillAddsIsExact: the pre-check must equal the delta a fill makes, over
// held content and the parts a newer marker masks, and must change nothing.
// Like writeAdds, a fill's pre-check runs under the read lock, so anything
// it touched would race.
func TestFillAddsIsExact(t *testing.T) {
	r := rec(3, 0)
	cases := []struct {
		name string
		set  func(f *file)
		gap  core.Extent
	}{
		{"over nothing", func(f *file) {}, ext(0, 8)},
		{"beside one piece", func(f *file) {
			f.write(write(8, 8, rec(1, 0), 5))
		}, ext(0, 8)},
		{"masked whole by a marker", func(f *file) {
			_ = f.deallocate(ext(0, 8), ver(5))
		}, ext(0, 8)},
		{"half masked by a marker", func(f *file) {
			_ = f.deallocate(ext(0, 4), ver(5))
		}, ext(0, 8)},
		{"across pieces, a gap and a marker", func(f *file) {
			f.write(write(0, 4, rec(1, 0), 5))
			f.write(write(8, 4, rec(1, 64), 5))
			_ = f.deallocate(ext(12, 4), ver(5))
		}, ext(0, 16)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Placed{Extent: c.gap, Record: r, Skip: uint32(c.gap.Off), Version: ver(3)}
			var probe file
			c.set(&probe)
			before := probe.entries()
			probe.fill(p)
			want := probe.entries() - before
			var f file
			c.set(&f)
			assertExactPreCheck(t, &f, want, func(f *file) int64 { return f.fillAdds(p) })
		})
	}
}

// TestSplits agrees with the pieces it names: only an extent with both ends
// inside one piece splits, so an extent covering whole pieces, one end of
// one, or nothing never does. A splits that answered by overlap would
// refuse the removals the RFC lets through.
func TestSplits(t *testing.T) {
	cases := []struct {
		name string
		e    core.Extent
		want bool
	}{
		{"inside one piece", ext(4, 4), true},
		{"across three pieces", ext(0, 12), false},
		{"over one end", ext(0, 4), false},
		{"over nothing", ext(100, 4), false},
		{"of nothing", ext(4, 0), false},
	}
	var f file
	f.write(write(0, 12, rec(1, 0), 1))
	f.write(write(12, 4, rec(1, 48), 1)) // not a join: its skip jumps
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := f.splits(c.e); got != c.want {
				t.Fatalf("splits(%+v) = %v, want %v", c.e, got, c.want)
			}
		})
	}
}

// TestGapsMatchesSpan: gaps is what fillAdds counts with, so it must count
// the same absent sub-extents a fill would hold, on the shapes held content
// leaves: a gap before and after a piece, a span inside one, and an empty
// span.
func TestGapsMatchesSpan(t *testing.T) {
	var s heldSet
	s.insert(write(8, 4, rec(1, 0), 1))
	s.insert(write(16, 4, rec(1, 64), 2))
	s.insert(write(24, 4, rec(1, 128), 3))
	for _, q := range []core.Extent{ext(0, 32), ext(10, 4), ext(0, 8), ext(28, 8), ext(4, 30), ext(16, 0), ext(40, 8)} {
		_, missing := s.span(q, nil, nil)
		if got := s.gaps(q); got != int64(len(missing)) {
			t.Fatalf("gaps(%+v) = %d, want %d", q, got, len(missing))
		}
	}
}

// TestSplitsAndGapsAllocateNothing: splits and gaps answer admission, which
// runs under the file's read lock on a write, so neither may allocate beyond
// the closure it hands visit. One that escaped would allocate per write
// forever.
func TestSplitsAndGapsAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	var s heldSet
	s.tree.cmps = new(atomic.Uint64)
	for i := range 500 {
		s.insert(write(int64(i)*8, 8, rec(core.SegmentID(1+i/60_000), uint32(i)*8), uint64(i+1)))
	}
	var n int64
	allocs := testing.AllocsPerRun(1000, func() { n = s.gaps(ext(1000, 800)) })
	_ = n
	if allocs != 0 {
		t.Fatalf("gaps allocated %v times per call, want 0", allocs)
	}
	var b bool
	allocs = testing.AllocsPerRun(1000, func() { b = s.straddle(ext(1000, 8)) })
	_ = b
	if allocs != 0 {
		t.Fatalf("straddle allocated %v times per call, want 0", allocs)
	}
}

// TestAdmissionAllocatesNothing: admission runs on the path of every content
// write, so its pre-checks under the file's read lock must not allocate
// beyond the closure they hand visit. A closure that escaped would allocate
// once per write, forever.
func TestAdmissionAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	var fs files
	fs.setExtentLimit(headroom)
	id := fid(1)
	for i := range 1010 {
		if err := fs.update(id, func(f *file) error {
			f.write(Placed{Extent: ext(int64(i)*8, 8), Record: rec(core.SegmentID(1+i/60_000), uint32(i)*8), Version: ver(uint64(i + 1))})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var add int64
	p := write(2009, 4, rec(1, 0), 1<<40)
	allocs := testing.AllocsPerRun(1000, func() {
		fs.view(id, func(f *file) { add = f.writeAdds(p.Extent, false) })
	})
	_ = add
	if allocs != 0 {
		t.Fatalf("admitWrite's pre-check allocated %v times per call, want 0", allocs)
	}
	q := Placed{Extent: ext(2009, 800), Record: rec(1, 0), Skip: 0, Version: ver(3)}
	allocs = testing.AllocsPerRun(1000, func() {
		fs.view(id, func(f *file) { add = f.fillAdds(q) })
	})
	_ = add
	if allocs != 0 {
		t.Fatalf("admitFill's pre-check allocated %v times per call, want 0", allocs)
	}
	var adm int64
	var err error
	allocs = testing.AllocsPerRun(1000, func() {
		adm, err = fs.admitWrite(id, p.Extent, false)
		if err != nil {
			panic(err)
		}
	})
	_ = adm
	if allocs != 0 {
		t.Fatalf("admitWrite (view, CAS and closure) = %v, want 0", allocs)
	}
	fs.abandon(adm)
}

// TestBoundRefusesTheWritePastIt is the §9.4 check's first half, at the
// index: scattered writes are admitted while they fit, the one that would
// pass the bound is refused at once, naming the index in entries, the count
// and the refusals are exact, and an overwrite that adds nothing is admitted
// at the bound. Over the bound, only what lowers the count gets through: an
// offload's mark, a release, and a truncate, and then a write is admitted
// again.
func TestBoundRefusesTheWritePastIt(t *testing.T) {
	var fs files
	const limit = 10000
	fs.setExtentLimit(limit)
	id := fid(1)
	const step = 3 * 4096
	for i := range int64(limit) {
		e := ext(i*step, 4096)
		a, err := fs.admitWrite(id, e, false)
		if err != nil || a != 1 {
			t.Fatalf("write %d admitted (%d, %v), want (1, nil)", i, a, err)
		}
		if err := fs.commit(id, a, func(f *file) error {
			f.write(Placed{Extent: e, Record: rec(core.SegmentID(1+i/60_000), uint32(i)*4096), Version: ver(uint64(i + 1))})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if c := fs.extentCount(); c != i+1 {
			t.Fatalf("commit %d left count %d", i, c)
		}
	}
	if r := fs.indexRefusals(); r != 0 {
		t.Fatalf("refusals = %d, want 0", r)
	}

	// The write that would pass the bound: refused at once, naming the
	// index, with the count unmoved and refusals counting it.
	next := int64(limit) * step
	a, err := fs.admitWrite(id, ext(next, 4096), false)
	if err == nil || a != 0 {
		t.Fatalf("write past the bound admitted: (%d, %v)", a, err)
	}
	assertIndexRefusal(t, err)
	var ns *core.NoSpaceError
	if errors.As(err, &ns) && ns.Missing != 1 {
		t.Fatalf("Missing = %d, want 1", ns.Missing)
	}
	if c := fs.extentCount(); c != limit {
		t.Fatalf("count moved to %d on a refusal, want %d", c, limit)
	}
	if r := fs.indexRefusals(); r != 1 {
		t.Fatalf("refusals = %d, want 1", r)
	}

	// At the bound, an overwrite of exactly one whole piece is admitted: it
	// cannot pass the bound, since it adds nothing.
	if a, err := fs.admitWrite(id, ext(0, 4096), false); err != nil || a != 0 {
		t.Fatalf("whole-piece overwrite at the bound = (%d, %v), want (0, nil)", a, err)
	}
	if r := fs.indexRefusals(); r != 1 {
		t.Fatalf("refusals = %d after the admitted overwrite, want 1", r)
	}

	// A truncate is never refused by the bound, and lowers the count: it is
	// how the engine gets out of the refusal.
	if err := fs.update(id, func(f *file) error {
		f.truncate(4096, ver(1<<40))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := fs.extentCount(); c >= limit {
		t.Fatalf("after a truncate over the bound, count = %d, want below %d", c, limit)
	}

	// An offload's mark and a release of one whole piece: never refused, and
	// the count falls again.
	if err := fs.update(id, func(f *file) error {
		f.held.markOffloaded(ext(step, 4096), core.Version{}, newest)
		_, err := f.held.release(ext(step, 4096), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := fs.indexRefusals(); r != 1 {
		t.Fatalf("refusals = %d after a mark and a release, want 1", r)
	}

	// The count is below the bound, so a write is admitted again.
	if a, err := fs.admitWrite(id, ext(next+step, 4096), false); err != nil || a != 1 {
		t.Fatalf("after the release, a write = (%d, %v), want (1, nil)", a, err)
	}
}

// TestPunchesPastAllowanceRefused is the §9.4 check's second half: inside
// one held extent, punches that split it in two are refused once the count
// stands an eighth of the bound above it, naming the index, while a punch at
// the extent's edge, a truncate and a delete never are. A bound that also
// refused them would wedge the very calls that lower the count.
func TestPunchesPastAllowanceRefused(t *testing.T) {
	var fs files
	const limit = 10000
	const ceiling = limit + limit/8
	fs.setExtentLimit(limit)
	id := fid(1)

	// One extent of 10^5 4 KiB pieces, marked offloaded so a release can
	// narrow it; the count is padded to the limit with another file's.
	const nPunch = 100000
	if err := fs.update(id, func(f *file) error {
		f.held.insert(Placed{Extent: ext(0, int64(nPunch)*2*4096), Record: rec(1, 0), Version: ver(1)})
		f.held.markOffloaded(whole, ver(1), ver(1))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	border := fid(2)
	for n := range int64(limit - 1) {
		if err := fs.update(border, func(f *file) error {
			f.write(Placed{Extent: fillExtent(n), Record: rec(core.SegmentID(2+n/60_000), uint32(n)*4096), Version: ver(uint64(n) + 1)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Punch ascending holes: while the count fits the ceiling the splitting
	// ones are admitted, and the first that would not fit is refused, naming
	// the index, with the count still at the ceiling. The refusal's Missing
	// counts what the reservation lacked: one entry here, the marker's, since
	// each committed punch replaced its reservation with the same two.
	refused := 0
	for i := range int64(nPunch) {
		e := ext(i*2*4096, 4096)
		a, err := fs.admitRemoval(id, e, 2)
		if err != nil {
			assertIndexRefusal(t, err)
			refused++
			if refused == 1 {
				if c := fs.extentCount(); c < ceiling-1 {
					t.Fatalf("the first refusal came at count %d, below the ceiling less its marker %d", c, ceiling)
				}
				var ns *core.NoSpaceError
				if errors.As(err, &ns) && ns.Missing < 1 {
					t.Fatalf("Missing = %d, want at least 1", ns.Missing)
				}
			}
			continue
		}
		if err := fs.commit(id, a, func(f *file) error { return f.deallocate(e, ver(2)) }); err != nil {
			t.Fatal(err)
		}
		if c := fs.extentCount(); c > ceiling+1 {
			t.Fatalf("count = %d after an admitted punch, above the ceiling %d and its marker", c, ceiling)
		}
	}
	if refused == 0 {
		t.Fatal("no punch was refused: the ceiling was never reached")
	}
	if r := fs.indexRefusals(); int64(r) < int64(refused) {
		t.Fatalf("refusals = %d, want at least %d", r, refused)
	}

	// A punch at the extent's edge narrows rather than splits, so it is
	// admitted at the ceiling.
	tail := int64(nPunch) * 2 * 4096
	if a, err := fs.admitRemoval(id, ext(tail-4096, 4096), 2); err != nil || a != 0 {
		t.Fatalf("edge punch at the ceiling = (%d, %v), want (0, nil)", a, err)
	}

	// A truncate and a delete are never refused.
	if err := fs.update(id, func(f *file) error { f.truncate(4096, ver(3)); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := fs.update(border, func(f *file) error { f.delete(ver(3)); return nil }); err != nil {
		t.Fatal(err)
	}

	// A release that would split is refused the same way. The punches left
	// the count at the ceiling less the truncate's drop, so the splitting
	// punches further along are refused for it: pad once, with the same
	// offload's writes the count was padded with, to stand at the ceiling
	// again, and a middle release of one offloaded piece — which would split
	// it in two — is refused, reserving its one entry.
	if c := fs.extentCount(); c < ceiling-1 {
		for n := range int64(ceiling - c) {
			if err := fs.update(border, func(f *file) error {
				f.write(Placed{Extent: fillExtent(100_000 + n), Record: rec(core.SegmentID(2+(100_000+n)/60_000), uint32(100_000+n)*4096), Version: ver(uint64(100_001 + n))})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	ring := fid(3)
	if err := fs.update(ring, func(f *file) error {
		f.held.insert(Placed{Extent: ext(0, 3*4096), Record: rec(3, 0), Version: ver(1)})
		f.held.markOffloaded(whole, ver(1), ver(1))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.admitRemoval(ring, ext(4096, 4096), 1); err == nil {
		t.Fatal("a splitting release past the ceiling was not refused")
	} else {
		assertIndexRefusal(t, err)
	}
}

// TestOverBoundAtOpen is R23: an index the bound never refused, or one opened
// under a lowered limit, still opens and still reads, and refuses every
// content write until the count falls below the bound. Refusing the open
// would turn the bound into a crash of the journal once it is reached.
func TestOverBoundAtOpen(t *testing.T) {
	var fs files
	const limit = 10000
	fs.setExtentLimit(limit)
	var ids [4]core.FileID
	for i := range ids {
		ids[i] = fid(byte(i + 1))
	}
	for n := range int64(2 * limit) {
		if err := fs.update(ids[n%4], func(f *file) error {
			// One length short of the next piece, so a write never meets its
			// neighbour and each file's pieces stay countable one a write.
			f.write(Placed{Extent: ext(n*8192, 4096), Record: rec(core.SegmentID(1+n/60_000), uint32(n)*4096), Version: ver(uint64(n) + 1)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if c := fs.extentCount(); c != 2*limit {
		t.Fatalf("count = %d, want %d", c, 2*limit)
	}

	// Every content write is refused, an overwrite that adds nothing among
	// them: over the bound the count must come down, not stand still.
	if _, err := fs.admitWrite(ids[0], ext(0, 4), false); err == nil {
		t.Fatal("a write over the bound at open was admitted")
	} else {
		assertIndexRefusal(t, err)
	}
	if _, err := fs.admitFill(ids[0], Placed{Extent: ext(0, 4096), Record: rec(1, 0), Version: ver(1)}); err == nil {
		t.Fatal("a fill over the bound at open was admitted")
	} else {
		assertIndexRefusal(t, err)
	}
	ps := heldPieces(&fs, ids[0])
	if a, err := fs.admitWrite(ids[0], ps[0].Extent, false); err == nil || a != 0 {
		t.Fatalf("a whole-piece overwrite over the bound = (%d, %v), want (0, refused)", a, err)
	}

	// A read still answers: nothing about the file has changed.
	if !fs.view(ids[1], func(f *file) {
		if _, ok := f.held.lookup(8192); !ok {
			t.Error("over the bound at open, a held extent could not be found")
		}
	}) {
		t.Fatal("over the bound at open, an entry the map holds had no entry")
	}

	// A release never needs admission, so it brings the count back below the
	// bound and a write is admitted again. Each file's release names its own
	// departure: a gap is no refusal, so a whole release of a file is fine too.
	for n := range int64(2 * limit) {
		if fs.extentCount() <= limit-1 {
			break
		}
		if err := fs.update(ids[n%4], func(f *file) error {
			f.held.markOffloaded(whole, core.Version{}, newest)
			// The file's own piece at that index: n = (4k + own), spaced
			// 8192 from every other piece, so this is the k-th piece
			// of file own and of no other.
			_, err := f.held.release(ext(n*8192, 4096), nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if c := fs.extentCount(); c > limit-1 {
		t.Fatalf("count = %d after the releases, want at most %d", c, limit-1)
	}
	if a, err := fs.admitWrite(ids[0], ext(0, 4), false); err != nil || a != 1 {
		t.Fatalf("after the count fell, a write = (%d, %v), want (1, nil)", a, err)
	}

	// Lowering the limit with the index populated behaves the same: the
	// entries stay, and the content writes are refused until it is drained.
	// A whole release of every file, as an offload pass would run one, brings
	// the count back under even the lowered bound, and a write is admitted.
	fs.setExtentLimit(limit / 2)
	if _, err := fs.admitWrite(ids[3], ext(0, 4096), false); err == nil {
		t.Fatal("a write under a lowered limit was admitted over the new bound")
	} else {
		assertIndexRefusal(t, err)
	}
	for _, id := range ids {
		if err := fs.update(id, func(f *file) error {
			f.held.markOffloaded(whole, core.Version{}, newest)
			_, err := f.held.release(whole, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.update(borderID(&fs, ids), func(f *file) error {
		f.held.markOffloaded(whole, core.Version{}, newest)
		_, err := f.held.release(whole, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c := fs.extentCount(); c > limit/2 {
		t.Fatalf("count = %d, want at most %d", c, limit/2)
	}
	if a, err := fs.admitWrite(ids[3], ext(0, 4096), false); err != nil || a != 1 {
		t.Fatalf("after the drain, a write = (%d, %v), want (1, nil)", a, err)
	}
}

// borderID returns a fresh FileID, of no file the check holds content on, so
// an offload pass can name nothing of a file the map holds.
func borderID(*files, [4]core.FileID) core.FileID { return fid(9) }

// TestAbandonReturnsAdmission: a content write whose I/O fails gives its
// admission back, so one failed write cannot hold the bound's headroom for
// good.
func TestAbandonReturnsAdmission(t *testing.T) {
	var fs files
	fs.setExtentLimit(100)
	id := fid(1)
	if err := fs.update(id, func(f *file) error {
		f.write(Placed{Extent: fillExtent(0), Record: rec(1, 0), Version: ver(1)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for n := range int64(10) {
		e := fillExtent(n + 1)
		a, err := fs.admitWrite(id, e, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := fs.commit(id, a, func(f *file) error {
			f.write(Placed{Extent: e, Record: rec(2, 0), Version: ver(2)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// The write made no entry, so nothing but its admission would bring
		// the count back down.
		fs.abandon(a)
	}
	if fs.extentCount() != 1 {
		t.Fatalf("count = %d before the abandoned release, want 1", fs.extentCount())
	}
	// One admission never given back is all that separates the count from
	// the truth, so a second lost write would take it to a spurious refusal.
	if a, err := fs.admitWrite(id, fillExtent(100), false); err != nil || a != 1 {
		t.Fatalf("a write after the abandoned admissions = (%d, %v)", a, err)
	}
}

// TestConcurrentAdmissionNeverPassesBound: admitted writes from many files at
// once must not let the count past the bound, or a concurrent stream would
// wedge every writer against a count above it that content writes cannot
// lower.
func TestConcurrentAdmissionNeverPassesBound(t *testing.T) {
	var fs files
	const limit = 1000
	fs.setExtentLimit(limit)
	const workers = 16
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			id := fid(byte(w + 1))
			for round := range 125 {
				e := fillExtent(int64(round))
				a, err := fs.admitWrite(id, e, false)
				if err != nil {
					return
				}
				if err := fs.commit(id, a, func(f *file) error {
					f.write(Placed{Extent: e, Record: rec(core.SegmentID(w+1), uint32(round)*4096), Version: ver(uint64(round) + 1)})
					return nil
				}); err != nil {
					return
				}
			}
		})
	}
	wg.Wait()
	if c := fs.extentCount(); c > limit {
		t.Fatalf("count = %d, want at most %d", c, limit)
	}
	if c, w := fs.extentCount(), walkCount(&fs); c != w {
		t.Fatalf("count = %d, walk = %d", c, w)
	}
}

// TestNonSplittingRemovalsNeverRefused: at a count far past the ceiling, a
// removal that narrows a piece, covers whole pieces or covers nothing enters
// for free. Refusing these too would wedge a trim pass against a bound it
// was itself lowering.
func TestNonSplittingRemovalsNeverRefused(t *testing.T) {
	var fs files
	const limit = 100
	fs.setExtentLimit(limit)
	id := fid(1)
	if err := fs.update(id, func(f *file) error {
		f.write(write(0, 32, rec(1, 0), 1))
		f.write(write(40, 8, rec(1, 64), 1))
		f.held.markOffloaded(whole, ver(1), ver(1))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for n := range int64(limit + limit/4) {
		if err := fs.update(fid(2), func(f *file) error {
			f.write(Placed{Extent: fillExtent(n), Record: rec(core.SegmentID(3+n/60_000), uint32(n)*4096), Version: ver(uint64(n) + 1)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for name, e := range map[string]core.Extent{
		"narrowing a piece":     ext(0, 4),
		"covering whole pieces": ext(0, 32),
		"covering nothing":      ext(100, 4096),
	} {
		t.Run(name, func(t *testing.T) {
			before := fs.indexRefusals()
			a, err := fs.admitRemoval(id, e, 2)
			if err != nil || a != 0 {
				t.Fatalf("admitRemoval(%+v) = (%d, %v), want (0, nil): a removal that splits nothing is never refused", e, a, err)
			}
			if fs.indexRefusals() != before {
				t.Fatal("a never-refused removal was counted as a refusal")
			}
		})
	}
}
