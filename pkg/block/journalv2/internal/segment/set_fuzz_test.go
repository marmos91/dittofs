package segment

import (
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// op is one operation the fuzz's input decodes into. The ids, tags and sizes
// come from the input too, so a sequence can paint any interleaving of the
// ledger's calls over a small space, including one human tests would not
// write: a release after a retire, a claim between two retires, a shrink
// against a shrink.
type op struct {
	kind  byte
	id    core.SegmentID
	fresh core.SegmentID
	tag   core.ShareTag
	n     int64
}

// errNoClaim stands for every refusal the claim's boolean answer covers; the
// test turns it into a false the oracle predicts.
var errNoClaim = errors.New("oracle: the claim is not held")

// The op kinds.
const (
	opAddSpare byte = iota
	opTake
	opSeal
	opEnd
	opShrink
	opAddRecord
	opClaim
	opRelease
	opRetire
	opRecycle
)

// decode turns the fuzz's input into a sequence of ops over ids 1..8, tags
// 1..3 and sizes drawn from the input and clamped to the cap: a sequence of
// the ledger's calls, with fields chosen from the input but shapes not
// constrained by the decoder, so any interleaving of states and byte counts
// can arise.
func decode(data []byte, segCap int64) []op {
	ops := make([]op, 0, len(data)/4)
	for len(data) >= 5 {
		var o op
		o.kind = data[0] % 10
		o.id = core.SegmentID(1 + uint64(data[1])%8)
		o.fresh = core.SegmentID(1 + uint64(data[2])%8)
		o.tag = core.ShareTag(1 + uint32(data[3])%3)
		o.n = 1 + (int64(data[4]) % segCap)
		data = data[5:]
		ops = append(ops, o)
	}
	return ops
}

// oracleSeg is what the plain-map oracle knows about one segment: its state
// and the size it is accounted at, and tags set only where it holds bytes.
type oracleSeg struct {
	state State
	size  int64
}

// oracle predicts the ledger's counters and per-tag bytes from plain maps
// bound to the same sequence of operations: which ops succeed, which refuse
// and with which error, and what the set holds afterwards. A divergence is a
// bug in the ledger, not in the input.
type oracle struct {
	segCap        int64
	spares        map[core.SegmentID]struct{}
	segs          map[core.SegmentID]oracleSeg
	segTags       map[core.SegmentID]map[core.ShareTag]int64
	tags          map[core.ShareTag]int64
	claimed       map[core.SegmentID]bool
	doubleRetires uint64
}

func newOracle(segCap int64) *oracle {
	return &oracle{
		segCap:  segCap,
		spares:  map[core.SegmentID]struct{}{},
		segs:    map[core.SegmentID]oracleSeg{},
		segTags: map[core.SegmentID]map[core.ShareTag]int64{},
		tags:    map[core.ShareTag]int64{},
		claimed: map[core.SegmentID]bool{},
	}
}

// addTag charges n bytes of tag to the segment and the share.
func (o *oracle) addTag(id core.SegmentID, tag core.ShareTag, n int64) {
	if o.segTags[id] == nil {
		o.segTags[id] = map[core.ShareTag]int64{}
	}
	o.segTags[id][tag] += n
	o.tags[tag] += n
}

// releaseSeg releases the segment's per-tag bytes from the shares' totals.
func (o *oracle) releaseSeg(id core.SegmentID) {
	for tag, n := range o.segTags[id] {
		o.tags[tag] -= n
		if o.tags[tag] <= 0 {
			delete(o.tags, tag)
		}
	}
	delete(o.segTags, id)
}

// doubleRetires counts every retire or recycle that found its own id out of
// the set. The oracle returns its own error class, which also names the
// state the ledger was expected to be found in, so the test can demand the
// same refusal from both.
func (o *oracle) apply(he op) (err error) {
	switch he.kind {
	case opAddSpare:
		if _, ok := o.segs[he.id]; ok {
			return ErrDuplicateID
		}
		if _, ok := o.spares[he.id]; ok {
			return ErrDuplicateID
		}
		o.spares[he.id] = struct{}{}
		return nil
	case opTake:
		if _, ok := o.spares[he.id]; !ok {
			return ErrNotInSet
		}
		delete(o.spares, he.id)
		o.segs[he.id] = oracleSeg{state: Active, size: o.segCap}
		return nil
	case opSeal:
		seg, ok := o.segs[he.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state != Active {
			return ErrWrongState
		}
		seg.state = Sealed
		o.segs[he.id] = seg
		return nil
	case opEnd:
		seg, ok := o.segs[he.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state != Active {
			return ErrWrongState
		}
		seg.state = Ended
		o.segs[he.id] = seg
		return nil
	case opShrink:
		seg, ok := o.segs[he.id]
		if !ok {
			return ErrNotInSet
		}
		if he.n <= 0 || he.n > seg.size || he.n > o.segCap {
			return ErrBadSize
		}
		seg.size = he.n
		o.segs[he.id] = seg
		return nil
	case opAddRecord:
		seg, ok := o.segs[he.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state != Active {
			return ErrWrongState
		}
		if he.n <= 0 {
			return ErrBadSize
		}
		o.addTag(he.id, he.tag, he.n)
		return nil
	case opClaim:
		seg, ok := o.segs[he.id]
		if !ok || seg.state == Active || o.claimed[he.id] {
			return errNoClaim
		}
		o.claimed[he.id] = true
		return nil
	case opRelease:
		if _, ok := o.segs[he.id]; !ok {
			return nil // no-op: out of the set (R57), nothing to release
		}
		delete(o.claimed, he.id)
		return nil
	case opRetire:
		seg, ok := o.segs[he.id]
		if !ok {
			o.doubleRetires++
			return ErrNotInSet
		}
		if seg.state == Active {
			return ErrWrongState
		}
		delete(o.segs, he.id)
		o.releaseSeg(he.id)
		delete(o.claimed, he.id)
		return nil
	case opRecycle:
		seg, ok := o.segs[he.id]
		if !ok {
			o.doubleRetires++
			return ErrNotInSet
		}
		if seg.state != Sealed {
			return ErrWrongState
		}
		if seg.size != o.segCap {
			return ErrBadSize
		}
		if he.id == he.fresh {
			return ErrDuplicateID
		}
		if _, ok := o.segs[he.fresh]; ok {
			return ErrDuplicateID
		}
		if o.inSpares(he.fresh) {
			return ErrDuplicateID
		}
		delete(o.segs, he.id)
		o.releaseSeg(he.id)
		delete(o.claimed, he.id)
		o.spares[he.fresh] = struct{}{}
		return nil
	}
	return nil
}

// sameErrClass reports whether the ledger's error and the oracle's refusal
// name the same rule. The oracle returns the sentinel itself, so the class is
// errors.Is — in either direction, since the ledger may wrap its reasons.
func sameErrClass(got, want error) bool {
	switch {
	case got == nil && want == nil:
		return true
	case got == nil || want == nil:
		return false
	case errors.Is(got, want):
		return true
	default:
		return errors.Is(want, got)
	}
}

// FuzzSet drives the segment set against its oracle: the same operation
// sequence goes to both, and after every single one the ledger must agree
// with the oracle on the error class, on every counter, on every share's
// record bytes and on the footprint check — and the footprint never dips
// below zero.
func FuzzSet(f *testing.F) {
	// One op is five bytes: kind, segment id, fresh id, share tag and record
	// bytes — the same fields the decoder reads.
	ops := func(ops ...[]byte) []byte {
		return slices.Concat(ops...)
	}
	five := func(kind byte, id, fresh byte, tag byte, n byte) []byte {
		return []byte{kind, id, fresh, tag, n}
	}

	f.Add(ops(
		// A double retire: a segment that leaves the set, then is retired
		// again, and a release that arrives too late for it.
		five(opAddSpare, 0, 0, 0, 0), five(opTake, 0, 1, 0, 0),
		five(opSeal, 0, 0, 0, 0), five(opRetire, 0, 0, 0, 0),
		five(opRetire, 0, 1, 0, 0), five(opRelease, 0, 0, 0, 0), five(opClaim, 0, 0, 0, 0),
	))
	f.Add(ops(
		// Recycle, then retire the old id: the footprint leaves once, and the
		// second retire is refused and counted.
		five(opAddSpare, 0, 0, 0, 0), five(opTake, 0, 1, 0, 0), five(opSeal, 0, 0, 0, 0),
		five(opRecycle, 0, 3, 0, 0), five(opRetire, 0, 0, 0, 0),
	))
	f.Add(ops(
		// Records then a retire: each tag keeps only the other segment's
		// bytes, and the tag that reaches zero leaves the ledger.
		five(opAddSpare, 0, 0, 0, 0), five(opTake, 0, 1, 0, 0), five(opAddRecord, 0, 1, 0, 9),
		five(opAddSpare, 1, 0, 0, 0), five(opTake, 1, 2, 0, 0), five(opAddRecord, 1, 2, 1, 4),
		five(opAddRecord, 1, 2, 2, 2), five(opSeal, 0, 0, 0, 0), five(opSeal, 1, 0, 0, 0),
		five(opRetire, 0, 0, 0, 0),
	))
	f.Add(ops(
		// Shrink, then recycle the shrunk segment: a sealed size below the
		// cap cannot come back as a spare, so the retire is the only way out.
		five(opAddSpare, 0, 0, 0, 0), five(opTake, 0, 1, 0, 0), five(opSeal, 0, 0, 0, 0),
		five(opShrink, 0, 0, 0, 5), five(opRecycle, 0, 3, 0, 0), five(opRetire, 0, 0, 0, 0),
	))
	f.Add(ops(
		// A recycle of a segment that never was, refused; then the making of a
		// spare that takes and seals instead.
		five(opRecycle, 0, 1, 0, 0), five(opAddSpare, 0, 0, 0, 0), five(opTake, 0, 1, 0, 0),
		five(opSeal, 0, 0, 0, 0), five(opRetire, 0, 0, 0, 0),
	))
	f.Add(ops(
		// A claim between two retires of one segment, both of which find it
		// out of the set.
		five(opAddSpare, 0, 0, 0, 0), five(opTake, 0, 1, 0, 0), five(opEnd, 0, 0, 0, 0),
		five(opRetire, 0, 0, 0, 0), five(opClaim, 0, 0, 0, 0), five(opRetire, 0, 0, 0, 0),
	))
	f.Add(ops(
		// A recycle whose fresh id names a spare the ledger already holds.
		five(opAddSpare, 0, 0, 0, 0), five(opAddSpare, 1, 0, 0, 0), five(opTake, 0, 2, 0, 0),
		five(opSeal, 0, 0, 0, 0), five(opRecycle, 0, 1, 0, 0),
	))

	segCap := int64(4096)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := NewSet(segCap, nil)
		o := newOracle(segCap)

		for _, he := range decode(data, segCap) {
			want := o.apply(he)
			var got error
			switch he.kind {
			case opAddSpare:
				got = s.AddSpare(he.id)
			case opTake:
				got = s.Take(he.id)
			case opSeal:
				got = s.Seal(he.id)
			case opEnd:
				got = s.End(he.id)
			case opShrink:
				got = s.Shrink(he.id, he.n)
			case opAddRecord:
				got = s.AddRecord(he.id, he.tag, he.n)
			case opClaim:
				if s.Claim(he.id) != (want == nil) {
					t.Fatalf("Claim(%d): the oracle holds the claim %v, the set does not", he.id, want == nil)
				}
				continue
			case opRelease:
				s.Release(he.id)
				continue
			case opRetire:
				_, got = s.Retire(he.id)
			case opRecycle:
				got = s.Recycle(he.id, he.fresh)
			}

			if !sameErrClass(got, want) {
				t.Fatalf("op %d (%d): the set returned %v, the oracle %v", he.kind, he.id, got, want)
			}
			c := s.Counters()
			if c.UsedBytes < 0 {
				t.Fatalf("op %d (%d): the footprint fell to %d", he.kind, he.id, c.UsedBytes)
			}
			if err := s.Check(); err != nil {
				t.Fatalf("op %d (%d): %v", he.kind, he.id, err)
			}
			if got := o.usedBytes(); got != c.UsedBytes {
				t.Fatalf("op %d (%d): the footprint is %d, the oracle's %d", he.kind, he.id, c.UsedBytes, got)
			}
			if got := o.countSegments(); got != c.Segments {
				t.Fatalf("op %d (%d): the set holds %d segments, the oracle %d", he.kind, he.id, c.Segments, got)
			}
			if got := o.countSealed(); got != c.Sealed {
				t.Fatalf("op %d (%d): the set holds %d sealed, the oracle %d", he.kind, he.id, c.Sealed, got)
			}
			if got := int64(len(o.spares)); got != c.Spares {
				t.Fatalf("op %d (%d): the set holds %d spares, the oracle %d", he.kind, he.id, c.Spares, got)
			}
			if c.DoubleRetires != o.doubleRetires {
				t.Fatalf("op %d (%d): the set counted %d double retires, the oracle %d", he.kind, he.id, c.DoubleRetires, o.doubleRetires)
			}
			for tag, want := range o.tags {
				if got := s.ShareBytes(tag); got != want {
					t.Fatalf("op %d (%d): tag %d holds %d, the oracle %d", he.kind, he.id, tag, got, want)
				}
			}
			for _, tag := range []core.ShareTag{1, 2, 3} {
				if _, ok := o.tags[tag]; !ok {
					if got := s.ShareBytes(tag); got != 0 {
						t.Fatalf("op %d (%d): the oracle holds none of tag %d, the set %d", he.kind, he.id, tag, got)
					}
				}
			}
		}
	})
}

// usedBytes sums the oracle's set sizes and spares at the cap.
func (o *oracle) usedBytes() int64 {
	var sum int64
	for _, s := range o.segs {
		sum += s.size
	}
	sum += int64(len(o.spares)) * o.segCap
	return sum
}

func (o *oracle) countSegments() int64 {
	return int64(len(o.segs))
}

func (o *oracle) countSealed() int64 {
	var n int64
	for _, s := range o.segs {
		if s.state == Sealed {
			n++
		}
	}
	return n
}

func (o *oracle) inSpares(id core.SegmentID) bool {
	_, ok := o.spares[id]
	return ok
}
