package durability

import (
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// mark is one failed window's report that a file is owed: the next Sync that
// names the file answers it and clears it.
type mark struct {
	window uint64
	seg    core.SegmentID
	err    error
}

// markLocked records that the failed window w is owed to file. It is a no-op
// when the file was already told of w, or a mark of w for it is still waiting,
// so a window that fails several records of a file, or goes on failing after a
// Sync reported it, tells the file once. The caller holds mu.
func (st *stream) markLocked(w *window, file core.FileID) {
	if _, told := st.told[w.id][file]; told {
		return
	}
	if slices.ContainsFunc(st.marks[file], func(m mark) bool { return m.window == w.id }) {
		return
	}
	if st.marks == nil {
		st.marks = map[core.FileID][]mark{}
	}
	st.marks[file] = append(st.marks[file], mark{window: w.id, seg: w.seg, err: w.err})
}

// takeMarksLocked answers a Sync naming ids with the failures the stream owes
// them: one *core.SyncError for the oldest window, naming every file of the
// call that was owed one, with every mark of those files cleared. It returns
// nil when none is owed. The check and the clear are one step under mu, so one
// failure is answered to one caller. The caller holds mu.
func (st *stream) takeMarksLocked(ids []core.FileID) error {
	if len(st.marks) == 0 {
		return nil
	}
	var oldest *mark
	var files []core.FileID
	for _, id := range ids {
		ms := st.marks[id]
		if len(ms) == 0 || slices.Contains(files, id) {
			continue
		}
		files = append(files, id)
		if oldest == nil || ms[0].window < oldest.window {
			oldest = &ms[0]
		}
	}
	if oldest == nil {
		return nil
	}
	se := &core.SyncError{Segment: oldest.seg, Err: oldest.err, Files: files}
	for _, id := range files {
		for _, m := range st.marks[id] {
			if !st.windowPendingLocked(m.window) {
				continue
			}
			// Remembered while the window can still fail more of the file's
			// records; a resolved window has nothing left to remember.
			if st.told == nil {
				st.told = map[uint64]map[core.FileID]struct{}{}
			}
			if st.told[m.window] == nil {
				st.told[m.window] = map[core.FileID]struct{}{}
			}
			st.told[m.window][id] = struct{}{}
		}
		delete(st.marks, id)
	}
	return se
}

// windowPendingLocked reports whether the window numbered id is pending. The
// caller holds mu.
func (st *stream) windowPendingLocked(id uint64) bool {
	return slices.ContainsFunc(st.windows, func(w *window) bool { return w.id == id })
}
