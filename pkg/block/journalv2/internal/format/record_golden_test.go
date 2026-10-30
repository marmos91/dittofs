package format

import (
	"bytes"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestRecordGoldens pins each of the 14 record goldens three ways: the
// encoder's bytes equal the hand-written hex file, which equals raw's bytes
// — raw is the independent account — and the decoder returns exactly the
// header and body the file holds. The kind byte is checked against the
// kinds table's literal number, never the package's constant, and every
// record ends on a multiple of 8, both in its whole size and in the bytes
// on disk.
//
// The epoch is zero and the counter is not in every golden, so a version
// written as one little-endian 128-bit integer instead of two 64-bit halves
// fails here: it would swap bytes between [56,64) and [64,72) and change the
// header checksum they are covered by.
func TestRecordGoldens(t *testing.T) {
	goldens := recordGoldens(t)
	if len(goldens) != 14 {
		t.Fatalf("%d goldens, want one per kind, 14", len(goldens))
	}

	for name, want := range goldens {
		golden := readGolden(t, "record_"+name)

		if got := golden[4]; got != want.n {
			t.Errorf("record_%s: kind byte at [4] is %d, want the table's literal %d", name, got, want.n)
		}
		if got := AppendRecord(nil, testIdentity, want.h, want.body); !bytes.Equal(got, golden) {
			t.Errorf("record_%s: AppendRecord =\n% x\nwant the golden file\n% x", name, got, golden)
		}
		if got := want.rawOf().Bytes(); !bytes.Equal(got, golden) {
			t.Errorf("record_%s: raw's bytes =\n% x\nwant the golden file\n% x", name, got, golden)
		}

		// Every record starts at a multiple of 8 within its segment and
		// ends on one: its own offset here, and its size, both.
		if want.h.Offset%8 != 0 {
			t.Errorf("record_%s: own offset %d is not a multiple of 8", name, want.h.Offset)
		}
		if len(golden)%8 != 0 {
			t.Errorf("record_%s: the golden is %d bytes, want a multiple of 8", name, len(golden))
		}

		// The golden's checksums come from its bytes: the payload
		// checksum over the body, the header checksum over [0,92).
		body := golden[RecordHeaderSize : RecordHeaderSize+want.h.BodySize()]
		want.h.PayloadCRC = wantSeededCRC(testIdentity, want.bodyBytes())

		h, err := DecodeRecordHeader(golden[:RecordHeaderSize], testIdentity, testSegment, want.h.Offset)
		if err != nil {
			t.Fatalf("record_%s: DecodeRecordHeader = %v, want no error", name, err)
		}
		if h != want.h {
			t.Errorf("record_%s: decoded\n%+v\nwant\n%+v", name, h, want.h)
		}
		if got, wantCRC := h.PayloadCRC, wantSeededCRC(testIdentity, body); got != wantCRC {
			t.Errorf("record_%s: payload checksum is 0x%08x, want the CRC32C of the identity then the body: 0x%08x", name, got, wantCRC)
		}

		verified, err := VerifyRecordBody(body, testIdentity, h)
		if err != nil {
			t.Fatalf("record_%s: VerifyRecordBody = %v, want no error", name, err)
		}
		if !recordBodiesEqual(verified, want.body) {
			t.Errorf("record_%s: verified body\n%+v\nwant\n%+v", name, verified, want.body)
		}

		if got := h.Size(); got != int64(len(golden)) {
			t.Errorf("record_%s: Size = %d, want the golden's %d bytes", name, got, len(golden))
		}
		if got := h.BodySize(); got != int64(len(body)) {
			t.Errorf("record_%s: BodySize = %d, want the golden's %d body bytes", name, got, len(body))
		}
	}
}

// recordBodiesEqual compares two bodies including their payload bytes, which
// == cannot: a slice's value is not its contents.
func recordBodiesEqual(a, b RecordBody) bool {
	if a.MTime != b.MTime || a.Loss != b.Loss || a.Clone != b.Clone || a.Cut != b.Cut {
		return false
	}
	return slices.Equal(a.Payload, b.Payload)
}

// TestRecordGoldenKindNames checks the golden files' names cover the kinds
// table exactly once each, in the table's own order, so no kind's golden can
// quietly go missing.
func TestRecordGoldenKindNames(t *testing.T) {
	goldens := recordGoldens(t)
	for _, want := range []struct {
		name string
		kind Kind
	}{
		{"write", KindWrite},
		{"fill", KindFill},
		{"release", KindRelease},
		{"truncate", KindTruncate},
		{"deallocate", KindDeallocate},
		{"delete", KindDelete},
		{"clone_target", KindCloneTarget},
		{"offloaded", KindOffloaded},
		{"unmark", KindUnmark},
		{"loss", KindLoss},
		{"hold", KindHold},
		{"unhold", KindUnhold},
		{"stamp", KindStamp},
		{"forget", KindForget},
	} {
		if _, ok := goldens[want.name]; !ok {
			t.Errorf("no golden named record_%s for kind %d", want.name, uint8(want.kind))
			continue
		}
		if goldens[want.name].h.Kind != want.kind {
			t.Errorf("golden record_%s holds kind %d, want %d", want.name, uint8(goldens[want.name].h.Kind), uint8(want.kind))
		}
	}
}

// wantRecordAt returns the named golden's record, a small stand-in letting
// other tests reach one golden by name without rebuilding the table.
func wantRecordAt(t *testing.T, name string) wantRecord {
	t.Helper()
	g := recordGoldens(t)
	want, ok := g[name]
	if !ok {
		t.Fatalf("no golden named record_%s", name)
	}
	return want
}

// TestRecordHeaderFileIsZeroForUnholdAndForget pins the kinds table's
// zero-field columns against the goldens: unhold and forget carry neither a
// FileID nor a version, delete, hold, unhold, stamp and forget carry a
// zero offset and length. Every other golden's FileID is non-zero, refused
// as damage when it is not.
func TestRecordHeaderFileIsZeroForUnholdAndForget(t *testing.T) {
	for name, want := range recordGoldens(t) {
		zeroFile := name == "unhold" || name == "forget"
		zeroExtent := name == "delete" || name == "hold" || name == "unhold" || name == "stamp" || name == "forget"
		zeroVersion := zeroFile

		if zeroFile != (want.h.File == (core.FileID{})) {
			t.Errorf("record_%s: FileID zero is %t, want %t", name, want.h.File == (core.FileID{}), zeroFile)
		}
		if zeroVersion != (want.h.Version == (core.Version{})) {
			t.Errorf("record_%s: version zero is %t, want %t", name, want.h.Version == (core.Version{}), zeroVersion)
		}
		if zeroExtent != (want.h.Extent == (core.Extent{})) {
			t.Errorf("record_%s: extent zero is %t, want %t", name, want.h.Extent == (core.Extent{}), zeroExtent)
		}
	}
}

// TestAppendRecordAppendsToDst pins that the encoder appends to dst rather
// than overwriting it, and that the checksums cover only the bytes the call
// appends, never a caller's prefix.
func TestAppendRecordAppendsToDst(t *testing.T) {
	prefix := []byte{0xee, 0xbe, 0xef}
	golden := readGolden(t, "record_release")

	if got := AppendRecord(slices.Clone(prefix), testIdentity, wantRecordAt(t, "release").h, RecordBody{}); !bytes.Equal(got, append(slices.Clone(prefix), golden...)) {
		t.Errorf("AppendRecord with a prefix =\n% x\nwant the prefix then the golden file\n% x", got, append(prefix, golden...))
	}
}
