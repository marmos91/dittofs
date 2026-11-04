package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Allocation is the longest run from off of file f of share s that is all
// hole or all data, the file's pending operations applied to block
// metadata's committed runs: a write makes data, a deallocate a hole, and a
// truncate the end of file. It never asks the journal, which cannot tell a
// hole from an evicted extent. Past the end of file the span is empty.
//
// decision: a run is answered Hole for a hole and Uncarved for data, with no
// ref, since a caller reads only hole against data. Answer Carved with its
// ref once a caller needs where the data lives.
func (e *Engine) Allocation(ctx context.Context, s entity.ShareID, f entity.FileID, off int64) (entity.Span, error) {
	if off < 0 {
		return entity.Span{}, vfserr.ErrInvalid
	}
	if _, err := e.bindingOf(s); err != nil {
		return entity.Span{}, err
	}
	limit := int64(math.MaxInt64) // the start of the first destination extent a clone holds past off
	if fe := e.files.lockOf(f); fe != nil {
		err := fe.refuseRead(off, off+1)
		for _, r := range fe.regs {
			if r.dst && r.off > off {
				limit = min(limit, r.off)
			}
		}
		e.files.unlock(f, fe)
		if err != nil {
			return entity.Span{}, err
		}
	}
	sp, err := e.spanFrom(ctx, s, f, off)
	if err == nil && sp.End() > limit {
		sp.Length = limit - off // the span stops where a clone's destination extent starts
	}
	return sp, err
}

// spanFrom is Allocation's span from off, f's pending operations read at each
// pass after the store.
func (e *Engine) spanFrom(ctx context.Context, s entity.ShareID, f entity.FileID, off int64) (entity.Span, error) {
	cursor, class := off, -1 // class: -1 unset, 0 data, 1 hole
	// ponytail: one store call per committed run a pending write spans, each
	// loading the store's window, so N alternating runs under one write cost
	// N+1 calls. Jump the cursor past the extent a pending write covers once
	// a profile shows Allocation over fragmented files.
	for {
		if err := ctx.Err(); err != nil {
			return entity.Span{}, err
		}
		gen := e.files.removals.Load()
		run, hole, err := e.d.Existence.Allocation(ctx, s, f, cursor)
		if err != nil {
			return entity.Span{}, e.storeErr(err)
		}
		// The pending operations are read after the store, and the pass is
		// read again if any committed operation left an entry between the
		// two reads: then every operation the store does not hold is in the
		// entry, and replaying one it already holds changes nothing. That
		// holds only because operations leave an entry in version order,
		// oldest first, which every commit path must keep.
		//
		// decision: each pass reads the store and the entry afresh, so a
		// commit landing between two passes is seen by the later one only; a
		// run stitched across passes is right at each pass but not one
		// snapshot, and can answer a hole over data written during the call:
		// committed hole [0, 4k) and data [4k, 8k), the first pass sees the
		// hole, then a write to [0, 4k) and a deallocate of [4k, 8k) are
		// staged, and the second pass sees a hole at 4k, so Allocation(0)
		// answers hole [0, 8k). Both operations are concurrent with the call.
		// Hold one version across passes, as Read's asOf does, if a caller
		// needs the run as of one instant.
		var ops []pendingOp
		if fe := e.files.lockOf(f); fe != nil {
			ops = slices.DeleteFunc(slices.Clone(fe.ops), func(op pendingOp) bool { return op.dropped })
			e.files.unlock(f, fe)
		}
		if e.files.removals.Load() != gen {
			continue // an operation committed mid-pass: read both again
		}
		a := allocation{eof: math.MaxInt64}
		end := run.End()
		if run.Length == 0 {
			a.eof, end = cursor, math.MaxInt64 // nothing committed from cursor on
		} else {
			a.segs = []seg{{off: cursor, end: end, hole: hole}}
		}
		for _, op := range ops {
			a.apply(op)
		}
		at, h, ok := a.classAt(cursor)
		if !ok {
			return span(off, cursor, class == 1), nil
		}
		if class == -1 {
			class = b2i(h)
		} else if b2i(h) != class {
			return span(off, cursor, class == 1), nil
		}
		// Past the committed run, what the store holds is unknown here, the
		// end of file included: the next pass reads it.
		runEnd := min(a.runFrom(at), end)
		if runEnd < end || runEnd >= a.eof {
			return span(off, min(runEnd, a.eof), class == 1), nil
		}
		cursor = runEnd // the committed run ended; the next may continue this class
	}
}

func span(off, end int64, hole bool) entity.Span {
	c := entity.Uncarved
	if hole || end == off {
		c = entity.Hole
	}
	return entity.Span{Extent: entity.Extent{Offset: off, Length: end - off}, Class: c}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// seg is a run of one class: hole or data. sp is the store's span it came
// from, which Read classifies; a run a pending operation painted has none.
type seg struct {
	off, end int64
	hole     bool
	sp       entity.Span
}

// allocation is what a file holds from a cursor on: runs of hole and data,
// and where it ends.
type allocation struct {
	segs []seg
	eof  int64 // math.MaxInt64: past every run here
}

// apply paints op over the runs: a write makes data, extending the file and
// leaving a hole between its end and the write; a deallocate makes a hole
// within the file; a truncate moves the end of file, a hole filling any
// extension.
func (a *allocation) apply(op pendingOp) {
	o, e := op.extent.Off, opEnd(op.extent)
	switch op.kind {
	case opWrite:
		if o > a.eof {
			a.paint(a.eof, o, true)
		}
		a.paint(o, e, false)
		if a.eof != math.MaxInt64 {
			a.eof = max(a.eof, e)
		}
	case opDeallocate:
		a.paint(o, min(e, a.eof), true)
	case opTruncate:
		if o > a.eof {
			a.paint(a.eof, o, true)
		}
		a.eof = o
	}
}

// opEnd is where x ends, capped so a length to the end of file does not wrap.
func opEnd(x journal.Extent) int64 {
	return entity.Extent{Offset: x.Off, Length: x.Len}.End()
}

// paint makes [o, e) all of one class.
//
// ponytail: a run paint splits keeps its store span whole, Extent and Ref
// unclipped, which nothing reads while a carved run answers unavailable. Clip
// the span and its Ref to the piece once the fetch reads a carved run's Ref.
func (a *allocation) paint(o, e int64, hole bool) {
	if o >= e {
		return
	}
	out := make([]seg, 0, len(a.segs)+2)
	for _, s := range a.segs {
		if s.end <= o || s.off >= e {
			out = append(out, s)
			continue
		}
		if s.off < o {
			l := s
			l.end = o
			out = append(out, l)
		}
		if s.end > e {
			r := s
			r.off = e
			out = append(out, r)
		}
	}
	out = append(out, seg{off: o, end: e, hole: hole})
	slices.SortFunc(out, func(x, y seg) int { return cmp.Compare(x.off, y.off) })
	a.segs = out
}

// classAt is the run holding at, before the end of file: its index, and
// whether it is a hole. ok false: at is past the end of file, or in no run.
func (a *allocation) classAt(at int64) (i int, hole, ok bool) {
	if at >= a.eof {
		return 0, false, false
	}
	for i, s := range a.segs {
		if s.off <= at && at < s.end {
			return i, s.hole, true
		}
	}
	return 0, false, false
}

// runFrom is where the class of run i stops: the end of the last adjoining
// run of its class.
func (a *allocation) runFrom(i int) int64 {
	end, hole := a.segs[i].end, a.segs[i].hole
	for _, s := range a.segs[i+1:] {
		if s.off != end || s.hole != hole {
			break
		}
		end = s.end
	}
	return end
}

// storeErr is the content methods' answer for an error the store returned:
// its ErrStale, the file gone, as ErrStale; its ErrDelay, which block
// metadata's reads answer only for an extent a running clone still writes,
// as a Delay with DelayClone; the context's own error as itself; anything
// else unavailable, logged at Error.
func (e *Engine) storeErr(err error) error {
	switch {
	case errors.Is(err, entity.ErrStale):
		return fmt.Errorf("engine: block metadata: %v: %w", err, vfserr.ErrStale)
	case errors.Is(err, entity.ErrDelay):
		return &vfserr.Delay{Cause: vfserr.DelayClone}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	e.d.Logger.Error("block metadata call failed", "err", err)
	return fmt.Errorf("engine: block metadata: %v: %w", err, vfserr.ErrUnavailable)
}
