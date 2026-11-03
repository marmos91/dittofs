package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

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
	var ops []pendingOp
	if fe := e.files.lockOf(f); fe != nil {
		ops = slices.Clone(fe.ops)
		e.files.unlock(f, fe)
	}
	cursor, class := off, -1 // class: -1 unset, 0 data, 1 hole
	for {
		run, hole, err := e.d.Existence.Allocation(ctx, s, f, cursor)
		if err != nil {
			return entity.Span{}, storeErr(err)
		}
		a := allocation{eof: math.MaxInt64}
		end := run.End()
		if run.Length == 0 {
			a.eof, end = cursor, math.MaxInt64 // nothing committed from cursor on
		} else {
			a.segs = []seg{{cursor, end, hole}}
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
		runEnd := a.runFrom(at)
		if run.Length > 0 {
			// Past the committed run, what the store holds is unknown here,
			// the end of file included: the next pass reads it.
			runEnd = min(runEnd, end)
		}
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

// seg is a run of one class: hole or data.
type seg struct {
	off, end int64
	hole     bool
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
	o, e := op.extent.Off, op.extent.Off+op.extent.Len
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

// paint makes [o, e) all of one class.
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
			out = append(out, seg{s.off, o, s.hole})
		}
		if s.end > e {
			out = append(out, seg{e, s.end, s.hole})
		}
	}
	out = append(out, seg{o, e, hole})
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
// else unavailable.
func storeErr(err error) error {
	switch {
	case errors.Is(err, entity.ErrStale):
		return fmt.Errorf("engine: block metadata: %v: %w", err, vfserr.ErrStale)
	case errors.Is(err, entity.ErrDelay):
		return &vfserr.Delay{Cause: vfserr.DelayClone}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return fmt.Errorf("engine: block metadata: %v: %w", err, vfserr.ErrUnavailable)
}
