package engine

import (
	"context"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
// A journal whose device was lost whole is synced no more: the sync answers
// the journal closed, so no Commit answers a verifier past the loss.
func (e *Engine) sync(ctx context.Context, b *binding, ids ...journal.FileID) error {
	b.gc.mu.Lock()
	lost := b.gc.halted
	b.gc.mu.Unlock()
	if lost {
		return journal.ErrClosed
	}
	err := b.h.Sync(ctx, ids...)
	e.drain(b)
	return err
}

// drain applies the loss events the journal reported since the last drain,
// once its loss generation moved: each corrupt or failed-window loss of an
// unoffloaded write raises its file's sequence, a gap in the ring raises
// every file's, and a corrupt loss of a write still pending marks that write
// lost, so its existence commit records it lost and charges nothing.
//
// The drain holds the journal's loss lock until every write it marks is
// marked, so a drain that finds the generation already applied finds its
// writes marked too.
//
// decision: a corrupt loss of a pending write is taken as of a synced write
// and committed lost, since the journal's loss event says why but not whether
// the write was synced. A failed window's loss raises the sequence only: its
// pending write is not dropped here, and is still committed as written until
// the drop of a failed window's writes, uncharged and with their Done, is
// built. Revisit both when the event says whether the write was synced.
func (e *Engine) drain(b *binding) {
	l := &b.gc.losses
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
		l.raise(entity.FileID(ev.File.Bytes()))
		if ev.Reason == journal.LossCorrupt {
			lost = append(lost, ev)
		}
	}
	l.gen = st.LossGeneration
	if h := e.hooks.drained.Load(); h != nil {
		(*h)()
	}
	for _, ev := range lost {
		e.markLost(entity.FileID(ev.File.Bytes()), ev)
	}
}

// markLost marks lost the pending write of f whose record ev dropped: the
// one at ev's version. The caller holds the journal's loss lock, which is
// taken before an entry's.
func (e *Engine) markLost(f entity.FileID, ev journal.LossEvent) {
	fe := e.files.lockOf(f)
	if fe == nil {
		return
	}
	defer e.files.unlock(f, fe)
	for i := range fe.ops {
		op := &fe.ops[i]
		if op.kind == opWrite && op.version == ev.Version && overlaps(op.extent, ev.Extent) {
			op.lost = true
		}
	}
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
