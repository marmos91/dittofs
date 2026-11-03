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
	// max is the journal maximum an AddSpare reserves against, drawn by
	// byte 5 — the one already-decoded size byte opAddSpare's own shape
	// ignores: 0 means no maximum, k else k caps. Only opAddSpare reads it.
	max int64
	// full is the seal's own word for itself: byte 5, which only the size
	// shapes read elsewhere, draws it for a seal.
	full bool
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
	opDropSpare
)

// decode turns the fuzz's input into a sequence of ops over ids 0..8, tags
// 1..3 and sizes drawn from six input bytes: byte 4 selects the size's shape —
// zero, the full cap, one past the cap, a negative, or a plain value — so the
// refusals at each boundary are reachable, and the record and shrink paths
// see n of 0 and below. The segment id spans 0 (the id that names no segment)
// and 1..8; the fresh id spans the same. Shapes are not constrained beyond
// that, so any interleaving of states and byte counts can arise.
func decode(data []byte, segCap int64) []op {
	ops := make([]op, 0, len(data)/6)
	for len(data) >= 6 {
		var o op
		o.kind = data[0] % 11
		o.id = core.SegmentID(uint64(data[1]) % 9)
		o.fresh = core.SegmentID(uint64(data[2]) % 9)
		o.tag = core.ShareTag(1 + uint32(data[3])%3)
		switch data[4] {
		case 0:
			o.n = 0
		case 1:
			o.n = segCap
		case 2:
			o.n = segCap + 1
		case 3:
			o.n = -int64(1 + data[5]%255)
		default:
			o.n = 1 + (int64(data[5]) % segCap)
		}
		o.full = data[5]%2 == 0
		// The maximum an AddSpare draws: byte 5 itself, so a spare whose
		// kind byte says AddSpare can draw one past what the footprint
		// holds, exactly on it, or none at all. k caps with k the byte's
		// value, and 0 no maximum, so every multiple of the cap up to 255
		// of them is reachable.
		if data[5] == 0 {
			o.max = noMax
		} else {
			o.max = int64(data[5]) * segCap
		}
		data = data[6:]
		ops = append(ops, o)
	}
	return ops
}

// oracleSeg is what the plain-map oracle knows about one segment: its state,
// the size it is accounted at and the record bytes it holds — the same
// running bound the ledger's entry keeps, mirrored so a record past the size
// and a cut below the bytes it holds refuse here too. Its record bytes per
// share live in the oracle's per-segment segTags, whose sum is held.
type oracleSeg struct {
	state State
	size  int64
	held  int64
	// full is the seal's own word, mirrored: the one fact a recycle trusts.
	full bool
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
	seg := o.segs[id]
	seg.held += n
	o.segs[id] = seg
}

// releaseSeg releases the segment's per-tag bytes from the shares' totals.
// The bytes the entries name are non-negative by construction, so a total
// that does not reach exactly zero is a divergence the fuzz reports at the
// comparison, not one this delete hides.
func (o *oracle) releaseSeg(id core.SegmentID) {
	for tag, n := range o.segTags[id] {
		o.tags[tag] -= n
		if o.tags[tag] == 0 {
			delete(o.tags, tag)
		}
	}
	delete(o.segTags, id)
}

// apply runs one op against the oracle's plain maps and returns the error
// class the ledger owes for it: the same refusal, or nil where both accept.
// Every map the oracle keeps mirrors one the ledger keeps, so a divergence
// between the two is a bug in the ledger, never in the input.
func (o *oracle) apply(op op) (err error) {
	switch op.kind {
	case opAddSpare:
		if op.id == 0 {
			return ErrBadID
		}
		if _, ok := o.segs[op.id]; ok {
			return ErrDuplicateID
		}
		if _, ok := o.spares[op.id]; ok {
			return ErrDuplicateID
		}
		// The maximum is checked against the footprint the oracle holds,
		// in the overflow-safe form the ledger's own check uses; the
		// refusal mirrors the ledger's, field for field.
		if op.max < o.segCap || o.usedBytes() > op.max-o.segCap {
			return &core.NoSpaceError{
				Limit:   core.LimitJournal,
				Missing: o.usedBytes() + o.segCap - op.max,
				Unit:    core.UnitBytes,
			}
		}
		o.spares[op.id] = struct{}{}
		return nil
	case opTake:
		if _, ok := o.spares[op.id]; !ok {
			return ErrNotInSet
		}
		delete(o.spares, op.id)
		o.segs[op.id] = oracleSeg{state: Active, size: o.segCap}
		return nil
	case opSeal:
		seg, ok := o.segs[op.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state != Active {
			return ErrWrongState
		}
		seg.state = Sealed
		seg.full = op.full
		o.segs[op.id] = seg
		return nil
	case opEnd:
		seg, ok := o.segs[op.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state != Active {
			return ErrWrongState
		}
		seg.state = Ended
		o.segs[op.id] = seg
		return nil
	case opShrink:
		seg, ok := o.segs[op.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state == Active {
			return ErrWrongState
		}
		if seg.state == Sealed && seg.full {
			return ErrWrongState
		}
		if op.n <= 0 || op.n > seg.size {
			return ErrBadSize
		}
		if op.n < seg.held {
			return ErrBadSize
		}
		seg.size = op.n
		o.segs[op.id] = seg
		return nil
	case opAddRecord:
		seg, ok := o.segs[op.id]
		if !ok {
			return ErrNotInSet
		}
		if seg.state != Active {
			return ErrWrongState
		}
		if op.n <= 0 {
			return ErrBadSize
		}
		if seg.held+op.n > seg.size {
			return ErrBadSize
		}
		o.addTag(op.id, op.tag, op.n)
		return nil
	case opClaim:
		seg, ok := o.segs[op.id]
		if !ok || seg.state == Active || o.claimed[op.id] {
			return errNoClaim
		}
		o.claimed[op.id] = true
		return nil
	case opRelease:
		if _, ok := o.segs[op.id]; !ok {
			return nil // no-op: out of the set, nothing to release
		}
		delete(o.claimed, op.id)
		return nil
	case opRetire:
		seg, ok := o.segs[op.id]
		if !ok {
			if o.inSpares(op.id) {
				return ErrWrongState
			}
			o.doubleRetires++
			return ErrNotInSet
		}
		if seg.state == Active {
			return ErrWrongState
		}
		delete(o.segs, op.id)
		o.releaseSeg(op.id)
		delete(o.claimed, op.id)
		return nil
	case opRecycle:
		seg, ok := o.segs[op.id]
		if !ok {
			if o.inSpares(op.id) {
				return ErrWrongState
			}
			o.doubleRetires++
			return ErrNotInSet
		}
		if seg.state != Sealed {
			return ErrWrongState
		}
		if !seg.full {
			return ErrBadSize
		}
		if op.fresh == 0 {
			return ErrBadID
		}
		if op.id == op.fresh {
			return ErrDuplicateID
		}
		if _, ok := o.segs[op.fresh]; ok {
			return ErrDuplicateID
		}
		if o.inSpares(op.fresh) {
			return ErrDuplicateID
		}
		delete(o.segs, op.id)
		o.releaseSeg(op.id)
		delete(o.claimed, op.id)
		o.spares[op.fresh] = struct{}{}
		return nil
	case opDropSpare:
		if !o.inSpares(op.id) {
			if _, seg := o.segs[op.id]; seg {
				return ErrWrongState
			}
			o.doubleRetires++
			return ErrNotInSet
		}
		delete(o.spares, op.id)
		return nil
	}
	return nil
}

// sameErrClass reports whether the ledger's error and the oracle's refusal
// name the same rule. The oracle returns the sentinel itself, so the class is
// errors.Is — in either direction, since the ledger may wrap its reasons. Two
// reservation refusals are the same rule only when they name the same limit,
// the same missing count and the same unit: a NoSpaceError that matches
// another under errors.Is alone could be off by the whole missing figure.
func sameErrClass(got, want error) bool {
	switch {
	case got == nil && want == nil:
		return true
	case got == nil || want == nil:
		return false
	default:
	}
	// Two reservation refusals compare by their three fields, not by the
	// sentinel both match: a NoSpaceError matches another *NoSpaceError under
	// errors.Is only through the ErrNoSpace everyone carries.
	var gn, wn *core.NoSpaceError
	if errors.As(got, &gn) && errors.As(want, &wn) {
		return *gn == *wn
	}
	if errors.As(got, &gn) || errors.As(want, &wn) {
		return false
	}
	if errors.Is(got, want) {
		return true
	}
	return errors.Is(want, got)
}

// op6 spells one six-byte op of the fuzz's input: kind, segment id, fresh
// id, share tag, a size shape selector and the size byte — the fields the
// decoder reads.
func op6(kind, id, fresh, tag, shape, n byte) []byte {
	return []byte{kind, id, fresh, tag, shape, n}
}

// seed is one entry of the corpus FuzzSet is seeded with. The bytes and the
// counts the sequence is expected to reach are one table, because a seed
// that stops exercising what its comment names is a silent loss: the corpus
// that runs in plain go test is the only one most checks ever run.
type seed struct {
	// name names the sequence, as its comment does.
	name string
	// ops is the six-byte-per-op input, exactly what f.Add receives.
	ops []byte
	// retires is the successful Retire calls the sequence must reach;
	// recycles, takes and seals likewise; drops counts the spares
	// DropSpare took back. The counts are the guard: a decoder or seed change
	// that turns a success the sequence is documented for into a refusal —
	// every seed naming the null id where the set reads a segment, say —
	// fails the replay on the count, not on a silent no-op.
	retires  int
	recycles int
	takes    int
	seals    int
	drops    int
}

// corpus is the FuzzSet seed corpus, the one table both FuzzSet and the
// replay test read.
var corpus = []seed{
	{"double retire", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddSpare, 1, 0, 0, 4, 0), // refused: the id names a segment of the set
		op6(opSeal, 1, 0, 0, 4, 0), op6(opRetire, 1, 0, 0, 4, 0),
		op6(opRetire, 1, 0, 0, 4, 0), op6(opRelease, 1, 0, 0, 4, 0),
		op6(opClaim, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"recycle then retire the old id", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 4, 0), op6(opRecycle, 1, 2, 0, 4, 0),
		op6(opRetire, 1, 0, 0, 4, 0), // refused: the recycle already took the segment
	), 0, 1, 1, 1, 0},
	{"records then a retire", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 0, 4, 9),
		op6(opAddSpare, 2, 0, 0, 4, 0), op6(opTake, 2, 0, 0, 4, 0),
		op6(opAddRecord, 2, 0, 1, 4, 4), op6(opAddRecord, 2, 0, 2, 4, 2),
		op6(opSeal, 1, 0, 0, 4, 0), op6(opSeal, 2, 0, 0, 4, 0),
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 2, 2, 0},
	{"shrink then recycle refused", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 5, 1), op6(opShrink, 1, 0, 0, 4, 5),
		op6(opRecycle, 1, 2, 0, 4, 0), // refused: the sealed size is below the cap
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"recycle of a segment that never was", slices.Concat(
		op6(opRecycle, 1, 2, 0, 4, 0), // refused: nothing under that id
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 4, 0), op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"claim between two retires", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opEnd, 1, 0, 0, 4, 0), op6(opRetire, 1, 0, 0, 4, 0),
		op6(opClaim, 1, 0, 0, 4, 0), // refused: the entry left with the retire
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"recycle onto a held spare", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opAddSpare, 2, 0, 0, 4, 0),
		op6(opTake, 1, 0, 0, 4, 0), op6(opSeal, 1, 0, 0, 4, 0),
		op6(opRecycle, 1, 2, 0, 4, 0), // refused: the fresh id is a held spare
	), 0, 0, 1, 1, 0},
	{"refusals at three boundaries", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opAddSpare, 1, 0, 0, 4, 0), // duplicate
		op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 0, 0, 0), // zero bytes
		op6(opAddRecord, 1, 0, 0, 3, 0), // negative
		// early seal, so the cuts below reach the size window rather than the
		// full-seal refusal; the segment holds no records, so nothing but the
		// window itself can refuse the cuts.
		op6(opSeal, 1, 0, 0, 4, 1), op6(opShrink, 1, 0, 0, 0, 0), // zero: no size
		op6(opShrink, 1, 0, 0, 2, 0), // one past the segment's size
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"null id refusals", slices.Concat(
		op6(opAddSpare, 0, 0, 0, 4, 0), // the null id: names no segment
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 4, 0),
		op6(opRecycle, 1, 0, 0, 4, 0), // fresh id 0: refused
	), 0, 0, 1, 1, 0},
	{"shrink of an Active segment", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opShrink, 1, 0, 0, 4, 5), // refused: a stream still appends
		op6(opEnd, 1, 0, 0, 4, 0), op6(opShrink, 1, 0, 0, 4, 5),
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"double seal", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 4, 0), op6(opSeal, 1, 0, 0, 4, 0), // refused: already sealed
		op6(opEnd, 1, 0, 0, 4, 0), // refused: a sealed segment is never ended
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"records within the segment's size", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 0, 4, 5),
		op6(opAddRecord, 1, 0, 0, 1, 0), // the cap itself: refused, it does not fit
		op6(opSeal, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 0, 4, 1), // refused: a full seal takes no records
		op6(opShrink, 1, 0, 0, 4, 2),    // refused: below the bytes the segment holds
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"an early seal is never recycled", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 2, 4, 99),
		op6(opSeal, 1, 0, 0, 5, 1),    // early: the truncation has not landed
		op6(opRecycle, 1, 2, 0, 4, 0), // refused at the cap: the seal never said full
		op6(opShrink, 1, 0, 0, 4, 5),  // refused: below the records the segment holds
		op6(opRecycle, 1, 2, 0, 4, 0), // still refused: an early seal is never recycled
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 1, 0},
	{"a full seal has no cut behind it", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 4, 0),   // full: the cap is the file, nothing was truncated
		op6(opShrink, 1, 0, 0, 4, 5), // refused: no cut exists to account
		op6(opRecycle, 1, 2, 0, 4, 0),
	), 0, 1, 1, 1, 0},
	{"a spare drops in one step", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0),
		op6(opDropSpare, 1, 0, 0, 4, 0), // the failed make's reservation, back
		op6(opDropSpare, 1, 0, 0, 4, 0), // refused: the drop already took it
	), 0, 0, 0, 0, 1},
	{"the drop of a segment and of nothing", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0),
		op6(opDropSpare, 0, 0, 0, 4, 0), // absent: counted, like the retire of nothing
		op6(opTake, 1, 0, 0, 4, 0),
		op6(opDropSpare, 1, 0, 0, 4, 0), // refused: a segment goes out by Retire, not here
	), 0, 0, 1, 0, 0},
	{"a record into a closed segment is refused", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opEnd, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 3, 4, 30), // refused: the segment no longer appends
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"seal and end of an ended segment", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opEnd, 1, 0, 0, 4, 0), op6(opSeal, 1, 0, 0, 4, 0), // refused: never sealed again
		op6(opEnd, 1, 0, 0, 4, 0), // refused: never ended twice
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"recycle of an Active and of an ended segment", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opRecycle, 1, 2, 0, 4, 0),                            // refused: a stream still appends
		op6(opEnd, 1, 0, 0, 4, 0), op6(opRecycle, 1, 2, 0, 4, 0), // refused: only a seal recycles
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"the fresh id is the old one, or a segment the set holds", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opAddSpare, 2, 0, 0, 4, 0),
		op6(opTake, 1, 0, 0, 4, 0), op6(opTake, 2, 0, 0, 4, 0),
		op6(opSeal, 1, 0, 0, 4, 0),
		op6(opRecycle, 1, 1, 0, 4, 0), // refused: the fresh id is the recycled segment itself
		op6(opRecycle, 1, 2, 0, 4, 0), // refused: the fresh id is a segment of the set
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 2, 1, 0},
	{"an ended segment keeps the records it holds", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 1, 4, 200), // 201 bytes of one share
		op6(opEnd, 1, 0, 0, 4, 0),
		op6(opShrink, 1, 0, 0, 4, 100), // refused: 51 is below the 201 held
		op6(opShrink, 1, 0, 0, 4, 200), // the floor: 201, the bytes the records hold
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"a record the size of the segment fits", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 1, 1, 0), // the cap itself: accepted, a fit lands on the size
		op6(opAddRecord, 1, 0, 1, 4, 0), // refused: one byte past the size
		op6(opEnd, 1, 0, 0, 4, 0), op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"the cut at the held bytes, both sides", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), op6(opTake, 1, 0, 0, 4, 0),
		op6(opAddRecord, 1, 0, 1, 4, 199), // 200 bytes of one share
		op6(opEnd, 1, 0, 0, 4, 0),
		op6(opShrink, 1, 0, 0, 4, 198), // refused: 199 is below the 200 held
		op6(opShrink, 1, 0, 0, 4, 199), // accepted: the cut lands on the bytes held
		op6(opRetire, 1, 0, 0, 4, 0),
	), 1, 0, 1, 0, 0},
	{"add a spare past the maximum", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), // no maximum: the first spare passes
		op6(opAddSpare, 2, 0, 0, 4, 1), // maximum one cap: refused, the footprint holds one
		op6(opAddSpare, 3, 0, 0, 4, 1), // and again, still one cap missing
	), 0, 0, 0, 0, 0},
	{"add a spare at exactly the maximum", slices.Concat(
		op6(opAddSpare, 1, 0, 0, 4, 0), // no maximum: passes
		op6(opAddSpare, 2, 0, 0, 4, 2), // maximum two caps: accepted, exactly on it
		op6(opTake, 2, 0, 0, 4, 0),
	), 0, 0, 1, 0, 0},
}

// FuzzSet drives the segment set against its oracle: the same operation
// sequence goes to both, and after every single one the ledger must agree
// with the oracle on the error class, on every counter, on every share's
// record bytes and on the footprint check — and the footprint never dips
// below zero.
func FuzzSet(f *testing.F) {
	// One op is six bytes: kind, segment id, fresh id, share tag, a shape
	// selector for the size and the size byte itself — the fields the decoder
	// reads. A size shape of 4 is a plain size of n+1 under the cap; 0 is
	// zero, 1 the cap, 2 one past, 3 a negative.
	for _, s := range corpus {
		f.Add(s.ops)
	}

	segCap := int64(4096)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := NewSet(segCap, nil)
		o := newOracle(segCap)

		for _, op := range decode(data, segCap) {
			want := o.apply(op)
			var got error
			switch op.kind {
			case opAddSpare:
				got = s.AddSpare(op.id, op.max)
			case opTake:
				got = s.Take(op.id)
			case opSeal:
				got = s.Seal(op.id, op.full)
			case opEnd:
				got = s.End(op.id)
			case opShrink:
				got = s.Shrink(op.id, op.n)
			case opAddRecord:
				got = s.AddRecord(op.id, op.tag, op.n)
			case opClaim:
				// Claim answers with a boolean, which the oracle encodes
				// as an error class; carrying the oracle's class over to
				// the check below reads the same fact back.
				if s.Claim(op.id) != (want == nil) {
					t.Fatalf("Claim(%d): the oracle holds the claim %v, the set does not", op.id, want == nil)
				}
				got = want
			case opRelease:
				s.Release(op.id)
			case opRetire:
				_, got = s.Retire(op.id)
			case opRecycle:
				got = s.Recycle(op.id, op.fresh)
			case opDropSpare:
				_, got = s.DropSpare(op.id)
			}

			if !sameErrClass(got, want) {
				t.Fatalf("op %d (%d): the set returned %v, the oracle %v", op.kind, op.id, got, want)
			}
			c := s.Counters()
			if c.UsedBytes < 0 {
				t.Fatalf("op %d (%d): the footprint fell to %d", op.kind, op.id, c.UsedBytes)
			}
			if err := s.Check(); err != nil {
				t.Fatalf("op %d (%d): %v", op.kind, op.id, err)
			}
			if got := o.usedBytes(); got != c.UsedBytes {
				t.Fatalf("op %d (%d): the footprint is %d, the oracle's %d", op.kind, op.id, c.UsedBytes, got)
			}
			if got := o.countSegments(); got != c.Segments {
				t.Fatalf("op %d (%d): the set holds %d segments, the oracle %d", op.kind, op.id, c.Segments, got)
			}
			if got := o.countSealed(); got != c.Sealed {
				t.Fatalf("op %d (%d): the set holds %d sealed, the oracle %d", op.kind, op.id, c.Sealed, got)
			}
			if got := int64(len(o.spares)); got != c.Spares {
				t.Fatalf("op %d (%d): the set holds %d spares, the oracle %d", op.kind, op.id, c.Spares, got)
			}
			if c.DoubleRetires != o.doubleRetires {
				t.Fatalf("op %d (%d): the set counted %d double retires, the oracle %d", op.kind, op.id, c.DoubleRetires, o.doubleRetires)
			}
			for tag, want := range o.tags {
				if got := s.ShareBytes(tag); got != want {
					t.Fatalf("op %d (%d): tag %d holds %d, the oracle %d", op.kind, op.id, tag, got, want)
				}
			}
			for _, tag := range []core.ShareTag{1, 2, 3} {
				if _, ok := o.tags[tag]; !ok {
					if got := s.ShareBytes(tag); got != 0 {
						t.Fatalf("op %d (%d): the oracle holds none of tag %d, the set %d", op.kind, op.id, tag, got)
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
