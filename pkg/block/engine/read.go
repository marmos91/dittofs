package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Read fills dst from off of file f of share s and answers the bytes filled
// in file order, then the error. It asks the journal first, and block
// metadata only about each extent the journal does not hold, one covering
// lookup per extent; it keeps nothing of the resolution past the call. Only
// a hole or a zero ref reads as zeros. Past the end of file, the pending
// operations applied, the read is short. A read that fails part-way answers
// the bytes filled before the first part that failed. A range below zero or
// past the largest offset is ErrInvalid.
func (e *Engine) Read(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, dst []byte) (int, error) {
	if off < 0 || int64(len(dst)) > math.MaxInt64-off {
		return 0, vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return 0, err
	}
	if fe := e.files.lockOf(f); fe != nil {
		err := fe.refuseRead(off, off+int64(len(dst)))
		e.files.unlock(f, fe)
		if err != nil {
			return 0, err
		}
	}
	// decision: a read refused before it starts, out of range, of an unbound
	// share or behind a clone, is counted in neither series: it reached no
	// content, so it has no class among engine_reads_total's, and its time is
	// no read's latency. Count it once RFC 8 §14 names a series for refusals.
	start := e.d.Clock.Elapsed()
	r := reader{e: e, b: b, s: s, f: f}
	n, err := r.window(ctx, off, dst)
	if errors.Is(err, vfserr.ErrCorrupt) {
		e.reportLoss(f, off, int64(len(dst)), "corrupt")
	}
	e.reads.record(r.class(err), e.d.Clock.Elapsed()-start)
	return n, err
}

// reader is one read in progress: its file, and whether any part of it read
// as zeros.
type reader struct {
	e     *Engine
	b     *binding
	s     entity.ShareID
	f     entity.FileID
	zeros bool
}

// window reads dst at off, passes until one needs no asking again. Each
// pass reads the whole window at one journal answer, so a read never mixes
// bytes from two of them.
//
// decision: existence keeps landing above the journal's answer only while
// writes race this read, so the window is asked again until the read's
// deadline, by the engine's clock, and then the read is told to retry rather
// than failed or answered stale. A read with no deadline whose writes never
// settle is asked again until its caller cancels. A context that ended is
// answered its own error. Overturned if a read is ever seen to end this way
// under a load whose writes have stopped.
func (r *reader) window(ctx context.Context, off int64, dst []byte) (int, error) {
	for {
		r.zeros = false
		n, again, err := r.pass(ctx, off, dst)
		if !again {
			return n, err
		}
		if dl, ok := ctx.Deadline(); ok && !r.e.d.Clock.Now().Before(dl) {
			return 0, &vfserr.Delay{Cause: vfserr.DelayConflict}
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
}

// pass reads dst at off once: the journal's answer, then each extent it
// reports missing resolved in file order. again: a part's existence landed
// after the journal answered, and the window is to be asked again.
func (r *reader) pass(ctx context.Context, off int64, dst []byte) (n int, again bool, err error) {
	_, missing, _, asOf, err := r.b.h.ReadAt(ctx, fileID(r.f), off, dst)
	if err != nil {
		return 0, false, r.e.journalErr(err)
	}
	if len(missing) == 0 {
		return len(dst), false, nil
	}
	ops := r.e.files.opsAt(r.f, asOf)
	at := entity.JournalVersion(asOf.Bytes())
	for _, m := range missing {
		parts, err := r.e.resolve(ctx, r.s, r.f, m, ops, at)
		if err != nil {
			return int(m.Off - off), false, err
		}
		for _, p := range parts {
			n, end := int(p.off-off), int(p.end-off)
			switch p.res {
			case resZeros:
				clear(dst[n:end])
				r.zeros = true
			case resEOF:
				return n, false, nil
			case resLost:
				r.e.reportLoss(r.f, p.off, p.end-p.off, "lost")
				return n, false, fmt.Errorf("engine: file %v [%d, %d) lost: %w", r.f, p.off, p.end, vfserr.ErrLost)
			case resCarved:
				// decision: a carved run fails the read unavailable, as an
				// unreachable remote does, because nothing here fetches a
				// chunk: no code of this engine writes a ref, so one is
				// another writer's. Withdraw it once the fetch is built.
				return n, false, fmt.Errorf("engine: file %v [%d, %d) is carved and not fetched: %w", r.f, p.off, p.end, vfserr.ErrUnavailable)
			case resReask:
				return 0, true, nil
			}
		}
	}
	return len(dst), false, nil
}

// opsAt is f's pending operations at or below asOf, the ones the journal had
// answered for, copied out under the entry's lock.
//
// decision: every operation at or below the journal's answer is taken to be
// in the entry already. A write is inserted after its journal step, which is
// harmless: a read between the two misses it and reads the older state. A
// removal stops the journal holding its extent at once, so a read between its
// journal step and its insert finds the extent missing with no pending
// removal over it and reads it lost: a truncate's or a deallocate's operation
// must be in the entry before, or atomically with, its journal step.
// Withdraw it if a read ever asks again while a removal is being staged.
func (fs *files) opsAt(f entity.FileID, asOf journal.Version) []pendingOp {
	fe := fs.lockOf(f)
	if fe == nil {
		return nil
	}
	var ops []pendingOp
	for _, op := range fe.ops {
		if !op.dropped && op.version.Compare(asOf) <= 0 {
			ops = append(ops, op)
		}
	}
	fs.unlock(f, fe)
	return ops
}

// res is how one part of a missing extent is answered.
type res uint8

const (
	resZeros  res = iota + 1 // a hole or a zero ref
	resReask                 // existence landed after the journal answered: ask it again
	resLost                  // the journal had answered for it, and does not hold it
	resCarved                // a chunk holds it
	resEOF                   // past the end of file
)

// part is one run of a missing extent and its answer.
type part struct {
	off, end int64
	res      res
}

// resolve answers missing extent m of f: block metadata's covering spans
// with ops, the pending operations the journal answered for at asOf,
// applied over them, each run answered by resolution. Past the end of the
// covering lookup's answer is the committed end of file.
func (e *Engine) resolve(ctx context.Context, s entity.ShareID, f entity.FileID, m journal.Extent, ops []pendingOp, asOf entity.JournalVersion) ([]part, error) {
	end := m.Off + m.Len
	a := allocation{eof: math.MaxInt64}
	var applied entity.JournalVersion
	var spans []entity.Span
	covered := m.Off
	for sp, err := range e.d.Content.Covering(ctx, s, f, m.Off, m.Len) {
		if err != nil {
			return nil, e.storeErr(err)
		}
		applied = sp.Applied
		spans = append(spans, sp)
		a.segs = append(a.segs, seg{off: sp.Offset, end: sp.End(), hole: sp.Class == entity.Hole, sp: sp})
		covered = sp.End()
	}
	if covered < end {
		a.eof = covered
	}
	for _, op := range ops {
		a.apply(op)
	}
	var parts []part
	at, stop := m.Off, min(end, a.eof)
	for _, sg := range a.segs {
		lo, hi := max(sg.off, at), min(sg.end, stop)
		if lo >= hi {
			continue
		}
		if lo > at {
			break // no run covers at: nothing is there to read
		}
		sp := sg.sp
		if !sg.hole && sp.Class == 0 {
			// A pending write's run: uncarved, under the newest overwrite
			// version the store holds over it.
			//
			// decision: the class of a committed run a pending write is
			// painted over is dropped, so a carved run whose write is still
			// in the entry reads as uncarved, and lost if its applied is at
			// or below asOf. Safe while an operation leaves the entry before
			// its extent can be offloaded and released. Withdraw it if a
			// carve can ever run ahead of the commit that drops the write's
			// operation.
			sp = entity.Span{Class: entity.Uncarved, Applied: applied}
			for _, c := range spans {
				if c.Offset < hi && lo < c.End() {
					sp.Overwrite = entity.MaxVersion(sp.Overwrite, c.Overwrite)
				}
			}
		}
		parts = append(parts, part{lo, hi, resolution(sg.hole, sp, asOf)})
		at = hi
	}
	if at < end {
		parts = append(parts, part{at, end, resEOF})
	}
	return parts, nil
}

// resolution is how a run below the end of file is answered: a hole or a
// zero ref reads as zeros; a carved run is a chunk's; an uncarved run whose
// file's applied or whose own overwrite version is above asOf was written
// after the journal answered, and is asked again; any other uncarved run is
// lost.
//
// ponytail: this table and the resolve around it are checked by table tests
// over a stub covering lookup, not by fuzz or a model. Add a fuzz target
// against a reference model, asserting zeros only for a hole or a zero ref,
// when a table check misses a bug a random input would have found.
func resolution(hole bool, sp entity.Span, asOf entity.JournalVersion) res {
	switch {
	case hole, sp.Class == entity.Carved && sp.Ref.IsZeroRef():
		return resZeros
	case sp.Class == entity.Carved:
		return resCarved
	case asOf.Less(sp.Applied), asOf.Less(sp.Overwrite):
		return resReask
	}
	return resLost
}

// readClass is a read's outcome, as engine_reads_total labels it.
type readClass uint8

const (
	classJournal readClass = iota
	classHole
	classLost
	classCorrupt
	classUnavailable
	classNone // counted under no class
	readClasses
)

var readClassNames = [readClasses]string{"journal", "hole", "lost", "corrupt", "unavailable", ""}

// class is the read's outcome once it answered err.
//
// decision: a read is counted once, by its outcome: a failed one by its
// failing part's class, lost, corrupt or unavailable; else hole when any
// part read as zeros; else journal. A read that ends with any other error,
// its context's own or a retry-later, is counted under no class, as it says
// nothing of where the content is. Revisit if those reads need a series.
func (r *reader) class(err error) readClass {
	switch {
	case err == nil && r.zeros:
		return classHole
	case err == nil:
		return classJournal
	case errors.Is(err, vfserr.ErrLost):
		return classLost
	case errors.Is(err, vfserr.ErrCorrupt):
		return classCorrupt
	case errors.Is(err, vfserr.ErrUnavailable):
		return classUnavailable
	}
	return classNone
}

// readSeries is engine_reads_total by class and engine_read_seconds, its
// count and sum.
//
// ponytail: engine_read_seconds is a count and a sum, not the histogram RFC 8
// §14 names, so no latency quantile can be read from it. Make it a histogram
// once the metrics registry exists.
type readSeries struct {
	byClass [readClasses]atomic.Int64
	count   atomic.Int64
	total   atomic.Int64 // nanoseconds
}

func (s *readSeries) record(c readClass, d time.Duration) {
	s.byClass[c].Add(1)
	s.count.Add(1)
	s.total.Add(int64(d))
}

// reportLoss logs a lost or corrupt extent of f at Error, the first time it
// is reported. The extent is keyed as the read saw it, the part it clipped
// or a corrupt read's whole window, so one lost extent read through two
// different windows logs twice.
func (e *Engine) reportLoss(f entity.FileID, off, n int64, what string) {
	if e.lost.first(lostKey{f, off, n}) {
		e.d.Logger.Error("read failed: content "+what, "file", f, "off", off, "len", n)
	}
}

type lostKey struct {
	f      entity.FileID
	off, n int64
}

// lostLogMax is the most extents lostLog remembers.
const lostLogMax = 4096

// lostLog is the node's memory of the extents whose loss it logged.
//
// ponytail: one node-wide set of the last 4096 extents reported, the oldest
// forgotten first, so an extent read again after 4096 later losses logs
// again; and each is keyed as the read clipped it, so one lost extent read
// through two different windows logs twice. Key it per file, in the file's
// entry, by the journal's lost extent, if logs show repeats.
type lostLog struct {
	mu    sync.Mutex
	seen  map[lostKey]struct{}
	order []lostKey // a ring once full; next is its oldest
	next  int
}

// first records k and reports whether it was not already remembered.
func (l *lostLog) first(k lostKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[k]; ok {
		return false
	}
	if l.seen == nil {
		l.seen = map[lostKey]struct{}{}
	}
	if len(l.order) < lostLogMax {
		l.order = append(l.order, k)
	} else {
		delete(l.seen, l.order[l.next])
		l.order[l.next] = k
		l.next = (l.next + 1) % lostLogMax
	}
	l.seen[k] = struct{}{}
	return true
}
