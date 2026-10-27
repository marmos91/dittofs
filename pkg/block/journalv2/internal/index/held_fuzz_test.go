package index

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// byteOracle is the held set as one state per byte of a small file: the
// simplest model with the same contract, whose correctness is evident. Each
// held byte knows its record, its own offset in that record's payload, its
// version and its bit.
type byteOracle [oracleSize]struct {
	held      bool
	record    RecordAt
	at        uint32
	version   core.Version
	offloaded bool
}

const oracleSize = 96

func (o *byteOracle) put(p Piece) {
	for i := range p.Extent.Len {
		b := &o[p.Extent.Off+i]
		b.held, b.record, b.at, b.version, b.offloaded = true, p.Record, p.Skip+uint32(i), p.Version, p.Offloaded
	}
}

// pieces folds the bytes into maximal runs: adjacent held bytes of one record,
// at consecutive payload offsets, with one version and one bit. That is the
// one-entry-per-piece form the set must keep.
func (o *byteOracle) pieces() []Piece {
	var out []Piece
	for i := range int64(oracleSize) {
		b := o[i]
		if !b.held {
			continue
		}
		if n := len(out); n > 0 {
			q := &out[n-1]
			if q.Extent.Off+q.Extent.Len == i && q.Record == b.record && q.Version == b.version &&
				q.Offloaded == b.offloaded && q.Skip+uint32(q.Extent.Len) == b.at {
				q.Extent.Len++
				continue
			}
		}
		out = append(out, Piece{Extent: ext(i, 1), Record: b.record, Skip: b.at, Version: b.version, Offloaded: b.offloaded})
	}
	return out
}

// span is what a read of e is told: maximal runs ignoring the bit, and gaps.
func (o *byteOracle) span(e core.Extent) (held []Placed, missing []core.Extent) {
	for i := e.Off; i < e.Off+e.Len; i++ {
		b := o[i]
		if !b.held {
			if n := len(missing); n > 0 && missing[n-1].Off+missing[n-1].Len == i {
				missing[n-1].Len++
			} else {
				missing = append(missing, ext(i, 1))
			}
			continue
		}
		if n := len(held); n > 0 {
			q := &held[n-1]
			if q.Extent.Off+q.Extent.Len == i && q.Record == b.record && q.Version == b.version && q.Skip+uint32(q.Extent.Len) == b.at {
				q.Extent.Len++
				continue
			}
		}
		held = append(held, Placed{Extent: ext(i, 1), Record: b.record, Skip: b.at, Version: b.version})
	}
	return held, missing
}

// heldOps drives a heldSet and a byteOracle with the same operations, decoded
// from data, and compares them after each. Records are fresh per operation, so
// two never collide by accident, but segments reuse each other's offsets, so a
// record is told apart only by both. A repoint targets the record holding a
// byte inside its span, so it moves something. A re-hold writes again from
// the record already holding a byte, at that byte's payload offset, so pieces
// of one record put by different operations meet and must merge. Half the
// spans start and end on a grid of four, so operations land on each other's
// split points often.
func heldOps(t *testing.T, data []byte) {
	var s heldSet
	var o byteOracle
	next := uint32(0)
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
	for step := 0; len(data) > 0; step++ {
		op, e := take()%7, span()
		v := ver(uint64(take()%4 + 1))
		switch op {
		case 0: // write
			p := Placed{Extent: e, Record: fresh(), Skip: uint32(take()), Version: v}
			s.insert(p)
			o.put(Piece{Extent: p.Extent, Record: p.Record, Skip: p.Skip, Version: p.Version})
		case 1: // fill
			p := Placed{Extent: e, Record: fresh(), Skip: uint32(take()), Version: v}
			s.fill(p)
			for i := range e.Len {
				if !o[e.Off+i].held {
					o.put(Piece{Extent: ext(e.Off+i, 1), Record: p.Record, Skip: p.Skip + uint32(i), Version: v, Offloaded: true})
				}
			}
		case 2: // mark
			hi := ver(v.Counter + uint64(take()%3))
			want := 0
			for _, p := range o.pieces() {
				if p.Offloaded || p.Version.Compare(v) < 0 || p.Version.Compare(hi) > 0 {
					continue
				}
				if lo, end := max(p.Extent.Off, e.Off), min(p.Extent.Off+p.Extent.Len, e.Off+e.Len); lo < end {
					want++
					for i := lo; i < end; i++ {
						o[i].offloaded = true
					}
				}
			}
			if got := s.markOffloaded(e, v, hi); got != want {
				t.Fatalf("step %d: markOffloaded(%+v) marked %d pieces, oracle %d", step, e, got, want)
			}
		case 3: // release
			want, all := []core.Held(nil), true
			for _, p := range o.pieces() {
				if lo, end := max(p.Extent.Off, e.Off), min(p.Extent.Off+p.Extent.Len, e.Off+e.Len); lo < end {
					all = all && p.Offloaded
					want = append(want, core.Held{Extent: ext(lo, end-lo), Version: p.Version})
				}
			}
			if got := s.allOffloaded(e); got != all {
				t.Fatalf("step %d: allOffloaded(%+v) = %v, oracle %v", step, e, got, all)
			}
			got, err := s.release(e, nil)
			if !all {
				if !errors.Is(err, ErrNotOffloaded) || len(got) != 0 {
					t.Fatalf("step %d: release(%+v) of dirty content returned %+v, %v", step, e, got, err)
				}
				break
			}
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("step %d: release(%+v) = %+v, %v; oracle %+v", step, e, got, err, want)
			}
			for i := range e.Len {
				o[e.Off+i].held = false
			}
		case 4: // repoint, inside e, the record holding e's first byte
			if e.Len == 0 || !o[e.Off].held {
				break
			}
			from, to, want := o[e.Off].record, fresh(), 0
			for _, p := range o.pieces() {
				if lo, end := max(p.Extent.Off, e.Off), min(p.Extent.Off+p.Extent.Len, e.Off+e.Len); lo < end && p.Record == from {
					want++
				}
			}
			for i := e.Off; i < e.Off+e.Len; i++ {
				if o[i].held && o[i].record == from {
					o[i].record = to
				}
			}
			if got := s.repoint(e, from, to); got != want {
				t.Fatalf("step %d: repoint moved %d pieces, oracle %d", step, got, want)
			}
		case 5: // read
			gotHeld, gotMissing := s.span(e, nil, nil)
			wantHeld, wantMissing := o.span(e)
			if !slices.Equal(gotHeld, wantHeld) || !slices.Equal(gotMissing, wantMissing) {
				t.Fatalf("step %d: span(%+v):\n got  %+v %+v\n want %+v %+v", step, e, gotHeld, gotMissing, wantHeld, wantMissing)
			}
			off := e.Off % oracleSize
			p, ok := s.lookup(off)
			if b := o[off]; ok != b.held || ok && (p.Record != b.record || p.Version != b.version ||
				p.Skip+uint32(off-p.Extent.Off) != b.at || off < p.Extent.Off || off >= p.Extent.Off+p.Extent.Len) {
				t.Fatalf("step %d: lookup(%d) = %+v, %v; oracle %+v", step, off, p, ok, b)
			}
		case 6: // re-hold from the record holding e's first byte
			if e.Len == 0 || !o[e.Off].held {
				break
			}
			b := o[e.Off]
			p := Placed{Extent: e, Record: b.record, Skip: b.at, Version: b.version}
			s.insert(p)
			o.put(Piece{Extent: p.Extent, Record: p.Record, Skip: p.Skip, Version: p.Version})
		}
		got, want := s.pieces(nil), o.pieces()
		if !slices.Equal(got, want) {
			t.Fatalf("step %d (op %d over %+v):\n got  %+v\n want %+v", step, op, e, got, want)
		}
		if s.len() != len(want) {
			t.Fatalf("step %d: len() = %d, oracle %d", step, s.len(), len(want))
		}
	}
}

// TestHeldMatchesByteOracle runs long random operation sequences against the
// per-byte oracle. It catches a wrong split or narrow, a gap reported where
// content is held or the reverse, a mark outside its span or version range, a
// partial release, a repoint of the wrong record, and a piece left as two
// entries.
func TestHeldMatchesByteOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 2))
	for range 200 {
		data := make([]byte, 2000)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		heldOps(t, data)
	}
}

func FuzzHeldSet(f *testing.F) {
	f.Add([]byte{0, 0, 40, 1, 0, 2, 10, 10, 2, 2, 0, 50, 1, 0, 3, 0, 60, 1, 5, 0, 60, 0})
	f.Add([]byte{0, 10, 20, 1, 0, 0, 12, 4, 2, 0, 2, 0, 30, 1, 0, 4, 11, 0, 0})
	f.Fuzz(heldOps)
}
