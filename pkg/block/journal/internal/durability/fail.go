package durability

import (
	"context"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// failWindows fails the content records of every pending window of the
// stream. It runs on the worker, after the windows are taken.
func (d *Durability) failWindows(st *stream) {
	st.mu.Lock()
	if len(st.windows) == 0 {
		st.mu.Unlock()
		return
	}
	ws := slices.Clone(st.windows)
	st.mu.Unlock()
	for _, w := range ws {
		d.failWindow(st, w)
	}
}

// recordAt is where the index places the record e was written at.
func recordAt(w segment.Written) index.RecordAt {
	return index.RecordAt{Segment: w.Loc.Segment, Offset: uint32(w.Loc.Off)}
}

// failWindow drops the window's content records from the index, which holds
// again what they superseded, raises the loss generation once for the window,
// names every dropped record in a loss record and an event, and resolves the
// window once it holds nothing more. It runs on the stream's worker.
//
// The index is changed under the file's append domain, one file at a time, so
// a writer that appended into the failed segment and has not yet published
// does so before the drop, and the drop then finds the record. The loss
// records are appended with no domain held: an append may roll, seal and sync,
// and a read of the file must not wait on it. st.mu is never held across a
// call into the index or the domains.
//
// decision: only content records are failed, and a window that still holds
// another kind keeps it and stays pending, with the interim answer check
// gives. Such a record holds nothing in the index and has no content to lose,
// but its operation must not be forgotten, and it is the re-append of the
// record that carries it forward; failing it here would drop the operation.
// Withdrawn once that re-append exists: the record is then neither failed nor
// kept here.
func (d *Durability) failWindow(st *stream, w *window) {
	st.mu.Lock()
	var content []entry
	for i := range w.entries {
		if isContent(w.entries[i].w) {
			content = append(content, w.entries[i])
		}
	}
	st.mu.Unlock()
	if len(content) == 0 {
		return
	}
	if d.beforeFail != nil {
		d.beforeFail(w)
	}

	// Raised before anything is published or answered, so a reader that sees a
	// loss event, or a Sync that returns its failure, finds the generation up.
	d.RaiseWindow()

	var files []core.FileID
	byFile := map[core.FileID][]entry{}
	for _, e := range content {
		if _, ok := byFile[e.w.File]; !ok {
			files = append(files, e.w.File)
		}
		byFile[e.w.File] = append(byFile[e.w.File], e)
	}
	for _, f := range files {
		d.failFile(st, w, f, byFile[f])
	}

	// The records leave the window and every file's mark is set in one step,
	// so a waiter never sees a record gone with no report owed, and a Sync
	// naming several files of the window hears of all of them at once.
	st.mu.Lock()
	for _, e := range content {
		st.lost[e.w.Loc] = w.err
	}
	w.entries = slices.DeleteFunc(w.entries, func(e entry) bool { return isContent(e.w) })
	for _, f := range files {
		st.markLocked(w, f)
	}
	empty := len(w.entries) == 0
	st.d.notifyAll()
	st.mu.Unlock()
	if d.afterFail != nil {
		d.afterFail()
	}
	if empty {
		d.resolve(st, w)
	}
}

// failFile fails the records of one file: the index first, under the file's
// domain, then a loss record for each piece the failure dropped.
func (d *Durability) failFile(st *stream, w *window, f core.FileID, entries []entry) {
	recs := make([]index.WindowRecord, len(entries))
	for i, e := range entries {
		recs[i] = index.WindowRecord{Record: recordAt(e.w), Version: e.w.Version}
	}

	unlock := d.domains.Lock(f)
	fw, hd, err := d.index.FailWindow(f, recs)
	if err != nil {
		// The drop did not happen, so the index may still hold the failed
		// bytes; the records are lost all the same.
		d.log.Error("journal could not fail a window's records in the index", "stream", st.no, "window", w.id, "file", f, "err", err)
	}
	d.accountDelta(hd)
	// A newer record that took over what a failed one had superseded holds it
	// retained until that record syncs. One that already synced was lifted
	// before the failed record handed it the parts, so it is lifted now; one
	// still in the ledger or a window lifts when it completes.
	for _, v := range fw.Covers {
		if !d.hasEntry(st, f, v) {
			d.lift(f, v)
		}
	}
	unlock()

	for _, e := range entries {
		d.loseRecord(w, e, fw.Dropped)
	}
}

// hasEntry reports whether the stream's ledger or a pending window holds a
// record of file at version v.
func (d *Durability) hasEntry(st *stream, file core.FileID, v core.Version) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	match := func(e entry) bool { return e.w.File == file && e.w.Version == v }
	if slices.ContainsFunc(st.ledger, match) {
		return true
	}
	for _, o := range st.windows {
		if slices.ContainsFunc(o.entries, match) {
			return true
		}
	}
	return false
}

// loseRecord appends the loss record and publishes the event for each piece
// of e that the failure dropped. A record with no piece left, overwritten
// since it was appended, is named once by its header extent, so that every
// failed record has a loss record.
func (d *Durability) loseRecord(w *window, e entry, dropped []index.Piece) {
	at := recordAt(e.w)
	l := Loss{
		Share:   e.w.Tag,
		File:    e.w.File,
		Extent:  e.w.Extent,
		Version: e.w.Version,
		Seq:     e.w.Seq,
		Reason:  core.LossFailedWindow,
		Raised:  true,
	}
	named := false
	for _, p := range dropped {
		if p.Record != at {
			continue
		}
		named = true
		l.Extent, l.Offloaded = p.Extent, p.Offloaded
		d.lose(l)
	}
	if !named {
		d.lose(l)
	}
}

// lose hands the loss to Lose. An error leaves the drop in place: Lose
// publishes the event, lists the record as owed whatever it could not append,
// and logs the failure, so the error is dropped here.
func (d *Durability) lose(l Loss) {
	_, _ = d.Lose(context.Background(), l)
}
