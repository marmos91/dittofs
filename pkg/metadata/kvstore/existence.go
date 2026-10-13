package kvstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Existence is the write path's view of block metadata: what of a file's
// content exists, committed around the protocol's stability points.
type Existence struct{ s *Store }

func (s *Store) Existence() Existence { return Existence{s} }

// OpKind is a journal operation an existence commit applies.
type OpKind uint8

const (
	// OpWrite is a write of Extent.
	OpWrite OpKind = iota + 1
	// OpGrow is a truncate up to Size. A truncate down is a removal.
	OpGrow
)

// PendingOp is one journal operation of a file whose existence is not yet
// committed, at the version the journal assigned it.
type PendingOp struct {
	Version entity.JournalVersion
	Kind    OpKind
	Extent  entity.Extent // OpWrite
	Size    int64         // OpGrow
	Time    time.Time     // the operation's modify and change time
}

// PendingExistence is one file's operations to commit, in version order.
type PendingExistence struct {
	Share entity.ShareID
	File  entity.FileID
	Epoch uint64 // the primary's epoch the journal holds them under
	Ops   []PendingOp
}

// ErrNotContent is an existence commit for a file that holds no content.
var ErrNotContent = errors.New("kvstore: file holds no content")

// Commit applies pending existence for many files, as few transactions as
// the key budget allows: one for the whole batch when it fits. For each file
// it grows size, shrinks the holes a write fills, makes a hole of the gap a
// write past size or a truncate up leaves, writes an overwrite record over
// every run of a write that covers content committed before the commit, and
// sets the times and applied. Operations at or below applied are skipped, so
// a replay after a crash is harmless. A file whose operations need more than
// the budget commits over several transactions in version order, applied
// reaching only the newest operation each wholly applied.
//
// Each file's existence fence and record are read tracked, so the commit
// conflicts with a removal, a release or a takeover. A file that fails on its
// own — released, holding no content, operations out of order — is reported
// in the returned map and fails no other.
//
// ponytail: a single operation that alone needs more than the budget — a
// write over 10^5 holes — still commits in one transaction, which the
// backend may refuse; split it by extent when one shows.
func (e Existence) Commit(ctx context.Context, pending []PendingExistence) (map[entity.FileID]error, error) {
	failed := map[entity.FileID]error{}
	next := make([]int, len(pending)) // per file, the first op not yet committed
	for {
		var progress []int
		err := e.s.kv.Update(ctx, func(tx kv.Txn) error {
			clear(failed)
			progress = slices.Clone(next)
			budget := e.s.k
			for i, p := range pending {
				if progress[i] == len(p.Ops) || failed[p.File] != nil {
					continue
				}
				n, used, err := commitExistence(tx, p, progress[i], budget, budget == e.s.k)
				if ff, ok := err.(fileFailure); ok {
					failed[p.File] = ff.err
					continue
				}
				if err != nil {
					return err
				}
				progress[i], budget = n, budget-used
				if n < len(p.Ops) {
					break // out of budget: the rest go in the next transaction
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		done := true
		for i, p := range pending {
			if failed[p.File] == nil && progress[i] < len(p.Ops) {
				done = false
			}
		}
		if done {
			return failed, nil
		}
		if slices.Equal(progress, next) {
			return nil, fmt.Errorf("%w: an existence commit made no progress", ErrInconsistent)
		}
		next = progress
	}
}

// fileFailure is a failure of one file that fails no other.
type fileFailure struct{ err error }

func (f fileFailure) Error() string { return f.err.Error() }

func fileFailed(err error) error { return fileFailure{err} }

// fileKeys is what one file's existence commit writes besides its holes and
// overwrite records: the File record and the existence fence.
const fileKeys = 2

// commitExistence applies p's operations from first on, while the keys they
// write fit budget, and writes the result. It returns the index of the first
// op left for a later transaction and the keys written. In a transaction
// that has written nothing yet, fresh, the first operation is applied
// whatever it costs, so every transaction makes progress.
func commitExistence(tx kv.Txn, p PendingExistence, first, budget int, fresh bool) (int, int, error) {
	fenced, err := existenceFence(tx, p)
	if err != nil {
		return 0, 0, err
	}
	f, err := fileIn(tx, p.Share, p.File)
	if err != nil {
		return 0, 0, fileFailed(err)
	}
	if f.Type != entity.Regular && f.Type != entity.Stream {
		return 0, 0, fileFailed(ErrNotContent)
	}
	ms, err := masksOf(tx, p.Share, p.File)
	if err != nil {
		return 0, 0, err
	}
	st := &existState{tx: tx, share: p.Share, file: p.File, size: f.Size, f: f, orig: f, was: f.Version, fenced: fenced, epoch: p.Epoch, ms: ms,
		holes: map[int64]hole{}, stored: map[int64]hole{}, ovs: map[int64]overwrite{},
		dirtyH: map[int64]bool{}, dirtyO: map[int64]bool{}}
	i := first
	for ; i < len(p.Ops); i++ {
		op := p.Ops[i]
		if i > 0 && !p.Ops[i-1].Version.Less(op.Version) {
			return 0, 0, fileFailed(merrs.NewInvalidArgumentError("operations not in version order"))
		}
		if !st.f.Applied.Less(op.Version) {
			continue // already committed
		}
		cost, err := st.bound(op)
		if err != nil {
			return 0, 0, err
		}
		if (i > first || !fresh) && st.keys()+cost > budget {
			break
		}
		if err := st.apply(op); err != nil {
			return 0, 0, err
		}
	}
	if st.keys() == fileKeys && st.f.Applied == f.Applied {
		return i, 0, nil // nothing above applied: nothing to write
	}
	return i, st.keys(), st.flush()
}

// existenceFence reads the file's existence fence, tracked, reporting
// whether the file has one; the commit writes it at its epoch when not. On a
// single node its epoch is not compared.
func existenceFence(tx kv.Txn, p PendingExistence) (bool, error) {
	_, ok, err := get(tx, existenceFenceKey(p.Share, p.File))
	return ok, err
}

// existState is one file's existence as a commit changes it: the File, and
// the holes and overwrite records over the extents its operations touch,
// loaded on first use.
type existState struct {
	tx          kv.Txn
	share       entity.ShareID
	file        entity.FileID
	f           entity.File
	fenced      bool                  // the file has an existence fence
	was         entity.JournalVersion // the Version stored before the commit
	orig        entity.File           // the record as stored before the commit
	ms          masks
	epoch       uint64
	size        int64          // size before the commit: below it, outside stored holes, is committed content
	holes       map[int64]hole // by start, as the commit leaves them; absent and dirty means deleted
	stored      map[int64]hole // as stored before the commit
	ovs         map[int64]overwrite
	dirtyH      map[int64]bool
	dirtyO      map[int64]bool
	loadedHoles []entity.Extent // windows already read
	loadedOvs   []entity.Extent
}

func (s *existState) keys() int { return fileKeys + len(s.dirtyH) + len(s.dirtyO) }

// window is what an operation reads around: its extent and a byte either
// side, for the holes it may join, reaching back to the end of file when it
// leaves a gap there. Nothing lies past the end of file to read.
func (s *existState) window(op PendingOp) entity.Extent {
	lo, hi := s.f.Size, s.f.Size
	switch op.Kind {
	case OpWrite:
		lo, hi = min(lo, op.Extent.Offset), op.Extent.End()
	case OpGrow:
		hi = op.Size
	}
	return entity.ExtentOf(max(0, lo-1), hi+1)
}

// bound is at most the keys op adds to the commit: every record over its
// window, and the pieces a split or a new record can add.
func (s *existState) bound(op PendingOp) (int, error) {
	w := s.window(op)
	if err := s.load(w); err != nil {
		return 0, err
	}
	n := 4
	for _, h := range s.holes {
		if h.Overlaps(w) {
			n++
		}
	}
	for _, o := range s.ovs {
		if o.Overlaps(w) {
			n++
		}
	}
	return n, nil
}

// load reads the holes and overwrite records over w that are not already
// in memory. A record the commit already changed or deleted keeps its
// in-memory state. A record a removal not done masks counts as absent, so
// the commit never cuts one, and writing a record at its key replaces it.
func (s *existState) load(w entity.Extent) error {
	if !covered(s.loadedHoles, w) {
		hs, err := holesOver(s.tx, s.share, s.file, w, s.ms.holeSpan(s.f))
		if err != nil {
			return err
		}
		for _, h := range hs {
			if !s.dirtyH[h.Offset] && !s.ms.masked(h.Version, h.Extent) {
				s.holes[h.Offset], s.stored[h.Offset] = h, h
			}
		}
		s.loadedHoles = append(s.loadedHoles, w)
	}
	if !covered(s.loadedOvs, w) {
		os, err := overwritesOver(s.tx, s.share, s.file, w, s.ms.ovSpan(s.f))
		if err != nil {
			return err
		}
		for _, o := range os {
			if !s.dirtyO[o.Offset] && !s.ms.masked(o.Version, o.Extent) {
				s.ovs[o.Offset] = o
			}
		}
		s.loadedOvs = append(s.loadedOvs, w)
	}
	return nil
}

func covered(ws []entity.Extent, e entity.Extent) bool {
	return slices.ContainsFunc(ws, func(w entity.Extent) bool { return w.Contains(e) })
}

func (s *existState) apply(op PendingOp) error {
	switch op.Kind {
	case OpWrite:
		e := op.Extent
		if e.IsEmpty() {
			break
		}
		for _, run := range s.committedRuns(e) {
			s.overwrite(run, op.Version)
		}
		if e.Offset > s.f.Size {
			s.addHole(entity.ExtentOf(s.f.Size, e.Offset), op.Version)
		}
		filled := s.removeHoles(e)
		grown := max(0, e.End()-max(e.Offset, s.f.Size))
		s.f.Charged += filled + grown
		s.f.Size = max(s.f.Size, e.End())
	case OpGrow:
		if op.Size < s.f.Size {
			return fileFailed(merrs.NewInvalidArgumentError("a truncate down is a removal"))
		}
		if op.Size > s.f.Size {
			s.addHole(entity.ExtentOf(s.f.Size, op.Size), op.Version)
			s.f.Size = op.Size
		}
	default:
		return fileFailed(merrs.NewInvalidArgumentError(fmt.Sprintf("unknown operation %d", op.Kind)))
	}
	s.f.Modify, s.f.Change = later(s.f.Modify, op.Time), later(s.f.Change, op.Time)
	s.f.Applied = entity.MaxVersion(s.f.Applied, op.Version)
	return nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// committedRuns is the part of e whose existence was committed before this
// commit: below the size it read, outside the holes stored then.
func (s *existState) committedRuns(e entity.Extent) []entity.Extent {
	e = e.Intersect(entity.Extent{Length: s.size})
	if e.IsEmpty() {
		return nil
	}
	var holes []entity.Extent
	for _, h := range s.stored {
		if h.Overlaps(e) {
			holes = append(holes, h.Extent)
		}
	}
	return subtract(e, holes)
}

// subtract is e less every extent of cut, in order.
func subtract(e entity.Extent, cut []entity.Extent) []entity.Extent {
	slices.SortFunc(cut, func(a, b entity.Extent) int { return cmp.Compare(a.Offset, b.Offset) })
	var out []entity.Extent
	at := e.Offset
	for _, c := range cut {
		if c.Offset > at {
			out = append(out, entity.ExtentOf(at, min(c.Offset, e.End())))
		}
		at = max(at, c.End())
		if at >= e.End() {
			break
		}
	}
	if at < e.End() {
		out = append(out, entity.ExtentOf(at, e.End()))
	}
	return out
}

// overwrite gives run version v in the overwrite set, splitting any record
// it overlaps around it; records of different versions never merge.
func (s *existState) overwrite(run entity.Extent, v entity.JournalVersion) {
	for _, o := range overlapping(s.ovs, run) {
		s.dropOv(o.Offset)
		if o.Offset < run.Offset {
			left := o
			left.Extent = entity.ExtentOf(o.Offset, run.Offset)
			s.setOv(left)
		}
		if o.End() > run.End() {
			right := o
			right.Extent = entity.ExtentOf(run.End(), o.End())
			s.setOv(right)
		}
	}
	s.setOv(overwrite{Extent: run, Version: v})
}

// removeHoles takes e out of the hole set and reports the bytes it filled.
func (s *existState) removeHoles(e entity.Extent) int64 {
	var filled int64
	for _, h := range overlapping(s.holes, e) {
		filled += h.Intersect(e).Length
		s.dropHole(h.Offset)
		if h.Offset < e.Offset {
			s.setHole(hole{Extent: entity.ExtentOf(h.Offset, e.Offset), Version: h.Version})
		}
		if h.End() > e.End() {
			s.setHole(hole{Extent: entity.ExtentOf(e.End(), h.End()), Version: h.Version})
		}
	}
	return filled
}

// addHole adds e at version v, joined with the holes it touches, the joined
// hole taking the higher version: holes never overlap or touch.
func (s *existState) addHole(e entity.Extent, v entity.JournalVersion) {
	nh := hole{Extent: e, Version: v}
	for _, h := range s.holes {
		if h.End() == nh.Offset || h.Offset == nh.End() {
			s.dropHole(h.Offset)
			nh.Extent = entity.ExtentOf(min(h.Offset, nh.Offset), max(h.End(), nh.End()))
			nh.Version = entity.MaxVersion(nh.Version, h.Version)
		}
	}
	s.setHole(nh)
}

// overlapping is the records of m over e.
func overlapping[T interface{ Overlaps(entity.Extent) bool }](m map[int64]T, e entity.Extent) []T {
	var out []T
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if m[k].Overlaps(e) {
			out = append(out, m[k])
		}
	}
	return out
}

func (s *existState) setHole(h hole) {
	s.holes[h.Offset], s.dirtyH[h.Offset] = h, true
	s.f.MaxSpan = max(s.f.MaxSpan, h.Length)
}

func (s *existState) dropHole(start int64) { delete(s.holes, start); s.dirtyH[start] = true }

func (s *existState) setOv(o overwrite) {
	s.ovs[o.Offset], s.dirtyO[o.Offset] = o, true
	s.f.MaxOverwrite = max(s.f.MaxOverwrite, o.Length)
}

func (s *existState) dropOv(start int64) { delete(s.ovs, start); s.dirtyO[start] = true }

// flush writes the File, its fence when it had none, and every changed hole
// and overwrite record.
func (s *existState) flush() error {
	if !s.fenced {
		if err := s.tx.Set(existenceFenceKey(s.share, s.file), encodeFence(fence{Epoch: s.epoch})); err != nil {
			return err
		}
	}
	s.f.Version = nextFileVersion(s.tx, entity.MaxVersion(s.f.Version, s.f.Applied))
	if err := putFile(s.tx, s.was, s.f); err != nil {
		return err
	}
	if err := chargeFile(s.tx, s.share, &s.orig, &s.f); err != nil {
		return err
	}
	for start := range s.dirtyH {
		key := holeKey(s.share, s.file, start)
		var err error
		if h, ok := s.holes[start]; ok {
			err = s.tx.Set(key, encodeHole(h))
		} else {
			err = s.tx.Delete(key)
		}
		if err != nil {
			return err
		}
	}
	for start := range s.dirtyO {
		key := overwriteKey(s.share, s.file, start)
		var err error
		if o, ok := s.ovs[start]; ok {
			err = s.tx.Set(key, encodeOverwrite(o))
		} else {
			err = s.tx.Delete(key)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Applied is the newest journal version whose existence the file records:
// recovery re-applies every operation above it.
func (e Existence) Applied(ctx context.Context, share entity.ShareID, file entity.FileID) (v entity.JournalVersion, err error) {
	err = e.s.kv.View(ctx, func(r kv.Reader) error {
		f, err := fileIn(r, share, file)
		v = f.Applied
		return err
	})
	return v, err
}

// Overwrites reports whether any run covers content whose existence is
// committed — below size, outside the holes — in one read transaction: a
// stability point waits for the existence commit of exactly such a write.
func (e Existence) Overwrites(ctx context.Context, share entity.ShareID, file entity.FileID, runs []entity.Extent) (yes bool, err error) {
	err = e.s.kv.View(ctx, func(r kv.Reader) error {
		f, err := fileIn(r, share, file)
		if err != nil {
			return err
		}
		ms, err := masksOf(r, share, file)
		if err != nil {
			return err
		}
		for _, run := range runs {
			run = run.Intersect(entity.Extent{Length: f.Size})
			if run.IsEmpty() {
				continue
			}
			hs, err := holesOver(r, share, file, run, ms.holeSpan(f))
			if err != nil {
				return err
			}
			var cut []entity.Extent
			for _, h := range hs {
				if !ms.masked(h.Version, h.Extent) {
					cut = append(cut, h.Extent)
				}
			}
			if len(subtract(run, cut)) > 0 {
				yes = true
				return nil
			}
		}
		return nil
	})
	return yes, err
}
