package format

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestDecodeRecordHeaderClasses pins that each malformed record matches
// exactly one outcome class and comes back as the zero header: bytes under
// another identity — a wrong seed is indistinguishable from damage, as with
// a seal marker — are damaged; a record written under another identity is
// damaged too; a record read at another segment or another offset is at the
// wrong place, its header zero, never filled; a kind this format version
// does not define is unknown, again with the zero header, so no length
// reaches a caller; a non-zero epoch is its own class; and every
// single-bit flip of the 96 header bytes is damage, never wrong place —
// a flip in the segment or the own offset breaks the checksum first.
func TestDecodeRecordHeaderClasses(t *testing.T) {
	release := wantRecordAt(t, "release").h

	for _, c := range []struct {
		name string
		err  error
		b    []byte
		id   core.JournalID
		seg  core.SegmentID
		off  int64
	}{
		{"decoding under another identity", ErrDamaged, readGolden(t, "record_release"), otherIdentity, testSegment, release.Offset},
		{"a record written under another identity", ErrDamaged, AppendRecord(nil, otherIdentity, release, RecordBody{}), testIdentity, testSegment, release.Offset},
		{"read in another segment", ErrWrongPlace, readGolden(t, "record_release"), testIdentity, testSegment + 1, release.Offset},
		{"read at another offset", ErrWrongPlace, readGolden(t, "record_release"), testIdentity, testSegment, release.Offset + 8},
	} {
		h, err := DecodeRecordHeader(c.b, c.id, c.seg, c.off)
		if !errors.Is(err, c.err) {
			t.Errorf("%s: DecodeRecordHeader = %v, want an error matching %v", c.name, err, c.err)
		}
		if h != (RecordHeader{}) {
			t.Errorf("%s: decoded %+v, want the zero header", c.name, h)
		}
	}

	// A wrong length and a wrong magic are damaged as the first steps, at
	// any identity.
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"95 header bytes", readGolden(t, "record_release")[:95]},
		{"97 header bytes", append(slices.Clone(readGolden(t, "record_release")), 0x00)},
		{"empty", nil},
		{"a slot of zeros", make([]byte, RecordHeaderSize)},
		{"a seal marker's magic", func() []byte {
			b := slices.Clone(readGolden(t, "record_release"))
			copy(b[0:4], "DJS1")
			rechecksumRecord(b, testIdentity)
			return b
		}()},
	} {
		h, err := DecodeRecordHeader(c.b, testIdentity, release.Segment, release.Offset)
		if !errors.Is(err, ErrDamaged) || h != (RecordHeader{}) {
			t.Errorf("%s: DecodeRecordHeader = (%+v, %v), want the zero header and ErrDamaged", c.name, h, err)
		}
	}

	// A kind this format version does not define, rechecksummed so only
	// the kind rule can refuse it. 0 is never a kind, 15 the first value
	// reserved for later core kinds, 128 the first extension's.
	for _, kind := range []uint8{0, 15, 128} {
		b := slices.Clone(readGolden(t, "record_release"))
		b[4] = kind
		rechecksumRecord(b, testIdentity)
		h, err := DecodeRecordHeader(b, testIdentity, testSegment, release.Offset)
		if !errors.Is(err, ErrUnknownKind) || h != (RecordHeader{}) {
			t.Errorf("kind %d: DecodeRecordHeader = (%+v, %v), want the zero header and ErrUnknownKind", kind, h, err)
		}
	}

	// A non-zero epoch half, rechecksummed: this format version forbids
	// it on every record, so it is refused in its own class, never as
	// damage.
	b := slices.Clone(readGolden(t, "record_release"))
	binary.LittleEndian.PutUint64(b[56:64], 1)
	rechecksumRecord(b, testIdentity)
	h, err := DecodeRecordHeader(b, testIdentity, testSegment, release.Offset)
	if !errors.Is(err, ErrNonZeroEpoch) || h != (RecordHeader{}) {
		t.Errorf("epoch 1: DecodeRecordHeader = (%+v, %v), want the zero header and ErrNonZeroEpoch", h, err)
	}

	// A verifying unknown kind and a non-zero epoch are refused after the
	// place, so a record at another offset naming either is still at the
	// wrong place first: no reader of an unknown record judges its bytes.
	for kind := range []uint8{0, 15} {
		b := slices.Clone(readGolden(t, "record_release"))
		b[4] = uint8(kind)
		rechecksumRecord(b, testIdentity)
		if _, err := DecodeRecordHeader(b, testIdentity, testSegment, release.Offset+8); !errors.Is(err, ErrWrongPlace) {
			t.Errorf("kind %d at another offset: DecodeRecordHeader = %v, want ErrWrongPlace", kind, err)
		}
	}

	// Every single-bit flip of the 96 header bytes is damage, never wrong
	// place: a flip in the covered bytes [0,92) breaks the checksum, and a
	// flip in the checksum itself breaks the comparison.
	golden := readGolden(t, "record_release")
	for i := range RecordHeaderSize {
		for bit := range 8 {
			b := slices.Clone(golden)
			b[i] ^= 1 << bit
			_, err := DecodeRecordHeader(b, testIdentity, testSegment, release.Offset)
			if !errors.Is(err, ErrDamaged) {
				t.Errorf("header byte %d bit %d flipped: DecodeRecordHeader = %v, want an error matching ErrDamaged", i, bit, err)
			}
		}
	}
}

// TestUnknownKindIsNeverSteppedOver pins that every kind value outside the
// kinds table — 0, which a zero slot never reads as, and each of 15-255 —
// decodes to ErrUnknownKind with the zero header, so no length ever reaches
// a caller that could step over a record it cannot name; and that Size and
// BodySize panic on such a kind instead of inventing one.
func TestUnknownKindIsNeverSteppedOver(t *testing.T) {
	// A release golden: its length and offset still name a real record, so
	// only the kind rule can refuse the byte.
	release := wantRecordAt(t, "release").h

	for kind := range 256 {
		if Kind(kind).known() {
			continue
		}

		b := slices.Clone(readGolden(t, "record_release"))
		b[4] = uint8(kind)
		rechecksumRecord(b, testIdentity)

		h, err := DecodeRecordHeader(b, testIdentity, testSegment, release.Offset)
		if !errors.Is(err, ErrUnknownKind) || h != (RecordHeader{}) {
			t.Errorf("kind %d: DecodeRecordHeader = (%+v, %v), want the zero header and ErrUnknownKind", kind, h, err)
		}
	}

	// Size and BodySize panic on an unknown kind rather than answering
	// with a length a caller could step over, as the decode's zero header
	// denies one to.
	alien := release
	alien.Kind = Kind(200)
	for name, size := range map[string]func() int64{
		"BodySize": alien.BodySize,
		"Size":     alien.Size,
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s on an unknown kind returned; want a panic", name)
				}
			}()
			size()
		}()
	}
}

// TestDecodeRecordHeaderRefusesFields pins that a header whose checksum
// verifies but whose fields break the kinds table's zero-field rules, or
// whose stored values top what a read can hold, is refused as damage: the
// refusal is checked per case against a record rechecksummed for the edit,
// so only the field's own rule can fail it, never a stale checksum.
func TestDecodeRecordHeaderRefusesFields(t *testing.T) {
	base := wantRecordAt(t, "release").h

	// cases is built per case below: name, the bytes, the class. The
	// header decodes at the golden's segment and own offset unless stated.
	type ref struct {
		name string
		set  func(b []byte)
		err  error
	}
	perGolden := map[string][]ref{}

	// Reserved bytes at 5, 6, 7: each is its own case.
	for i := range []int{5, 6, 7} {
		perGolden["release"] = append(perGolden["release"], ref{
			name: "a non-zero reserved byte at " + string(rune('0'+i)),
			set:  func(b []byte) { b[i] = 0xff },
			err:  ErrDamaged,
		})
	}

	// Each 64-bit field that a core int64 must hold, set past its top.
	perGolden["release"] = append(perGolden["release"],
		ref{"file offset above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[32:40], 0x8000000000000000) }, ErrDamaged},
		ref{"length above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 0x8000000000000000) }, ErrDamaged},
		ref{"synced-through offset above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[76:84], 0x8000000000000000) }, ErrDamaged},
		ref{"file offset + length overflows", func(b []byte) {
			binary.LittleEndian.PutUint64(b[32:40], 0xffffffffffffffff)
			binary.LittleEndian.PutUint64(b[40:48], 2)
		}, ErrDamaged},
	)

	// A write whose length leaves no room for its own record at its own
	// offset: offset + Size() overflows.
	maxWrite := slices.Clone(readGolden(t, "record_write"))
	maxWriteOffset := wantRecordAt(t, "write").h.Offset
	{
		binary.LittleEndian.PutUint64(maxWrite[40:48], 0x7fffffffffffffff)
		rechecksumRecord(maxWrite, testIdentity)
		_, err := DecodeRecordHeader(maxWrite, testIdentity, testSegment, maxWriteOffset)
		if !errors.Is(err, ErrDamaged) {
			t.Errorf("a write whose length overflows its own offset: DecodeRecordHeader = %v, want ErrDamaged", err)
		}
	}

	// The kinds table's zero-field rules, per golden.
	perGolden["truncate"] = append(perGolden["truncate"],
		ref{"a truncate with a non-zero length", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 1) }, ErrDamaged})
	perGolden["deallocate"] = append(perGolden["deallocate"],
		ref{"a zero-length deallocate", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 0) }, ErrDamaged})
	perGolden["clone_target"] = append(perGolden["clone_target"],
		ref{"a zero-length clone target", func(b []byte) { rechecksumRecordBody(b, testIdentity, 56); binary.LittleEndian.PutUint64(b[40:48], 0) }, ErrDamaged})
	for _, kind := range []string{"delete", "hold", "stamp", "unhold", "forget"} {
		perGolden[kind] = append(perGolden[kind],
			ref{"a non-zero file offset", func(b []byte) { binary.LittleEndian.PutUint64(b[32:40], 1) }, ErrDamaged},
			ref{"a non-zero length", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 1) }, ErrDamaged})
	}
	for _, kind := range []string{"unhold", "forget"} {
		fid := testRecordFile.Bytes()
		counter := binary.LittleEndian.AppendUint64(nil, 1)
		perGolden[kind] = append(perGolden[kind],
			ref{"a non-zero FileID", func(b []byte) {
				for i, v := range fid {
					b[16+i] = v
				}
			}, ErrDamaged},
			ref{"a non-zero version counter", func(b []byte) {
				for i, v := range counter {
					b[64+i] = v
				}
			}, ErrDamaged})
	}
	perGolden["write"] = append(perGolden["write"],
		ref{"a zero FileID", func(b []byte) { clear(b[16:32]) }, ErrDamaged})

	for kind, refs := range perGolden {
		goldenName := "record_" + kind
		for _, r := range refs {
			b := slices.Clone(readGolden(t, goldenName))
			r.set(b)
			rechecksumRecordBody(b, testIdentity, int(wantRecordAt(t, kind).h.BodySize()))
			rechecksumRecord(b, testIdentity)

			h, err := DecodeRecordHeader(b, testIdentity, testSegment, wantRecordAt(t, kind).h.Offset)
			if !errors.Is(err, r.err) {
				t.Errorf("%s: %s: DecodeRecordHeader = %v, want an error matching %v", kind, r.name, err, r.err)
			}
			if h != (RecordHeader{}) {
				t.Errorf("%s: %s: decoded %+v on refusal, want the zero header", kind, r.name, h)
			}
		}
	}

	_ = base
}
