package engine

import (
	"context"
	"sync"

	"errors"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"slices"
)

// losses is one journal's loss sequences: a journal-wide floor and an entry
// for each file whose own losses raised it above the floor. A file's sequence
// is the larger of the two. It lives in memory and only rises.
//
// ponytail: a raise of every file of the journal raises the floor past every
// entry and drops the entries, so memory holds only the files raised since;
// one entry per file raised alone stays until the next journal-wide raise.
// Expire entries of closed files when their count shows in a profile.
type losses struct {
	mu    sync.Mutex
	floor uint64
	files map[entity.FileID]uint64
	gen   uint64 // the journal's loss generation the drain last applied
	seq   uint64 // the newest loss event the drain read
	// windows are the files a failed-window event raised whose failed sync
	// has not yet reached a Sync of theirs: that Sync's SyncError raises them
	// no further.
	windows map[entity.FileID]bool

	// kept is each file's loss events that found no pending write of it:
	// the write may be between the journal's WriteAt and its insert, which
	// checks here. It is read and written only under the file's entry lock,
	// and keptMu, taken after that lock, guards the map itself.
	//
	// ponytail: an event of a file never written again, as of a write
	// already committed, stays until the process ends. Prune it with the
	// store-side loss path (#3734), which takes the events of committed writes.
	keptMu sync.Mutex
	kept   map[entity.FileID][]journal.LossEvent
}

// keep keeps ev, which found no pending write of f. The caller holds f's
// entry lock.
func (l *losses) keep(f entity.FileID, ev journal.LossEvent) {
	l.keptMu.Lock()
	defer l.keptMu.Unlock()
	if l.kept == nil {
		l.kept = map[entity.FileID][]journal.LossEvent{}
	}
	l.kept[f] = append(l.kept[f], ev)
}

// unmatched is the kept event naming f's write at version v, which is being
// inserted, taken out, and found reports one was kept. The caller holds f's
// entry lock.
func (l *losses) unmatched(f entity.FileID, v journal.Version) (ev journal.LossEvent, found bool) {
	l.keptMu.Lock()
	defer l.keptMu.Unlock()
	evs := l.kept[f]
	if len(evs) == 0 {
		return journal.LossEvent{}, false
	}
	// decision: a kept event below v belongs to a write already committed
	// and is discarded, since a file's writes reach the engine in version
	// order under its accept lock (see insert), so any earlier write was in
	// the entry, where markLost would have found it, until it committed.
	// Withdrawn with insert's own marker.
	evs = slices.DeleteFunc(evs, func(e journal.LossEvent) bool {
		if e.Version == v && !found {
			ev, found = e, true
			return true
		}
		return e.Version.Compare(v) < 0
	})
	if len(evs) == 0 {
		delete(l.kept, f)
	} else {
		l.kept[f] = evs
	}
	return ev, found
}

// seqOf is f's loss sequence. The caller holds mu.
func (l *losses) seqOf(f entity.FileID) uint64 { return max(l.floor, l.files[f]) }

// raise raises f's sequence. The caller holds mu.
func (l *losses) raise(f entity.FileID) {
	if l.files == nil {
		l.files = map[entity.FileID]uint64{}
	}
	l.files[f] = l.seqOf(f) + 1
}

// raiseAll raises every file's sequence through the floor. The caller holds
// mu.
func (l *losses) raiseAll() {
	for _, s := range l.files {
		l.floor = max(l.floor, s)
	}
	l.floor++
	clear(l.files)
}

// sync syncs ids of share b in the journal and then drains the journal's
// loss ring, so a caller that commits existence after it never commits a
// write the journal dropped as data.
//
// decision: every Sync the engine makes goes through this function, so no
// existence commit follows a sync whose losses were not drained first. A
// call of b.h.Sync elsewhere breaks that; revisit only if the journal ever
// reports its losses with its Sync's answer.
//
// A failed sync window, the journal's SyncError, is not an error of the sync:
// the drain drops the window's pending writes, each file the error names that
// no failed-window event already raised has its loss sequence raised, so the
// next Commit of each answers the raised sequence and the verifier the raised
// loss generation gives, never an I/O error, and the files are synced again,
// as many times as a window fails.
//
// A journal whose device was lost whole is synced no more: the sync answers
// the journal closed, so no Commit answers a verifier past the loss.
func (e *Engine) sync(ctx context.Context, b *binding, ids ...journal.FileID) error {
	b.gc.mu.Lock()
	lost := b.gc.halted
	b.gc.mu.Unlock()
	if lost {
		return journal.ErrClosed
	}
	for {
		err := b.h.Sync(ctx, ids...)
		e.drain(b)
		var failed *journal.SyncError
		if !errors.As(err, &failed) {
			return err
		}
		// decision: a file the SyncError names that no failed-window event
		// covers, as when the loss ring overflowed, keeps its pending writes
		// and commits them as data, since the engine cannot tell which of
		// them the window held. Overturned once the journal names the
		// failed versions with its SyncError.
		l := &b.gc.losses
		l.mu.Lock()
		for _, id := range failed.Files {
			if f := entity.FileID(id.Bytes()); l.windows[f] {
				delete(l.windows, f)
			} else {
				l.raise(f)
			}
		}
		l.mu.Unlock()
		// The journal promises nothing durable past a failed window, the
		// other files' writes and later ones included: sync again.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// drain applies the loss events the journal reported since the last drain,
// once its loss generation moved: each corrupt or failed-window loss of an
// unoffloaded write raises its file's sequence, a gap in the ring raises
// every file's, a corrupt loss of a write still pending marks that write
// lost, so its existence commit records it lost and charges nothing, and a
// failed window's loss drops its pending write, calling its Done, charging
// nothing, and records its file as raised by its window.
//
// The drain holds the journal's loss lock until every write it marks is
// marked, so a drain that finds the generation already applied finds its
// writes marked too.
//
// decision: a corrupt loss of a pending write is taken as of a synced write
// and committed lost, and a failed window's as of an unsynced one, dropped,
// since the journal's loss event says why but not whether the write was
// synced. Revisit when the event says it.
func (e *Engine) drain(b *binding) {
	l := &b.gc.losses
	var dones []func()
	defer func() {
		for _, d := range dones {
			d()
		}
	}()
	l.mu.Lock()
	defer l.mu.Unlock()
	if b.j.Stats().LossGeneration == l.gen {
		return
	}
	st := b.j.Stats()
	var lost []journal.LossEvent
	for _, ev := range st.Losses {
		if ev.SeqNum <= l.seq {
			continue
		}
		if ev.SeqNum > l.seq+1 {
			l.raiseAll() // the ring dropped events this drain never saw
		}
		l.seq = ev.SeqNum
		if ev.Offloaded || ev.Reason == journal.LossStale {
			continue
		}
		f := entity.FileID(ev.File.Bytes())
		l.raise(f)
		lost = append(lost, ev)
		if ev.Reason == journal.LossFailedWindow {
			if l.windows == nil {
				l.windows = map[entity.FileID]bool{}
			}
			l.windows[f] = true
		}
	}
	l.gen = st.LossGeneration
	if h := e.hooks.drained.Load(); h != nil {
		(*h)()
	}
	for _, ev := range lost {
		dones = append(dones, e.markLost(l, entity.FileID(ev.File.Bytes()), ev)...)
	}
}

// markLost applies ev to the pending write of f whose record it dropped, the
// one at ev's version: a corrupt loss marks it lost; a failed window's drops
// it, out of the entry, or, when a batch claimed it, out of that batch, which
// neither commits it nor calls its Done. An event that finds no pending write
// is kept, under the entry's lock, for the write's insert, which applies it.
// It answers the Done of a dropped write, which the caller runs once its
// locks are released. The caller holds the journal's loss lock, which is
// taken before an entry's.
func (e *Engine) markLost(l *losses, f entity.FileID, ev journal.LossEvent) (dones []func()) {
	fe := e.files.lockFor(f)
	defer e.files.unlock(f, fe)
	found := false
	for i := 0; i < len(fe.ops); i++ {
		op := &fe.ops[i]
		if op.kind != opWrite || op.dropped || op.version != ev.Version || !overlaps(op.extent, ev.Extent) {
			continue
		}
		found = true
		if ev.Reason != journal.LossFailedWindow {
			op.lost = true
			continue
		}
		if op.done != nil {
			dones = append(dones, op.done)
			op.done = nil
		}
		if op.claimed {
			op.dropped = true
			continue
		}
		fe.ops = slices.Delete(fe.ops, i, i+1)
		i--
	}
	if !found {
		l.keep(f, ev)
	}
	return dones
}

// LossSeq is the file's loss sequence now, with no sync; 0 for a share with
// no binding.
func (e *Engine) LossSeq(s entity.ShareID, f entity.FileID) uint64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}
	e.drain(b)
	l := &b.gc.losses
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seqOf(f)
}

// lostWhole is journal j's device lost whole under the running process: every
// file's sequence is raised, no batch runs on j again, and every uncommitted
// write of j's files leaves its entry with its Done, charging nothing.
//
// decision: the journal reports no device lost while it runs, so nothing
// calls this but a test. Call it from that report once the journal has one.
//
// It runs to the end whatever its caller does, so no uncommitted write is
// left without its Done.
func (e *Engine) lostWhole(j Journal) {
	g := e.groupCommitFor(j)
	g.losses.mu.Lock()
	g.losses.raiseAll()
	g.losses.mu.Unlock()
	for {
		g.mu.Lock()
		g.halted = true
		inFlight, returned := g.inFlight, g.returned
		g.mu.Unlock()
		if !inFlight {
			break
		}
		<-returned
	}
	g.mu.Lock()
	var fs []entity.FileID
	for f := range g.due {
		fs = append(fs, f)
	}
	clear(g.due)
	clear(g.failing) // no file of a lost journal is left to commit
	g.mu.Unlock()
	var dones []func()
	for _, f := range fs {
		fe := e.files.lockOf(f)
		if fe == nil {
			continue
		}
		kept := fe.ops[:0]
		for _, op := range fe.ops {
			if op.kind != opWrite {
				kept = append(kept, op)
				continue
			}
			if op.done != nil {
				dones = append(dones, op.done)
			}
		}
		fe.ops = kept
		e.files.unlock(f, fe)
	}
	g.mu.Lock()
	close(g.returned) // wakes every waiter, which then finds the journal lost
	g.returned = make(chan struct{})
	g.mu.Unlock()
	for _, d := range dones {
		d()
	}
}

func overlaps(a, b journal.Extent) bool {
	return a.Off < b.Off+b.Len && b.Off < a.Off+a.Len
}
