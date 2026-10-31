package format

import (
	"fmt"
	"math"
	"slices"
	"strings"
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
// is a caller bug, and the bug is a crash, never a bad record. The bounds the
// decoder refuses as damage — an overflowing extent, a record that leaves no
// room for itself, a clone source a core extent cannot hold — are panics for
// the same reason: what a writer could write and no reader could take back
// never reaches the disk. One case per condition, each over a known-good
// header, and each matched by its own message so a case can never pass on an
// unrelated condition it triggers by accident.
func TestAppendRecordPanics(t *testing.T) {
	write := wantRecordAt(t, "write")
	cloneTarget := wantRecordAt(t, "clone_target")

	for _, c := range []struct {
		name string
		h    RecordHeader
		body RecordBody
		want string
	}{
		{"an unknown kind", func() RecordHeader {
			h := write.h
			h.Kind = Kind(15)
			return h
		}(), write.body, "unknown kind"},
		{"kind 0", func() RecordHeader {
			h := write.h
			h.Kind = Kind(0)
			return h
		}(), write.body, "unknown kind"},
		{"a negative own offset", func() RecordHeader {
			h := write.h
			h.Offset = -8
			return h
		}(), write.body, "record offset -8 is below 0"},
		{"an own offset above MaxUint32", func() RecordHeader {
			h := write.h
			h.Offset = 1 << 32
			return h
		}(), write.body, "record offset"},
		{"an own offset not a multiple of 8", func() RecordHeader {
			h := write.h
			h.Offset++
			return h
		}(), write.body, "not a multiple of 8"},
		{"a negative file offset", func() RecordHeader {
			h := write.h
			h.Extent.Off = -1
			return h
		}(), write.body, "file offset -1 is below 0"},
		{"a negative length", func() RecordHeader {
			h := write.h
			h.Extent.Len = -1
			return h
		}(), write.body, "length -1 is below 0"},
		{"a negative synced-through offset", func() RecordHeader {
			h := write.h
			h.SyncedThrough = -1
			return h
		}(), write.body, "synced-through offset -1 is below 0"},
		{"a file offset that overflows with the length", func() RecordHeader {
			h := write.h
			h.Extent.Off = math.MaxInt64
			h.Extent.Len = 1
			return h
		}(), write.body, "plus the length 1 overflows"},
		{"a write whose length leaves no room for the record itself", func() RecordHeader {
			h := write.h
			h.Extent.Off = 0
			h.Extent.Len = math.MaxInt64 - 50
			return h
		}(), write.body, "size -9223372036854775752 leaves no room for itself"},
		{"a write whose payload is not its length", write.h, RecordBody{MTime: write.body.MTime, Payload: write.body.Payload[:4]}, "payload of 4 bytes"},
		{"a write with an empty payload and a non-zero length", write.h, RecordBody{MTime: write.body.MTime}, "payload of 0 bytes"},
		{"a release carrying a payload", wantRecordAt(t, "release").h, RecordBody{Payload: []byte{1}}, "carries a payload"},
		{"a non-zero version epoch", func() RecordHeader {
			h := write.h
			h.Version = core.Version{Epoch: 1, Counter: write.h.Version.Counter}
			return h
		}(), write.body, "version epoch 1"},
		{"a clone target with a non-zero AsOf epoch", cloneTarget.h, RecordBody{Clone: CloneSource{Share: testClone.Share, File: testClone.File, Off: testClone.Off, AsOf: core.Version{Epoch: 1}}}, "clone AsOf epoch 1"},
		{"a clone target with a negative source offset", cloneTarget.h, RecordBody{Clone: CloneSource{Share: testClone.Share, File: testClone.File, Off: -1, AsOf: testClone.AsOf}}, "clone source offset"},
		{"a clone source offset that overflows with the length", cloneTarget.h, RecordBody{Clone: CloneSource{Share: testClone.Share, File: testClone.File, Off: math.MaxInt64, AsOf: testClone.AsOf}}, "clone source offset"},
		{"a loss reason of 0", wantRecordAt(t, "loss").h, RecordBody{Loss: Loss{Dropped: 1, Synced: true}}, "reason 0"},
		{"a loss reason of 5", wantRecordAt(t, "loss").h, RecordBody{Loss: Loss{Dropped: 1, Reason: LossReason(5)}}, "reason 5"},
		{"a loss carrying a payload", wantRecordAt(t, "loss").h, RecordBody{Loss: testLoss, Payload: []byte{1}}, "carries a payload"},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("%s: AppendRecord returned; want a panic matching %q", c.name, c.want)
					return
				}
				if got := fmt.Sprint(r); !strings.Contains(got, c.want) {
					t.Errorf("%s: AppendRecord panicked with %q, want it to mention %q", c.name, got, c.want)
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
