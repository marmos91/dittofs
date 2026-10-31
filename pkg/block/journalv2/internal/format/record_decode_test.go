package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
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

	// A stored own offset that is not a multiple of 8 is damage, never a
	// header handed out: no record of this format starts anywhere but a
	// multiple of 8, so a caller holding one would hold a header no writer
	// could have produced. It is refused where it says it was read too, so
	// it never comes back as at the wrong place either.
	b = slices.Clone(readGolden(t, "record_release"))
	binary.LittleEndian.PutUint32(b[88:92], 4)
	rechecksumRecord(b, testIdentity)
	h, err = DecodeRecordHeader(b, testIdentity, testSegment, 4)
	if !errors.Is(err, ErrDamaged) || h != (RecordHeader{}) {
		t.Errorf("a misaligned own offset: DecodeRecordHeader = (%+v, %v), want the zero header and ErrDamaged", h, err)
	}

	// A verifying unknown kind and a non-zero epoch are refused after the
	// place, so a record at another offset naming either is still at the
	// wrong place first: no reader of an unknown record judges its bytes.
	for _, kind := range []uint8{0, 15} {
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
// whose stored values top what a read can hold, is refused as damage. Each
// case's record is rechecksummed for its edit, so only the field's own rule
// can fail it, never a stale checksum — and each case first proves the
// unedited bytes decode, so a case can never pass because the golden it
// edits was unusable to begin with. A case's error must also name the rule
// it targets, so a case cannot pass on a refusal another rule made first.
func TestDecodeRecordHeaderRefusesFields(t *testing.T) {
	// name, how the bytes are edited, the class, and a piece of that rule's
	// own message. The header decodes at the golden's segment and own
	// offset unless stated.
	type ref struct {
		name string
		set  func(b []byte)
		err  error
		say  string
	}
	perGolden := map[string][]ref{}

	// Reserved bytes at 5, 6 and 7: each is its own case.
	for _, i := range []int{5, 6, 7} {
		perGolden["release"] = append(perGolden["release"], ref{
			name: fmt.Sprintf("a non-zero reserved byte at %d", i),
			set:  func(b []byte) { b[i] = 0xff },
			err:  ErrDamaged,
			say:  "reserved bytes are not zero",
		})
	}

	// Each 64-bit field that a core int64 must hold, set past its top, then
	// the two overflow rules, each built from in-range halves so only the
	// rule it names can refuse them.
	perGolden["release"] = append(perGolden["release"],
		ref{"file offset above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[32:40], 0x8000000000000000) }, ErrDamaged, "file offset 9223372036854775808 is above MaxInt64"},
		ref{"length above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 0x8000000000000000) }, ErrDamaged, "length 9223372036854775808 is above MaxInt64"},
		ref{"synced-through offset above MaxInt64", func(b []byte) { binary.LittleEndian.PutUint64(b[76:84], 0x8000000000000000) }, ErrDamaged, "synced-through offset 9223372036854775808 is above MaxInt64"},
		ref{"file offset + length overflows", func(b []byte) {
			binary.LittleEndian.PutUint64(b[32:40], 0x7fffffffffffffff)
			binary.LittleEndian.PutUint64(b[40:48], 1)
		}, ErrDamaged, "plus length 1 overflows"},
	)

	// A fill whose length leaves no room for its own record at its own
	// offset: one with file offset 0 is accepted by the extent rules and
	// refused only by offset + Size().
	perGolden["fill"] = append(perGolden["fill"],
		ref{"a length that leaves the record no room at its own offset", func(b []byte) {
			binary.LittleEndian.PutUint64(b[32:40], 0)
			binary.LittleEndian.PutUint64(b[40:48], uint64(math.MaxInt64-100))
		}, ErrDamaged, "overflows"})

	// The kinds table's zero-field rules, per golden.
	perGolden["truncate"] = append(perGolden["truncate"],
		ref{"a truncate with a non-zero length", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 1) }, ErrDamaged, "truncate length 1, want 0"})
	perGolden["deallocate"] = append(perGolden["deallocate"],
		ref{"a zero-length deallocate", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 0) }, ErrDamaged, "length 0, want more"})
	perGolden["clone_target"] = append(perGolden["clone_target"],
		ref{"a zero-length clone target", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 0) }, ErrDamaged, "length 0, want more"})
	for _, kind := range []string{"delete", "hold", "stamp", "unhold", "forget"} {
		perGolden[kind] = append(perGolden[kind],
			ref{"a non-zero file offset", func(b []byte) { binary.LittleEndian.PutUint64(b[32:40], 1) }, ErrDamaged, "file offset 1, want 0"},
			ref{"a non-zero length", func(b []byte) { binary.LittleEndian.PutUint64(b[40:48], 1) }, ErrDamaged, "length 1, want 0"})
	}
	for _, kind := range []string{"unhold", "forget"} {
		fid := testRecordFile.Bytes()
		perGolden[kind] = append(perGolden[kind],
			ref{"a non-zero FileID", func(b []byte) {
				copy(b[16:32], fid[:])
			}, ErrDamaged, "names the FileID"},
			ref{"a non-zero version counter", func(b []byte) {
				binary.LittleEndian.PutUint64(b[64:72], 1)
			}, ErrDamaged, "names version"})
	}

	// Every kind that names a file refuses one with no FileID at all: all
	// twelve but unhold and forget, looped so a later edit that narrows the
	// rule in kinds.go to a few kinds fails here on the kinds it dropped.
	for _, kind := range []string{
		"write", "fill", "release", "truncate", "deallocate", "delete",
		"clone_target", "offloaded", "unmark", "loss", "hold", "stamp",
	} {
		perGolden[kind] = append(perGolden[kind],
			ref{"a zero FileID", func(b []byte) { clear(b[16:32]) }, ErrDamaged, "names no FileID"})
	}

	for kind, refs := range perGolden {
		goldenName := "record_" + kind
		want := wantRecordAt(t, kind)
		bodyLen := int(want.h.BodySize())
		for _, r := range refs {
			b := slices.Clone(readGolden(t, goldenName))

			// The unedited bytes, rechecksummed for the loop's own
			// rewrite, must decode: a case that edits an unusable
			// golden proves nothing.
			rechecksumRecordBody(b, testIdentity, bodyLen)
			if _, err := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, testSegment, want.h.Offset); err != nil {
				t.Fatalf("record_%s before %s: DecodeRecordHeader = %v, want no error", kind, r.name, err)
			}

			r.set(b)
			rechecksumRecordBody(b, testIdentity, bodyLen)

			h, err := DecodeRecordHeader(b[:RecordHeaderSize], testIdentity, testSegment, want.h.Offset)
			if !errors.Is(err, r.err) {
				t.Errorf("record_%s: %s: DecodeRecordHeader = %v, want an error matching %v", kind, r.name, err, r.err)
				continue
			}
			if !strings.Contains(err.Error(), r.say) {
				t.Errorf("record_%s: %s: DecodeRecordHeader = %v, want it to say %q", kind, r.name, err, r.say)
			}
			if h != (RecordHeader{}) {
				t.Errorf("record_%s: %s: decoded %+v on refusal, want the zero header", kind, r.name, h)
			}
		}
	}
}
