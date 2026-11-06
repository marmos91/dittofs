package segment_test

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// nopSink is a durability sink that drops every event, for the tests that
// build a stream and care for neither report.
func nopSink() segment.Sink {
	return segment.Sink{
		Track:  func(segment.Written) {},
		Synced: func(core.SegmentID, int64) {},
	}
}

// nopSinkFor gives every stream a sink that drops its events.
func nopSinkFor(uint32) segment.Sink { return nopSink() }

// nopSynced drops every sync report, for the direct WriteSeal callers that
// care for none.
func nopSynced(core.SegmentID, int64) {}

// syncedEvent is one Synced call a recordingSink saw.
type syncedEvent struct {
	Seg     core.SegmentID
	Through int64
}

// recordingSink keeps every event the stream reports, in the order it arrived.
type recordingSink struct {
	mu      sync.Mutex
	written []segment.Written
	synced  []syncedEvent
}

func (r *recordingSink) sink() segment.Sink {
	return segment.Sink{
		Track: func(w segment.Written) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.written = append(r.written, w)
		},
		Synced: func(seg core.SegmentID, through int64) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.synced = append(r.synced, syncedEvent{seg, through})
		},
	}
}

func (r *recordingSink) tracked() []segment.Written {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]segment.Written(nil), r.written...)
}

// sinkStream builds a stream over a fresh env that reports to sink, and closes
// it when the test ends.
func sinkStream(t *testing.T, sink segment.Sink) streamEnv {
	t.Helper()
	e := streamOver(t, streamCap, func(e streamEnv) { e.cacheBuilt(t) })
	e.s = segment.NewStream(1, streamIdentity, e.set, e.pool, e.cache, sink)
	t.Cleanup(func() { _ = e.s.Close() })
	return e
}

// TestSinkTracksEveryRecordOnce pins that every record Append wrote reaches
// Track once, with the fields it was written with, whichever kind it is and
// including both pieces of a write split at the cap.
func TestSinkTracksEveryRecordOnce(t *testing.T) {
	rec := &recordingSink{}
	e := sinkStream(t, rec.sink())

	fid := core.NewFileID([16]byte{1})
	var placed []segment.Placed

	h, body := streamWriteHeader(fid, 0, bytes.Repeat([]byte{0xA}, 100), 7, core.Version{Counter: 1})
	placed = append(placed, streamAppendWrite(t, e, h, body))

	h, body = streamWriteHeader(fid, 100, bytes.Repeat([]byte{0xB}, 50), 7, core.Version{Counter: 2})
	h.Kind = format.KindFill
	body.MTime = 0
	placed = append(placed, streamAppendWrite(t, e, h, body))

	trunc := format.RecordHeader{Kind: format.KindTruncate, File: fid, Extent: core.Extent{Off: 10}, Version: core.Version{Counter: 3}, Tag: 7}
	placed = append(placed, streamAppendWrite(t, e, trunc, format.RecordBody{}))

	clone := format.RecordHeader{Kind: format.KindCloneTarget, File: fid, Extent: core.Extent{Off: 0, Len: 64}, Version: core.Version{Counter: 4}, Tag: 7}
	cloneBody := format.RecordBody{Clone: format.CloneSource{File: core.NewFileID([16]byte{2}), Off: 8, AsOf: core.Version{Counter: 1}}}
	placed = append(placed, streamAppendWrite(t, e, clone, cloneBody))

	// A write larger than what remains of the segment is split: the first
	// piece fills the segment, the second rolls and lands in the next.
	big := bytes.Repeat([]byte{0xC}, int(streamCap))
	h, body = streamWriteHeader(fid, 200, big, 7, core.Version{Counter: 5})
	for sent := int64(0); sent < int64(len(big)); {
		h.Extent = core.Extent{Off: 200 + sent, Len: int64(len(big)) - sent}
		body.Payload = big[sent:]
		p, err := e.s.Append(h, body)
		if err != nil {
			t.Fatalf("Append of a split write: %v", err)
		}
		placed = append(placed, p)
		sent += p.N
	}
	if len(placed) < 6 {
		t.Fatalf("the split write placed %d pieces in all, want at least two", len(placed)-4)
	}

	got := rec.tracked()
	if len(got) != len(placed) {
		t.Fatalf("Track ran %d times for %d appended records, want one each", len(got), len(placed))
	}
	for i, w := range got {
		if w.Loc != placed[i].Loc {
			t.Errorf("record %d: Track saw %+v, Append returned %+v", i, w.Loc, placed[i].Loc)
		}
		hdr, _ := streamReadRecord(t, e, w.Loc)
		if w.Seq != hdr.Seq || w.Version != hdr.Version || w.File != hdr.File || w.Kind != hdr.Kind {
			t.Errorf("record %d: Track saw seq %d version %v file %v kind %v, the record holds %d %v %v %v",
				i, w.Seq, w.Version, w.File, w.Kind, hdr.Seq, hdr.Version, hdr.File, hdr.Kind)
		}
		if w.Extent != hdr.Extent || w.Tag != hdr.Tag || w.Tag != 7 {
			t.Errorf("record %d: Track saw extent %+v tag %d, the record holds %+v and %d, written with tag 7",
				i, w.Extent, w.Tag, hdr.Extent, hdr.Tag)
		}
		if w.PayloadCRC != hdr.PayloadCRC {
			t.Errorf("record %d: Track saw checksum %#x, the record holds %#x", i, w.PayloadCRC, hdr.PayloadCRC)
		}

		raw := make([]byte, w.Loc.Len)
		if _, err := e.s.ReadAt(w.Loc.Segment, raw, w.Loc.Off); err != nil {
			t.Fatalf("record %d: ReadAt: %v", i, err)
		}
		hasPayload := w.Kind == format.KindWrite || w.Kind == format.KindFill
		switch {
		case hasPayload && w.Bytes != nil:
			t.Errorf("record %d: a %v carries a payload and Track handed its bytes", i, w.Kind)
		case !hasPayload && !bytes.Equal(w.Bytes, raw):
			t.Errorf("record %d: Track's bytes of a %v are not the record's on-disk bytes", i, w.Kind)
		}
	}
}

// TestSinkTrackRunsBeforeAppendReturns pins that Track has run by the time
// Append returns and that the bytes it names are readable then. That Track
// runs only after the write succeeded is pinned by
// TestSinkFailedWriteNotTracked and TestSinkShortWriteWithNilErrorNotTracked.
func TestSinkTrackRunsBeforeAppendReturns(t *testing.T) {
	var tracked []segment.Written
	e := sinkStream(t, segment.Sink{
		Track:  func(w segment.Written) { tracked = append(tracked, w) },
		Synced: func(core.SegmentID, int64) {},
	})

	h, body := streamWriteHeader(core.NewFileID([16]byte{1}), 0, []byte("payload"), 1, core.Version{Counter: 1})
	streamAppendWrite(t, e, h, body)
	if len(tracked) != 1 {
		t.Fatalf("Track ran %d times by the time Append returned, want 1", len(tracked))
	}
	// The sink may not take a lock of this package, so the bytes are read
	// back once Append has returned.
	w := tracked[0]
	raw := make([]byte, format.RecordHeaderSize)
	if _, err := e.s.ReadAt(w.Loc.Segment, raw, w.Loc.Off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if _, err := format.DecodeRecordHeader(raw, streamIdentity, w.Loc.Segment, w.Loc.Off); err != nil {
		t.Errorf("the tracked record's bytes cannot be decoded: %v", err)
	}
}

// TestSinkFailedWriteNotTracked pins that a write that fails, or that lands
// short, is not registered, and that the appends after it are.
func TestSinkFailedWriteNotTracked(t *testing.T) {
	for name, rule := range map[string]func(key string) journaltest.Rule{
		"a failed write": func(key string) journaltest.Rule {
			return journaltest.Failing(journaltest.OpWrite, key, 1, syscall.EIO)
		},
		"a write torn with an error": func(key string) journaltest.Rule {
			return journaltest.Tearing(journaltest.OpWrite, key, 1, 8, syscall.EIO)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingSink{}
			e := sinkStream(t, rec.sink())
			fid := core.NewFileID([16]byte{1})

			h, body := streamWriteHeader(fid, 0, []byte("first"), 1, core.Version{Counter: 1})
			first := streamAppendWrite(t, e, h, body)

			if err := e.seam.Schedule(rule(streamSegName(first.Loc.Segment))); err != nil {
				t.Fatalf("Schedule: %v", err)
			}
			h, body = streamWriteHeader(fid, 10, []byte("failing"), 1, core.Version{Counter: 2})
			if _, err := e.s.Append(h, body); err == nil {
				t.Fatal("Append over a failing write returned no error")
			}
			if n := len(rec.tracked()); n != 1 {
				t.Fatalf("Track ran %d times after the failed write, want only the first record's", n)
			}

			h, body = streamWriteHeader(fid, 20, []byte("after"), 1, core.Version{Counter: 3})
			last := streamAppendWrite(t, e, h, body)
			got := rec.tracked()
			if len(got) != 2 || got[1].Loc != last.Loc {
				t.Fatalf("Track saw %+v after the append that followed the failure, want it at %+v", got, last.Loc)
			}
		})
	}
}

// TestSinkShortWriteWithNilErrorNotTracked pins that a write landing short and
// reporting success, which Append turns into io.ErrShortWrite, is not tracked.
func TestSinkShortWriteWithNilErrorNotTracked(t *testing.T) {
	rec := &recordingSink{}
	streamFS := &streamShortFS{FS: newSeam(t).FS(), name: streamSegName(1)}
	e := streamOverFS(t, streamCap, streamFS, func(e streamEnv) { e.cacheBuilt(t) })
	e.s = segment.NewStream(1, streamIdentity, e.set, e.pool, e.cache, rec.sink())
	t.Cleanup(func() { _ = e.s.Close() })

	h, body := streamWriteHeader(streamFileID(1), 0, []byte("first"), 1, core.Version{Counter: 1})
	streamAppendWrite(t, e, h, body)

	streamFS.armed.Store(true)
	h, body = streamWriteHeader(streamFileID(2), 64, []byte("short"), 1, core.Version{Counter: 2})
	if _, err := e.s.Append(h, body); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("the short Append = %v, want io.ErrShortWrite", err)
	}
	if n := len(rec.tracked()); n != 1 {
		t.Fatalf("Track ran %d times, want only the whole record's", n)
	}
}

// syncedOf returns the Synced calls the sink saw so far, in arrival order.
func (r *recordingSink) syncedOf() []syncedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]syncedEvent(nil), r.synced...)
}

// sinkAppendN appends n small writes of one file through the stream and
// returns where they landed.
func sinkAppendN(t *testing.T, e streamEnv, n int) []segment.Placed {
	t.Helper()
	var placed []segment.Placed
	for i := range n {
		h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, streamTestBody(64, byte(i)), 7, core.Version{Counter: uint64(i + 1)})
		placed = append(placed, streamAppendWrite(t, e, h, body))
	}
	return placed
}

// TestSinkSyncedOnStreamSync pins that every completed Sync reports the
// offset it covered, an idle one included, and that a failed sync reports
// nothing, nor does a later sync of the same segment, for the failure froze
// what the segment may claim.
func TestSinkSyncedOnStreamSync(t *testing.T) {
	t.Run("a sync reports what it covered", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		placed := sinkAppendN(t, e, 3)
		last := placed[2].Loc

		if err := e.s.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		want := []syncedEvent{{last.Segment, last.Off + last.Len}}
		if got := rec.syncedOf(); !slices.Equal(got, want) {
			t.Fatalf("Synced saw %+v after the first Sync, want %+v", got, want)
		}

		if err := e.s.Sync(); err != nil {
			t.Fatalf("the idle Sync: %v", err)
		}
		want = append(want, want[0])
		if got := rec.syncedOf(); !slices.Equal(got, want) {
			t.Fatalf("Synced saw %+v after a Sync with nothing new, want %+v", got, want)
		}
	})

	t.Run("a failed sync and the one after it report nothing", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		placed := sinkAppendN(t, e, 3)

		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, streamSegName(placed[0].Loc.Segment), 1, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if err := e.s.Sync(); err == nil {
			t.Fatal("the Sync over a failing fsync returned no error")
		}
		if got := rec.syncedOf(); len(got) != 0 {
			t.Fatalf("Synced saw %+v after a failed Sync, want nothing", got)
		}
		if err := e.s.Sync(); err != nil {
			t.Fatalf("the Sync after the failure: %v", err)
		}
		if got := rec.syncedOf(); len(got) != 0 {
			t.Fatalf("Synced saw %+v for a Sync after a failed one, want nothing", got)
		}
	})
}

// TestSinkTrackPrecedesCoveringSynced pins, under appenders racing a syncing
// goroutine, that every Synced is reported after the Track of each record it
// covers: a ledger told a segment is durable through an offset must already
// hold every record that ends at or below it.
func TestSinkTrackPrecedesCoveringSynced(t *testing.T) {
	type event struct {
		seg     core.SegmentID
		end     int64 // a Track's record end, or a Synced's through
		isTrack bool
	}
	var (
		mu  sync.Mutex
		log []event
	)
	e := sinkStream(t, segment.Sink{
		Track: func(w segment.Written) {
			mu.Lock()
			defer mu.Unlock()
			log = append(log, event{w.Loc.Segment, w.Loc.Off + w.Loc.Len, true})
		},
		Synced: func(seg core.SegmentID, through int64) {
			mu.Lock()
			defer mu.Unlock()
			log = append(log, event{seg, through, false})
		},
	})

	const appenders, each = 16, 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := e.s.Sync(); err != nil {
				t.Errorf("Sync: %v", err)
				return
			}
			runtime.Gosched()
		}
	}()
	for a := range appenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				h, body := streamWriteHeader(streamFileID(a+1), int64(i)*32, streamTestBody(32, byte(a)), 7, core.Version{Counter: uint64(a*each + i + 1)})
				if _, err := e.s.Append(h, body); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-syncDone
	// One Sync after the appenders left, so the order check always has a
	// Synced to hold, however the scheduler split the racing ones.
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the final Sync: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	tracks, syncs := 0, 0
	for _, ev := range log {
		if ev.isTrack {
			tracks++
		} else {
			syncs++
		}
	}
	if syncs == 0 {
		t.Fatal("no Synced was reported, so the order was not exercised")
	}
	if tracks != appenders*each {
		t.Fatalf("Track ran %d times for %d appends", tracks, appenders*each)
	}
	// Walked backwards, so minEnd holds the smallest end among the Tracks
	// that arrived after the event, per segment: one pass, however many
	// Synced a hot syncing goroutine logged.
	minEnd := map[core.SegmentID]int64{}
	for i := len(log) - 1; i >= 0; i-- {
		ev := log[i]
		if ev.isTrack {
			if m, ok := minEnd[ev.seg]; !ok || ev.end < m {
				minEnd[ev.seg] = ev.end
			}
			continue
		}
		if m, ok := minEnd[ev.seg]; ok && m <= ev.end {
			t.Fatalf("Synced(%d, %d) was reported before the Track of a record ending at %d", ev.seg, ev.end, m)
		}
	}
}

// TestSinkSealSyncs pins which syncs of a seal reach the sink, and with what
// offset, on every outcome of the seal, through the roll's seal and through
// WriteSeal itself.
func TestSinkSealSyncs(t *testing.T) {
	seg := streamSegName(1)

	t.Run("a full seal", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		placed := sinkAppendN(t, e, 5)
		res, err := e.s.Seal(false)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		// The explicit Sync of the records is the seal's own first step.
		want := []syncedEvent{{1, res.Marker}, {1, streamCap}}
		if got := rec.syncedOf(); !slices.Equal(got, want) {
			t.Fatalf("a full seal reported %+v, want %+v", got, want)
		}
		if end := placed[4].Loc.Off + placed[4].Loc.Len; res.Marker != end {
			t.Fatalf("marker %d, last record ends %d", res.Marker, end)
		}
	})

	t.Run("an early seal", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		sinkAppendN(t, e, 5)
		res, err := e.s.Seal(true)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		want := []syncedEvent{{1, res.Marker}, {1, res.Size}, {1, res.Size}}
		if got := rec.syncedOf(); !slices.Equal(got, want) {
			t.Fatalf("an early seal reported %+v, want %+v", got, want)
		}
	})

	t.Run("an early seal whose truncation fails stops at its second sync", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		sinkAppendN(t, e, 5)
		e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpTruncate, n: 1, err: syscall.EIO})
		res, err := e.s.Seal(true)
		if err != nil || res.TailErr == nil {
			t.Fatalf("Seal = %v, TailErr %v, want a clean seal with a failed tail", err, res.TailErr)
		}
		got := rec.syncedOf()
		if len(got) != 2 || got[0].Through != res.Marker || got[1].Through <= res.Marker {
			t.Fatalf("a seal whose truncation failed reported %+v, want the marker then the trailer's end", got)
		}
	})

	t.Run("an early seal whose truncation sync fails stops at its second sync", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		sinkAppendN(t, e, 5)
		e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpSync, n: 3, err: syscall.EIO})
		res, err := e.s.Seal(true)
		if err != nil || res.TailErr == nil {
			t.Fatalf("Seal = %v, TailErr %v, want a clean seal with a failed tail", err, res.TailErr)
		}
		got := rec.syncedOf()
		if len(got) != 2 || got[0].Through != res.Marker || got[1].Through <= res.Marker {
			t.Fatalf("a seal whose truncation sync failed reported %+v, want the marker then the trailer's end and no third event", got)
		}
	})

	for _, tc := range []struct {
		name string
		nth  int
		want func(marker int64) []syncedEvent
	}{
		{"a failed record sync reports nothing", 1, func(int64) []syncedEvent { return nil }},
		{"a failed step-2 sync reports the records only", 2, func(m int64) []syncedEvent { return []syncedEvent{{1, m}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingSink{}
			e := sinkStream(t, rec.sink())
			placed := sinkAppendN(t, e, 5)
			marker := placed[4].Loc.Off + placed[4].Loc.Len
			e.seam.SetFault(&sealNthFault{seg: seg, op: journaltest.OpSync, n: tc.nth, err: syscall.EIO})
			if _, err := e.s.Seal(false); !errorsIsEnded(err) {
				t.Fatalf("Seal = %v, want ErrSegmentEnded", err)
			}
			if got, want := rec.syncedOf(), tc.want(marker); !slices.Equal(got, want) {
				t.Fatalf("reported %+v, want %+v", got, want)
			}
		})
	}

	t.Run("the roll's seal of the segment it leaves", func(t *testing.T) {
		rec := &recordingSink{}
		e := sinkStream(t, rec.sink())
		fid := core.NewFileID([16]byte{1})
		big := bytes.Repeat([]byte{0xC}, int(streamCap))
		h, body := streamWriteHeader(fid, 0, big, 7, core.Version{Counter: 1})
		for sent := int64(0); sent < int64(len(big)); {
			h.Extent = core.Extent{Off: sent, Len: int64(len(big)) - sent}
			body.Payload = big[sent:]
			p, err := e.s.Append(h, body)
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			sent += p.N
		}
		var first []syncedEvent
		for _, ev := range rec.syncedOf() {
			if ev.Seg == 1 {
				first = append(first, ev)
			}
		}
		if len(first) < 2 || first[len(first)-1].Through <= first[0].Through {
			t.Fatalf("the roll's seal of segment 1 reported %+v, want the records' sync then the footer's", first)
		}
	})

	t.Run("WriteSeal reports through the function it is given", func(t *testing.T) {
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()
		placed := sinkAppendN(t, e, 3)
		entries := sealEntriesOf(t, e, placed)
		marker := placed[2].Loc.Off + placed[2].Loc.Len
		ref, err := e.cache.Acquire(1)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer ref.Release()
		var got []syncedEvent
		_, err = segment.WriteSeal(e.set, ref, streamIdentity, 1, marker, entries, false, func(id core.SegmentID, through int64) {
			got = append(got, syncedEvent{id, through})
		})
		if err != nil {
			t.Fatalf("WriteSeal: %v", err)
		}
		if want := []syncedEvent{{1, marker}, {1, streamCap}}; !slices.Equal(got, want) {
			t.Fatalf("WriteSeal reported %+v, want %+v", got, want)
		}
	})

	t.Run("WriteSeal panics on a nil function before any I/O", func(t *testing.T) {
		e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
		defer func() { _ = e.s.Close() }()
		placed := sinkAppendN(t, e, 3)
		entries := sealEntriesOf(t, e, placed)
		marker := placed[2].Loc.Off + placed[2].Loc.Len
		ref, err := e.cache.Acquire(1)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer ref.Release()
		defer func() {
			msg, _ := recover().(string)
			if !strings.Contains(msg, "reports its syncs") {
				t.Fatalf("WriteSeal with a nil synced function panicked with %q, want the guard's own message", msg)
			}
		}()
		_, _ = segment.WriteSeal(e.set, ref, streamIdentity, 1, marker, entries, false, nil)
	})
}

func errorsIsEnded(err error) bool { return errors.Is(err, segment.ErrSegmentEnded) }

// TestSinkHandOffDoesNotBlockAppend pins the consumer pattern a hand-off sink
// uses: a Track and a Synced that only queue, and a worker that re-enters the
// stream through Sync and then parks. Such a sink neither deadlocks nor stalls
// appends. It does not prove the stream tolerates a blocking sink: the worker
// parks only after Sync returned, holding no stream lock, and the stream calls
// the sink synchronously under its append domain, so "a sink that blocks does
// not block the append" is met by the sink's own contract (it never blocks),
// not enforced by the stream.
func TestSinkHandOffDoesNotBlockAppend(t *testing.T) {
	type event struct {
		track bool
	}
	var (
		mu    sync.Mutex
		queue []event
	)
	signal := make(chan struct{}, 1)
	push := func(ev event) {
		mu.Lock()
		queue = append(queue, ev)
		mu.Unlock()
		select {
		case signal <- struct{}{}:
		default:
		}
	}
	pop := func() (event, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) == 0 {
			return event{}, false
		}
		ev := queue[0]
		queue = queue[1:]
		return ev, true
	}

	e := sinkStream(t, segment.Sink{
		Track:  func(segment.Written) { push(event{track: true}) },
		Synced: func(core.SegmentID, int64) { push(event{}) },
	})

	gate := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			ev, ok := pop()
			if !ok {
				select {
				case <-signal:
					continue
				case <-stop:
					// One last look: an event pushed before stop is
					// processed, not dropped.
					ev, ok = pop()
					if !ok {
						return
					}
				}
			}
			if ev.track {
				_ = e.s.Sync() // re-enters the stream's append domain
				<-gate
			}
		}
	}()

	appended := make(chan struct{})
	go func() {
		defer close(appended)
		for i := range 16 {
			h, body := streamWriteHeader(streamFileID(i+1), int64(i)*64, streamTestBody(64, byte(i)), 7, core.Version{Counter: uint64(i + 1)})
			if _, err := e.s.Append(h, body); err != nil {
				t.Errorf("Append %d: %v", i, err)
				return
			}
		}
	}()
	select {
	case <-appended:
	case <-time.After(10 * time.Second):
		close(gate)
		// The appender may still call t.Errorf, so it ends before the test.
		<-appended
		t.Fatal("16 appends did not return while the sink's worker was parked")
	}

	close(gate)
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not drain after the gate opened")
	}
	if err := e.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSinkEachStreamReportsToItsOwnSink pins that NewStreams asks for one
// sink per stream number and a stream reports only to its own: a Track and a
// Synced of one stream never reach another's sink, so a consumer finds its
// stream's state without a map from segment to stream.
func TestSinkEachStreamReportsToItsOwnSink(t *testing.T) {
	e := rollBuild(t, streamCap, segment.Targets{Content: 4}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })

	const n = 4
	recs := make([]*recordingSink, n)
	asked := map[uint32]int{}
	ss := segment.NewStreams(n, streamIdentity, e.set, e.pool, e.cache, func(no uint32) segment.Sink {
		asked[no]++
		recs[no] = &recordingSink{}
		return recs[no].sink()
	})
	t.Cleanup(func() { _ = ss.Close() })
	for no := uint32(0); no < n; no++ {
		if asked[no] != 1 {
			t.Fatalf("NewStreams asked for stream %d's sink %d times, want once", no, asked[no])
		}
	}

	fid := streamFileID(3)
	h, body := streamWriteHeader(fid, 0, bytes.Repeat([]byte{0xA}, 100), 7, core.Version{Counter: 1})
	s := ss.For(fid)
	if _, err := s.Append(h, body); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	owner := -1
	for no, r := range recs {
		if len(r.tracked()) > 0 {
			if owner != -1 {
				t.Fatalf("streams %d and %d both tracked the record", owner, no)
			}
			owner = no
		}
	}
	if owner == -1 {
		t.Fatal("no stream's sink tracked the record")
	}
	for no, r := range recs {
		r.mu.Lock()
		nSynced := len(r.synced)
		r.mu.Unlock()
		if no == owner && nSynced == 0 {
			t.Errorf("stream %d synced and its own sink saw no Synced", no)
		}
		if no != owner && nSynced != 0 {
			t.Errorf("stream %d's sink saw %d Synced of another stream's sync", no, nSynced)
		}
	}
}
