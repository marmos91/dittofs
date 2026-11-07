package durability

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// quietAfterFailures installs the hooks that let a model wait for the worker to
// finish failing windows, and returns the wait. A failed window's loss records
// are appended by the worker, and each one consumes a write or a sync the seam
// has scheduled to fail, so a model lets the worker finish a window before it
// schedules a fault or appends. It must run before the layer starts.
func (r *rig) quietAfterFailures() func() {
	var entered, exited atomic.Int64
	r.d.beforeFail = func(*window) { entered.Add(1) }
	r.d.afterFail = func() { exited.Add(1) }
	return func() {
		st := r.d.made[0]
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			st.mu.Lock()
			idle := len(st.ends) == 0
			for _, w := range st.windows {
				for i := range w.entries {
					idle = idle && !isContent(w.entries[i].w)
				}
			}
			st.mu.Unlock()
			if idle && entered.Load() == exited.Load() {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// FuzzSyncInterleaving drives append, Sync, a Sync with a cancelled context
// and a scheduled sync failure, and a seal of a clean segment, over two files on one stream, and checks what
// each Sync may return against what the test knows happened:
//   - nil only if every earlier record of the file is Durable, bar the ones a
//     SyncError already reported lost;
//   - a SyncError only if a failure was scheduled while one of the file's
//     records was unsynced, or on a segment a failure was scheduled for. A
//     repeat SyncError for a file already told is accepted, not checked: the
//     deterministic marks tests pin that a failure is reported once;
//   - the context's error only for the cancelled call.
func FuzzSyncInterleaving(f *testing.F) {
	f.Add([]byte{0, 1})
	f.Add([]byte{0, 4, 1, 5})
	f.Add([]byte{0, 3, 1, 4, 1})
	f.Add([]byte{0, 2, 1, 6, 0, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 24 {
			ops = ops[:24]
		}
		r := newRigUnstarted(t, 1)
		quiet := r.quietAfterFailures()
		r.d.Start(r.streams)
		files := [2]core.FileID{file(1), file(2)}
		var locs [2][]segment.Location
		var off int64
		maybeFailed := map[core.SegmentID]bool{}
		var last segment.Location
		appended := false
		seals := 0
		// told holds the records a SyncError reported lost: they stay not
		// durable, and the failure is told once.
		told := map[segment.Location]bool{}

		for _, op := range ops {
			quiet()
			r.settlePool(t) // a loss record's roll takes a spare too
			i := int(op>>2) & 1
			switch op & 3 {
			case 0:
				h := format.RecordHeader{Kind: format.KindWrite, File: files[i], Extent: core.Extent{Off: off, Len: 1}, Version: core.Version{Counter: uint64(off + 1)}, Tag: 1}
				p, err := r.streams.For(files[i]).Append(h, format.RecordBody{Payload: []byte("x"), MTime: 1})
				if errors.Is(err, segment.ErrStreamStopped) {
					// A second failed segment in a row stops the stream: the
					// model ends there.
					return
				}
				if err != nil {
					t.Fatalf("Append: %v", err)
				}
				last = p.Loc
				off += 8
				appended = true
				locs[i] = append(locs[i], last)
			case 1:
				err := r.d.Sync(context.Background(), files[i])
				switch {
				case err == nil:
					for _, l := range locs[i] {
						if !told[l] && !r.d.Durable(l) {
							t.Fatalf("Sync of file %d returned nil with %+v not durable", i, l)
						}
					}
				case errors.Is(err, core.ErrSyncFailed):
					hit := false
					for _, l := range locs[i] {
						if !told[l] && !r.d.Durable(l) && maybeFailed[l.Segment] {
							hit = true
							told[l] = true
						}
					}
					// A file that lost a record may hear of a failure again: its
					// loss record can sit in a failed segment's window as a
					// header-only record, which fails every Sync naming the
					// file until it is re-appended.
					for _, l := range locs[i] {
						hit = hit || maybeFailed[l.Segment]
					}
					if !hit {
						t.Fatalf("Sync of file %d failed with no failure scheduled for it, or was told before: %v", i, err)
					}
				default:
					t.Fatalf("Sync of file %d returned %v", i, err)
				}
			case 2:
				if op&8 != 0 && appended && !maybeFailed[last.Segment] && seals < 2 {
					seals++
					r.settlePool(t) // a roll takes a spare
					// A seal of a segment no failure was scheduled for, so
					// the stream rolls and the oracle sees several segments.
					if _, err := r.streams.For(files[i]).Seal(false); err != nil && !errors.Is(err, segment.ErrWrongState) {
						t.Fatalf("Seal: %v", err)
					}
					continue
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := r.d.Sync(ctx, files[i]); !errors.Is(err, context.Canceled) {
					t.Fatalf("a cancelled Sync returned %v, want context.Canceled", err)
				}
			case 3:
				if !appended {
					continue
				}
				rule := journaltest.Failing(journaltest.OpSync, format.SegmentName(last.Segment), 1, errors.New("injected"))
				if err := r.seam.Schedule(rule); err != nil {
					t.Fatal(err)
				}
				maybeFailed[last.Segment] = true
			}
		}
	})
}

// FuzzFailedSyncWindow drives writes over three files on one stream, Syncs,
// and scheduled sync and write failures, and checks R14: a Sync that returns
// nil never covers a record of a segment whose sync or append failed while the
// record was unsynced, no Sync hangs, and a file whose record a failure took
// hears of it from a SyncError (that it hears once is pinned by the marks
// tests, not by this oracle).
//
// The model dooms a record when a failure is certain to meet it: it was
// unsynced when a sync failure was scheduled on its segment, was written to a
// segment a failure was scheduled on or one a Sync already saw fail, or was
// unsynced when a write to its segment failed. Nothing syncs by itself, since
// the driven clock never moves, so a scheduled sync failure fires on the first
// Sync that has a record of its segment to cover.
func FuzzFailedSyncWindow(f *testing.F) {
	f.Add([]byte{0, 4, 2, 1, 5})             // the RFC row: two files, one failed sync, both synced again
	f.Add([]byte{0, 2, 1, 0, 2, 1, 0})       // the second refusal: a failure in the spare before a sync
	f.Add([]byte{0, 1, 0, 3, 0, 1, 0, 5, 9}) // a failed append over a synced prefix
	f.Add([]byte{0, 4, 8, 3, 0, 1, 5, 9})
	f.Add([]byte("870027a9")) // a sync failure in the loss record of a failed window
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 24 {
			ops = ops[:24]
		}
		r := newRigUnstarted(t, 1)
		quiet := r.quietAfterFailures()
		r.d.Start(r.streams)
		files := [3]core.FileID{file(1), file(2), file(3)}
		type rec struct {
			file   int
			loc    segment.Location
			doomed bool
			told   bool // a SyncError reported the loss
		}
		var recs []*rec
		armedSync := map[core.SegmentID]bool{}
		failedSeg := map[core.SegmentID]bool{}
		var writeArmed core.SegmentID
		var last core.SegmentID
		appended, stopped := false, false
		var off int64

		// The seam holds one scheduled fault, a second replacing the first.
		anyArmed := func() bool {
			if writeArmed != 0 {
				return true
			}
			for _, on := range armedSync {
				if on {
					return true
				}
			}
			return false
		}
		doomSegment := func(seg core.SegmentID, onlyUnsynced bool) {
			for _, x := range recs {
				if x.loc.Segment == seg && (!onlyUnsynced || !r.d.Durable(x.loc)) {
					x.doomed = true
				}
			}
		}

		for _, op := range ops {
			quiet()
			i := int(op>>2) % 3
			switch op & 3 {
			case 0:
				if stopped {
					continue
				}
				h := format.RecordHeader{Kind: format.KindWrite, File: files[i], Extent: core.Extent{Off: off, Len: 1}, Version: core.Version{Counter: uint64(off + 1)}, Tag: 1}
				off++
				p, err := r.streams.For(files[i]).Append(h, format.RecordBody{Payload: []byte("x"), MTime: 1})
				switch {
				case err == nil:
					x := &rec{file: i, loc: p.Loc}
					x.doomed = armedSync[p.Loc.Segment] || failedSeg[p.Loc.Segment]
					recs = append(recs, x)
					last, appended = p.Loc.Segment, true
					if writeArmed != 0 && writeArmed != p.Loc.Segment {
						writeArmed = 0 // the segment ended before the write failure met it
					}
					if writeArmed != 0 {
						t.Fatalf("an append to segment %d succeeded with a write failure scheduled for it", writeArmed)
					}
				case errors.Is(err, segment.ErrStreamStopped):
					stopped = true
				case writeArmed != 0:
					doomSegment(writeArmed, true)
					failedSeg[writeArmed] = true
					writeArmed = 0
				default:
					t.Fatalf("Append failed with no failure scheduled: %v", err)
				}
				r.settlePool(t) // a roll takes a spare; keep one ready
			case 1:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := r.d.Sync(ctx, files[i])
				cancel()
				doomedFile := false // a failure is owed to the file
				lossAtRisk := false
				for _, x := range recs {
					doomedFile = doomedFile || (x.file == i && x.doomed && !x.told)
					lossAtRisk = lossAtRisk || (x.file == i && x.doomed)
				}
				switch {
				case err == nil:
					if doomedFile {
						t.Fatalf("Sync of file %d returned nil with a record of a failed window", i)
					}
					for _, x := range recs {
						if x.file == i && !x.told && !r.d.Durable(x.loc) {
							t.Fatalf("Sync of file %d returned nil with %+v not durable", i, x.loc)
						}
					}
				case errors.Is(err, core.ErrSyncFailed):
					// decision: a file that lost a record may hear of a failure
					// again, since its loss record is appended into the stream's
					// next segment, and a sync failure of that segment leaves
					// the loss record, a header-only one, pending in its window
					// until it is re-appended, which fails every Sync naming
					// the file. Withdrawn once that re-append exists.
					if !doomedFile && !lossAtRisk {
						t.Fatalf("Sync of file %d failed with no failure scheduled for it, or was told before: %v", i, err)
					}
					// The armed failure fired if the file had an unsynced
					// record in its segment. The error reports every failure
					// of a segment known failed by now.
					for _, x := range recs {
						if x.file == i && armedSync[x.loc.Segment] && !r.d.Durable(x.loc) {
							failedSeg[x.loc.Segment] = true
							armedSync[x.loc.Segment] = false
						}
					}
					for _, x := range recs {
						if x.file == i && x.doomed && failedSeg[x.loc.Segment] {
							x.told = true
						}
					}
				default:
					t.Fatalf("Sync of file %d returned %v (a hang or an unexpected error)", i, err)
				}
			case 2:
				if !appended || anyArmed() || failedSeg[last] {
					continue
				}
				if st, err := r.set.State(last); err != nil || st != segment.Active {
					continue
				}
				rule := journaltest.Failing(journaltest.OpSync, format.SegmentName(last), 1, errors.New("injected sync"))
				if err := r.seam.Schedule(rule); err != nil {
					t.Fatal(err)
				}
				armedSync[last] = true
				doomSegment(last, true)
			case 3:
				if !appended || stopped || anyArmed() || failedSeg[last] {
					continue
				}
				if st, err := r.set.State(last); err != nil || st != segment.Active {
					continue
				}
				rule := journaltest.Failing(journaltest.OpWrite, format.SegmentName(last), 1, errors.New("injected write"))
				if err := r.seam.Schedule(rule); err != nil {
					t.Fatal(err)
				}
				writeArmed = last
			}
		}
	})
}
