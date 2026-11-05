package segment_test

import (
	"bytes"
	"sync"
	"syscall"
	"testing"

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

// TestSinkTrackBeforeReturnAndAfterWrite pins that Track runs after the
// record's bytes are readable and before Append returns.
func TestSinkTrackBeforeReturnAndAfterWrite(t *testing.T) {
	var e streamEnv
	var tracked, readable bool
	e = sinkStream(t, segment.Sink{
		Track: func(w segment.Written) {
			tracked = true
			raw := make([]byte, format.RecordHeaderSize)
			if _, err := e.s.ReadAt(w.Loc.Segment, raw, w.Loc.Off); err != nil {
				return
			}
			_, err := format.DecodeRecordHeader(raw, streamIdentity, w.Loc.Segment, w.Loc.Off)
			readable = err == nil
		},
		Synced: func(core.SegmentID, int64) {},
	})

	h, body := streamWriteHeader(core.NewFileID([16]byte{1}), 0, []byte("payload"), 1, core.Version{Counter: 1})
	streamAppendWrite(t, e, h, body)
	if !tracked {
		t.Fatal("Append returned before Track ran")
	}
	if !readable {
		t.Error("Track ran before the record's bytes could be read back and decoded")
	}
}

// TestSinkFailedWriteNotTracked pins that a write that fails, or that lands
// short, is not registered, and that the appends after it are.
func TestSinkFailedWriteNotTracked(t *testing.T) {
	for name, rule := range map[string]func(key string) journaltest.Rule{
		"a failed write": func(key string) journaltest.Rule {
			return journaltest.Failing(journaltest.OpWrite, key, 1, syscall.EIO)
		},
		"a short write": func(key string) journaltest.Rule {
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
