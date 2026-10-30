package format

import (
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestAppendRecordAllocatesNothing pins the write path's encoder at zero
// allocations per record, into a dst with capacity, for every kind: the
// checksums are derived from the identity's seed without slicing the
// identity, and no field of the record is formatted on the way down. The
// race detector changes allocation counts too much to compare.
func TestAppendRecordAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}

	goldens := recordGoldens(t)

	// Every kind, through a fresh local identity: a local is what escapes
	// to the heap, while a package-level one is already there and would
	// hide the cost.
	identity := testIdentity
	for name, want := range goldens {
		b := AppendRecord(nil, identity, want.h, want.body)
		if got := testing.AllocsPerRun(1000, func() {
			clear(b)
			AppendRecord(b[:0], identity, want.h, want.body)
		}); got != 0 {
			t.Errorf("record_%s: AppendRecord allocates %v times per call, want 0", name, got)
		}
	}

	// And the decode of every kind, the read path's cost.
	for name, want := range goldens {
		b := AppendRecord(nil, identity, want.h, want.body)
		header := b[:RecordHeaderSize]
		if got := testing.AllocsPerRun(1000, func() {
			_, sinkErr = DecodeRecordHeader(header, identity, want.h.Segment, want.h.Offset)
		}); got != 0 {
			t.Errorf("record_%s: DecodeRecordHeader allocates %v times per call, want 0", name, got)
		}
		if sinkErr != nil {
			t.Fatalf("record_%s: DecodeRecordHeader = %v, want no error", name, sinkErr)
		}
	}
}

// TestAppendRecordPanics pins every condition the encoder refuses at the
// call rather than encoding onto disk: a record its own decoder would refuse
// is a caller bug, and the bug is a crash, never a bad record. One case per
// condition, each over a known-good header.
func TestAppendRecordPanics(t *testing.T) {
	write := wantRecordAt(t, "write")
	cloneTarget := wantRecordAt(t, "clone_target")

	for _, c := range []struct {
		name string
		h    RecordHeader
		body RecordBody
	}{
		{"an unknown kind", func() RecordHeader {
			h := write.h
			h.Kind = Kind(15)
			return h
		}(), write.body},
		{"kind 0", func() RecordHeader {
			h := write.h
			h.Kind = Kind(0)
			return h
		}(), write.body},
		{"a negative own offset", func() RecordHeader {
			h := write.h
			h.Offset = -8
			return h
		}(), write.body},
		{"an own offset above MaxUint32", func() RecordHeader {
			h := write.h
			h.Offset = 1 << 32
			return h
		}(), write.body},
		{"an own offset not a multiple of 8", func() RecordHeader {
			h := write.h
			h.Offset++
			return h
		}(), write.body},
		{"a negative file offset", func() RecordHeader {
			h := write.h
			h.Extent.Off = -1
			return h
		}(), write.body},
		{"a negative length", func() RecordHeader {
			h := write.h
			h.Extent.Len = -1
			return h
		}(), write.body},
		{"a negative synced-through offset", func() RecordHeader {
			h := write.h
			h.SyncedThrough = -1
			return h
		}(), write.body},
		{"a write whose payload is not its length", write.h, RecordBody{MTime: write.body.MTime, Payload: write.body.Payload[:4]}},
		{"a write with an empty payload and a non-zero length", write.h, RecordBody{MTime: write.body.MTime}},
		{"a fill carrying a last-modified time", func() RecordHeader {
			h := wantRecordAt(t, "fill").h
			return h
		}(), RecordBody{MTime: 1}},
		{"a release carrying a payload", func() RecordHeader {
			h := wantRecordAt(t, "release").h
			return h
		}(), RecordBody{Payload: []byte{1}}},
		{"a non-zero version epoch", func() RecordHeader {
			h := write.h
			h.Version = core.Version{Epoch: 1, Counter: write.h.Version.Counter}
			return h
		}(), write.body},
		{"a clone target with a non-zero AsOf epoch", cloneTarget.h, RecordBody{Clone: CloneSource{Share: testClone.Share, File: testClone.File, Off: testClone.Off, AsOf: core.Version{Epoch: 1}}}},
		{"a loss reason of 0", wantRecordAt(t, "loss").h, RecordBody{Loss: Loss{Dropped: 1, Synced: true}}},
		{"a loss reason of 5", wantRecordAt(t, "loss").h, RecordBody{Loss: Loss{Dropped: 1, Reason: LossReason(5)}}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: AppendRecord returned; want a panic", c.name)
				}
			}()
			AppendRecord(nil, testIdentity, c.h, c.body)
		}()
	}
}

// TestAppendRecordWritesWhatItIsGiven pins that the encoder writes the
// fields it is handed for a record whose decoder accepts them: a header
// decoded from the encoder's own bytes equals the header in, with the
// payload checksum the body names.
func TestAppendRecordWritesWhatItIsGiven(t *testing.T) {
	for name, want := range recordGoldens(t) {
		b := AppendRecord(nil, testIdentity, want.h, want.body)

		h, err := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, want.h.Segment, want.h.Offset)
		if err != nil {
			t.Fatalf("record_%s: DecodeRecordHeader over the encoder's bytes = %v, want no error", name, err)
		}
		want.h.PayloadCRC = wantSeededCRC(testIdentity, want.bodyBytes())
		if h != want.h {
			t.Errorf("record_%s: decoded %+v, want %+v", name, h, want.h)
		}

		verified, err := VerifyRecordBody(b[RecordHeaderSize:RecordHeaderSize+want.h.BodySize()], testIdentity, h)
		if err != nil {
			t.Fatalf("record_%s: VerifyRecordBody = %v, want no error", name, err)
		}
		if !recordBodiesEqual(verified, want.body) {
			t.Errorf("record_%s: verified %+v, want %+v", name, verified, want.body)
		}

		// A round trip of the body aliases nothing the decoder wrote:
		// the payload is the caller's own bytes, never a copy, and the
		// encoder's buffer holds what was written.
		if !slices.EqualFunc(b[RecordHeaderSize:RecordHeaderSize+want.h.BodySize()], want.bodyBytes(), func(a, bb byte) bool { return a == bb }) {
			t.Errorf("record_%s: the bytes on disk are not the body's", name)
		}
	}
}
