package format

import (
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestVerifyRecordBodyRefusesAChangedByte flips every byte of a body, one
// byte at a time — the write's modification time included — and checks that
// each flip is refused as damage with the zero body and no payload slice:
// no part of a record that does not verify is ever handed out. The unchanged
// body verifies first, so each flip is judged against a checksum that matches
// it: a test whose body never verifies would refuse every flip for the wrong
// reason.
func TestVerifyRecordBodyRefusesAChangedByte(t *testing.T) {
	for _, name := range []string{"write", "fill", "loss", "clone_target"} {
		want := wantRecordAt(t, name)
		want.h.PayloadCRC = wantSeededCRC(testIdentity, want.bodyBytes())
		golden := readGolden(t, "record_"+name)
		body := golden[RecordHeaderSize : RecordHeaderSize+want.h.BodySize()]

		verified, err := VerifyRecordBody(body, testIdentity, want.h)
		if err != nil {
			t.Fatalf("record_%s unchanged: VerifyRecordBody = %v, want no error", name, err)
		}
		if !recordBodiesEqual(verified, want.body) {
			t.Fatalf("record_%s unchanged: verified %+v, want the golden's %+v", name, verified, want.body)
		}

		for i := range body {
			b := slices.Clone(golden)
			b[RecordHeaderSize+i] ^= 0xff

			verified, err := VerifyRecordBody(b[RecordHeaderSize:RecordHeaderSize+want.h.BodySize()], testIdentity, want.h)
			if !errors.Is(err, ErrDamaged) {
				t.Errorf("record_%s body byte %d changed: VerifyRecordBody = %v, want an error matching ErrDamaged", name, i, err)
			}
			if !recordBodiesEqual(verified, RecordBody{}) || verified.Payload != nil {
				t.Errorf("record_%s body byte %d changed: verified %+v, want the zero body and no payload", name, i, verified)
			}
		}
	}
}

// TestVerifyRecordBodyRefusesAWrongLength pins that a body of any length
// other than the one the header names is refused before its checksum is
// even read, one byte short or one long included, and comes back as the
// zero body. Each wrong-length body carries a checksum that matches its
// own bytes, so only the length rule can refuse it — the short body's
// checksum is set to the seeded checksum of the bytes it does hold.
func TestVerifyRecordBodyRefusesAWrongLength(t *testing.T) {
	for _, name := range []string{"write", "fill", "loss", "clone_target", "hold"} {
		want := wantRecordAt(t, name)
		body := readGolden(t, "record_"+name)[RecordHeaderSize:]

		for _, n := range []int{int(want.h.BodySize()) - 1, int(want.h.BodySize()) + 1} {
			if n < 0 {
				continue // the empty-body kinds have no shorter length
			}
			// The longer body is built by hand, never a re-slice past
			// the golden: a slice beyond its length would panic before
			// the decoder refused it.
			arg := make([]byte, n)
			copy(arg, body)
			h := want.h
			h.PayloadCRC = wantSeededCRC(testIdentity, arg)
			verified, err := VerifyRecordBody(arg, testIdentity, h)
			if !errors.Is(err, ErrDamaged) {
				t.Errorf("record_%s body of %d bytes, want %d: VerifyRecordBody = %v, want an error matching ErrDamaged", name, n, want.h.BodySize(), err)
			}
			if !recordBodiesEqual(verified, RecordBody{}) || verified.Payload != nil {
				t.Errorf("record_%s body of %d bytes: verified %+v, want the zero body and no payload", name, n, verified)
			}
		}
	}
}

// TestVerifyRecordBodyRefusesFields pins the per-kind body rules: a loss
// record's reason outside 1-5 is refused as damage — reason 4, a
// superseding record, and reason 5, an acknowledged unreadable one, are
// accepted — and so is a flag word holding bits outside bit 0, bit 1 and
// bits 8-15; a clone target's source offset above
// MaxInt64 or overflowing with its length is damage, and a non-zero AsOf
// epoch is refused in its own class. Each case's payload and header are
// rechecksummed, so only the rule can refuse it.
func TestVerifyRecordBodyRefusesFields(t *testing.T) {
	// Loss reasons: 0 and 6 are refused, 4 and 5 accepted.
	for reason, want := range map[byte]error{0: ErrDamaged, 6: ErrDamaged, 4: nil, 5: nil} {
		b := slices.Clone(readGolden(t, "record_loss"))
		// The reason lives at bits 8-15; bits 0 and 1 stay set, as the
		// golden has them.
		binary.LittleEndian.PutUint64(b[104:112], 0x03|uint64(reason)<<8)
		rechecksumRecordBody(b, testIdentity, 16)

		h, err := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, testSegment, wantRecordAt(t, "loss").h.Offset)
		if err != nil {
			t.Fatalf("loss reason %d: DecodeRecordHeader = %v, want no error", reason, err)
		}
		_, err = VerifyRecordBody(b[RecordHeaderSize:112], testIdentity, h)
		if want == nil && err != nil {
			t.Errorf("loss reason %d: VerifyRecordBody = %v, want no error", reason, err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("loss reason %d: VerifyRecordBody = %v, want an error matching %v", reason, err, want)
		}
	}

	// Flag words with a bit outside 0, 1 and 8-15: bit 2 and bit 16 are
	// each refused as damage, rechecksummed so only the flag rule can.
	for _, bit := range []int{2, 16} {
		b := slices.Clone(readGolden(t, "record_loss"))
		binary.LittleEndian.PutUint64(b[104:112], 0x0303|1<<bit)
		rechecksumRecordBody(b, testIdentity, 16)

		h, hdrErr := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, testSegment, wantRecordAt(t, "loss").h.Offset)
		if hdrErr != nil {
			t.Fatalf("loss flag bit %d: DecodeRecordHeader = %v, want no error", bit, hdrErr)
		}
		verified, err := VerifyRecordBody(b[RecordHeaderSize:112], testIdentity, h)
		if !errors.Is(err, ErrDamaged) {
			t.Errorf("loss flag bit %d set: VerifyRecordBody = %v, want an error matching ErrDamaged", bit, err)
		}
		if !recordBodiesEqual(verified, RecordBody{}) {
			t.Errorf("loss flag bit %d set: verified %+v, want the zero body", bit, verified)
		}
	}

	// Clone target: a source offset above MaxInt64, and one that overflows
	// with the header's length — each built from an in-range operand, so
	// only the rule it names can refuse it — are damage; an AsOf epoch is
	// its own class.
	for _, c := range []struct {
		name string
		set  func(b []byte)
		err  error
		say  string
	}{
		{"a source offset above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[128:136], 0x8000000000000000) }, ErrDamaged, "source offset 9223372036854775808 is above MaxInt64"},
		{"a source offset that overflows with the length", func(b []byte) {
			binary.LittleEndian.PutUint64(b[128:136], 0x7fffffffffffffff)
		}, ErrDamaged, "plus length 5135868584551137600"},
		{"a non-zero AsOf epoch", func(b []byte) { binary.LittleEndian.PutUint64(b[136:144], 1) }, ErrNonZeroEpoch, "AsOf epoch 1, want 0"},
	} {
		b := slices.Clone(readGolden(t, "record_clone_target"))
		c.set(b)
		rechecksumRecordBody(b, testIdentity, 56)

		h, hdrErr := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, testSegment, wantRecordAt(t, "clone_target").h.Offset)
		if hdrErr != nil {
			t.Fatalf("%s: DecodeRecordHeader = %v, want no error", c.name, hdrErr)
		}
		verified, err := VerifyRecordBody(b[RecordHeaderSize:152], testIdentity, h)
		if !errors.Is(err, c.err) {
			t.Errorf("%s: VerifyRecordBody = %v, want an error matching %v", c.name, err, c.err)
			continue
		}
		if !strings.Contains(err.Error(), c.say) {
			t.Errorf("%s: VerifyRecordBody = %v, want it to say %q", c.name, err, c.say)
		}
		if !recordBodiesEqual(verified, RecordBody{}) {
			t.Errorf("%s: verified %+v, want the zero body", c.name, verified)
		}
	}
}

// TestRecordPaddingIsIgnored pins that a record's zero padding is ignored,
// as the format says a checksum never covers it and no reader checks it:
// the same record with its padding set reads and verifies exactly the same.
func TestRecordPaddingIsIgnored(t *testing.T) {
	for _, name := range []string{"write", "fill"} {
		want := wantRecordAt(t, name)
		golden := readGolden(t, "record_"+name)
		dirty := slices.Clone(golden)
		for i := RecordHeaderSize + int(want.h.BodySize()); i < len(dirty); i++ {
			dirty[i] = 0xff
		}

		h, err := DecodeRecordHeader(dirty[:RecordHeaderSize], testIdentity, testSegment, want.h.Offset)
		if err != nil {
			t.Fatalf("record_%s with dirty padding: DecodeRecordHeader = %v, want no error", name, err)
		}
		want.h.PayloadCRC = wantSeededCRC(testIdentity, want.bodyBytes())
		if h != want.h {
			t.Errorf("record_%s with dirty padding: decoded %+v, want the golden's %+v", name, h, want.h)
		}

		verified, err := VerifyRecordBody(dirty[RecordHeaderSize:RecordHeaderSize+want.h.BodySize()], testIdentity, h)
		if err != nil {
			t.Fatalf("record_%s with dirty padding: VerifyRecordBody = %v, want no error", name, err)
		}
		if !recordBodiesEqual(verified, want.body) {
			t.Errorf("record_%s with dirty padding: verified body %+v, want the golden's %+v", name, verified, want.body)
		}

		// And the encoder zeroes the padding whatever a caller left in the
		// buffer's tail: the padding is the record's own zero bytes, never
		// a caller's leftovers, so the golden comes back over dirty memory.
		dstBuffer := make([]byte, len(golden))
		for i := range dstBuffer {
			dstBuffer[i] = 0xff
		}
		dst := dstBuffer[:0]
		if got := AppendRecord(dst, testIdentity, want.h, want.body); !slices.Equal(got, golden) {
			t.Errorf("record_%s: AppendRecord over memory filled with 0xff =\n% x\nwant the golden file\n% x", name, got, golden)
		}
	}
}

// TestLossReasonAcknowledgedRoundTrips checks that a loss record naming reason
// 5, an operator's acknowledgement that a record is unreadable, encodes and
// decodes to the same body. A codec that refused reason 5 would drop the
// record at open as damage.
func TestLossReasonAcknowledgedRoundTrips(t *testing.T) {
	want := wantRecordAt(t, "loss")
	body := RecordBody{Loss: Loss{Dropped: testLoss.Dropped, Synced: true, Reason: LossAcknowledged}}
	b := AppendRecord(nil, testIdentity, want.h, body)

	h, err := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, testSegment, want.h.Offset)
	if err != nil {
		t.Fatalf("DecodeRecordHeader = %v, want no error", err)
	}
	got, err := VerifyRecordBody(b[RecordHeaderSize:], testIdentity, h)
	if err != nil {
		t.Fatalf("VerifyRecordBody = %v, want no error", err)
	}
	if got.Loss != body.Loss {
		t.Fatalf("loss body = %+v, want %+v", got.Loss, body.Loss)
	}
}
